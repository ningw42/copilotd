//go:build !windows

package sqlitestore_test

import (
	"context"
	"database/sql"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func externalLiteralSQLiteURI(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	return uri.String()
}

func queryLiteralVersionAndCount(t *testing.T, path string) (int, int) {
	t.Helper()
	db, err := sql.Open("sqlite", externalLiteralSQLiteURI(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("query literal %q version: %v", path, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count); err != nil {
		t.Fatalf("query literal %q rows: %v", path, err)
	}
	return version, count
}

func TestStoreOpensPunctuationFilenameAsLiteralDestination(t *testing.T) {
	for index, name := range []string{
		"usage?label=x.db",
		"usage%percent.db",
		"file:usage.db",
	} {
		t.Run(name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "private")
			path := filepath.Join(parent, name)
			store, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
			if err != nil {
				t.Fatalf("Open literal %q: %v", path, err)
			}
			store.Record(usage.Turn{
				At: time.UnixMilli(int64(index + 1)), ResponseID: "literal-" + strconv.Itoa(index),
				Model: "m", Transport: usage.TransportBuffered,
				Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1},
			})
			store.StopAdmission()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			report := store.Close(ctx)
			cancel()
			if !report.DriverCleanupCompleted || report.FinalFlushLosses != 0 {
				t.Fatalf("Close literal %q: %+v", path, report)
			}
			if version, count := queryLiteralVersionAndCount(t, path); version != 1 || count != 1 {
				t.Errorf("literal %q version/count = %d/%d, want 1/1", path, version, count)
			}
		})
	}
}

func TestStoreLiteralQueryFilenameCannotRedirectToNeighborSymlink(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	targetParent := filepath.Join(root, "target-private")
	if err := os.Mkdir(targetParent, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetParent, "sentinel.db")
	targetDB, err := sql.Open("sqlite", externalLiteralSQLiteURI(t, target))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetDB.Exec("CREATE TABLE neighbor_sentinel(value TEXT NOT NULL); INSERT INTO neighbor_sentinel VALUES ('untouched')"); err != nil {
		t.Fatal(err)
	}
	if err := targetDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	neighbor := filepath.Join(parent, "usage.db")
	if err := os.Symlink(target, neighbor); err != nil {
		t.Fatal(err)
	}
	literal := neighbor + "?_pragma=user_version(77)"
	store, err := sqlitestore.Open(literal, testStoreLogger(io.Discard))
	if err != nil {
		t.Fatalf("Open literal query filename: %v", err)
	}
	store.Record(usage.Turn{At: time.UnixMilli(1), ResponseID: "literal", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	report := store.Close(ctx)
	cancel()
	if !report.DriverCleanupCompleted || report.FinalFlushLosses != 0 {
		t.Fatalf("Close literal query filename: %+v", report)
	}
	if version, count := queryLiteralVersionAndCount(t, literal); version != 1 || count != 1 {
		t.Errorf("literal destination version/count = %d/%d, want 1/1", version, count)
	}

	targetDB, err = sql.Open("sqlite", externalLiteralSQLiteURI(t, target))
	if err != nil {
		t.Fatal(err)
	}
	defer targetDB.Close()
	var version int
	if err := targetDB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Errorf("neighbor target user_version = %d, %v; want untouched 0", version, err)
	}
	var sentinel string
	if err := targetDB.QueryRow("SELECT value FROM neighbor_sentinel").Scan(&sentinel); err != nil || sentinel != "untouched" {
		t.Errorf("neighbor sentinel = %q, %v; want untouched", sentinel, err)
	}
}
