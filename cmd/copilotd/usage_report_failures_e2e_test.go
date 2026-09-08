package main

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestUsageStorageFailuresDoNotChangeInferenceReadinessOrWriterAdmission(t *testing.T) {
	for _, fixture := range []struct{ name, encoding, alter string }{
		{"missing native table", "UTF-8", "DROP TABLE anthropic_turn"},
		{"incompatible UTF16", "UTF-16le", ""},
		{"negative count", "UTF-8", `INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'private-row-id','',0,'private-bad-model','buffered',-1,2)`},
		{"invalid model encoding", "UTF-8", `INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'private-row-id','',0,CAST(x'ff' AS TEXT),'buffered',1,2)`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"still-admitted","status":"completed","model":"live-openai","usage":{"input_tokens":8012,"output_tokens":9}}`)
			}))
			t.Cleanup(upstream.Close)
			logs := newUsageReportLogs()
			h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, logs), func(cfg *config.ServeConfig) {
				// Prepare damaged/incompatible history BEFORE opening the daemon's
				// writer. No live replacement, deletion, or schema upgrade is tested
				// as though it were a supported operation.
				prepareUsageReportHistory(t, cfg.UsageDBPath, fixture.encoding, fixture.alter)
			}, nil)
			for range 4 {
				requestReportStatus(t, h, "GET", largeReportQuery, 503, "usage_unavailable")
			}
			for _, key := range []string{"", "wrong", testAPIKey} {
				req, _ := http.NewRequest("POST", h.baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-not-reported"}`))
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				want := 401
				if key == testAPIKey {
					want = 200
				}
				if resp.StatusCode != want {
					t.Fatalf("inference authentication changed: key=%q status=%d", key, resp.StatusCode)
				}
			}
			assertHTTPStatusEventually(t, h.baseURL+"/readyz", 200)
			if err := h.stop(); err != nil {
				t.Fatal(err)
			}
			assertCleanUsageReport(t, h.closeStore())
			db := openReportWriter(t, h.cfg.UsageDBPath)
			var n, input int
			if err := db.QueryRow("SELECT count(*),sum(input_tokens) FROM openai_turn WHERE response_id='still-admitted'").Scan(&n, &input); err != nil || n != 1 || input != 8012 {
				t.Fatalf("report failure changed writer admission: count=%d input=%d %v", n, input, err)
			}
			var journal string
			if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatalf("report failure changed WAL: %s %v", journal, err)
			}
			for _, line := range phase4LogLinesContaining(logs.String(), "msg=access", "inbound=/usage/v1/report") {
				for _, forbidden := range []string{h.cfg.UsageDBPath, "private-row-id", "private-bad-model", "synthetic-do-not-log", "SELECT", "input_tokens", "timezone=", "surface="} {
					if strings.Contains(line, forbidden) {
						t.Fatalf("failure access leaked %q: %s", forbidden, line)
					}
				}
			}
		})
	}
}

func TestUsageEncodedLimitRejectsWholeRealReportAndReleasesSlots(t *testing.T) {
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), nil, nil)
	writer := openReportWriter(t, h.cfg.UsageDBPath)
	// One legal 1 MiB identity fits the native reader's model/group budgets,
	// but its escaped occurrence in both row and model total exceeds 8 MiB.
	if _, err := writer.Exec(`INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'','',0,?,'buffered',1,2)`, strings.Repeat("\x01", 1<<20)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		requestReportStatus(t, h, "GET", largeReportQuery, 422, "report_too_large")
	}
	requestReportStatus(t, h, "HEAD", largeReportQuery, 422, "report_too_large")
	assertHTTPStatusEventually(t, h.baseURL+"/readyz", 200)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	assertCleanUsageReport(t, h.closeStore())
}

func prepareUsageReportHistory(t *testing.T, path, encoding, alter string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA encoding='" + encoding + "'"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_requested_model.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "internal", "usage", "sqlitestore", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if alter != "" {
		if _, err := db.Exec(alter); err != nil {
			t.Fatal(err)
		}
	}
}
