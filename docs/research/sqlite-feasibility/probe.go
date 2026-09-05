package sqliteprobe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// CandidateDriverVersion is the explicitly pinned driver under test.
	CandidateDriverVersion = "v1.58.0"
	// StartupContentionBudget is the design's single setup/acquisition budget.
	StartupContentionBudget = 5 * time.Second
)

// Stage names an externally observable setup or migration boundary in this
// disposable probe.
type Stage string

const (
	StageOpen           Stage = "open_native_connection"
	StageJournalMode    Stage = "journal_mode_wal"
	StageSynchronous    Stage = "synchronous_normal"
	StageBeginImmediate Stage = "begin_immediate"
	StageUserVersion    Stage = "user_version_inside_transaction"
	StageMigration      Stage = "migration"
	StageVersionBump    Stage = "user_version_bump"
	StageCommit         Stage = "commit"
)

// StageEvent exposes timing and SQLite results so evidence tests can coordinate
// real external lock holders without mocking the driver.
type StageEvent struct {
	Attempt       int
	Stage         Stage
	Before        bool
	Remaining     time.Duration
	NativeTimeout time.Duration
	SQLiteCode    int
	Err           error
}

// AdmissionOptions configures the disposable startup probe. A zero Budget uses
// StartupContentionBudget. Migrations are ordered; index+1 is user_version.
type AdmissionOptions struct {
	Budget     time.Duration
	Migrations []string
	Trace      func(StageEvent)
}

// Admission holds the one database/sql connection pinned to the configured
// native connection for the lifetime of the probe.
type Admission struct {
	Conn          *sql.Conn
	SQLiteVersion string
	JournalMode   string
	Synchronous   int
	UserVersion   int

	db *sql.DB
}

// Close releases the dedicated connection and its one-connection pool. This is
// evidence cleanup, not a proposed unbounded production shutdown contract.
func (a *Admission) Close() error {
	return errors.Join(a.Conn.Close(), a.db.Close())
}

// Admit configures WAL and NORMAL synchronous mode, then serializes the schema
// check and migrations under BEGIN IMMEDIATE. SQLITE_BUSY before transaction
// acquisition retries from a fresh physical connection under one monotonic
// budget. Errors after acquisition and non-contention errors are never retried.
func Admit(ctx context.Context, path string, options AdmissionOptions) (*Admission, error) {
	budget := options.Budget
	if budget == 0 {
		budget = StartupContentionBudget
	}
	if budget < 0 {
		return nil, fmt.Errorf("contention budget must not be negative: %s", budget)
	}
	deadline := time.Now().Add(budget)

	for attempt := 1; ; attempt++ {
		admission, acquired, err := attemptAdmission(ctx, path, deadline, attempt, options)
		if err == nil {
			return admission, nil
		}
		if remaining := time.Until(deadline); remaining <= 0 && (isBusy(err) || errors.Is(err, context.DeadlineExceeded)) {
			return nil, fmt.Errorf("startup contention budget exhausted after %d attempts: %w", attempt, err)
		}
		if acquired || !isBusy(err) {
			return nil, err
		}
		if err := waitForRetry(ctx, deadline, 10*time.Millisecond); err != nil {
			return nil, fmt.Errorf("startup contention budget exhausted after %d attempts: %w", attempt, err)
		}
	}
}

func attemptAdmission(ctx context.Context, path string, deadline time.Time, attempt int, options AdmissionOptions) (_ *Admission, acquired bool, resultErr error) {
	nativeTimeout, err := remainingNativeTimeout(deadline)
	if err != nil {
		return nil, false, err
	}
	emit(options.Trace, StageEvent{Attempt: attempt, Stage: StageOpen, Before: true, Remaining: time.Until(deadline), NativeTimeout: nativeTimeout})
	db, err := sql.Open("sqlite", sqliteDSN(path, nativeTimeout))
	if err != nil {
		emitAfter(options.Trace, attempt, StageOpen, deadline, nativeTimeout, err)
		return nil, false, fmt.Errorf("open sqlite driver: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	keep := false
	var conn *sql.Conn
	defer func() {
		if keep {
			return
		}
		if conn != nil {
			resultErr = errors.Join(resultErr, conn.Close())
		}
		resultErr = errors.Join(resultErr, db.Close())
	}()

	stageCtx, cancel := context.WithDeadline(ctx, deadline)
	conn, err = db.Conn(stageCtx)
	cancel()
	emitAfter(options.Trace, attempt, StageOpen, deadline, nativeTimeout, err)
	if err != nil {
		return nil, false, fmt.Errorf("open native connection: %w", err)
	}

	var journalMode string
	err = runSetupStage(ctx, conn, deadline, attempt, StageJournalMode, options.Trace, func(stageCtx context.Context) error {
		return conn.QueryRowContext(stageCtx, `PRAGMA journal_mode=WAL`).Scan(&journalMode)
	})
	if err != nil {
		return nil, false, fmt.Errorf("activate WAL: %w", err)
	}
	journalMode = strings.ToLower(journalMode)
	if journalMode != "wal" {
		return nil, false, fmt.Errorf("activate WAL returned %q, want wal", journalMode)
	}

	err = runSetupStage(ctx, conn, deadline, attempt, StageSynchronous, options.Trace, func(stageCtx context.Context) error {
		_, err := conn.ExecContext(stageCtx, `PRAGMA synchronous=NORMAL`)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("set synchronous NORMAL: %w", err)
	}

	err = runSetupStage(ctx, conn, deadline, attempt, StageBeginImmediate, options.Trace, func(stageCtx context.Context) error {
		_, err := conn.ExecContext(stageCtx, `BEGIN IMMEDIATE`)
		return err
	})
	if err != nil {
		return nil, false, fmt.Errorf("acquire schema transaction: %w", err)
	}
	acquired = true
	committed := false
	defer func() {
		if !committed && conn != nil {
			_, rollbackErr := conn.ExecContext(context.Background(), `ROLLBACK`)
			resultErr = errors.Join(resultErr, rollbackErr)
		}
	}()

	var userVersion int
	if err := runTransactionStage(ctx, conn, attempt, StageUserVersion, options.Trace, func(stageCtx context.Context) error {
		return conn.QueryRowContext(stageCtx, `PRAGMA user_version`).Scan(&userVersion)
	}); err != nil {
		return nil, true, fmt.Errorf("read user_version inside schema transaction: %w", err)
	}
	if userVersion > len(options.Migrations) {
		return nil, true, fmt.Errorf("database user_version %d is newer than supported version %d", userVersion, len(options.Migrations))
	}
	for index := userVersion; index < len(options.Migrations); index++ {
		migration := options.Migrations[index]
		if err := runTransactionStage(ctx, conn, attempt, StageMigration, options.Trace, func(stageCtx context.Context) error {
			_, err := conn.ExecContext(stageCtx, migration)
			return err
		}); err != nil {
			return nil, true, fmt.Errorf("apply migration %d: %w", index+1, err)
		}
	}
	if userVersion != len(options.Migrations) {
		statement := fmt.Sprintf("PRAGMA user_version=%d", len(options.Migrations))
		if err := runTransactionStage(ctx, conn, attempt, StageVersionBump, options.Trace, func(stageCtx context.Context) error {
			_, err := conn.ExecContext(stageCtx, statement)
			return err
		}); err != nil {
			return nil, true, fmt.Errorf("set user_version: %w", err)
		}
		userVersion = len(options.Migrations)
	}
	if err := runTransactionStage(ctx, conn, attempt, StageCommit, options.Trace, func(stageCtx context.Context) error {
		_, err := conn.ExecContext(stageCtx, `COMMIT`)
		return err
	}); err != nil {
		return nil, true, fmt.Errorf("commit schema transaction: %w", err)
	}
	committed = true
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return nil, true, fmt.Errorf("set admitted runtime busy timeout: %w", err)
	}

	var sqliteVersion string
	if err := conn.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		return nil, true, fmt.Errorf("read SQLite version: %w", err)
	}
	var synchronous int
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return nil, true, fmt.Errorf("read synchronous mode: %w", err)
	}

	keep = true
	return &Admission{
		Conn:          conn,
		SQLiteVersion: sqliteVersion,
		JournalMode:   journalMode,
		Synchronous:   synchronous,
		UserVersion:   userVersion,
		db:            db,
	}, true, nil
}

func runSetupStage(ctx context.Context, conn *sql.Conn, deadline time.Time, attempt int, stage Stage, trace func(StageEvent), operation func(context.Context) error) error {
	nativeTimeout, err := remainingNativeTimeout(deadline)
	if err != nil {
		return err
	}
	stageCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err := conn.ExecContext(stageCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", nativeTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set native busy timeout before %s: %w", stage, err)
	}
	emit(trace, StageEvent{Attempt: attempt, Stage: stage, Before: true, Remaining: time.Until(deadline), NativeTimeout: nativeTimeout})
	err = operation(stageCtx)
	emitAfter(trace, attempt, stage, deadline, nativeTimeout, err)
	return err
}

func runTransactionStage(ctx context.Context, conn *sql.Conn, attempt int, stage Stage, trace func(StageEvent), operation func(context.Context) error) error {
	emit(trace, StageEvent{Attempt: attempt, Stage: stage, Before: true})
	err := operation(ctx)
	emit(trace, StageEvent{Attempt: attempt, Stage: stage, Before: false, SQLiteCode: sqliteCode(err), Err: err})
	return err
}

func remainingNativeTimeout(deadline time.Time) (time.Duration, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	milliseconds := remaining.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func sqliteDSN(path string, timeout time.Duration) string {
	dsn := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := dsn.Query()
	query.Set("_busy_timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func emitAfter(trace func(StageEvent), attempt int, stage Stage, deadline time.Time, timeout time.Duration, err error) {
	emit(trace, StageEvent{
		Attempt:       attempt,
		Stage:         stage,
		Remaining:     time.Until(deadline),
		NativeTimeout: timeout,
		SQLiteCode:    sqliteCode(err),
		Err:           err,
	})
}

func emit(trace func(StageEvent), event StageEvent) {
	if trace != nil {
		trace(event)
	}
}

func sqliteCode(err error) int {
	if err == nil {
		return 0
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()
	}
	return 0
}

func isBusy(err error) bool {
	code := sqliteCode(err)
	return code&0xff == sqlite3.SQLITE_BUSY
}

func waitForRetry(ctx context.Context, deadline time.Time, delay time.Duration) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
