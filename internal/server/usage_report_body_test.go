package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// reportBodyFixtureWatchdog bounds test setup and phase coordination that is
// not itself a latency contract. Keep it wider than the route's real five-second
// deadline so transient hosted-runner I/O does not fail the fixture first.
const reportBodyFixtureWatchdog = 10 * time.Second

// orderedReportDeadlineWriter controls notification ordering at the public HTTP
// boundary, not the handler's five-second deadline values. In the read-first
// case, defer delivering the native write deadline until the real request-body
// drain and flush finish. In the write-first case, wait for that same installed
// deadline before allowing the handler's first write. Ordinary cases use neither.
// These controlled cases expose both outcomes without relying on a timer race;
// they are not evidence of unmodified native write-timer scheduling.
type orderedReportDeadlineWriter struct {
	http.ResponseWriter
	readFirst    bool
	readDeadline time.Time
	deadline     time.Time
}

func (w *orderedReportDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *orderedReportDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
}
func (w *orderedReportDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.Equal(w.readDeadline) {
		return fmt.Errorf("report read/write deadlines differ: %s / %s", w.readDeadline, deadline)
	}
	w.deadline = deadline
	if w.readFirst {
		return nil
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}
func (w *orderedReportDeadlineWriter) Write(p []byte) (int, error) {
	if !w.readFirst {
		<-time.NewTimer(time.Until(w.deadline)).C
	}
	return w.ResponseWriter.Write(p)
}

func startReportBodyServer(t *testing.T, query reporthttp.QueryFunc, order string) string {
	t.Helper()
	logger := discardLogger(t)
	provider := readyStub("")
	fwd := newTestForwarder(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)
	handler := reporthttp.Handler(query)
	edge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pin only incidental HTTP metadata, so a raw prefix comparison also
		// validates partial headers. Request IDs are supplied by each client.
		w.Header().Set("Date", "Mon, 07 Sep 2026 12:00:00 GMT")
		if order == "read_first" {
			// Literal syntax-error body size. This control delivers a complete
			// original response; read_first_chunked also exercises a prefix
			// whose final chunk is prevented by the delayed write notification.
			w.Header().Set("Content-Length", "98")
		}
		if order == "" || (order == "write_first" && r.ContentLength == 0) {
			handler.ServeHTTP(w, r)
			return
		}
		ordered := &orderedReportDeadlineWriter{ResponseWriter: w, readFirst: strings.HasPrefix(order, "read_first")}
		handler.ServeHTTP(ordered, r)
		if ordered.readFirst {
			// Flush uses net/http's real implicit drain, released by the exact
			// native read deadline. Only its competing write notification is held.
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("read-first original response flush: %v", err)
			}
			if err := http.NewResponseController(w).SetWriteDeadline(ordered.deadline); err != nil {
				t.Error(err)
			}
		}
	})
	srv := New(testConfig(), logger, logger, newTestDependencyErrorLog(), provider, newTestReadyObservers(), fwd, newTestCatalogSource(provider), newTestWSProxy(provider), NewStreamOutcomeCounter(), catalog.RenderDescriptors{}, edge)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String()
}

func closeReportBodyFixtureStore(t *testing.T, store *sqlitestore.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), reportBodyFixtureWatchdog)
	defer cancel()
	report := store.Close(ctx)
	if !report.DriverCleanupCompleted || report.FinalFlushLosses != 0 {
		t.Fatalf("fixture writer did not close within %s: %+v", reportBodyFixtureWatchdog, report)
	}
}

// Capture a separately completed, Connection: close exchange on a fresh TCP
// connection. Its independently checked status/headers/body are the byte oracle
// for incomplete requests, including HTTP framing. No parser for partial HTTP is
// needed: zero bytes or an exact prefix is allowed; unrelated or appended bytes
// cannot be a prefix. Neither this control nor the attack uses client close to
// finish the read.
func reportBodyControl(t *testing.T, addr, method, target string, status int) ([]byte, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(reportBodyFixtureWatchdog)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nX-Request-Id: report-body-fixture\r\nConnection: close\r\n\r\n", method, target); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("control did not reach server EOF: %v", err)
	}
	reader := bufio.NewReader(bytes.NewReader(wire))
	response, err := http.ReadResponse(reader, &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	extra, extraErr := io.ReadAll(reader)
	if err != nil || extraErr != nil || response.StatusCode != status || len(extra) != 0 {
		t.Fatalf("control status=%d want=%d error=%v trailing-error=%v extra=%q", response.StatusCode, status, err, extraErr, extra)
	}
	wantHeaders := map[string]string{"X-Request-Id": "report-body-fixture", "Date": "Mon, 07 Sep 2026 12:00:00 GMT", "Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}
	if status == 500 {
		wantHeaders["Content-Type"] = "text/plain; charset=utf-8"
		wantHeaders["Cache-Control"] = ""
		wantHeaders["X-Content-Type-Options"] = ""
	}
	if status == 405 {
		wantHeaders["Allow"] = "GET, HEAD"
	}
	for key, want := range wantHeaders {
		if got := response.Header.Get(key); got != want {
			t.Fatalf("control %s=%q want=%q", key, got, want)
		}
	}
	return wire, body
}

func TestUsageIncompleteRequestBodiesReleaseReportSlotsWithinWriteBudget(t *testing.T) {
	for _, framing := range []struct{ name, headers, order string }{
		{"content_length", "Content-Length: 1\r\n\r\n", ""},
		{"chunked", "Transfer-Encoding: chunked\r\n\r\n1\r\n", ""},
		{"content_length_read_first", "Content-Length: 1\r\n\r\n", "read_first_chunked"},
	} {
		t.Run(framing.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "usage.db")
			store, err := sqlitestore.Open(path, logging.ForComponent(discardLogger(t), "internal/usage/sqlitestore"))
			if err != nil {
				t.Fatal(err)
			}
			// Legal, small enough not to exhaust TCP buffers, but larger than
			// net/http's response buffer: Write must flush while holding its slot.
			store.Record(usage.Turn{At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Model: strings.Repeat("m", 4096), Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
			closeReportBodyFixtureStore(t, store)
			reader := report.New(path)
			completed := make(chan struct{}, 2)
			addr := startReportBodyServer(t, func(ctx context.Context, q report.Query) (report.Report, error) {
				result, err := reader.Query(ctx, q)
				// Keep the real SQLite query/materialization; pin only its public
				// result's incidental query clock for the wire-control comparison.
				result.GeneratedAt = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
				if q.Model != nil {
					// Observe the approved QueryFunc boundary, not internal SQL or
					// handler state. Do not race a status probe for admission before
					// both incomplete requests have finished real materialization.
					completed <- struct{}{}
				}
				return result, err
			}, framing.order)
			transport := http.DefaultTransport.(*http.Transport).Clone()
			client := &http.Client{Transport: transport, Timeout: reportBodyFixtureWatchdog}
			t.Cleanup(client.CloseIdleConnections)
			target := reporthttp.Path + "?timezone=UTC&since=2026-09-01&until=2026-09-02&surface=openai"
			attackTarget := target + "&model=" + strings.Repeat("m", 4096)
			original, body := reportBodyControl(t, addr, "GET", attackTarget, 200)
			<-completed
			var control report.Report
			if err := json.Unmarshal(body, &control); err != nil {
				t.Fatal(err)
			}
			if len(body) < 8192 || len(body) > 16384 || control.SchemaVersion != 1 || control.Surface != "openai" || control.Anthropic != nil || control.Model == nil || *control.Model != strings.Repeat("m", 4096) || control.OpenAI == nil {
				t.Fatalf("materializable native report: bytes=%d", len(body))
			}
			section := control.OpenAI
			if len(section.Rows) != 1 || len(section.Models) != 1 {
				t.Fatalf("native rows=%d models=%d", len(section.Rows), len(section.Models))
			}
			for _, total := range []report.Total{section.Rows[0].Total, section.Models[0].Total, section.Total} {
				if total.Turns != 1 || total.Usage["input_tokens"].Sum == nil || *total.Usage["input_tokens"].Sum != 1 || total.Usage["output_tokens"].Sum == nil || *total.Usage["output_tokens"].Sum != 2 || total.Usage["total_tokens"].Sum != nil {
					t.Fatalf("control native counts: %+v", total)
				}
			}
			started := time.Now()
			type result struct {
				body []byte
				err  error
			}
			done := make(chan result, 2)
			for range 2 {
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				// Cleanup, not the behavioral test, closes attackers even on RED.
				t.Cleanup(func() { _ = conn.Close() })
				if err := conn.SetDeadline(started.Add(7500 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: localhost\r\nX-Request-Id: report-body-fixture\r\n%s", attackTarget, framing.headers); err != nil {
					t.Fatal(err)
				}
				go func() {
					body, err := io.ReadAll(conn)
					done <- result{body, err}
				}()
			}
			for range 2 {
				select {
				case <-completed:
				case <-time.After(reportBodyFixtureWatchdog):
					t.Fatal("real query did not materialize before network waiting")
				}
			}
			status := func() int {
				response, err := client.Head("http://" + addr + target)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				return response.StatusCode
			}
			if got := status(); got != 429 {
				t.Fatalf("materialized reports did not hold both slots during network waiting: %d", got)
			}
			for range 2 {
				result := <-done
				if result.err != nil {
					t.Fatalf("withheld request body outlived 5s write budget plus tolerance: %v; later report status=%d", result.err, status())
				}
				if !bytes.HasPrefix(original, result.body) {
					t.Fatalf("incomplete-body response is not a prefix of the original 200: %q", result.body)
				}
				if framing.order != "" && len(result.body) == 0 {
					t.Fatal("read-first control did not deliver original report bytes")
				}
				t.Logf("server EOF: original response bytes=%d/%d", len(result.body), len(original))
			}
			elapsed := time.Since(started)
			if got := status(); elapsed < 4500*time.Millisecond || elapsed > 7500*time.Millisecond || got != 200 {
				t.Fatalf("report slots not released at route budget: elapsed=%s status=%d", elapsed, got)
			}
			t.Logf("two real queries materialized; withheld request bodies returned without client close in %s; later report=200", elapsed)
		})
	}
}

func TestReportIncompleteBodiesBoundFinalFlushAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, method, query string
		status              int
		invoked             bool
		order               string
	}{
		{"small_success", "GET", "timezone=UTC", 200, true, ""},
		{"head", "HEAD", "timezone=UTC", 200, true, ""},
		{"method", "POST", "timezone=%xx", 405, false, ""},
		{"disabled", "GET", "timezone=%xx", 503, false, ""},
		{"syntax", "GET", "timezone=%xx", 400, false, ""},
		{"semantic", "GET", "timezone=invalid", 400, true, ""},
		{"panic", "GET", "timezone=UTC", 500, true, ""},
		{"syntax_read_first", "GET", "timezone=%xx", 400, false, "read_first"},
		{"syntax_write_first", "GET", "timezone=%xx", 400, false, "write_first"},
		{"syntax_read_first_chunked", "GET", "timezone=%xx", 400, false, "read_first_chunked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger := discardLogger(t)
			path := filepath.Join(t.TempDir(), "private", "usage.db")
			store, err := sqlitestore.Open(path, logging.ForComponent(logger, "internal/usage/sqlitestore"))
			if err != nil {
				t.Fatal(err)
			}
			closeReportBodyFixtureStore(t, store)
			reader := report.New(path)
			completed := make(chan struct{}, 2)
			var query reporthttp.QueryFunc = func(ctx context.Context, q report.Query) (report.Report, error) {
				defer func() { completed <- struct{}{} }()
				if tc.name == "panic" {
					panic("synthetic report failure")
				}
				result, err := reader.Query(ctx, q)
				result.GeneratedAt = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
				return result, err
			}
			if tc.name == "disabled" {
				query = nil
			}
			addr := startReportBodyServer(t, query, tc.order)
			// Establish the usual response contract separately from the incomplete
			// body. Both use the same production handler and recovery chain;
			// ordered cases control only the public ResponseWriter boundary.
			target := reporthttp.Path + "?" + tc.query + "&since=2026-09-01&until=2026-09-02"
			original, controlBody := reportBodyControl(t, addr, tc.method, target, tc.status)
			switch tc.status {
			case 200:
				if tc.method == "HEAD" {
					if len(controlBody) != 0 {
						t.Fatalf("HEAD body: %q", controlBody)
					}
					break
				}
				var control report.Report
				if err := json.Unmarshal(controlBody, &control); err != nil || control.SchemaVersion != 1 || control.Surface != "all" || control.Anthropic == nil || control.OpenAI == nil {
					t.Fatalf("empty native control report: %s error=%v", controlBody, err)
				}
				for _, section := range []*report.Section{control.Anthropic, control.OpenAI} {
					if len(section.Rows) != 0 || len(section.Models) != 0 || section.Total.Turns != 0 || section.Total.Usage["input_tokens"].Sum == nil || *section.Total.Usage["input_tokens"].Sum != 0 || section.Total.Usage["output_tokens"].Sum == nil || *section.Total.Usage["output_tokens"].Sum != 0 {
						t.Fatalf("control invented native usage: %+v", section)
					}
				}
			case 500:
				if string(controlBody) != "internal server error" {
					t.Fatalf("generic recovery control: %q", controlBody)
				}
			default:
				wantCode := map[int]string{400: "invalid_query", 405: "method_not_allowed", 503: "usage_meter_disabled"}[tc.status]
				var control struct {
					Version int                            `json:"schema_version"`
					Error   struct{ Code, Message string } `json:"error"`
				}
				if err := json.Unmarshal(controlBody, &control); err != nil || control.Version != 1 || control.Error.Code != wantCode || control.Error.Message == "" {
					t.Fatalf("control error envelope: %s error=%v", controlBody, err)
				}
				if strings.HasPrefix(tc.name, "syntax") && string(controlBody) != `{"schema_version":1,"error":{"code":"invalid_query","message":"Invalid report query parameters."}}` {
					t.Fatalf("syntax control: %q", controlBody)
				}
			}
			if tc.invoked {
				<-completed
			}
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			started := time.Now()
			if err := conn.SetDeadline(started.Add(7500 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nX-Request-Id: report-body-fixture\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n", tc.method, target); err != nil {
				t.Fatal(err)
			}
			if tc.invoked {
				select {
				case <-completed:
				case <-time.After(reportBodyFixtureWatchdog):
					t.Fatal("query did not finish before blocked final flush")
				}
			}
			body, err := io.ReadAll(conn)
			elapsed := time.Since(started)
			if err != nil || elapsed < 4500*time.Millisecond || elapsed > 7500*time.Millisecond {
				t.Fatalf("post-handler flush/body cleanup: elapsed=%s error=%v response=%q", elapsed, err, body)
			}
			if !bytes.HasPrefix(original, body) {
				t.Fatalf("incomplete-body response is not a prefix of the original %d: %q; original=%q", tc.status, body, original)
			}
			switch tc.order {
			case "read_first":
				if !bytes.Equal(original, body) {
					t.Fatal("read-first control did not deliver the complete original response")
				}
			case "write_first":
				if len(body) != 0 {
					t.Fatal("write-first control unexpectedly delivered bytes")
				}
			case "read_first_chunked":
				if len(body) == 0 || len(body) == len(original) {
					t.Fatal("chunked read-first control did not deliver a partial original response")
				}
			}
			t.Logf("query/control status=%d; server EOF in %s without client close; original response bytes=%d/%d", tc.status, elapsed, len(body), len(original))
		})
	}
}
