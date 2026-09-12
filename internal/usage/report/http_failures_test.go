package report_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestRealSQLiteFailuresUseGenericHTTPResponsesAndReleaseAdmission(t *testing.T) {
	for _, name := range []string{"missing", "schema", "contention", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			path := stored(t)
			query := report.New(path).Query
			switch name {
			case "missing":
				path = filepath.Join(t.TempDir(), "missing-parent", "private.db")
				query = report.New(path).Query
			case "cleanup":
				query = report.NewCleanupFailureForTest(path).Query
			default:
				db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				db.SetMaxOpenConns(1)
				if name == "schema" {
					if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
						t.Fatal(err)
					}
				} else {
					// A closed-writer fixture in rollback-journal mode induces a
					// native read lock. Never change a running daemon's WAL mode.
					if _, err := db.Exec("PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err != nil {
						t.Fatal(err)
					}
					defer db.Exec("ROLLBACK")
				}
			}
			srv := httptest.NewServer(reporthttp.Handler(query))
			defer srv.Close()
			started := time.Now()
			for range 4 {
				assertSQLiteHTTP(t, srv.URL, "?timezone=UTC&since=2026-09-01&until=2026-09-02", 503, `{"schema_version":1,"error":{"code":"usage_unavailable","message":"Usage data is unavailable on this daemon."}}`)
			}
			if name == "contention" {
				elapsed := time.Since(started)
				if elapsed > time.Second {
					t.Fatalf("four native 100ms caps exceeded tolerance: %s", elapsed)
				}
				t.Logf("four real native-lock HTTP failures returned 503 in %s", elapsed)
			}
			if name == "missing" {
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatalf("report created missing storage: %v", err)
				}
			}
		})
	}
}

func TestInterruptedRealSQLiteScanUsesHTTPDeadlinePrecedenceAndReleasesAdmission(t *testing.T) {
	path := stored(t)
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
		INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens)
		SELECT 1788220800000,'','',0,'native-scan','buffered',1,2 FROM n`); err != nil {
		t.Fatal(err)
	}
	reader := report.New(path)
	// A shorter parent HTTP context drives the adapter's deadline precedence.
	// Actual scanning, cancellation classification, and cleanup remain real
	// SQLite; this characterizes precedence, not a changed production 5s budget.
	handler := reporthttp.Handler(reader.Query)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The final empty-range request checks reuse under the normal budget,
		// not the platform's ability to open SQLite in ten milliseconds.
		if r.URL.Query().Get("since") == "2026-09-03" {
			handler.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Millisecond)
		defer cancel()
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()
	started := time.Now()
	for range 4 {
		assertSQLiteHTTP(t, srv.URL, "?timezone=UTC&since=2026-09-01&until=2026-09-02", 504, `{"schema_version":1,"error":{"code":"report_timeout","message":"Report work timed out."}}`)
	}
	assertSQLiteHTTP(t, srv.URL, "?timezone=UTC&since=2026-09-03&until=2026-09-04", 200, "")
	if _, err := db.Exec("BEGIN EXCLUSIVE; ROLLBACK"); err != nil {
		t.Fatalf("interrupted readers left transactions open: %v", err)
	}
	t.Logf("four real 100000-row scan interruptions and an empty-range success took %s", time.Since(started))
}

func assertSQLiteHTTP(t *testing.T, base, query string, status int, expected string) {
	t.Helper()
	response, err := (&http.Client{Timeout: 2 * time.Second}).Get(base + reporthttp.Path + query)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != status || expected != "" && string(body) != expected {
		t.Fatalf("HTTP status=%d body=%s error=%v", response.StatusCode, body, err)
	}
}
