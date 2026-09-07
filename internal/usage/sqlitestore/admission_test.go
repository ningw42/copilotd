package sqlitestore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAdmissionConfiguresActualDedicatedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if err := preparePrivateDatabase(path); err != nil {
		t.Fatal(err)
	}
	// Use the same admission seam as Open, before handing the returned physical
	// connection to the writer. An unrelated external connection cannot verify
	// connection-local synchronous or busy_timeout settings.
	db, conn, err := admit(path, sql.Open)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()

	for _, check := range []struct {
		query string
		want  string
	}{
		{"PRAGMA journal_mode", "wal"},
		{"PRAGMA synchronous", "1"},     // NORMAL, not the external connection's default.
		{"PRAGMA busy_timeout", "5000"}, // Full runtime policy, not the last startup remainder.
		{"PRAGMA user_version", "2"},
		// Upgrade-sensitive identity check for the driver selected in root go.mod.
		{"SELECT sqlite_version()", "3.53.4"},
	} {
		var got string
		if err := conn.QueryRowContext(context.Background(), check.query).Scan(&got); err != nil {
			t.Fatalf("%s on admitted connection: %v", check.query, err)
		}
		if got != check.want {
			t.Errorf("%s on admitted connection = %q, want %q", check.query, got, check.want)
		}
		t.Logf("admitted connection: %s = %s", check.query, got)
	}
}
