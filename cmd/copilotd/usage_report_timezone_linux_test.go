//go:build linux

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/elf"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// Resolve host tools before sanitizing the CLI environment. No host PATH (or
// companion executable) is available in the jailed process.
func usageIsolationTools(t *testing.T) (unshare, chroot string) {
	t.Helper()
	resolve := func(name string) string {
		t.Helper()
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	return resolve("unshare"), resolve("chroot")
}

func TestUsageExecutableDiscoversContainerSystemTimezone(t *testing.T) {
	binaryPath := os.Getenv("COPILOTD_TEST_STATIC_BINARY")
	if binaryPath == "" {
		t.Skip("set COPILOTD_TEST_STATIC_BINARY to the CGO-disabled copilotd executable")
	}
	unshare, chroot := usageIsolationTools(t)
	root := t.TempDir()
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "copilotd"), data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"etc", "usr/share", "nix/store/fixture-tzdata/share/zoneinfo/Europe"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Only the configured zone asset is installed inside the CLI's filesystem.
	// Its root alias and relative localtime link model a NixOS/container layout.
	zones, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zones.Close()
	found := false
	for _, zone := range zones.File {
		if zone.Name != "Europe/Berlin" {
			continue
		}
		reader, err := zone.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "nix/store/fixture-tzdata/share/zoneinfo/Europe/Berlin"), data, 0600); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found {
		t.Fatal("missing timezone fixture")
	}
	if err := os.Symlink("/nix/store/fixture-tzdata/share/zoneinfo", filepath.Join(root, "usr/share/zoneinfo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../usr/share/zoneinfo/Europe/Berlin", filepath.Join(root, "etc/localtime")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/timezone"), []byte("America/New_York\n"), 0600); err != nil {
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
	requests := make(chan string, 10)
	handler := reporthttp.Handler(report.New(path).Query)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("timezone")
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"system link", nil, "Europe/Berlin"},
		{"configured empty TZ", []string{"TZ="}, "UTC"},
		{"named TZ", []string{"TZ=:Asia/Tokyo"}, "Asia/Tokyo"},
		{"invalid TZ cannot use system setting", []string{"TZ=:"}, ""},
		{"custom data disables discovery", []string{"ZONEINFO=/absent"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(unshare, "-Ur", chroot, root, "/copilotd", "usage", "--endpoint", server.URL, "--since", "2026-09-01", "--until", "2026-09-02")
			command.Env = append([]string{"PATH=/absent", "GOROOT=/absent"}, tc.env...)
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.Output()
			if tc.want == "" {
				if err == nil || len(stdout) != 0 || len(requests) != 0 || !strings.Contains(stderr.String(), "--timezone Area/City") {
					t.Fatalf("err=%v stdout=%s stderr=%s requests=%d", err, stdout, stderr.String(), len(requests))
				}
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
					t.Fatalf("expected exit 1: %v", err)
				}
			} else {
				if err != nil || stderr.Len() != 0 || !strings.Contains(string(stdout), `Timezone: "`+tc.want+`"`) || len(requests) != 1 {
					t.Fatalf("err=%v stdout=%s stderr=%s requests=%d", err, stdout, stderr.String(), len(requests))
				}
				if zone := <-requests; zone != tc.want {
					t.Fatalf("requested %q, want %q", zone, tc.want)
				}
			}
		})
	}
	t.Log("native Linux static executable: CLI-local Nix-style root alias, relative system link, stale metadata ignored, named/empty TZ, and pre-HTTP exit-1 failures verified in isolated filesystem")
}

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
	unshare, chroot := usageIsolationTools(t)
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
		command := exec.Command(unshare, append([]string{"-Ur", chroot, root, "/copilotd"}, args...)...)
		command.Env = []string{"PATH=/absent", "ZONEINFO=/absent", "GOROOT=/absent", "TZ=invalid/rules", "COPILOTD_CONFIG=/absent/config"}
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
		command := exec.Command(unshare, "-Ur", chroot, root, "/copilotd", "usage", "--endpoint", server.URL, "--timezone", zone, "--period", "month", "--since", "2024-02-29", "--until", "2024-03-01")
		command.Env = []string{"PATH=/absent", "ZONEINFO=/absent", "GOROOT=/absent", "TZ=invalid/rules"}
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
