package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestStoreStartupSharesBudgetAcrossSequentialWALAndBeginContention(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blockerDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blockerDB.Close()
	blocker, err := blockerDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.ExecContext(context.Background(), "ROLLBACK")

	contention := &startupContention{t: t, blocker: blocker}
	openDB := func(name, dsn string) (*sql.DB, error) {
		if name != "sqlite" {
			t.Fatalf("driver = %q, want sqlite", name)
		}
		connector, err := sqlite.NewConnector(dsn)
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(startupContentionConnector{Connector: connector, contention: contention}), nil
	}
	started := time.Now()
	store, err := openStore(path, slog.New(slog.NewTextHandler(io.Discard, nil)), openDB)
	elapsed := time.Since(started)
	if store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		store.Close(ctx)
	}
	t.Logf("WAL busy results=%d initial native cap=%dms later BEGIN native cap=%dms BEGIN wait=%s total=%s error=%v",
		contention.walBusy, contention.walCap, contention.beginCap, contention.beginWait, elapsed, err)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "startup contention budget exhausted") {
		t.Fatalf("Open error = %v, want one exhausted startup contention budget", err)
	}
	if contention.walBusy < 2 || !contention.walReleased {
		t.Fatalf("WAL contention was not retried and released: busy=%d released=%v", contention.walBusy, contention.walReleased)
	}
	if contention.walCap < 4000 || contention.walCap > 5000 {
		t.Errorf("initial WAL native cap = %dms, want initial five-second budget", contention.walCap)
	}
	// The first lock was held across at least 250ms of actual WAL retries.
	// Readback on the physical connection, not the supplied DSN or deadline,
	// must show that later acquisition did not receive a fresh five seconds.
	if contention.beginCap <= 0 || contention.beginCap > contention.walCap-200 {
		t.Errorf("later BEGIN native cap = %dms, want positive and at least 200ms smaller than initial WAL cap %dms",
			contention.beginCap, contention.walCap)
	}
	if contention.beginWait < 4*time.Second {
		t.Errorf("BEGIN waited %s, want real native contention through the remaining budget", contention.beginWait)
	}
	if elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
		t.Errorf("startup elapsed = %s, want one approximately five-second budget", elapsed)
	}
}

// This wrapper forwards every SQL operation to the selected real driver. Only
// this Open's connections are wrapped; no registered driver or global hook is
// changed. Admission uses them serially, and returns before assertions read the
// observations. The lock handoff is synchronous with SQL, not a sleep-order race.
type startupContention struct {
	t           *testing.T
	blocker     *sql.Conn
	firstWAL    time.Time
	walBusy     int
	walReleased bool
	walCap      int64
	beginCap    int64
	beginWait   time.Duration
}

type startupContentionConnector struct {
	driver.Connector
	contention *startupContention
}

func (c startupContentionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &startupContentionConn{Conn: conn, contention: c.contention}, nil
}

type startupContentionConn struct {
	driver.Conn
	contention *startupContention
}

func (c *startupContentionConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.contention
	if query == "PRAGMA journal_mode=WAL" && s.firstWAL.IsZero() {
		s.walCap = c.nativeBusyTimeout()
		s.firstWAL = time.Now()
	}
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	var sqliteErr *sqlite.Error
	if query == "PRAGMA journal_mode=WAL" && errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == 5 {
		s.walBusy++
		// Let the shipped clean-connection retries spend time on the first
		// lock, then release it only after observing a real SQLITE_BUSY.
		if !s.walReleased && time.Since(s.firstWAL) >= 250*time.Millisecond {
			if _, err := s.blocker.ExecContext(context.Background(), "ROLLBACK"); err != nil {
				s.t.Fatal(err)
			}
			s.walReleased = true
		}
	}
	return rows, err
}

func (c *startupContentionConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	s := c.contention
	if query != "BEGIN IMMEDIATE" {
		return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	}
	if s.beginCap == 0 {
		// WAL is now established. Acquire a new real write transaction before
		// forwarding BEGIN, and keep it held until Open exhausts its budget.
		// This guarantees later contention without a racing timer or goroutine.
		if _, err := s.blocker.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
			s.t.Fatal(err)
		}
		s.beginCap = c.nativeBusyTimeout()
	}
	started := time.Now()
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	s.beginWait += time.Since(started)
	return result, err
}

func (c *startupContentionConn) nativeBusyTimeout() int64 {
	c.contention.t.Helper()
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(context.Background(), "PRAGMA busy_timeout", nil)
	if err != nil {
		c.contention.t.Fatal(err)
	}
	defer rows.Close()
	values := make([]driver.Value, 1)
	if err := rows.Next(values); err != nil {
		c.contention.t.Fatal(err)
	}
	return values[0].(int64)
}
