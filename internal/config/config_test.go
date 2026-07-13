package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envFunc builds a lookupEnv function backed by a map, so Load can be driven
// without touching the process environment.
func envFunc(m map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// noEnv is an empty environment.
func noEnv() func(string) (string, bool) { return envFunc(nil) }

func defaultConfig() Config {
	return Config{
		Addr:            "127.0.0.1:8080",
		LogLevel:        "info",
		LogFormat:       "text",
		LogFile:         "",
		ShutdownTimeout: 10 * time.Second,
	}
}

func TestLoadDefaults(t *testing.T) {
	got, err := Load(nil, noEnv())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != defaultConfig() {
		t.Errorf("Load() = %+v, want %+v", got, defaultConfig())
	}
}

func TestLoadPrecedence(t *testing.T) {
	// A TOML file setting every key; env and flags will override subsets of it
	// so we can observe the flags > env > file > default ordering.
	toml := strings.Join([]string{
		`addr = "10.0.0.1:1111"`,
		`log-level = "warn"`,
		`log-format = "json"`,
		`log-file = "/tmp/from-file.log"`,
		`shutdown-timeout = "30s"`,
	}, "\n")

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		writeFile  bool // write the TOML above and point --config/env at it
		fileViaEnv bool
		want       Config
	}{
		{
			name: "env overrides default",
			env:  map[string]string{"COPILOTD_ADDR": "0.0.0.0:9090", "COPILOTD_LOG_LEVEL": "debug"},
			want: Config{Addr: "0.0.0.0:9090", LogLevel: "debug", LogFormat: "text", ShutdownTimeout: 10 * time.Second},
		},
		{
			name: "flag overrides env",
			args: []string{"--addr", "127.0.0.1:7000", "--log-level=error"},
			env:  map[string]string{"COPILOTD_ADDR": "0.0.0.0:9090", "COPILOTD_LOG_LEVEL": "debug"},
			want: Config{Addr: "127.0.0.1:7000", LogLevel: "error", LogFormat: "text", ShutdownTimeout: 10 * time.Second},
		},
		{
			name:      "file under env under flag; file-only keys still apply",
			writeFile: true,
			// --config is supplied per-test in the body; here flag overrides addr,
			// env overrides log-level, the rest come from the file.
			args: []string{"--addr", "127.0.0.1:7000"},
			env:  map[string]string{"COPILOTD_LOG_LEVEL": "error"},
			want: Config{
				Addr:            "127.0.0.1:7000",     // flag wins
				LogLevel:        "error",              // env wins over file "warn"
				LogFormat:       "json",               // from file
				LogFile:         "/tmp/from-file.log", // from file
				ShutdownTimeout: 30 * time.Second,     // from file
			},
		},
		{
			name:       "config path honored via COPILOTD_CONFIG env",
			writeFile:  true,
			fileViaEnv: true,
			env:        map[string]string{},
			want: Config{
				Addr:            "10.0.0.1:1111",
				LogLevel:        "warn",
				LogFormat:       "json",
				LogFile:         "/tmp/from-file.log",
				ShutdownTimeout: 30 * time.Second,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			args := append([]string(nil), tc.args...)
			if tc.writeFile {
				path := filepath.Join(t.TempDir(), "copilotd.toml")
				if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
					t.Fatalf("write toml: %v", err)
				}
				if tc.fileViaEnv {
					env["COPILOTD_CONFIG"] = path
				} else {
					args = append(args, "--config", path)
				}
			}

			got, err := Load(args, envFunc(env))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("Load() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"bad addr missing port", []string{"--addr", "127.0.0.1"}, "addr"},
		{"bad addr non-numeric port", []string{"--addr", "127.0.0.1:notaport"}, "addr"},
		{"unknown log level", []string{"--log-level", "trace"}, "log-level"},
		{"unknown log format", []string{"--log-format", "xml"}, "log-format"},
		{"non-positive shutdown timeout", []string{"--shutdown-timeout", "0s"}, "shutdown-timeout"},
		{"negative shutdown timeout", []string{"--shutdown-timeout", "-1s"}, "shutdown-timeout"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.args, noEnv())
			if err == nil {
				t.Fatalf("Load() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Load() error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestConfigLogValueEmitsOnlyNonSecretFields(t *testing.T) {
	cfg := Config{
		Addr:            "127.0.0.1:8080",
		LogLevel:        "info",
		LogFormat:       "text",
		LogFile:         "/var/log/copilotd.log",
		ShutdownTimeout: 10 * time.Second,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("effective config", "config", cfg)
	out := buf.String()

	for _, want := range []string{
		"config.addr=127.0.0.1:8080",
		"config.log-level=info",
		"config.log-format=text",
		"config.log-file=/var/log/copilotd.log",
		"config.shutdown-timeout=10s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\nfull: %s", want, out)
		}
	}
}

func TestVersionRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no version", []string{"--addr", ":8080"}, false},
		{"long version", []string{"--version"}, true},
		{"version among others", []string{"--addr", ":8080", "--version"}, true},
		{"nil args", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VersionRequested(tc.args); got != tc.want {
				t.Errorf("VersionRequested(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
