package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
)

func TestStoreBatchFillFlushesBeforeTimer(t *testing.T) {
	// Bound the entire observation before opening the store. A one-hour timer
	// cannot satisfy this oracle, even if the writer starts before Record.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := openStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)), sql.Open, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if report := store.Close(ctx); report != (Report{DriverCleanupCompleted: true}) {
			t.Errorf("Close report = %+v", report)
		}
	}()

	for i := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(i + 1)), ResponseID: fmt.Sprintf("fill-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var count int
		err := db.QueryRowContext(ctx, "SELECT count(*) FROM openai_turn").Scan(&count)
		if err == nil && count == 128 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("externally visible rows = %d, %v; want 128 from batch fill before timer or Close", count, err)
		case <-poll.C:
		}
	}
}
