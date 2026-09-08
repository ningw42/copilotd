package report_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestPinnedDriverFirstReadOnlyWALConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first-readonly-wal.db")
	uri := sqlitestore.LiteralFileURL(path)
	writer, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec("PRAGMA journal_mode=WAL; CREATE TABLE history(value INTEGER); INSERT INTO history VALUES(7)"); err != nil {
		t.Fatal(err)
	}
	query := uri.Query()
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "100")
	uri.RawQuery = query.Encode()
	for range 2 {
		// This reader's FIRST connection opens a live WAL database, not a prior
		// writable connection recycled with query_only. The writer stays open.
		reader, err := sql.Open("sqlite", uri.String())
		if err != nil {
			t.Fatal(err)
		}
		reader.SetMaxOpenConns(1)
		conn, err := reader.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(context.Background(), "PRAGMA query_only=ON"); err != nil {
			t.Fatal(err)
		}
		var version, journal, encoding string
		var busy, only, value int
		for _, check := range []struct {
			sql   string
			value any
		}{{"SELECT sqlite_version()", &version}, {"PRAGMA journal_mode", &journal}, {"PRAGMA encoding", &encoding}, {"PRAGMA busy_timeout", &busy}, {"PRAGMA query_only", &only}, {"SELECT value FROM history", &value}} {
			if err := conn.QueryRowContext(context.Background(), check.sql).Scan(check.value); err != nil {
				t.Fatal(err)
			}
		}
		if version != "3.53.4" || journal != "wal" || encoding != "UTF-8" || busy != 100 || only != 1 || value != 7 {
			t.Fatalf("first read-only WAL connection: %s %s %s %d %d %d", version, journal, encoding, busy, only, value)
		}
		if _, err := conn.ExecContext(context.Background(), "INSERT INTO history VALUES(8)"); err == nil {
			t.Fatal("read-only WAL connection wrote")
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s/%s SQLite=%s first mode=ro WAL connection: UTF-8, busy_timeout=100ms, query_only=1, committed value=7, clean close", runtime.Version(), runtime.GOOS, runtime.GOARCH, version)
	}
	var busy, pages, checkpointed int
	if err := writer.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil || busy != 0 || pages != 0 || checkpointed != 0 {
		t.Fatalf("first-open cleanup retained WAL: %d/%d/%d %v", busy, pages, checkpointed, err)
	}
}

// The pinned driver's public API is an approved characterization seam. These
// observations establish transfer/interrupt behavior, not a peak-native-memory
// guarantee or permission to change the reporting SQL policy.
func TestPinnedDriverReadOnlyGuardAndInterruptCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver.db")
	uri := sqlitestore.LiteralFileURL(path)
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE example(model TEXT); INSERT INTO example VALUES(printf('%.*c',1048577,'x')),('模型'),('模型 ') ,('KEPT')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	query := uri.Query()
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "17")
	uri.RawQuery = query.Encode()
	for range 2 {
		reader, err := sql.Open("sqlite", uri.String())
		if err != nil {
			t.Fatal(err)
		}
		reader.SetMaxOpenConns(1)
		conn, err := reader.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(context.Background(), "PRAGMA query_only=ON"); err != nil {
			t.Fatal(err)
		}
		var busy, only int
		var encoding string
		if err = conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 17 {
			t.Fatalf("busy pragma: %d %v", busy, err)
		}
		if err = conn.QueryRowContext(context.Background(), "PRAGMA query_only").Scan(&only); err != nil || only != 1 {
			t.Fatalf("query_only: %d %v", only, err)
		}
		if err = conn.QueryRowContext(context.Background(), "PRAGMA encoding").Scan(&encoding); err != nil || encoding != "UTF-8" {
			t.Fatalf("encoding: %s %v", encoding, err)
		}
		var bytes int64
		var identity sql.NullString
		if err = conn.QueryRowContext(context.Background(), `SELECT octet_length(model),CASE WHEN octet_length(model)<=1048576 THEN model ELSE NULL END FROM example LIMIT 1`).Scan(&bytes, &identity); err != nil {
			t.Fatal(err)
		}
		if bytes != 1048577 || identity.Valid {
			t.Fatalf("oversized identity transferred: bytes=%d valid=%v", bytes, identity.Valid)
		}
		var selected string
		if err = conn.QueryRowContext(context.Background(), `SELECT CASE WHEN octet_length(model)<=? THEN model ELSE NULL END FROM example WHERE CASE WHEN octet_length(model)=? THEN model=? COLLATE BINARY ELSE 0 END`, 1048576, 6, "模型").Scan(&selected); err != nil || selected != "模型" {
			t.Fatalf("guarded exact predicate: %q %v", selected, err)
		}
		if _, err = conn.ExecContext(context.Background(), "DELETE FROM example"); err == nil {
			t.Fatal("read-only connection wrote")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		var n int
		err = conn.QueryRowContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000000) SELECT SUM(x) FROM n`).Scan(&n)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("native interrupt: %v", err)
		}
		if err = conn.QueryRowContext(context.Background(), "SELECT 1").Scan(&n); err != nil || n != 1 {
			t.Fatalf("post-interrupt cleanup: %d %v", n, err)
		}
		if err = conn.Close(); err != nil {
			t.Fatal(err)
		}
		if err = reader.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("driver UTF-8 model guard transferred length=1048577 and NULL; native read interruption and connection/database cleanup succeeded")
	}
}
