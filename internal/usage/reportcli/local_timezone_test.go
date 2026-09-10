package reportcli

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
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
	files.system.open = func(string) (*os.File, error) {
		t.Fatal("fatal root traversal reached zone file open")
		return nil, nil
	}
	assertLocalTimezone(t, files, "")
}

func TestCommandPreviouslyObservedZoneinfoRootMissingFailsBeforeFileOpen(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	rootObservations := 0
	files.system.lstat = func(name string) (os.FileInfo, error) {
		if name == "/usr/share/zoneinfo" {
			rootObservations++
			if rootObservations == 2 {
				return nil, os.ErrNotExist
			}
		}
		return os.Lstat(files.path(name))
	}
	opened := false
	files.system.open = func(name string) (*os.File, error) {
		opened = true
		return os.Open(files.path(name))
	}
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = files.system
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot determine a named local timezone") || strings.Contains(err.Error(), "/usr/share/zoneinfo") || strings.Contains(err.Error(), os.ErrNotExist.Error()) || rootObservations != 2 || opened || len(*requested) != 0 || out.Len() != 0 {
		t.Fatalf("err=%v root observations=%d opened=%t requests=%v stdout=%s", err, rootObservations, opened, *requested, out.String())
	}
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

func TestCommandChangedUnusedAbsentZoneinfoRootSucceeds(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	changed := false
	files.system.open = func(name string) (*os.File, error) {
		if !changed {
			changed = true
			files.link("/etc/zoneinfo", "/usr/share/zoneinfo")
		}
		return os.Open(files.path(name))
	}
	assertLocalTimezone(t, files, "Europe/Berlin")
}

func TestCommandChangedUnusedExistingZoneinfoRootSucceeds(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.file("/unused/first/.keep", nil)
	files.file("/unused/second/.keep", nil)
	files.link("/usr/share/lib/zoneinfo", "/unused/first")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	changed := false
	files.system.open = func(name string) (*os.File, error) {
		if !changed {
			changed = true
			if err := os.Remove(files.path("/usr/share/lib/zoneinfo")); err != nil {
				t.Fatal(err)
			}
			files.link("/usr/share/lib/zoneinfo", "/unused/second")
		}
		return os.Open(files.path(name))
	}
	assertLocalTimezone(t, files, "Europe/Berlin")
}

func TestCommandChangedSelectedDirectoryMetadataSucceeds(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	files.zone("/usr/share/zoneinfo/Europe/Berlin")
	files.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
	root, err := os.Stat(files.path("/usr/share/zoneinfo"))
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	files.system.open = func(name string) (*os.File, error) {
		if !changed {
			changed = true
			files.file("/usr/share/zoneinfo/unrelated", nil)
			modified := root.ModTime().Add(time.Hour)
			if err := os.Chtimes(files.path("/usr/share/zoneinfo"), modified, modified); err != nil {
				t.Fatal(err)
			}
			current, err := os.Stat(files.path("/usr/share/zoneinfo"))
			if err != nil || current.ModTime().Equal(root.ModTime()) {
				t.Fatalf("directory metadata did not change: before=%v after=%v err=%v", root.ModTime(), current.ModTime(), err)
			}
		}
		return os.Open(files.path(name))
	}
	assertLocalTimezone(t, files, "Europe/Berlin")
}

func TestCommandChangedSelectedRootAliasFailsBeforeHTTP(t *testing.T) {
	files := newTimezoneFiles(t, "linux", nil)
	first := "/nix/store/first-tzdata/share/zoneinfo"
	second := "/nix/store/second-tzdata/share/zoneinfo"
	files.zone(first + "/Europe/Berlin")
	files.zone(second + "/Europe/Berlin")
	files.link("/etc/zoneinfo", first)
	// The source path reaches the store directly. Only the root walk observes
	// the alias that establishes the Europe/Berlin suffix.
	files.link("/etc/localtime", first+"/Europe/Berlin")
	files.system.open = func(name string) (*os.File, error) {
		if err := os.Remove(files.path("/etc/zoneinfo")); err != nil {
			t.Fatal(err)
		}
		files.link("/etc/zoneinfo", second)
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

func TestCommandChangedSelectedTZifFailsBeforeHTTP(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", nil)
			selected := "/usr/share/zoneinfo/Europe/Berlin"
			files.zone(selected)
			files.zone("/replacement")
			files.link("/etc/localtime", selected)
			opened := false
			files.system.open = func(name string) (*os.File, error) {
				file, err := os.Open(files.path(name))
				opened = err == nil
				return file, err
			}
			changed := false
			files.system.lstat = func(name string) (os.FileInfo, error) {
				if opened && !changed && name == selected {
					changed = true
					if replace {
						return os.Lstat(files.path("/replacement"))
					}
					before, err := os.Stat(files.path(selected))
					if err != nil {
						t.Fatal(err)
					}
					file, err := os.OpenFile(files.path(selected), os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteAt([]byte{'X'}, 0)
					closeErr := file.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatalf("mutate TZif: write=%v close=%v", writeErr, closeErr)
					}
					modified := before.ModTime().Add(time.Hour)
					if err := os.Chtimes(files.path(selected), modified, modified); err != nil {
						t.Fatal(err)
					}
				}
				return os.Lstat(files.path(name))
			}
			assertLocalTimezone(t, files, "")
			if !changed {
				t.Fatal("selected TZif was not changed")
			}
		})
	}
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

func TestCommandAcceptsNixOSSystemZoneinfoDataRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"system timezone with TZDIR", map[string]string{"TZDIR": "/etc/zoneinfo"}},
		{"named TZ with TZDIR", map[string]string{"TZ": "Europe/Berlin", "TZDIR": "/etc/zoneinfo"}},
		{"system timezone with ZONEINFO", map[string]string{"ZONEINFO": "/etc/zoneinfo"}},
		{"named TZ with ZONEINFO", map[string]string{"TZ": "Europe/Berlin", "ZONEINFO": "/etc/zoneinfo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := newTimezoneFiles(t, "linux", tc.env)
			files.zone("/nix/store/fixture-tzdata/share/zoneinfo/Europe/Berlin")
			files.link("/etc/zoneinfo", "/nix/store/fixture-tzdata/share/zoneinfo")
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
		})
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
