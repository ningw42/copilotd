package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// These characterize the root-selected modernc driver, not a product promise
// that future drivers must remain slow to cancel. Reevaluate the expectations
// and FINDINGS.md when upgrading the driver; native waits are not inferred from
// database/sql's context-bearing signatures or Store.Close's coordinator bound.
func TestDriverWALActivationReturnsImmediateBusyDespiteNativeTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	blocker := driverConnection(t, path)
	if _, err := blocker.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.ExecContext(context.Background(), "ROLLBACK")

	challenger := driverConnection(t, path)
	if _, err := challenger.ExecContext(context.Background(), "PRAGMA busy_timeout=2000"); err != nil {
		t.Fatal(err)
	}
	var timeout int
	if err := challenger.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 2000 {
		t.Fatalf("native busy_timeout = %d, %v; want 2000ms", timeout, err)
	}
	started := time.Now()
	var mode string
	err := challenger.QueryRowContext(context.Background(), "PRAGMA journal_mode=WAL").Scan(&mode)
	elapsed := time.Since(started)
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 5 {
		t.Fatalf("WAL activation error = %v, want native SQLITE_BUSY (5)", err)
	}
	t.Logf("WAL activation code=%d elapsed=%s native_timeout=%dms", sqliteErr.Code(), elapsed, timeout)
	if elapsed >= 500*time.Millisecond {
		t.Errorf("WAL activation took %s, want immediate BUSY despite 2s native timeout", elapsed)
	}
}

func TestDriverCancellationWaitsForNativeBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	blocker := driverConnection(t, path)
	var mode string
	if err := blocker.QueryRowContext(context.Background(), "PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if _, err := blocker.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.ExecContext(context.Background(), "ROLLBACK")

	challenger := driverConnection(t, path)
	if _, err := challenger.ExecContext(context.Background(), "PRAGMA busy_timeout=500"); err != nil {
		t.Fatal(err)
	}
	var timeout int
	if err := challenger.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 500 {
		t.Fatalf("native busy_timeout = %d, %v; want 500ms", timeout, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := challenger.ExecContext(ctx, "BEGIN IMMEDIATE")
	elapsed := time.Since(started)
	t.Logf("contended BEGIN elapsed=%s context_timeout=50ms native_timeout=%dms error=%v", elapsed, timeout, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended BEGIN error = %v, want context deadline exceeded", err)
	}
	if elapsed < 400*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("contended BEGIN took %s, want observed native wait near 500ms despite 50ms context", elapsed)
	}
	var one int
	if err := challenger.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("connection query after cancellation = %d, %v; want usable connection", one, err)
	}
	if _, err := blocker.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := challenger.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("connection cannot acquire a transaction after cancellation and lock release: %v", err)
	}
	if _, err := challenger.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func driverConnection(t *testing.T, path string) *sql.Conn {
	t.Helper()
	db := openExternal(t, path)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return conn
}
