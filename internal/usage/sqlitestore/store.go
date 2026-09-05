// Package sqlitestore persists best-effort usage observations in a private,
// forward-migrated SQLite database. Hooks see only usage.Sink; this adapter owns
// all filesystem, driver, batching, loss reporting, and cleanup work.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"modernc.org/sqlite"
)

const (
	queueCapacity       = 1024
	batchCapacity       = 128
	flushInterval       = time.Second
	startupBudget       = 5 * time.Second
	runtimeWriteTimeout = 5 * time.Second
	runtimeBusyTimeout  = 5 * time.Second
	setupRetryBackoff   = 10 * time.Millisecond
)

var (
	//go:embed migrations/*.sql
	migrationFiles embed.FS
	migrationNames = []string{"migrations/001_initial.sql"}
)

// Report is the bounded loss and cleanup result observed through Close's final
// aggregate publication. It is not a persistence acknowledgement for any Turn.
type Report struct {
	QueueFullDrops         uint64
	RuntimeWriteLosses     uint64
	LateAfterCutoffDrops   uint64
	FinalFlushLosses       uint64
	DriverCleanupCompleted bool
}

type finalizeRequest struct {
	ctx context.Context
}

// Store is a concurrency-safe usage Sink. Record performs only an immutable
// snapshot copy and one non-blocking queue admission attempt.
type Store struct {
	logger *slog.Logger
	conn   *sql.Conn
	db     *sql.DB

	queue       chan usage.Turn
	finalize    chan finalizeRequest
	cleanupDone chan struct{}

	admitting atomic.Bool
	producers atomic.Int64

	recordStarted        atomic.Uint64
	queueFullDrops       atomic.Uint64
	runtimeWriteLosses   atomic.Uint64
	lateAfterCutoffDrops atomic.Uint64
	committed            atomic.Uint64
	cleanupSuccessful    atomic.Bool

	outcomeMu                sync.Mutex
	outcomesSealed           bool
	writeFailureActive       bool
	writeFailurePersistent   bool
	consecutiveWriteFailures uint64
	failureStateVersion      uint64
	lastWriteError           error

	logMu         sync.Mutex
	loggingSealed bool

	lastReportedQueueFull      uint64
	lastReportedRuntime        uint64
	lastReportedFailureVersion uint64

	closeOnce        sync.Once
	publishFinalOnce sync.Once
	finalReportMu    sync.Mutex
	finalReport      Report
}

var _ usage.Sink = (*Store)(nil)

// Open validates and prepares path, admits one dedicated configured SQLite
// connection under the single startup contention budget, migrates atomically,
// and starts the writer. logger is required and must belong to
// internal/usage/sqlitestore.
func Open(path string, logger *slog.Logger) (*Store, error) {
	literalPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve usage database path %q: %w", path, err)
	}
	if err := preparePrivateDatabase(literalPath); err != nil {
		return nil, err
	}
	db, conn, err := admit(literalPath)
	if err != nil {
		return nil, err
	}
	store := &Store{
		logger:      logger,
		conn:        conn,
		db:          db,
		queue:       make(chan usage.Turn, queueCapacity),
		finalize:    make(chan finalizeRequest, 1),
		cleanupDone: make(chan struct{}),
	}
	store.admitting.Store(true)
	go store.runWriter()
	return store, nil
}

// Record attempts to admit one immutable Turn snapshot. It never waits for the
// writer, touches SQLite, or logs. A racing or post-cutoff call is counted and
// returns promptly; the queue is never closed under producers.
func (s *Store) Record(turn usage.Turn) {
	saturatingAdd(&s.recordStarted, 1)
	if !s.admitting.Load() {
		saturatingAdd(&s.lateAfterCutoffDrops, 1)
		return
	}
	s.producers.Add(1)
	defer s.producers.Add(-1)
	if !s.admitting.Load() {
		saturatingAdd(&s.lateAfterCutoffDrops, 1)
		return
	}

	turn = cloneTurn(turn)
	select {
	case s.queue <- turn:
	default:
		saturatingAdd(&s.queueFullDrops, 1)
	}
}

// StopAdmission atomically cuts off new Turn admission. It is idempotent and
// intentionally does not wait for already admitted prompt Record calls.
func (s *Store) StopAdmission() {
	s.admitting.Store(false)
}

// Close cuts off admission if needed, asks the writer to drain and clean up,
// waits only through ctx, and publishes exactly one final aggregate. The writer
// owns all native SQL work and cleanup even when this coordinator stops waiting.
func (s *Store) Close(ctx context.Context) Report {
	s.StopAdmission()
	s.closeOnce.Do(func() {
		s.finalize <- finalizeRequest{ctx: ctx}
	})

	select {
	case <-s.cleanupDone:
	case <-ctx.Done():
	}

	s.publishFinalOnce.Do(func() {
		report := s.publishFinal()
		s.finalReportMu.Lock()
		s.finalReport = report
		s.finalReportMu.Unlock()
	})
	s.finalReportMu.Lock()
	report := s.finalReport
	s.finalReportMu.Unlock()
	return report
}

func (s *Store) runWriter() {
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()
	batch := make([]usage.Turn, 0, batchCapacity)
	nextLossReport := time.Now().Add(flushInterval)

	for {
		select {
		case request := <-s.finalize:
			s.finish(request.ctx, batch)
			return
		default:
		}

		if len(batch) == batchCapacity {
			s.flushRuntime(batch)
			batch = batch[:0]
			if !time.Now().Before(nextLossReport) {
				s.publishRuntimeLosses()
				nextLossReport = time.Now().Add(flushInterval)
			}
			continue
		}

		select {
		case request := <-s.finalize:
			s.finish(request.ctx, batch)
			return
		case turn := <-s.queue:
			batch = append(batch, turn)
		case <-timer.C:
			if len(batch) > 0 {
				s.flushRuntime(batch)
				batch = batch[:0]
			}
			s.publishRuntimeLosses()
			nextLossReport = time.Now().Add(flushInterval)
			timer.Reset(flushInterval)
		}
	}
}

func (s *Store) flushRuntime(batch []usage.Turn) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeWriteTimeout)
	err := s.writeBatch(ctx, batch)
	cancel()
	if err != nil {
		s.settleRuntimeFailure(uint64(len(batch)), err)
		return
	}
	s.settleCommitted(uint64(len(batch)))
}

func (s *Store) finish(ctx context.Context, initial []usage.Turn) {
	batch := append(make([]usage.Turn, 0, batchCapacity), initial...)
	if !waitForProducers(ctx, &s.producers) {
		s.cleanup()
		return
	}

	failed := false
	for {
		for len(batch) < batchCapacity {
			select {
			case turn := <-s.queue:
				batch = append(batch, turn)
			default:
				goto drained
			}
		}
		if err := s.writeBatch(ctx, batch); err != nil {
			s.settleFinalFailure(err)
			failed = true
			batch = batch[:0]
			break
		}
		s.settleCommitted(uint64(len(batch)))
		batch = batch[:0]
	}

drained:
	if !failed && len(batch) > 0 {
		if err := s.writeBatch(ctx, batch); err != nil {
			s.settleFinalFailure(err)
			failed = true
		} else {
			s.settleCommitted(uint64(len(batch)))
		}
		batch = batch[:0]
	}
	if failed || ctx.Err() != nil {
		_ = drainCount(s.queue)
	}

	s.cleanup()
}

func (s *Store) cleanup() {
	connErr := s.conn.Close()
	dbErr := s.db.Close()
	s.cleanupSuccessful.Store(connErr == nil && dbErr == nil)
	close(s.cleanupDone)
}

func (s *Store) writeBatch(ctx context.Context, batch []usage.Turn) error {
	if len(batch) == 0 {
		return nil
	}
	committed := false
	defer func() {
		if !committed {
			// BEGIN may have acquired the transaction before the driver surfaced
			// ctx.Err. ROLLBACK is therefore required even when BEGIN returned an
			// error; "no transaction is active" is the harmless alternative.
			_, _ = s.conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := setRemainingBusyTimeout(ctx, s.conn); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin usage batch: %w", err)
	}

	for _, turn := range batch {
		if err := setRemainingBusyTimeout(ctx, s.conn); err != nil {
			return err
		}
		if err := insertTurn(ctx, s.conn, turn); err != nil {
			return err
		}
	}
	if err := setRemainingBusyTimeout(ctx, s.conn); err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit usage batch: %w", err)
	}
	committed = true
	return nil
}

func insertTurn(ctx context.Context, conn *sql.Conn, turn usage.Turn) error {
	atMS := turn.At.UnixMilli()
	switch native := turn.Usage.(type) {
	case usage.AnthropicUsage:
		_, err := conn.ExecContext(ctx, `INSERT INTO anthropic_turn (
			at_ms, request_id, message_id, turn_index, model, transport,
			input_tokens, output_tokens, cache_creation_input_tokens,
			cache_read_input_tokens, ephemeral_5m_input_tokens,
			ephemeral_1h_input_tokens, thinking_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			atMS, turn.RequestID, turn.ResponseID, turn.TurnIndex, turn.Model, string(turn.Transport),
			native.InputTokens, native.OutputTokens, nullable(native.CacheCreationInputTokens),
			nullable(native.CacheReadInputTokens), nullable(native.Ephemeral5mInputTokens),
			nullable(native.Ephemeral1hInputTokens), nullable(native.ThinkingTokens),
		)
		if err != nil {
			return fmt.Errorf("insert Anthropic Turn: %w", err)
		}
	case usage.OpenAIUsage:
		_, err := conn.ExecContext(ctx, `INSERT INTO openai_turn (
			at_ms, request_id, response_id, turn_index, model, transport,
			input_tokens, cached_tokens, cache_write_tokens, output_tokens,
			reasoning_tokens, total_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			atMS, turn.RequestID, turn.ResponseID, turn.TurnIndex, turn.Model, string(turn.Transport),
			native.InputTokens, nullable(native.CachedTokens), nullable(native.CacheWriteTokens),
			native.OutputTokens, nullable(native.ReasoningTokens), nullable(native.TotalTokens),
		)
		if err != nil {
			return fmt.Errorf("insert OpenAI Turn: %w", err)
		}
	default:
		return errors.New("usage Turn has no supported Surface-native usage")
	}
	return nil
}

func nullable(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func cloneTurn(turn usage.Turn) usage.Turn {
	switch native := turn.Usage.(type) {
	case usage.AnthropicUsage:
		native.CacheCreationInputTokens = cloneInt64(native.CacheCreationInputTokens)
		native.CacheReadInputTokens = cloneInt64(native.CacheReadInputTokens)
		native.Ephemeral5mInputTokens = cloneInt64(native.Ephemeral5mInputTokens)
		native.Ephemeral1hInputTokens = cloneInt64(native.Ephemeral1hInputTokens)
		native.ThinkingTokens = cloneInt64(native.ThinkingTokens)
		turn.Usage = native
	case usage.OpenAIUsage:
		native.CachedTokens = cloneInt64(native.CachedTokens)
		native.CacheWriteTokens = cloneInt64(native.CacheWriteTokens)
		native.ReasoningTokens = cloneInt64(native.ReasoningTokens)
		native.TotalTokens = cloneInt64(native.TotalTokens)
		turn.Usage = native
	}
	return turn
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type liveFailureState struct {
	active     bool
	persistent bool
	version    uint64
	err        error
}

func (s *Store) settleCommitted(count uint64) {
	s.outcomeMu.Lock()
	defer s.outcomeMu.Unlock()
	if s.outcomesSealed {
		return
	}
	saturatingAdd(&s.committed, count)
	if s.writeFailureActive {
		s.writeFailureActive = false
		s.writeFailurePersistent = false
		s.consecutiveWriteFailures = 0
		s.lastWriteError = nil
		s.advanceFailureStateLocked()
	}
}

func (s *Store) settleRuntimeFailure(count uint64, err error) {
	s.outcomeMu.Lock()
	defer s.outcomeMu.Unlock()
	if s.outcomesSealed {
		return
	}
	saturatingAdd(&s.runtimeWriteLosses, count)
	s.noteWriteFailureLocked(err)
}

func (s *Store) settleFinalFailure(err error) {
	s.outcomeMu.Lock()
	defer s.outcomeMu.Unlock()
	if !s.outcomesSealed {
		s.noteWriteFailureLocked(err)
	}
}

func (s *Store) noteWriteFailureLocked(err error) {
	s.writeFailureActive = true
	if s.consecutiveWriteFailures != ^uint64(0) {
		s.consecutiveWriteFailures++
	}
	s.writeFailurePersistent = s.consecutiveWriteFailures > 1
	s.lastWriteError = err
	s.advanceFailureStateLocked()
}

func (s *Store) advanceFailureStateLocked() {
	if s.failureStateVersion != ^uint64(0) {
		s.failureStateVersion++
	}
}

func (s *Store) failureState() liveFailureState {
	s.outcomeMu.Lock()
	defer s.outcomeMu.Unlock()
	return liveFailureState{
		active:     s.writeFailureActive,
		persistent: s.writeFailurePersistent,
		version:    s.failureStateVersion,
		err:        s.lastWriteError,
	}
}

func drainCount(queue <-chan usage.Turn) int {
	count := 0
	for {
		select {
		case <-queue:
			count++
		default:
			return count
		}
	}
}

func waitForProducers(ctx context.Context, active *atomic.Int64) bool {
	for active.Load() != 0 {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return true
}

func saturatingAdd(counter *atomic.Uint64, delta uint64) {
	for {
		old := counter.Load()
		if old == ^uint64(0) {
			return
		}
		next := old + delta
		if next < old {
			next = ^uint64(0)
		}
		if counter.CompareAndSwap(old, next) {
			return
		}
	}
}

func (s *Store) runtimeSnapshot() (Report, liveFailureState) {
	state := s.failureState()
	return Report{
		QueueFullDrops:       s.queueFullDrops.Load(),
		RuntimeWriteLosses:   s.runtimeWriteLosses.Load(),
		LateAfterCutoffDrops: s.lateAfterCutoffDrops.Load(),
	}, state
}

// settleFinalReport permanently seals writer outcomes, then classifies every
// Record call observed through this snapshot exactly once. Calls whose prompt
// outcome has not completed at the snapshot are conservatively unconfirmed.
func (s *Store) settleFinalReport(cleanup bool) Report {
	s.outcomeMu.Lock()
	s.outcomesSealed = true
	committed := s.committed.Load()
	runtimeLost := s.runtimeWriteLosses.Load()
	s.outcomeMu.Unlock()

	// Rejected counters are sampled before recordStarted. Record increments
	// started first, so a call racing this snapshot is either represented by its
	// completed outcome or conservatively remains in the direct residual below.
	// Writer outcomes no longer depend on a post-send admission counter, so they
	// cannot overtake a stale counter that would cap confirmed outcomes.
	queueFull := s.queueFullDrops.Load()
	late := s.lateAfterCutoffDrops.Load()
	started := s.recordStarted.Load()
	settled := saturatingSum(queueFull, late)
	settled = saturatingSum(settled, committed)
	settled = saturatingSum(settled, runtimeLost)
	finalLost := positiveDifference(started, settled)

	return Report{
		QueueFullDrops:         queueFull,
		RuntimeWriteLosses:     runtimeLost,
		LateAfterCutoffDrops:   late,
		FinalFlushLosses:       finalLost,
		DriverCleanupCompleted: cleanup,
	}
}

func saturatingSum(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func positiveDifference(left, right uint64) uint64 {
	if right >= left {
		return 0
	}
	return left - right
}

func (s *Store) publishRuntimeLosses() {
	report, failure := s.runtimeSnapshot()
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.loggingSealed {
		return
	}
	queueChanged := report.QueueFullDrops != s.lastReportedQueueFull
	runtimeChanged := report.RuntimeWriteLosses != s.lastReportedRuntime
	failureChanged := failure.version != s.lastReportedFailureVersion
	if !queueChanged && !runtimeChanged && !failureChanged {
		return
	}
	s.lastReportedQueueFull = report.QueueFullDrops
	s.lastReportedRuntime = report.RuntimeWriteLosses
	s.lastReportedFailureVersion = failure.version

	message := "usage observations lost"
	level := slog.LevelWarn
	failureClass := "queue_pressure"
	switch {
	case failure.persistent:
		level = slog.LevelError
		failureClass = "persistent"
	case failure.active:
		failureClass = "transient"
	case failureChanged:
		message = "usage storage recovered"
		failureClass = "recovered"
		if !runtimeChanged && !queueChanged {
			level = slog.LevelInfo
		}
	}
	attrs := []any{
		slog.Uint64(logging.QueueFullDropsKey, report.QueueFullDrops),
		slog.Uint64(logging.RuntimeWriteLossesKey, report.RuntimeWriteLosses),
		slog.String(logging.FailureClassKey, failureClass),
	}
	if failure.active && failure.err != nil {
		attrs = append(attrs, slog.Any(logging.ErrorKey, failure.err))
	}
	s.logger.Log(context.Background(), level, message, attrs...)
}

func (s *Store) publishFinal() Report {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	s.loggingSealed = true
	// Snapshot only after the bounded native wait and after all earlier logging
	// has left the gate. Calls completed while waiting are in this final scope.
	cleanupCompleted := false
	select {
	case <-s.cleanupDone:
		cleanupCompleted = s.cleanupSuccessful.Load()
	default:
	}
	report := s.settleFinalReport(cleanupCompleted)

	failure := s.failureState()
	level := slog.LevelInfo
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 {
		level = slog.LevelWarn
	}
	if failure.persistent || !report.DriverCleanupCompleted {
		level = slog.LevelError
	}
	attrs := []any{
		slog.Uint64(logging.QueueFullDropsKey, report.QueueFullDrops),
		slog.Uint64(logging.RuntimeWriteLossesKey, report.RuntimeWriteLosses),
		slog.Uint64(logging.LateAfterCutoffDropsKey, report.LateAfterCutoffDrops),
		slog.Uint64(logging.FinalFlushLossesKey, report.FinalFlushLosses),
		slog.Bool(logging.DriverCleanupCompletedKey, report.DriverCleanupCompleted),
	}
	if failure.active {
		failureClass := "transient"
		if failure.persistent {
			failureClass = "persistent"
		}
		attrs = append(attrs, slog.String(logging.FailureClassKey, failureClass))
		if failure.err != nil {
			attrs = append(attrs, slog.Any(logging.ErrorKey, failure.err))
		}
	}
	s.logger.Log(context.Background(), level, "usage store finalized", attrs...)
	return report
}

func admit(path string) (*sql.DB, *sql.Conn, error) {
	deadline := time.Now().Add(startupBudget)
	var lastBusy error
	for {
		if time.Until(deadline) <= 0 {
			return nil, nil, startupBudgetError(lastBusy)
		}
		db, conn, acquired, err := attemptAdmission(path, deadline)
		if err == nil {
			return db, conn, nil
		}
		if db != nil {
			if conn != nil {
				_ = conn.Close()
			}
			_ = db.Close()
		}
		if acquired {
			return nil, nil, err
		}
		if errors.Is(err, context.DeadlineExceeded) || time.Until(deadline) <= 0 {
			return nil, nil, startupBudgetError(errors.Join(lastBusy, err))
		}
		if !isBusy(err) {
			return nil, nil, err
		}
		lastBusy = err
		remaining := time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		backoff := min(setupRetryBackoff, remaining)
		timer := time.NewTimer(backoff)
		<-timer.C
	}
}

func startupBudgetError(cause error) error {
	return fmt.Errorf("usage database startup contention budget exhausted after %s: %w", startupBudget, errors.Join(context.DeadlineExceeded, cause))
}

func attemptAdmission(path string, deadline time.Time) (*sql.DB, *sql.Conn, bool, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, nil, false, context.DeadlineExceeded
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, remaining))
	if err != nil {
		return nil, nil, false, fmt.Errorf("open usage database %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return db, nil, false, fmt.Errorf("acquire usage database connection: %w", err)
	}

	if err := setDeadlineBusyTimeout(ctx, conn, deadline); err != nil {
		return db, conn, false, fmt.Errorf("configure usage database setup timeout: %w", err)
	}
	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return db, conn, false, fmt.Errorf("enable usage database WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return db, conn, false, fmt.Errorf("enable usage database WAL: got journal_mode %q", journalMode)
	}

	if err := setDeadlineBusyTimeout(ctx, conn, deadline); err != nil {
		return db, conn, false, fmt.Errorf("recap usage database setup timeout before synchronous: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous=NORMAL"); err != nil {
		return db, conn, false, fmt.Errorf("set usage database synchronous NORMAL: %w", err)
	}

	if err := setDeadlineBusyTimeout(ctx, conn, deadline); err != nil {
		return db, conn, false, fmt.Errorf("recap usage database setup timeout before migration acquisition: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return db, conn, false, fmt.Errorf("acquire usage database migration transaction: %w", err)
	}
	if err := migrate(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return db, conn, true, err
	}
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", runtimeBusyTimeout.Milliseconds())); err != nil {
		return db, conn, true, fmt.Errorf("set usage database runtime busy timeout: %w", err)
	}
	return db, conn, true, nil
}

func migrate(conn *sql.Conn) error {
	var version int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read usage schema version: %w", err)
	}
	if version > len(migrationNames) {
		return fmt.Errorf("usage database schema version %d is newer than supported version %d", version, len(migrationNames))
	}
	for index := version; index < len(migrationNames); index++ {
		script, err := migrationFiles.ReadFile(migrationNames[index])
		if err != nil {
			return fmt.Errorf("read embedded usage migration %d: %w", index+1, err)
		}
		if _, err := conn.ExecContext(context.Background(), string(script)); err != nil {
			return fmt.Errorf("apply usage migration %d: %w", index+1, err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA user_version=%d", len(migrationNames))); err != nil {
		return fmt.Errorf("set usage schema version %d: %w", len(migrationNames), err)
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return fmt.Errorf("commit usage schema migration: %w", err)
	}
	return nil
}

func setDeadlineBusyTimeout(ctx context.Context, conn *sql.Conn, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	return setBusyTimeout(ctx, conn, remaining)
}

func setRemainingBusyTimeout(ctx context.Context, conn *sql.Conn) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return setBusyTimeout(ctx, conn, runtimeBusyTimeout)
	}
	return setDeadlineBusyTimeout(ctx, conn, deadline)
}

func setBusyTimeout(ctx context.Context, conn *sql.Conn, timeout time.Duration) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeoutMilliseconds(timeout)))
	return err
}

func isBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5
}
