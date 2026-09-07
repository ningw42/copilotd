package report_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

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
	if _, err = db.Exec(`CREATE TABLE example(model TEXT); INSERT INTO example VALUES(printf('%.*c',1048577,'x')),('kept')`); err != nil {
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
		if err = conn.QueryRowContext(context.Background(), `SELECT model FROM example WHERE CASE WHEN octet_length(model)=4 THEN model='kept' COLLATE BINARY ELSE 0 END`).Scan(&selected); err != nil || selected != "kept" {
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
