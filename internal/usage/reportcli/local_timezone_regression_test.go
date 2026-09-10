package reportcli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
)

// These retained boundary cases exercise the implemented policy through Run;
// the development log distinguishes them from the preceding red/green tracers.
func TestCommandLocalTimezoneConfigurationRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, goos string
		env        map[string]string
		setup      func(timezoneFiles)
		want       string
	}{
		{"linux named", "linux", map[string]string{"TZ": "Europe/Berlin"}, nil, "Europe/Berlin"},
		{"macOS named alias", "darwin", map[string]string{"TZ": ":US/Eastern"}, nil, "US/Eastern"},
		{"configured UTC", "linux", map[string]string{"TZ": ""}, nil, "UTC"},
		{"macOS configured UTC", "darwin", map[string]string{"TZ": ""}, nil, "UTC"},
		{"empty data overrides", "linux", map[string]string{"TZ": "Etc/GMT+5", "TZDIR": "", "ZONEINFO": ""}, nil, "Etc/GMT+5"},
		{"Windows ignores named TZ", "windows", map[string]string{"TZ": "Europe/Berlin"}, nil, ""},
		{"Windows ignores empty TZ", "windows", map[string]string{"TZ": ""}, nil, ""},
		{"Windows absent TZ", "windows", nil, nil, ""},
		{"lone colon", "linux", map[string]string{"TZ": ":"}, nil, ""},
		{"two colons", "linux", map[string]string{"TZ": "::Europe/Berlin"}, nil, ""},
		{"Local is not discovery", "linux", map[string]string{"TZ": "Local"}, nil, ""},
		{"abbreviation is not discovery", "linux", map[string]string{"TZ": "CET"}, nil, ""},
		{"legacy rule name", "linux", map[string]string{"TZ": "EST5EDT"}, nil, ""},
		{"rules", "darwin", map[string]string{"TZ": "EST5EDT,M3.2.0,M11.1.0"}, nil, ""},
		{"offset", "linux", map[string]string{"TZ": "+05:00"}, nil, ""},
		{"invalid named value", "linux", map[string]string{"TZ": "Europe//Berlin"}, nil, ""},
		{"unknown named value", "linux", map[string]string{"TZ": "Unknown/Unavailable"}, nil, ""},
		{"untrimmed named value", "linux", map[string]string{"TZ": " Europe/Berlin"}, nil, ""},
		{"safe large invalid value", "linux", map[string]string{"TZ": strings.Repeat("\x1b\n", 2000)}, nil, ""},
		{"custom TZDIR", "linux", map[string]string{"TZ": "Europe/Berlin", "TZDIR": "/custom"}, nil, ""},
		{"custom ZONEINFO", "darwin", map[string]string{"ZONEINFO": "/custom"}, nil, ""},
		{"custom absolute file", "linux", map[string]string{"TZ": "/custom"}, func(f timezoneFiles) { f.zone("/custom") }, ""},
		{"Linux arbitrary zoneinfo component", "linux", map[string]string{"TZ": "/tmp/zoneinfo/Europe/Berlin"}, func(f timezoneFiles) { f.zone("/tmp/zoneinfo/Europe/Berlin") }, ""},
		{"absolute root file", "linux", map[string]string{"TZ": "/usr/lib/locale/TZ/Europe/Berlin"}, func(f timezoneFiles) { f.zone("/usr/lib/locale/TZ/Europe/Berlin") }, "Europe/Berlin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTimezoneFiles(t, tc.goos, tc.env)
			// A valid system link must not rescue an invalid, present TZ.
			if tc.goos != "windows" {
				f.zone("/usr/share/zoneinfo/Europe/Berlin")
				f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
			}
			if tc.setup != nil {
				tc.setup(f)
			}
			assertLocalTimezone(t, f, tc.want)
		})
	}
}

func assertLocalTimezone(t *testing.T, f timezoneFiles, want string) {
	t.Helper()
	client, options, requested := localTimezoneCommand(t)
	options.localSystem = f.system
	// The process setting wins over stale query metadata supplied by a caller.
	options.Query.Timezone = "Asia/Tokyo"
	var out bytes.Buffer
	err := Run(context.Background(), client, options, &out)
	if want == "" {
		if err == nil || !strings.Contains(err.Error(), "cannot determine a named local timezone") || !strings.Contains(err.Error(), "--timezone Area/City") || !strings.Contains(err.Error(), "--timezone UTC") || strings.ContainsAny(err.Error(), "\x1b\n") || len(err.Error()) > 300 || len(*requested) != 0 || out.Len() != 0 {
			t.Fatalf("err=%v requests=%v stdout=%s", err, *requested, out.String())
		}
		return
	}
	if err != nil || len(*requested) != 1 || (*requested)[0] != want || !strings.Contains(out.String(), "Timezone: "+want) {
		t.Fatalf("want=%s err=%v requests=%v stdout=%s", want, err, *requested, out.String())
	}
	// Discovery does not consume a one-time setting; every invocation transmits it.
	out.Reset()
	if err := Run(context.Background(), client, options, &out); err != nil || len(*requested) != 2 || (*requested)[1] != want {
		t.Fatalf("second invocation err=%v requests=%v", err, *requested)
	}
}

func TestCommandSystemTimezoneFilesystemRegressions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(timezoneFiles)
		want  string
	}{
		{"copied localtime with stale metadata", func(f timezoneFiles) { f.zone("/etc/localtime"); f.file("/etc/timezone", []byte("Europe/Berlin\n")) }, ""},
		{"timezone metadata only", func(f timezoneFiles) { f.file("/etc/timezone", []byte("Europe/Berlin\n")) }, ""},
		{"missing localtime", func(timezoneFiles) {}, ""},
		{"broken target", func(f timezoneFiles) { f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin") }, ""},
		{"custom copied target", func(f timezoneFiles) { f.zone("/custom"); f.link("/etc/localtime", "/custom") }, ""},
		{"Linux unrecognized root", func(f timezoneFiles) {
			f.zone("/tmp/zoneinfo/Europe/Berlin")
			f.link("/etc/localtime", "/tmp/zoneinfo/Europe/Berlin")
		}, ""},
		{"unreadable target", func(f timezoneFiles) {
			f.zone("/usr/share/zoneinfo/Europe/Berlin")
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
			f.system.open = func(string) (*os.File, error) {
				return nil, &os.PathError{Op: "open", Path: "unsafe\x1b[31m", Err: os.ErrPermission}
			}
		}, ""},
		{"unreadable symlink", func(f timezoneFiles) {
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
			f.system.readlink = func(string) (string, error) { return "", os.ErrPermission }
		}, ""},
		{"non-TZif", func(f timezoneFiles) {
			f.file("/usr/share/zoneinfo/Europe/Berlin", []byte("not a timezone"))
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
		}, ""},
		{"oversized TZif", func(f timezoneFiles) {
			f.zone("/usr/share/zoneinfo/Europe/Berlin")
			file, err := os.OpenFile(f.path("/usr/share/zoneinfo/Europe/Berlin"), os.O_WRONLY, 0)
			if err != nil {
				f.t.Fatal(err)
			}
			err = file.Truncate((1 << 20) + 1)
			_ = file.Close()
			if err != nil {
				f.t.Fatal(err)
			}
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
		}, ""},
		{"non-regular target", func(f timezoneFiles) {
			f.file("/usr/share/zoneinfo/Europe/Berlin/child", nil)
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
		}, ""},
		{"file symlink loop", func(f timezoneFiles) { f.link("/etc/localtime", "selected"); f.link("/etc/selected", "localtime") }, ""},
		{"directory symlink loop", func(f timezoneFiles) {
			f.link("/usr", "usr")
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
		}, ""},
		{"right subtree", func(f timezoneFiles) {
			f.zone("/usr/share/zoneinfo/right/Europe/Berlin")
			f.link("/etc/localtime", "/usr/share/zoneinfo/right/Europe/Berlin")
		}, ""},
		{"posix subtree alias", func(f timezoneFiles) {
			f.zone("/usr/share/zoneinfo/Europe/Berlin")
			f.link("/usr/share/zoneinfo/posix", ".")
			f.link("/etc/localtime", "/usr/share/zoneinfo/posix/Europe/Berlin")
		}, ""},
		{"right root alias", func(f timezoneFiles) {
			f.zone("/usr/share/lib/zoneinfo/right/Europe/Berlin")
			f.link("/usr/share/zoneinfo", "/usr/share/lib/zoneinfo/right")
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
		}, ""},
		{"stale metadata ignored", func(f timezoneFiles) {
			f.zone("/usr/share/zoneinfo/Europe/Berlin")
			f.link("/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
			f.file("/etc/timezone", []byte("America/New_York\n"))
		}, "Europe/Berlin"},
		{"etc zoneinfo", func(f timezoneFiles) {
			f.zone("/etc/zoneinfo/Europe/Berlin")
			f.link("/etc/localtime", "zoneinfo/Europe/Berlin")
		}, "Europe/Berlin"},
		{"directory and root aliases", func(f timezoneFiles) {
			f.zone("/data/tz/Europe/Berlin")
			f.link("/private/etc/localtime", "/usr/share/zoneinfo/Europe/Berlin")
			f.link("/etc", "private/etc")
			f.link("/usr/share/zoneinfo", "/etc/zoneinfo")
			f.link("/private/etc/zoneinfo", "/data/tz")
		}, "Europe/Berlin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTimezoneFiles(t, "linux", nil)
			tc.setup(f)
			assertLocalTimezone(t, f, tc.want)
		})
	}
}

func TestCommandTimezoneSymlinkFortyHopBoundary(t *testing.T) {
	for _, directories := range []bool{false, true} {
		for _, hops := range []int{40, 41} {
			t.Run(fmt.Sprintf("directories=%v/hops=%d", directories, hops), func(t *testing.T) {
				f := newTimezoneFiles(t, "linux", nil)
				f.zone("/data/Europe/Berlin")
				for n := 1; n < hops; n++ {
					target := fmt.Sprintf("/link%d", n+1)
					if n == hops-1 {
						target = "/data"
						if !directories {
							target += "/Europe/Berlin"
						}
					}
					f.link(fmt.Sprintf("/link%d", n), target)
				}
				if directories {
					// The localtime link plus the directory chain totals exactly hops.
					f.link("/etc/localtime", "/link1/Europe/Berlin")
				} else {
					f.link("/etc/localtime", "/link1")
				}
				f.link("/usr/share/zoneinfo", "/data")
				want := "Europe/Berlin"
				if hops == 41 {
					want = ""
				}
				assertLocalTimezone(t, f, want)
			})
		}
	}
}

func TestCommandExplicitTimezoneBypassesAllDiscovery(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			client, options, requested := localTimezoneCommand(t)
			// Nil filesystem dependencies and a fatal lookup make any discovery
			// visible without exposing test controls through the public API.
			options.localSystem = &localTimezoneSystem{goos: goos, lookupEnv: func(string) (string, bool) { t.Fatal("explicit override inspected environment"); return "", false }}
			for _, zone := range []string{"Europe/Berlin", "UTC", "", "Local", "NoSuch/Zone"} {
				options.Timezone = &zone
				before := len(*requested)
				var out bytes.Buffer
				err := Run(context.Background(), client, options, &out)
				if zone == "Europe/Berlin" || zone == "UTC" {
					if err != nil || len(*requested) != before+1 || (*requested)[before] != zone {
						t.Fatalf("zone=%q err=%v requests=%v", zone, err, *requested)
					}
				} else if err == nil || out.Len() != 0 || len(*requested) != before {
					t.Fatalf("invalid explicit zone=%q err=%v requests=%v", zone, err, *requested)
				}
			}
			// A raw HTTP query still has no daemon-local default.
			if _, err := client.Query(context.Background(), report.Query{}); err == nil || !strings.Contains(err.Error(), "400") {
				t.Fatalf("raw missing timezone: %v", err)
			}
		})
	}
}
