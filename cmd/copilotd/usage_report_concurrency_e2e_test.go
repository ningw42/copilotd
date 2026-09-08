package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

const concurrentOpenAICompletion = `{"type":"response.completed","response":{"id":"synthetic-repeated","status":"completed","model":"live-openai","usage":{"input_tokens":8012,"output_tokens":9,"input_tokens_details":{"cached_tokens":6000,"cache_write_tokens":2000},"output_tokens_details":{"reasoning_tokens":4},"total_tokens":8021}}}`
const concurrentAnthropicSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"synthetic-repeated\",\"type\":\"message\",\"model\":\"live-anthropic\",\"usage\":{\"input_tokens\":12,\"output_tokens\":0,\"cache_creation_input_tokens\":2000,\"cache_read_input_tokens\":6000}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func reportSelectionNow() report.Query {
	now := time.Now().UTC()
	// Include either side of midnight during a long test without depending on
	// queued observations becoming visible at any particular wall-clock time.
	return report.Query{Timezone: "UTC", Since: now.AddDate(0, 0, -1).Format(time.DateOnly), Until: now.AddDate(0, 0, 2).Format(time.DateOnly)}
}

func openReportWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA busy_timeout=1000; PRAGMA synchronous=NORMAL"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUsageReportsOverlapNativeInferenceAndAnotherCommittedWriter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(r.Context()); err == nil {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(concurrentOpenAICompletion))
				_ = conn.Close(websocket.StatusNormalClosure, "completed")
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.URL.Path, "messages") {
			_, _ = io.WriteString(w, concurrentAnthropicSSE)
		} else {
			_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", concurrentOpenAICompletion)
		}
	}))
	t.Cleanup(upstream.Close)
	h := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
	writer := openReportWriter(t, h.cfg.UsageDBPath)
	at := time.Now().UnixMilli()
	if _, err := writer.Exec(`INSERT INTO anthropic_turn(at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(?,'','',0,'other-writer','buffered',12,9);
		INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(?,'','',0,'other-writer','buffered',8012,9)`, at, at); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writeDone := make(chan error, 1)
	var commits atomic.Int64
	go func() {
		for ctx.Err() == nil {
			a, o := 12, 8012
			if commits.Load()%2 == 0 {
				a, o = 24, 16024
			}
			tx, err := writer.Begin()
			if err == nil {
				_, err = tx.Exec("UPDATE anthropic_turn SET input_tokens=? WHERE model='other-writer'", a)
				if err == nil {
					_, err = tx.Exec("UPDATE openai_turn SET input_tokens=? WHERE model='other-writer'", o)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				writeDone <- err
				return
			}
			commits.Add(1)
		}
		writeDone <- nil
	}()
	joined := false
	defer func() {
		if !joined {
			cancel()
			<-writeDone
		}
	}()

	var inference sync.WaitGroup
	var completions atomic.Int64
	for _, path := range []string{"/anthropic/v1/messages", "/openai/v1/responses"} {
		inference.Add(1)
		go func() {
			defer inference.Done()
			for range 64 {
				req, _ := http.NewRequest("POST", h.baseURL+path, strings.NewReader(`{"model":"requested-not-reported","stream":true}`))
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "synthetic-repeated") {
					t.Errorf("inference status=%d read=%v", resp.StatusCode, err)
					return
				}
				completions.Add(1)
			}
		}()
	}
	t.Cleanup(inference.Wait)
	client, err := reporthttp.NewClient(h.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	q := reportSelectionNow()
	writeOverlap, inferenceOverlap := false, false
	for range 32 {
		beforeWrites, beforeInference := commits.Load(), completions.Load()
		result, err := client.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		writeOverlap = writeOverlap || commits.Load() > beforeWrites
		inferenceOverlap = inferenceOverlap || completions.Load() > beforeInference
		a, o := result.Report.Anthropic, result.Report.OpenAI
		assertReportCoherent(t, a)
		assertReportCoherent(t, o)
		av, ov := modelInput(t, a, "other-writer"), modelInput(t, o, "other-writer")
		if !(av == 12 && ov == 8012 || av == 24 && ov == 16024) {
			t.Fatalf("impossible two-table commit: Anthropic=%d OpenAI=%d", av, ov)
		}
	}
	inference.Wait()
	if completions.Load() != 128 || !writeOverlap || !inferenceOverlap {
		t.Fatalf("inconclusive overlap: SSE completions=%d writer overlap=%v inference overlap=%v", completions.Load(), writeOverlap, inferenceOverlap)
	}
	conn := dialUsageMeterWebSocket(t, h.baseURL, "report-concurrent-websocket")
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wsCancel()
	if err := conn.Write(wsCtx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	if _, message, err := conn.Read(wsCtx); err != nil || string(message) != concurrentOpenAICompletion {
		t.Fatalf("WebSocket completion: %q %v", message, err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	cancel()
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	joined = true
	if _, err := writer.Exec("BEGIN IMMEDIATE; UPDATE anthropic_turn SET input_tokens=12 WHERE model='other-writer'; UPDATE openai_turn SET input_tokens=8012 WHERE model='other-writer'; COMMIT"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(4 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := client.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		a, o := result.Report.Anthropic, result.Report.OpenAI
		if a.Total.Turns == 65 && o.Total.Turns == 66 {
			assertReportCoherent(t, a)
			assertReportCoherent(t, o)
			for _, want := range []struct {
				section              *report.Section
				input, output, cache int64
			}{{a, 780, 585, 384000}, {o, 528792, 594, 390000}} {
				if *want.section.Total.Usage["input_tokens"].Sum != want.input || *want.section.Total.Usage["output_tokens"].Sum != want.output {
					t.Fatalf("native total: %+v", want.section.Total)
				}
				metric := "cached_tokens"
				if want.section == a {
					metric = "cache_read_input_tokens"
				}
				if *want.section.Total.Usage[metric].Sum != want.cache || want.section.Total.Usage[metric].ReportedTurns != want.section.Total.Turns-1 {
					t.Fatalf("native optional coverage: %+v", want.section.Total)
				}
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("normal writer persistence not observed: Anthropic=%d OpenAI=%d", a.Total.Turns, o.Total.Turns)
		case <-ticker.C:
		}
	}
	var journal string
	var busy, synchronous, queryOnly int
	for _, check := range []struct {
		pragma string
		value  any
	}{{"journal_mode", &journal}, {"busy_timeout", &busy}, {"synchronous", &synchronous}, {"query_only", &queryOnly}} {
		if err := writer.QueryRow("PRAGMA " + check.pragma).Scan(check.value); err != nil {
			t.Fatal(err)
		}
	}
	if journal != "wal" || busy != 1000 || synchronous != 1 || queryOnly != 0 {
		t.Fatalf("writer settings changed: %s %d %d %d", journal, busy, synchronous, queryOnly)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	assertCleanUsageReport(t, h.closeStore())
	t.Logf("128 native SSE completions, one WebSocket Turn, %d other-writer commits; real writes and inference completed during HTTP reports", commits.Load())
}

func modelInput(t *testing.T, section *report.Section, model string) int64 {
	t.Helper()
	for _, total := range section.Models {
		if total.Model == model {
			return *total.Usage["input_tokens"].Sum
		}
	}
	t.Fatalf("missing model %q", model)
	return 0
}

func assertReportCoherent(t *testing.T, section *report.Section) {
	t.Helper()
	if section == nil {
		t.Fatal("missing selected section")
	}
	// Independent conservation checks supplement literal native fixture totals.
	// These are not the production aggregation algorithm or a client recalculation.
	var turns, input, output int64
	for _, model := range section.Models {
		turns += model.Turns
		input += *model.Usage["input_tokens"].Sum
		output += *model.Usage["output_tokens"].Sum
		var rows []report.Row
		for _, row := range section.Rows {
			if row.Model == model.Model {
				rows = append(rows, row)
			}
		}
		if len(rows) == 1 && !reflect.DeepEqual(rows[0].Total, model.Total) {
			t.Fatal("single-bucket row and model totals disagree")
		}
		var rowTurns int64
		for _, row := range rows {
			rowTurns += row.Turns
		}
		if rowTurns != model.Turns {
			t.Fatal("row/model Turn counts disagree")
		}
	}
	if section.Total.Turns != turns || *section.Total.Usage["input_tokens"].Sum != input || *section.Total.Usage["output_tokens"].Sum != output || section.Total.Usage["input_tokens"].ReportedTurns != turns || section.Total.Usage["output_tokens"].ReportedTurns != turns {
		t.Fatalf("incoherent native section: %+v", section.Total)
	}
}
