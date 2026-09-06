package main

import (
	"bytes"
	"context"
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

// Never open SQLite to check an off-mode absence: doing so creates the artifact
// whose absence is the contract. Check again after the server lifecycle ends.
func assertDisabledUsageStore(t *testing.T, harness *usageMeterServeHarness) {
	t.Helper()
	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	if report := harness.closeStore(); report != (sqlitestore.Report{}) {
		t.Errorf("disabled meter unexpectedly finalized a store: %+v", report)
	}
	assertUsageFilesAbsent(t, harness.cfg.UsageDBPath)
}

func assertUsageFilesAbsent(t *testing.T, path string) {
	t.Helper()
	for _, artifact := range []string{filepath.Dir(path), path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(artifact); !os.IsNotExist(err) {
			t.Errorf("disabled usage artifact %q: %v; want absent", artifact, err)
		}
	}
}

func TestRunBoundServeDisabledUsageMeterCreatesNothing(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, surface.completion)
			}))
			t.Cleanup(upstream.Close)
			var logs bytes.Buffer
			harness := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, &logs), func(cfg *config.ServeConfig) {
				cfg.ShimUsageMeterEnabled = false
			}, nil)

			resp, body := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "disabled-usage", `{}`)
			if resp.StatusCode != http.StatusOK || string(body) != surface.completion {
				t.Errorf("disabled response = %d %q, want unchanged successful inference", resp.StatusCode, body)
			}
			assertUsageFilesAbsent(t, harness.cfg.UsageDBPath)
			assertDisabledUsageStore(t, harness)
			if strings.Contains(logs.String(), "usage store finalized") {
				t.Errorf("disabled meter emitted a usage-finalization record:\n%s", logs.String())
			}
		})
	}
}
