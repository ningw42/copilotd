//go:build windows

package sqlitestore_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestStoreWindowsPermissionsAreExplicitlyBestEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	report := store.Close(ctx)
	cancel()
	if !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("main database = %#v, %v; want a regular exclusively-created file", info, err)
	}
	// Deliberately no FileMode assertion: Go mode bits neither configure nor
	// certify Windows ACL inheritance for the main file or SQLite sidecars.
}
