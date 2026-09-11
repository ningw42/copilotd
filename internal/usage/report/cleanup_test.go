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

// NewCleanupFailureForTest is compiled only into this package's test binary.
// It lets external HTTP-adapter tests drive the approved private native-close
// fault without exporting a production fault hook or mocking the SQLite read.
func NewCleanupFailureForTest(path string) *Reporter {
	reader := newReporterForTest(path)
	reader.closeDB = func(db *sql.DB) error {
		return errors.Join(db.Close(), errors.New("injected native cleanup failure at private/path"))
	}
	return reader
}

func TestQueryRejectsReportWhenNativeCleanupFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close(context.Background())
	// The private fault keeps the real SQLite read and native close. Only the
	// close result is failed; the observation remains the Query interface.
	reader := NewCleanupFailureForTest(path)
	result, err := reader.Query(context.Background(), Query{Surface: "openai", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != Unavailable || result.OpenAI != nil {
		t.Fatalf("cleanup error yielded report: %+v %v", result, err)
	}
}
