package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
)

func TestFinishWritesFullAndPartialFinalBatches(t *testing.T) {
	path, store := newFinalBatchStore(t)
	const turnCount = 257
	for index := range turnCount {
		store.Record(finalBatchTurn(index, usage.TransportBuffered))
	}

	// Model the partial batch already owned by runWriter when finalization starts.
	initial := make([]usage.Turn, 0, batchCapacity)
	for range 17 {
		initial = append(initial, <-store.queue)
	}
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store.finish(ctx, initial)

	assertFinalBatchCleanup(t, store)
	if report := store.settleFinalReport(store.cleanupSuccessful.Load()); report != (Report{DriverCleanupCompleted: true}) {
		t.Fatalf("final report = %+v, want all %d Turns committed", report, turnCount)
	}
	if got, want := finalBatchResponseIDs(t, path), finalBatchIDs(0, turnCount); !slices.Equal(got, want) {
		t.Fatalf("persisted response IDs = %v, want %v", got, want)
	}
}

func TestFinishStopsAfterFailedFullBatchAndDiscardsRemainingPartial(t *testing.T) {
	path, store := newFinalBatchStore(t)
	const turnCount = 257
	for index := range turnCount {
		transport := usage.TransportBuffered
		if index == 200 {
			transport = usage.Transport("invalid")
		}
		store.Record(finalBatchTurn(index, transport))
	}

	// The first final batch combines writer-owned and queued Turns. The invalid
	// Turn poisons the second full transaction; Turn 256 remains queued.
	initial := make([]usage.Turn, 0, batchCapacity)
	for range 17 {
		initial = append(initial, <-store.queue)
	}
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store.finish(ctx, initial)

	assertFinalBatchCleanup(t, store)
	if queued := len(store.queue); queued != 0 {
		t.Errorf("queued Turns after failed final batch = %d, want discarded", queued)
	}
	wantReport := Report{FinalFlushLosses: 129, DriverCleanupCompleted: true}
	if report := store.settleFinalReport(store.cleanupSuccessful.Load()); report != wantReport {
		t.Fatalf("final report = %+v, want %+v (failed batch and later partial lost without runtime classification)", report, wantReport)
	}
	if got, want := finalBatchResponseIDs(t, path), finalBatchIDs(0, batchCapacity); !slices.Equal(got, want) {
		t.Fatalf("persisted response IDs = %v, want only first committed batch %v", got, want)
	}
}

func newFinalBatchStore(t *testing.T) (string, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if err := preparePrivateDatabase(path); err != nil {
		t.Fatal(err)
	}
	db, conn, err := admit(path, sql.Open)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{
		conn:        conn,
		db:          db,
		queue:       make(chan usage.Turn, queueCapacity),
		cleanupDone: make(chan struct{}),
	}
	store.admitting.Store(true)
	t.Cleanup(func() {
		select {
		case <-store.cleanupDone:
		default:
			_ = conn.Close()
			_ = db.Close()
		}
	})
	return path, store
}

func finalBatchTurn(index int, transport usage.Transport) usage.Turn {
	return usage.Turn{
		At:         time.UnixMilli(int64(index + 1)),
		ResponseID: fmt.Sprintf("final-%03d", index),
		Model:      "m",
		Transport:  transport,
		Usage:      usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1},
	}
}

func finalBatchIDs(first, last int) []string {
	ids := make([]string, 0, last-first)
	for index := first; index < last; index++ {
		ids = append(ids, fmt.Sprintf("final-%03d", index))
	}
	return ids
}

func finalBatchResponseIDs(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT response_id FROM openai_turn ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func assertFinalBatchCleanup(t *testing.T, store *Store) {
	t.Helper()
	select {
	case <-store.cleanupDone:
		if !store.cleanupSuccessful.Load() {
			t.Fatal("writer cleanup completed unsuccessfully")
		}
	default:
		t.Fatal("writer cleanup did not complete")
	}
}
