package report_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestQueryBothNativeSectionsShareOneCommittedSnapshot(t *testing.T) {
	var turns []usage.Turn
	for range 256 {
		turns = append(turns,
			anthropicTurn("2026-09-01T00:00:00Z", "same-model", usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9}),
			turn("2026-09-01T00:00:00Z", "same-model", usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9}))
	}
	path := stored(t, turns...)
	writer, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err = writer.Exec("PRAGMA busy_timeout=73; PRAGMA synchronous=NORMAL"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, done := make(chan struct{}), make(chan error, 1)
	var commits atomic.Int64
	go func() {
		var writeErr error
		defer func() { done <- writeErr }()
		for revision := 0; ctx.Err() == nil; revision++ {
			a, o := 24, 16024
			if revision%2 != 0 {
				a, o = 12, 8012
			}
			// Stop between transactions rather than canceling a SQL operation:
			// this fixture observes the same writer connection's pragmas below.
			tx, err := writer.Begin()
			if err == nil {
				_, err = tx.Exec("UPDATE anthropic_turn SET input_tokens=?", a)
				if err == nil {
					_, err = tx.Exec("UPDATE openai_turn SET input_tokens=?", o)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				writeErr = err
				if revision == 0 {
					close(ready)
				}
				return
			}
			commits.Add(1)
			if revision == 0 {
				close(ready)
			}
		}
	}()
	joined := false
	defer func() {
		if !joined {
			cancel()
			<-done
		}
	}()
	<-ready
	reporter := report.New(path)
	q := selection()
	q.Surface = "all"
	overlapped := false
	for range 20 {
		before := commits.Load()
		got, err := reporter.Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		overlapped = overlapped || commits.Load() > before
		a, o := got.Anthropic, got.OpenAI
		if a == nil || o == nil || len(a.Rows) != 1 || len(o.Rows) != 1 || len(a.Models) != 1 || len(o.Models) != 1 {
			t.Fatalf("snapshot sections: %+v", got)
		}
		inputA, inputO := *a.Total.Usage["input_tokens"].Sum, *o.Total.Usage["input_tokens"].Sum
		// Only these two states can be committed. A read begun for each
		// Surface independently can mix them into an impossible pair.
		if !(inputA == 3072 && inputO == 2051072 || inputA == 6144 && inputO == 4102144) {
			t.Fatalf("mixed committed snapshots: Anthropic=%d OpenAI=%d", inputA, inputO)
		}
		for _, native := range []struct {
			section *report.Section
			input   int64
		}{{a, inputA}, {o, inputO}} {
			for _, total := range []report.Total{native.section.Total, native.section.Rows[0].Total, native.section.Models[0].Total} {
				if total.Turns != 256 || *total.Usage["input_tokens"].Sum != native.input || total.Usage["input_tokens"].ReportedTurns != 256 || *total.Usage["output_tokens"].Sum != 2304 {
					t.Fatalf("inconsistent snapshot aggregates: %+v", total)
				}
			}
		}
	}
	cancel()
	writeErr := <-done
	joined = true
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if !overlapped {
		t.Fatal("fixture did not commit during a report; snapshot evidence is inconclusive")
	}
	// Observe the real driver's writer connection, not a report-internal SQL
	// hook: read-only reports must not alter its connection-local pragmas/WAL.
	var journal string
	var busy, synchronous, queryOnly int
	if err := writer.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := writer.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := writer.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := writer.QueryRow("PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || busy != 73 || synchronous != 1 || queryOnly != 0 {
		t.Fatalf("writer pragmas changed: %q %d %d %d", journal, busy, synchronous, queryOnly)
	}
	if _, err := writer.Exec("BEGIN IMMEDIATE; ROLLBACK"); err != nil {
		t.Fatalf("report retained writer-blocking resources: %v", err)
	}
	t.Logf("%d atomic two-table commits; commits overlapped reporting without mixed snapshots", commits.Load())
}

func TestQueryReadsCommittedHistoryWithoutFlushingOrRetainingWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	writer, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	reporter := report.New(path)
	before, err := reporter.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(turn("2026-09-01T00:00:00Z", "committed", usage.OpenAIUsage{InputTokens: 7, OutputTokens: 2}))
	// Do not request a report flush. Only the writer's own finalization commits
	// the queued fixture, after which the same reader sees the persisted history.
	writer.Close(context.Background())
	after, err := reporter.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if before.OpenAI.Total.Turns != 0 || after.OpenAI.Total.Turns != 1 || *after.OpenAI.Total.Usage["input_tokens"].Sum != 7 {
		t.Fatalf("independent reports: before=%+v after=%+v", before, after)
	}
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE openai_turn SET input_tokens=99"); err != nil {
		t.Fatal(err)
	}
	during, err := reporter.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if *during.OpenAI.Total.Usage["input_tokens"].Sum != 7 {
		t.Fatal("uncommitted observation escaped")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if *after.OpenAI.Total.Usage["input_tokens"].Sum != 7 {
		t.Fatal("later write mutated independent report")
	}
}

func TestQueryRejectsUTF16SchemaWithoutMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utf16.db")
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA encoding='UTF-16le'"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_requested_model.sql"} {
		script, err := os.ReadFile(filepath.Join("..", "sqlitestore", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(script)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = report.New(path).Query(context.Background(), selection())
	if err == nil {
		t.Fatal("accepted incompatible text encoding")
	}
}

func TestQueryEmptyFutureBucketMetadata(t *testing.T) {
	q := selection()
	q.Since = "9998-12-31"
	q.Until = "9999-01-01"
	result, err := report.New(stored(t)).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 1 || result.Buckets[0].InProgress || result.Buckets[0].RangePartial || result.Buckets[0].UntilDate != "9999-01-01" || result.OpenAI.Rows == nil || result.OpenAI.Models == nil {
		t.Fatalf("future empty report: %+v", result)
	}
	if result.WindowEnd != time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatal("future date overflow")
	}
}
