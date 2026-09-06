package main

import (
	"database/sql"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunServeUsageSchemaFailurePrecedesBind(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	for _, tc := range []struct {
		name    string
		version int
		pragma  string
		wantErr []string
	}{
		{name: "future version", version: 2, pragma: "PRAGMA user_version=2", wantErr: []string{"schema version 2", "supported version 1"}},
		{name: "conflicting migration", version: 0, pragma: "PRAGMA user_version=0", wantErr: []string{"migration 1", "openai_turn"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "private")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "usage.db")
			// Private on Unix, ordinary exclusive regular-file creation on Windows;
			// the schema, not platform-specific permission rejection, must fail.
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, statement := range []string{
				`CREATE TABLE openai_turn (sentinel TEXT) STRICT`,
				`INSERT INTO openai_turn VALUES ('preserve existing data')`,
				tc.pragma,
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			logPath := filepath.Join(root, "serve.log")
			code := run([]string{
				"serve", "--apikey", testAPIKey, "--github-oauth-token", "gho-local",
				"--addr", held.Addr().String(), "--shim-usage-meter-enabled=true",
				"--usage-db-path", path, "--log-file", logPath,
				"--impersonation-refresh-interval", "0",
			}, noEnv(), io.Discard, io.Discard)
			if code != 1 {
				t.Fatalf("exit code = %d, want schema startup failure", code)
			}
			logs, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range append([]string{"opening usage database failed"}, tc.wantErr...) {
				if !strings.Contains(string(logs), want) {
					t.Errorf("schema refusal logs missing %q:\n%s", want, logs)
				}
			}
			if strings.Contains(string(logs), "bind failed") {
				t.Errorf("attempted occupied bind before schema refusal:\n%s", logs)
			}

			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var version, tables, rows int
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != tc.version {
				t.Errorf("user_version = %d, %v; want preserved %d", version, err, tc.version)
			}
			if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE sentinel='preserve existing data'`).Scan(&rows); err != nil || rows != 1 {
				t.Errorf("preserved rows = %d, %v; want 1", rows, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='anthropic_turn'`).Scan(&tables); err != nil || tables != 0 {
				t.Errorf("partial migration tables = %d, %v; want none", tables, err)
			}
		})
	}
}
