//go:build !windows

package report_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestQueryUsesLiteralConfiguredPathPunctuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "file:usage?mode=memory#%25.db")
	writer, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(turn("2026-09-01T00:00:00Z", "literal", usage.OpenAIUsage{InputTokens: 19}))
	writer.Close(context.Background())
	result, err := newReporter(t, path).Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if result.OpenAI.Total.Turns != 1 || *result.OpenAI.Total.Usage["input_tokens"].Sum != 19 {
		t.Fatalf("literal path selected wrong database: %+v", result)
	}
}
