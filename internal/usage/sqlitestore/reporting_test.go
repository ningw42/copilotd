package sqlitestore_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

type observedStoreLog struct {
	level            slog.Level
	msg              string
	cleanupCompleted *bool
	queueFullDrops   uint64
}

type storeLogHandler struct {
	mu      sync.Mutex
	records []observedStoreLog

	delayRuntime   bool
	runtimeEntered chan struct{}
	releaseRuntime chan struct{}
	enteredOnce    sync.Once
}

func (h *storeLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *storeLogHandler) Handle(_ context.Context, record slog.Record) error {
	if h.delayRuntime && record.Message == "usage observations lost" {
		h.enteredOnce.Do(func() { close(h.runtimeEntered) })
		<-h.releaseRuntime
	}
	observed := observedStoreLog{level: record.Level, msg: record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == logging.QueueFullDropsKey && attr.Value.Kind() == slog.KindUint64 {
			observed.queueFullDrops = attr.Value.Uint64()
		}
		if attr.Key == logging.DriverCleanupCompletedKey && attr.Value.Kind() == slog.KindBool {
			completed := attr.Value.Bool()
			observed.cleanupCompleted = &completed
		}
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, observed)
	h.mu.Unlock()
	return nil
}

func (h *storeLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *storeLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *storeLogHandler) snapshot() []observedStoreLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]observedStoreLog(nil), h.records...)
}

func fillRejectedRuntimeBatch(store *sqlitestore.Store) {
	for index := range 128 {
		transport := usage.TransportBuffered
		if index == 127 {
			transport = usage.Transport("invalid")
		}
		store.Record(usage.Turn{
			At: time.UnixMilli(int64(index + 1)), ResponseID: fmt.Sprintf("rejected-%d", index),
			Model: "m", Transport: transport,
			Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}
}

func waitForStoreLog(t *testing.T, handler *storeLogHandler, message string, timeout time.Duration) observedStoreLog {
	t.Helper()
	return waitForStoreLogAfter(t, handler, message, 0, timeout)
}

func waitForStoreLogAfter(t *testing.T, handler *storeLogHandler, message string, after int, timeout time.Duration) observedStoreLog {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		records := handler.snapshot()
		for _, record := range records[min(after, len(records)):] {
			if record.msg == message {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("log %q not observed after %d; records=%+v", message, after, records)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStoreRecoveredWriteFailureDoesNotPoisonLaterOrFinalLevels(t *testing.T) {
	handler := &storeLogHandler{}
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, slog.New(handler))
	if err != nil {
		t.Fatal(err)
	}
	fillRejectedRuntimeBatch(store)
	first := waitForStoreLog(t, handler, "usage observations lost", 2*time.Second)
	if first.level != slog.LevelWarn {
		t.Errorf("first contained write failure level = %s, want WARN", first.level)
	}

	for index := range 128 {
		store.Record(usage.Turn{
			At: time.UnixMilli(int64(index + 1000)), ResponseID: fmt.Sprintf("recovered-%d", index),
			Model: "m", Transport: usage.TransportBuffered,
			Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1},
		})
	}
	recovered := waitForStoreLog(t, handler, "usage storage recovered", 2*time.Second)
	if recovered.level == slog.LevelError {
		t.Errorf("recovery level = %s, must not remain ERROR because of cumulative history", recovered.level)
	}

	beforePressure := len(handler.snapshot())
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for index := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(index + 2000)), ResponseID: fmt.Sprintf("pressure-writer-%d", index), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	time.Sleep(100 * time.Millisecond)
	for index := range 1025 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(index + 3000)), ResponseID: fmt.Sprintf("pressure-queue-%d", index), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	pressure := waitForStoreLogAfter(t, handler, "usage observations lost", beforePressure, 2*time.Second)
	if pressure.level == slog.LevelError {
		t.Errorf("recovered queue-pressure level = %s, want contained WARN", pressure.level)
	}

	store.StopAdmission()
	if pressure.queueFullDrops == 0 || pressure.queueFullDrops > 1025 {
		t.Fatalf("queue pressure fixture drops = %d", pressure.queueFullDrops)
	}
	// This test concerns recovered log severity, not a one-second queue-drain
	// SLA. A pressure log can be published between batches with admitted work
	// still queued. All Record calls have returned: the public drop count and
	// synthetic valid history independently determine the committed-row target.
	expected := uint64(128+128+1025) - pressure.queueFullDrops
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	for {
		var committed uint64
		if err := locker.QueryRowContext(drainCtx, "SELECT count(*) FROM openai_turn").Scan(&committed); err != nil {
			t.Fatalf("await committed admitted history (want %d): %v", expected, err)
		}
		if committed == expected {
			t.Logf("committed %d admitted valid Turns before final-severity check; queue drops=%d", committed, pressure.queueFullDrops)
			break
		}
		if committed > expected || drainCtx.Err() != nil {
			t.Fatalf("committed=%d, want admitted=%d before final-severity check", committed, expected)
		}
		time.Sleep(10 * time.Millisecond)
	}
	report := closeStore(t, store)
	if report.RuntimeWriteLosses != 128 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("recovered final report = %+v", report)
	}
	records := handler.snapshot()
	final := records[len(records)-1]
	if final.msg != "usage store finalized" || final.level != slog.LevelWarn {
		t.Errorf("recovered final log = %+v, want terminal WARN aggregate for historical loss", final)
	}
	if final.cleanupCompleted == nil || !*final.cleanupCompleted {
		t.Errorf("recovered final log cleanup = %v, want confirmed completed cleanup", final.cleanupCompleted)
	}
}

func TestStoreRepeatedWriteFailuresEscalateCurrentPersistentState(t *testing.T) {
	handler := &storeLogHandler{}
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, slog.New(handler))
	if err != nil {
		t.Fatal(err)
	}
	fillRejectedRuntimeBatch(store)
	fillRejectedRuntimeBatch(store)
	failure := waitForStoreLog(t, handler, "usage observations lost", 2*time.Second)
	if failure.level != slog.LevelError {
		t.Errorf("repeated current failure level = %s, want ERROR", failure.level)
	}
	_ = closeStore(t, store)
}

func TestStoreFinalLogWaitsForInProgressRuntimeLogAndRemainsTerminal(t *testing.T) {
	handler := &storeLogHandler{
		delayRuntime:   true,
		runtimeEntered: make(chan struct{}),
		releaseRuntime: make(chan struct{}),
	}
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, slog.New(handler))
	if err != nil {
		t.Fatal(err)
	}
	fillRejectedRuntimeBatch(store)
	select {
	case <-handler.runtimeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime loss record did not reach delayed handler")
	}

	closeResult := make(chan sqlitestore.Report, 1)
	go func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		defer closeCancel()
		closeResult <- store.Close(closeCtx)
	}()

	early := false
	select {
	case <-closeResult:
		early = true
	case <-time.After(150 * time.Millisecond):
	}
	// This call completes after the bounded native wait but before the final
	// logging handoff can publish. The final snapshot must include it.
	store.Record(usage.Turn{})
	close(handler.releaseRuntime)
	if early {
		t.Fatal("Close published the final record while an earlier runtime record was still in progress")
	}
	var report sqlitestore.Report
	select {
	case report = <-closeResult:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the finite delayed handler was released")
	}

	if report.LateAfterCutoffDrops != 1 {
		t.Errorf("final report late_after_cutoff = %d, want call completed before publication", report.LateAfterCutoffDrops)
	}
	if report.RuntimeWriteLosses != 128 || report.QueueFullDrops != 0 || report.FinalFlushLosses != 0 {
		t.Errorf("final losses = %+v, want only the rejected batch and late call", report)
	}
	records := handler.snapshot()
	if len(records) < 2 || records[len(records)-1].msg != "usage store finalized" {
		t.Fatalf("log order = %+v, want final aggregate as terminal record", records)
	}
	final := records[len(records)-1]
	if final.cleanupCompleted == nil || *final.cleanupCompleted != report.DriverCleanupCompleted {
		t.Errorf("final log cleanup = %v, want returned snapshot %t", final.cleanupCompleted, report.DriverCleanupCompleted)
	}
	// Releasing the runtime log lets the writer begin cleanup, but the native
	// wait has already expired. Cleanup and final publication may finish in
	// either order; GOMAXPROCS does not establish a happens-before relationship.
	// The final level must describe the snapshot, not eventual native cleanup.
	wantLevel := slog.LevelWarn
	if !report.DriverCleanupCompleted {
		wantLevel = slog.LevelError
	}
	if final.level != wantLevel {
		t.Errorf("final level = %s, want %s for cleanup_completed=%t", final.level, wantLevel, report.DriverCleanupCompleted)
	}
	for index, record := range records[:len(records)-1] {
		if record.msg == "usage store finalized" {
			t.Fatalf("final aggregate appeared before record %d: %+v", index, records)
		}
	}
}
