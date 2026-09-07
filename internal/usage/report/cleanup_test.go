package report

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestQueryRejectsReportWhenNativeCleanupFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close(context.Background())
	reader := New(path)
	// Private fault fixture keeps the real SQLite read and native close. Only
	// the close result is failed; the observation remains the Query interface.
	reader.closeDB = func(db *sql.DB) error { return errors.Join(db.Close(), errors.New("injected native cleanup failure")) }
	result, err := reader.Query(context.Background(), Query{Surface: "openai", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != Unavailable || result.OpenAI != nil {
		t.Fatalf("cleanup error yielded report: %+v %v", result, err)
	}
}
