package report_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func stored(t *testing.T, turns ...usage.Turn) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	s, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		s.Record(turn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	if result := s.Close(ctx); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("store fixture: %+v", result)
	}
	return path
}
func ptr(n int64) *int64 { return &n }
func selection() report.Query {
	return report.Query{Surface: "openai", Period: "day", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-03"}
}
func turn(date, model string, counts usage.OpenAIUsage) usage.Turn {
	at, err := time.Parse(time.RFC3339, date)
	if err != nil {
		panic(err)
	}
	return usage.Turn{At: at, Model: model, Transport: usage.TransportBuffered, Usage: counts}
}
func TestQueryCapsNativeLockWaitingByRemainingBudget(t *testing.T) {
	path := stored(t)
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, err := conn.ExecContext(context.Background(), "ROLLBACK")
		released <- err
	}()
	_, err = report.New(path).Query(context.Background(), selection())
	if releaseErr := <-released; releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("brief lock should fit native cap: %v", err)
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = report.New(path).Query(ctx, selection())
	elapsed := time.Since(started)
	if err == nil || elapsed > 250*time.Millisecond {
		t.Fatalf("contended read elapsed=%s err=%v", elapsed, err)
	}
	t.Logf("native contention with 15ms work remaining returned in %s: %v", elapsed, err)
}

func TestQueryEnforcesWholeReportResourceLimits(t *testing.T) {
	for _, tc := range []struct {
		name, statement                           string
		maxRows, maxGroups, maxDistinctModelBytes int
		cutoff                                    int
		wantAnthropic, wantOpenAI                 int64
	}{
		{
			name:                  "groups",
			statement:             `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<11) INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) SELECT 1788220800000,'','',0,printf('model-%02d',x),'buffered',0,0 FROM n`,
			maxRows:               report.MaxRows,
			maxGroups:             10,
			maxDistinctModelBytes: report.MaxDistinctModelBytes,
			cutoff:                5,
			wantAnthropic:         5,
			wantOpenAI:            6,
		},
		{
			name:                  "distinct model bytes",
			statement:             `INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'','',0,printf('%.*c',6,'a'),'buffered',0,0),(1788220800000,'','',0,printf('%.*c',6,'b'),'buffered',0,0)`,
			maxRows:               report.MaxRows,
			maxGroups:             report.MaxGroups,
			maxDistinctModelBytes: 10,
			cutoff:                1,
			wantAnthropic:         1,
			wantOpenAI:            1,
		},
		{
			name:                  "examined Turns",
			statement:             `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<11) INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) SELECT 1788220800000,'','',0,'x','buffered',0,0 FROM n`,
			maxRows:               10,
			maxGroups:             report.MaxGroups,
			maxDistinctModelBytes: report.MaxDistinctModelBytes,
			cutoff:                5,
			wantAnthropic:         5,
			wantOpenAI:            6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stored(t)
			db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(tc.statement); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			reader := report.NewReadLimitsForTest(path, tc.maxRows, tc.maxGroups, tc.maxDistinctModelBytes)
			got, err := reader.Query(context.Background(), selection())
			var failure *report.Error
			if !errors.As(err, &failure) || failure.Code != report.TooLarge || got.OpenAI != nil {
				t.Fatalf("resource limit: report=%+v error=%v", got, err)
			}
			// Reuse this boundary fixture: split its rows across native tables.
			// Each Surface is now below the injected limit, while the combined
			// query must still fail atomically.
			db, err = sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO anthropic_turn(at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens) SELECT at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens FROM openai_turn WHERE rowid<=?`, tc.cutoff); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`DELETE FROM openai_turn WHERE rowid<=?`, tc.cutoff); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			q := selection()
			q.Surface = "anthropic"
			a, err := reader.Query(context.Background(), q)
			if err != nil || a.Anthropic == nil || a.Anthropic.Total.Turns != tc.wantAnthropic {
				t.Fatalf("Anthropic alone should fit: %+v %v", a, err)
			}
			q.Surface = "openai"
			o, err := reader.Query(context.Background(), q)
			if err != nil || o.OpenAI == nil || o.OpenAI.Total.Turns != tc.wantOpenAI {
				t.Fatalf("OpenAI alone should fit: %+v %v", o, err)
			}
			q.Surface = "all"
			got, err = reader.Query(context.Background(), q)
			if !errors.As(err, &failure) || failure.Code != report.TooLarge || got.OpenAI != nil || got.Anthropic != nil {
				t.Fatalf("request-wide limit: report=%+v error=%v", got, err)
			}
			if tc.name == "examined Turns" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				interrupted := report.NewReadLimitsForTest(path, tc.maxRows, tc.maxGroups, tc.maxDistinctModelBytes)
				report.NotifyAfterExaminedTurnForTest(interrupted, func(examined int) {
					if examined == int(tc.wantAnthropic)+1 {
						cancel()
					}
				})
				got, err = interrupted.Query(ctx, q)
				if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Code != report.Unavailable || got.OpenAI != nil || got.Anthropic != nil {
					t.Fatalf("interrupted scan returned partial report: %+v %v", got, err)
				}
			}
		})
	}
}

func TestQueryFailsWholeReportOnUnavailableOrExcessiveData(t *testing.T) {
	cases := []struct {
		name, sql string
		code      report.Code
	}{
		{"schema", "PRAGMA user_version=99", report.Unavailable},
		{"missing unselected table", "DROP TABLE anthropic_turn", report.Unavailable},
		{"negative", "UPDATE openai_turn SET input_tokens=-1", report.Unavailable},
		{"invalid UTF8", "UPDATE openai_turn SET model=CAST(x'ff' AS TEXT)", report.Unavailable},
		{"huge model", "UPDATE openai_turn SET model=printf('%.*c',1048577,'x')", report.TooLarge},
		{"overflow", "UPDATE openai_turn SET input_tokens=9223372036854775807", report.Overflow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := stored(t, turn("2026-09-01T00:00:00Z", "x", usage.OpenAIUsage{InputTokens: 1}), turn("2026-09-01T00:00:00Z", "x", usage.OpenAIUsage{InputTokens: 1}))
			db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			for _, period := range []string{"day", "week", "month", "year"} {
				for _, zone := range []string{"UTC", "Europe/Berlin"} {
					q := selection()
					q.Period, q.Timezone = period, zone
					got, err := report.New(path).Query(context.Background(), q)
					var failure *report.Error
					if !errors.As(err, &failure) || failure.Code != tc.code || got.OpenAI != nil {
						t.Fatalf("%s/%s: report=%+v err=%v; want %s, no partial report", period, zone, got, err, tc.code)
					}
					if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "SELECT") {
						t.Fatal("private diagnostic escaped")
					}
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "missing", "usage.db")
	_, err := report.New(path).Query(context.Background(), selection())
	var failure *report.Error
	if !errors.As(err, &failure) || failure.Code != report.Unavailable {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("created missing path: %v", err)
	}
	path = stored(t, turn("2026-09-01T00:00:00Z", "max", usage.OpenAIUsage{InputTokens: math.MaxInt64}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := report.New(path).Query(ctx, selection())
	if !errors.Is(err, context.Canceled) || got.OpenAI != nil {
		t.Fatalf("canceled: %+v %v", got, err)
	}
}

func TestQueryRejectsUnsupportedSelectionsBeforeOpeningFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "usage.db")
	for _, change := range []func(*report.Query){
		func(q *report.Query) { q.Surface = "invalid" },
		func(q *report.Query) { q.Period = "fortnight" }, func(q *report.Query) { q.Timezone = "NoSuch/Zone" },
		func(q *report.Query) { q.Timezone = "" }, func(q *report.Query) { q.Since = "2026-9-01" },
		func(q *report.Query) { q.Since = "2026-02-30" }, func(q *report.Query) { q.Until = q.Since },
		func(q *report.Query) { q.Since = "1969-12-31" }, func(q *report.Query) { q.Until = "9999-01-02" },
		func(q *report.Query) { q.Until = "2026-09-31" },
		func(q *report.Query) { v := ""; q.Model = &v },
		func(q *report.Query) { v := "invalid\xff"; q.Model = &v },
	} {
		q := selection()
		change(&q)
		_, err := report.New(path).Query(context.Background(), q)
		var failure *report.Error
		if !errors.As(err, &failure) || failure.Code != report.InvalidQuery {
			t.Errorf("query %+v: %v, want typed invalid_query", q, err)
		}
	}
	q := selection()
	q.Since = "1970-01-01"
	q.Until = "1981-01-01"
	_, err := report.New(path).Query(context.Background(), q)
	var failure *report.Error
	if !errors.As(err, &failure) || failure.Code != report.TooLarge {
		t.Fatalf("calendar limit: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("query touched absent files: %v", err)
	}
}

func TestQueryDailyOpenAINativeCounts(t *testing.T) {
	sse := turn("2026-09-01T12:00:00Z", "z", usage.OpenAIUsage{InputTokens: 10, OutputTokens: 3, CacheWriteTokens: ptr(0)})
	sse.Transport = usage.TransportSSE
	requested := "not-the-reported-model"
	sse.RequestedModel = &requested
	websocket := turn("2026-09-02T23:59:59Z", "A", usage.OpenAIUsage{InputTokens: 9007199254740993, OutputTokens: 1})
	websocket.Transport = usage.TransportWebSocket
	path := stored(t,
		turn("2026-09-01T00:00:00Z", "z", usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9, CachedTokens: ptr(6000), CacheWriteTokens: ptr(2000), ReasoningTokens: ptr(4), TotalTokens: ptr(8021)}),
		sse, websocket,
		turn("2026-09-03T00:00:00Z", "excluded", usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}))
	got, err := report.New(path).Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.Surface != "openai" || got.Timezone != "UTC" || got.Scope != "configured_database" || got.Collection != "best_effort" || len(got.Buckets) != 2 {
		t.Fatalf("metadata: %+v", got)
	}
	s := got.OpenAI
	if s == nil || len(s.Rows) != 2 || len(s.Models) != 2 || s.Models[0].Model != "A" || s.Models[1].Model != "z" || s.Rows[0].BucketStart != "2026-09-01" {
		t.Fatalf("section: %+v", s)
	}
	if s.Total.Turns != 3 || *s.Total.Usage["input_tokens"].Sum != 9007199254749015 || *s.Total.Usage["output_tokens"].Sum != 13 {
		t.Fatalf("total: %+v", s.Total)
	}
	want := map[string]report.Metric{"input_tokens": {Sum: ptr(8022), ReportedTurns: 2}, "output_tokens": {Sum: ptr(12), ReportedTurns: 2}, "cached_tokens": {Sum: ptr(6000), ReportedTurns: 1}, "cache_write_tokens": {Sum: ptr(2000), ReportedTurns: 2}, "reasoning_tokens": {Sum: ptr(4), ReportedTurns: 1}, "total_tokens": {Sum: ptr(8021), ReportedTurns: 1}}
	for name, m := range want {
		g := s.Rows[0].Usage[name]
		if g.Sum == nil || *g.Sum != *m.Sum || g.ReportedTurns != m.ReportedTurns {
			t.Errorf("%s: %+v want %+v", name, g, m)
		}
	}
	if s.Rows[1].Usage["cached_tokens"].Sum != nil || s.Rows[1].Usage["cached_tokens"].ReportedTurns != 0 {
		t.Fatal("absent metric repaired")
	}
}
