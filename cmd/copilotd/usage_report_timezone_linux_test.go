//go:build linux

package main

import (
	"context"
	"debug/elf"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// Opt-in real executable acceptance: build CGO_ENABLED=0 in ignored scratch,
// then provide its absolute path. This is Linux runtime evidence only, not
// native macOS/Windows certification. No zone asset is copied into the jail.
func TestUsageExecutableEmbeddedTimezoneWithoutHostData(t *testing.T) {
	binaryPath := os.Getenv("COPILOTD_TEST_STATIC_BINARY")
	if binaryPath == "" {
		t.Skip("set COPILOTD_TEST_STATIC_BINARY to the CGO-disabled copilotd executable")
	}
	file, err := elf.Open(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			t.Fatal("timezone isolation requires a static executable")
		}
	}
	_ = file.Close()
	root := t.TempDir()
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "copilotd"), data, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(discardLogger(t), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	if result := store.Close(context.Background()); !result.DriverCleanupCompleted {
		t.Fatalf("fixture: %+v", result)
	}
	var calls atomic.Int32
	handler := reporthttp.Handler(report.New(path).Query)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler.ServeHTTP(w, r) }))
	defer server.Close()
	invoke := func(args ...string) (string, error) {
		command := exec.Command("unshare", append([]string{"-Ur", "chroot", root, "/copilotd"}, args...)...)
		command.Env = append(os.Environ(), "ZONEINFO=/absent", "GOROOT=/absent", "TZ=invalid/rules", "COPILOTD_CONFIG=/absent/config")
		output, err := command.CombinedOutput()
		return string(output), err
	}
	// Help/root/version must not load the missing config, resolve TZ, or request.
	for _, args := range [][]string{{}, {"--help"}, {"usage", "--help"}, {"help", "usage"}, {"version"}} {
		if output, err := invoke(args...); err != nil {
			t.Fatalf("side-effect-free %v: %v %s", args, err, output)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("help contacted daemon")
	}
	for _, zone := range []string{"America/New_York", "Asia/Tokyo", "US/Eastern", "Etc/UTC", "Etc/GMT+5", "Europe/Berlin", "UTC"} {
		// Override the intentionally invalid/missing configuration only for execution.
		command := exec.Command("unshare", "-Ur", "chroot", root, "/copilotd", "usage", "--endpoint", server.URL, "--timezone", zone, "--period", "month", "--since", "2024-02-29", "--until", "2024-03-01")
		command.Env = []string{"ZONEINFO=/absent", "GOROOT=/absent", "TZ=invalid/rules"}
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), `Timezone: "`+zone+`"`) || !strings.Contains(string(output), "No stored Turns in the selected range.") {
			t.Fatalf("embedded %s: %v\n%s", zone, err, output)
		}
	}
	if calls.Load() != 7 {
		t.Fatalf("one request per explicit zone: %d", calls.Load())
	}
	t.Log("static /copilotd executed under unshare -Ur chroot; jail contains only executable, with no host/Nix/GOROOT timezone data; seven named zones/aliases and side-effect-free help/root/version passed")
}
