package report_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

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
