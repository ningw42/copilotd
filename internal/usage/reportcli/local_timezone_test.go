package reportcli

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// A real store and adapter keep discovery assertions at Run's HTTP/output seam.
func localTimezoneCommand(t *testing.T) (*reporthttp.Client, Options, *[]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	at, _ := time.Parse(time.RFC3339, "2026-08-31T23:30:00Z")
	store.Record(usage.Turn{At: at, Model: "boundary", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 17, OutputTokens: 3}})
	if result := store.Close(context.Background()); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("fixture: %+v", result)
	}
	var requested []string
	handler := reporthttp.Handler(report.New(path).Query)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Query().Get("timezone"))
		for _, key := range []string{"Authorization", "X-Api-Key", "Cookie"} {
			if r.Header.Get(key) != "" {
				t.Errorf("unexpected %s", key)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return client, Options{Endpoint: server.URL, Query: report.Query{Surface: "openai", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: 5 * time.Second}, &requested
}

type timezoneFiles struct {
	t      *testing.T
	root   string
	system *localTimezoneSystem
}

func newTimezoneFiles(t *testing.T, goos string, env map[string]string) timezoneFiles {
	t.Helper()
	f := timezoneFiles{t: t, root: t.TempDir()}
	f.system = &localTimezoneSystem{goos: goos, lookupEnv: func(key string) (string, bool) { value, ok := env[key]; return value, ok },
		readlink: func(name string) (string, error) {
			target, err := os.Readlink(f.path(name))
			// The fixture models Unix names even on Windows, where os.Symlink
			// converts authored slashes to native separators in the reparse data.
			return filepath.ToSlash(target), err
		},
		lstat: func(name string) (os.FileInfo, error) { return os.Lstat(f.path(name)) },
		open:  func(name string) (*os.File, error) { return os.Open(f.path(name)) },
	}
	return f
}
func (f timezoneFiles) path(name string) string {
	return filepath.Join(f.root, filepath.FromSlash(strings.TrimPrefix(name, "/")))
}
func (f timezoneFiles) file(name string, data []byte) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(name)), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.path(name), data, 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f timezoneFiles) zone(name string) {
	f.t.Helper()
	z, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		f.t.Fatal(err)
	}
	defer z.Close()
	for _, entry := range z.File {
		if entry.Name != "Europe/Berlin" {
			continue
		}
		r, err := entry.Open()
		if err != nil {
			f.t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			f.t.Fatal(err)
		}
		f.file(name, data)
		return
	}
	f.t.Fatal("timezone fixture not in toolchain data")
}
func (f timezoneFiles) link(name, target string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(name)), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, f.path(name)); err != nil {
		if runtime.GOOS == "windows" {
			f.t.Skipf("symlink fixture unavailable: %v", err)
		}
		f.t.Fatal(err)
	}
	actual, err := os.Readlink(f.path(name))
	if err != nil || filepath.ToSlash(actual) != target {
		f.t.Fatalf("Unix symlink fixture %q: native target=%q, authored=%q, err=%v", name, actual, target, err)
	}
}

func TestCommandRelativeNamedAliasRemainsAmbiguous(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/America/New_York")
	files.link("/usr/share/zoneinfo/US/Eastern", "../America/New_York")
	files.link("/etc/localtime", "../usr/share/zoneinfo/US/Eastern")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandUnverifiableZoneinfoRootFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	files.system.lstat = func(name string) (os.FileInfo, error) {
		if name == "/etc/zoneinfo" {
			return nil, os.ErrPermission
		}
		return os.Lstat(files.path(name))
	}
	assertLocalTimezone(t, files, "")
}

func TestCommandPreservesMacOSVersionedRootAliasSuffix(t *testing.T) {
	files := newTimezoneFiles(t, "darwin", nil)
	files.zone("/private/timezone-data/Europe/Berlin")
	files.link("/private/var/db/timezone/tz/2026b.1.0/zoneinfo", "/private/timezone-data")
	files.link("/etc/localtime", "/private/var/db/timezone/tz/2026b.1.0/zoneinfo/Europe/Berlin")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandChangedZoneinfoRootSetFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	// Change an initially absent recognized root while the final file is opened.
	// Keep parent directory metadata stable so the changed root itself matters.
	etc, err := os.Stat(files.path("/etc"))
	if err != nil {
		t.Fatal(err)
	}
	files.system.open = func(name string) (*os.File, error) {
		files.link("/etc/zoneinfo", "/usr/share/zoneinfo")
		if err := os.Chtimes(files.path("/etc"), etc.ModTime(), etc.ModTime()); err != nil {
			t.Fatal(err)
		}
		return os.Open(files.path(name))
	}
	assertLocalTimezone(t, files, "")
}

func TestCommandNamedPosixTZNeedsExplicitOverride(t *testing.T) {
	files := newTimezoneFiles(t, "linux", map[string]string{"TZ": "posix/Europe/Berlin"})
	assertLocalTimezone(t, files, "")
}

func TestCommandTruncatedTZifFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.file("/usr/share/zoneinfo/Europe/Berlin", []byte("TZif"))
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	assertLocalTimezone(t, files, "")
}

func TestCommandInconsistentOpenedZoneFileFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.zone("/replacement")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	files.system.open = func(string) (*os.File, error) { return os.Open(files.path("/replacement")) }
	assertLocalTimezone(t, files, "")
}

func TestCommandChangedSystemTimezoneFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.zone("/usr/share/zoneinfo/America/New_York")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	files.system.open = func(name string) (*os.File, error) {
		if err := os.Remove(files.path("/etc/localtime")); err != nil {
			t.Fatal(err)
		}
		files.link("/etc/localtime", "/usr/share/zoneinfo/America/New_York")
		return os.Open(files.path(name))
	}
	assertLocalTimezone(t, files, "")
}

func TestCommandRejectsPosixSubtreeInsteadOfStrippingIt(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/posix/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/posix/Europe/Berlin")
	assertLocalTimezone(t, files, "")
}

func TestCommandAmbiguousNamedSymlinkChainFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/America/New_York")
	files.link("/usr/share/zoneinfo/US/Eastern", "../America/New_York")
	files.link("/etc/localtime", "/usr/share/zoneinfo/US/Eastern")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandDiscoversMacOSVersionedZoneinfo(t *testing.T) {
	files := newTimezoneFiles(t, "darwin", nil)
	files.zone("/private/var/db/timezone/tz/2026b.1.0/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/var/db/timezone/zoneinfo/Europe/Berlin")
	files.link("/var", "private/var")
	files.link("/private/var/db/timezone/zoneinfo", "tz/2026b.1.0/zoneinfo")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandDiscoversAbsoluteTZZoneFile(t *testing.T) {
	files := newTimezoneFiles(t, "linux", map[string]string{"TZ": ":/usr/share/lib/zoneinfo/Europe/Berlin"})
	files.zone("/usr/share/lib/zoneinfo/Europe/Berlin")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandDiscoversResolvedNixOSZoneinfoRoot(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/nix/store/fixture-tzdata/share/zoneinfo/Europe/Berlin")
	files.link("/usr/share/zoneinfo", "/nix/store/fixture-tzdata/share/zoneinfo")
	files.link("/etc/localtime", "/nix/store/fixture-tzdata/share/zoneinfo/Europe/Berlin")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandDiscoversRelativeSystemSymlinkChain(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "../etc/selected")
	files.link("/etc/selected", "../usr/share/zoneinfo/Europe/Berlin")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandDiscoversSystemTimezoneSymlink(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" || !strings.Contains(out.String(), "Timezone: Europe/Berlin") {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandInvalidUnixTZNeedsSafeGuidanceBeforeHTTP(t *testing.T) {
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = &localTimezoneSystem{goos: "linux", lookupEnv: func(key string) (string, bool) {
		if key == "TZ" {
			return "EST5EDT,M3.2.0,M11.1.0\x1b[31m", true
		}
		return "", false
	}}
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot determine a named local timezone") || !strings.Contains(err.Error(), "--timezone Area/City") || strings.ContainsAny(err.Error(), "\x1b\n") || len(err.Error()) > 300 || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandNativeWindowsRequiresExplicitTimezone(t *testing.T) {
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = &localTimezoneSystem{goos: "windows", lookupEnv: func(key string) (string, bool) {
		if key == "TZ" {
			return "Europe/Berlin", true
		}
		return "", false
	}}
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "native Windows") || !strings.Contains(err.Error(), "--timezone Area/City") || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandCustomZONEINFORequiresOverrideBeforeHTTP(t *testing.T) {
	t.Setenv("TZ", "") // Even configured UTC cannot bypass a custom data override.
	t.Setenv("TZDIR", "")
	t.Setenv("ZONEINFO", "/custom")
	client, options, requested := localTimezoneCommand(t)
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "--timezone Area/City") || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandCustomTZDIRRequiresOverrideBeforeHTTP(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	t.Setenv("TZDIR", "/custom\x1b[31m")
	t.Setenv("ZONEINFO", "")
	client, options, requested := localTimezoneCommand(t)
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot determine a named local timezone") || !strings.Contains(err.Error(), "--timezone Area/City") || strings.ContainsAny(err.Error(), "\x1b\n") || len(err.Error()) > 300 || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("unsupported automatic configuration: err=%v requests=%v stdout=%s", err, *requested, out.String())
	}
}

func TestCommandUnixTZAllowsOneLeadingColon(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix environment")
	}
	t.Setenv("TZ", ":US/Eastern")
	t.Setenv("TZDIR", "")
	t.Setenv("ZONEINFO", "")
	client, options, requested := localTimezoneCommand(t)
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "US/Eastern" || !strings.Contains(out.String(), "Timezone: US/Eastern") {
		t.Fatalf("requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandEmptyUnixTZSelectsConfiguredUTC(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix environment")
	}
	t.Setenv("TZ", "")
	t.Setenv("TZDIR", "")
	t.Setenv("ZONEINFO", "")
	client, options, requested := localTimezoneCommand(t)
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "UTC" || !strings.Contains(out.String(), "Timezone: UTC") || !strings.Contains(out.String(), "No stored Turns in the selected range.") {
		t.Fatalf("configured UTC: requests=%v\n%s", *requested, out.String())
	}
}

func TestCommandDiscoversNamedUnixTimezoneThroughHTTP(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix environment")
	}
	t.Setenv("TZ", "Europe/Berlin")
	t.Setenv("TZDIR", "")
	t.Setenv("ZONEINFO", "")
	previous := time.Local
	time.Local = time.FixedZone("daemon", -7*3600)
	t.Cleanup(func() { time.Local = previous })
	client, options, requested := localTimezoneCommand(t)
	var out bytes.Buffer
	if err := Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != 1 || (*requested)[0] != "Europe/Berlin" || !strings.Contains(out.String(), "Timezone: Europe/Berlin") || !strings.Contains(out.String(), "boundary") || strings.Contains(out.String(), `"boundary"`) || !strings.Contains(out.String(), "  17 ") {
		t.Fatalf("terminal-local selection: requests=%v\n%s", *requested, out.String())
	}
}
