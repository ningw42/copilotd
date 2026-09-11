//go:build !windows

package report_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestQueryDeniedDatabaseReadIsGenericUnavailable(t *testing.T) {
	path := stored(t, turn("2026-09-01T00:00:00Z", "permission", usage.OpenAIUsage{InputTokens: 1}))
	if _, err := newReporter(t, path).Query(context.Background(), selection()); err != nil {
		t.Fatalf("query closed writer fixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Errorf("restore database permissions: %v", err)
		}
	})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	file, openErr := os.Open(path)
	if openErr == nil {
		_ = file.Close()
		message := "database read denial cannot be established (privileged user or ineffective mode bits)"
		if os.Getenv("COPILOTD_TEST_NATIVE_TARGET") != "" {
			t.Fatal(message)
		}
		t.Skip(message)
	}
	if !errors.Is(openErr, os.ErrPermission) {
		t.Fatalf("database read denial preflight: %v; want os.ErrPermission", openErr)
	}

	got, err := newReporter(t, path).Query(context.Background(), selection())
	var failure *report.Error
	if !errors.As(err, &failure) {
		t.Fatalf("permission failure type: %v", err)
	}
	const message = "Usage data is unavailable on this daemon."
	if failure.Code != report.Unavailable || failure.Message != message || err.Error() != message {
		t.Fatalf("permission failure: %#v", failure)
	}
	if !reflect.DeepEqual(got, report.Report{}) {
		t.Fatalf("permission failure returned report: %+v", got)
	}
}
