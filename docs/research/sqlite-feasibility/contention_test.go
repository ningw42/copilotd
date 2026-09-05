package sqliteprobe

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWALActivationReturnsImmediateBusyDespiteNativeTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	ctx := context.Background()
	blockerDB, err := sql.Open("sqlite", sqliteDSN(path, 5*time.Second))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blockerDB.Close()
	blocker, err := blockerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker connection: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer blocker.ExecContext(context.Background(), `ROLLBACK`)

	challengerDB, err := sql.Open("sqlite", sqliteDSN(path, 2*time.Second))
	if err != nil {
		t.Fatalf("open challenger: %v", err)
	}
	defer challengerDB.Close()
	challenger, err := challengerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("challenger connection: %v", err)
	}
	defer challenger.Close()
	if _, err := challenger.ExecContext(ctx, `PRAGMA busy_timeout=2000`); err != nil {
		t.Fatalf("set challenger busy timeout: %v", err)
	}
	var configured int
	if err := challenger.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&configured); err != nil {
		t.Fatalf("read challenger busy timeout: %v", err)
	}
	if configured != 2000 {
		t.Fatalf("busy_timeout = %d, want 2000", configured)
	}

	started := time.Now()
	var mode string
	err = challenger.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode)
	elapsed := time.Since(started)
	t.Logf("WAL activation error code=%d elapsed=%s native_timeout=2s", sqliteCode(err), elapsed)
	if code := sqliteCode(err); code&0xff != 5 {
		t.Fatalf("journal mode error = %v (code %d), want SQLITE_BUSY (5)", err, code)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("WAL activation returned after %s, want immediate result despite 2s native timeout", elapsed)
	}
}

func TestContendedExecContextCancellationWaitsForNativeBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	base := context.Background()
	admission, err := Admit(base, path, AdmissionOptions{})
	if err != nil {
		t.Fatalf("admit database: %v", err)
	}
	defer admission.Close()
	if _, err := admission.Conn.ExecContext(base, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer admission.Conn.ExecContext(context.Background(), `ROLLBACK`)

	challengerDB, err := sql.Open("sqlite", sqliteDSN(path, StartupContentionBudget))
	if err != nil {
		t.Fatalf("open challenger: %v", err)
	}
	defer challengerDB.Close()
	challenger, err := challengerDB.Conn(base)
	if err != nil {
		t.Fatalf("challenger connection: %v", err)
	}
	defer challenger.Close()
	if _, err := challenger.ExecContext(base, `PRAGMA busy_timeout=500`); err != nil {
		t.Fatalf("set challenger busy timeout: %v", err)
	}

	ctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = challenger.ExecContext(ctx, `BEGIN IMMEDIATE`)
	elapsed := time.Since(started)
	t.Logf("contended ExecContext elapsed=%s context_timeout=50ms native_timeout=500ms error=%v", elapsed, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended ExecContext error = %v (SQLite code %d), want context deadline exceeded", err, sqliteCode(err))
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("contended ExecContext elapsed = %s, want observed native timeout near 500ms despite 50ms context", elapsed)
	}
	var one int
	if err := challenger.QueryRowContext(base, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("connection unusable after canceled busy operation: %v", err)
	}
	if one != 1 {
		t.Errorf("post-cancel query = %d, want 1", one)
	}
}

func TestAdmitSharesFiveSecondBudgetAcrossSequentialContendedStages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	ctx := context.Background()
	firstDB, err := sql.Open("sqlite", sqliteDSN(path, StartupContentionBudget))
	if err != nil {
		t.Fatalf("open first blocker: %v", err)
	}
	defer firstDB.Close()
	first, err := firstDB.Conn(ctx)
	if err != nil {
		t.Fatalf("first blocker connection: %v", err)
	}
	defer first.Close()
	if _, err := first.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin first blocker: %v", err)
	}
	defer first.ExecContext(context.Background(), `ROLLBACK`)

	secondDB, err := sql.Open("sqlite", sqliteDSN(path, StartupContentionBudget))
	if err != nil {
		t.Fatalf("open second blocker: %v", err)
	}
	defer secondDB.Close()
	second, err := secondDB.Conn(ctx)
	if err != nil {
		t.Fatalf("second blocker connection: %v", err)
	}
	defer second.Close()
	defer second.ExecContext(context.Background(), `ROLLBACK`)

	firstReleased := make(chan struct{})
	time.AfterFunc(150*time.Millisecond, func() {
		_, _ = first.ExecContext(context.Background(), `ROLLBACK`)
		close(firstReleased)
	})
	var events []StageEvent
	var secondLockErr error
	secondLocked := false
	started := time.Now()
	admission, err := Admit(ctx, path, AdmissionOptions{
		Migrations: []string{`CREATE TABLE evidence(value TEXT NOT NULL)`},
		Trace: func(event StageEvent) {
			events = append(events, event)
			if event.Before && event.Stage == StageBeginImmediate && !secondLocked {
				_, secondLockErr = second.ExecContext(ctx, `BEGIN IMMEDIATE`)
				if secondLockErr == nil {
					secondLocked = true
					time.AfterFunc(200*time.Millisecond, func() {
						_, _ = second.ExecContext(context.Background(), `ROLLBACK`)
					})
				}
			}
		},
	})
	elapsed := time.Since(started)
	<-firstReleased
	if err != nil {
		t.Fatalf("admit after sequential contention: %v", err)
	}
	defer admission.Close()
	if secondLockErr != nil {
		t.Fatalf("acquire second real SQLite blocker: %v", secondLockErr)
	}
	if !secondLocked {
		t.Fatal("second stage was not contended")
	}
	if elapsed < 300*time.Millisecond || elapsed >= StartupContentionBudget {
		t.Errorf("elapsed = %s, want sequential waits within one 5s budget", elapsed)
	}

	var firstWALTimeout, beginTimeout time.Duration
	var sawImmediateBusy bool
	for _, event := range events {
		if event.Stage == StageJournalMode && event.Before && firstWALTimeout == 0 {
			firstWALTimeout = event.NativeTimeout
		}
		if event.Stage == StageJournalMode && !event.Before && event.SQLiteCode&0xff == 5 {
			sawImmediateBusy = true
		}
		if event.Stage == StageBeginImmediate && event.Before {
			beginTimeout = event.NativeTimeout
		}
	}
	if !sawImmediateBusy {
		t.Fatal("WAL stage did not observe SQLITE_BUSY")
	}
	if firstWALTimeout < 4*time.Second || firstWALTimeout >= StartupContentionBudget {
		t.Errorf("first WAL native timeout = %s, want initial five-second budget cap", firstWALTimeout)
	}
	if beginTimeout <= 0 || beginTimeout >= firstWALTimeout {
		t.Errorf("BEGIN native timeout = %s, want positive and smaller than first WAL timeout %s", beginTimeout, firstWALTimeout)
	}
	t.Logf("sequential contention elapsed=%s first_WAL_timeout=%s later_BEGIN_timeout=%s", elapsed, firstWALTimeout, beginTimeout)
}

func TestAdmitExhaustsOneContentionBudgetAcrossFreshAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	ctx := context.Background()
	blockerDB, err := sql.Open("sqlite", sqliteDSN(path, time.Second))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blockerDB.Close()
	blocker, err := blockerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("blocker connection: %v", err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer blocker.ExecContext(context.Background(), `ROLLBACK`)

	var attempts int
	started := time.Now()
	_, err = Admit(ctx, path, AdmissionOptions{
		Budget: 150 * time.Millisecond,
		Trace: func(event StageEvent) {
			if event.Before && event.Stage == StageOpen {
				attempts++
			}
		},
	})
	elapsed := time.Since(started)
	t.Logf("budget exhaustion elapsed=%s attempts=%d error=%v", elapsed, attempts, err)
	if err == nil {
		t.Fatal("admit succeeded, want exhausted contention budget")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("admit error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "contention budget exhausted") {
		t.Errorf("admit error = %v, want explicit budget exhaustion", err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want multiple clean-connection attempts", attempts)
	}
	if elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Errorf("elapsed = %s, want one approximately 150ms budget", elapsed)
	}
}
