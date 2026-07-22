package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peterbourgon/ff/v4"
)

// envFunc builds a lookupEnv function backed by a map, so Resolve can be driven
// without touching the process environment.
func envFunc(m map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// noEnv is an empty environment.
func noEnv() func(string) (string, bool) { return envFunc(nil) }

// testAPIKey is the required inbound key supplied by tests that are not about the
// key itself, so Resolve passes its fail-fast validation.
const testAPIKey = "test-api-key"

// loadServe builds the serve flag set the way the command tree does, parses
// args, and resolves. It is
// the test seam that keeps the Phase 0 precedence/validation tests intact after
// the split into RegisterServe + Resolve.
func loadServe(args []string, lookupEnv func(string) (string, bool)) (ServeConfig, error) {
	fs := ff.NewFlagSet("copilotd")
	f := RegisterServe(fs)
	if err := ff.Parse(fs, args); err != nil {
		return ServeConfig{}, fmt.Errorf("parse flags: %w", err)
	}
	return f.Resolve(lookupEnv)
}

func defaultConfig() ServeConfig {
	return ServeConfig{
		Addr:                         "127.0.0.1:8080",
		LogLevel:                     "info",
		LogFormat:                    "text",
		LogFile:                      "",
		ShutdownTimeout:              10 * time.Second,
		GithubOAuthTokenFile:         defaultOAuthTokenFile(),
		APIKey:                       testAPIKey,
		OutboundTimeout:              600 * time.Second,
		StreamIdleTimeout:            5 * time.Minute,
		StreamKeepaliveInterval:      15 * time.Second,
		WriteTimeout:                 90 * time.Second,
		ResponseHeaderTimeout:        600 * time.Second,
		WebSocketHandshakeTimeout:    10 * time.Second,
		MaxRequestBytes:              33554432,
		MaxBufferedResponseBytes:     33554432,
		CodexCatalogRefreshInterval:  24 * time.Hour,
		StartupMintRetries:           3,
		VSCodeVersionFallback:        "1.104.1",
		PluginVersionFallback:        "0.26.7",
		CopilotIntegrationID:         "vscode-chat",
		GithubAPIVersion:             "2025-04-01",
		ImpersonationRefreshInterval: 24 * time.Hour,
	}
}

func TestLoadDefaults(t *testing.T) {
	got, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v", err)
	}
	if !reflect.DeepEqual(got, defaultConfig()) {
		t.Errorf("loadServe() = %+v, want %+v", got, defaultConfig())
	}
}

func TestCodexAutoReviewModelOverridesNormalizesEmptyInputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "default"},
		{name: "explicit empty", env: map[string]string{"COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES": ""}},
		{name: "empty segments", args: []string{"--codex-auto-review-model-overrides", " , , "}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--apikey", testAPIKey}, tc.args...)
			got, err := loadServe(args, envFunc(tc.env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.Codex.AutoReviewModelOverrides != nil {
				t.Errorf("AutoReviewModelOverrides = %#v, want canonical nil map", got.Codex.AutoReviewModelOverrides)
			}
		})
	}
}

func TestCodexAutoReviewModelOverridesResolvesFlag(t *testing.T) {
	got, err := loadServe([]string{
		"--apikey", testAPIKey,
		"--codex-auto-review-model-overrides", "gpt-5=reviewer-mini",
	}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v", err)
	}
	want := CodexConfig{AutoReviewModelOverrides: map[string]string{"gpt-5": "reviewer-mini"}}
	if !reflect.DeepEqual(got.Codex, want) {
		t.Errorf("Codex = %+v, want resolved config %+v", got.Codex, want)
	}
}

func TestCodexAutoReviewModelOverridesNormalizesPairs(t *testing.T) {
	got, err := loadServe([]string{
		"--apikey", testAPIKey,
		"--codex-auto-review-model-overrides", "  gpt-5 = reviewer=variant ,, mini = fast ,  ",
	}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v", err)
	}
	want := map[string]string{
		"gpt-5": "reviewer=variant",
		"mini":  "fast",
	}
	if !reflect.DeepEqual(got.Codex.AutoReviewModelOverrides, want) {
		t.Errorf("AutoReviewModelOverrides = %v, want %v", got.Codex.AutoReviewModelOverrides, want)
	}
}

func TestCodexAutoReviewModelOverridesRejectsMalformedPairs(t *testing.T) {
	tests := map[string]string{
		"missing equals": "gpt-5-reviewer",
		"empty key":      "=reviewer",
		"empty value":    "gpt-5=",
		"duplicate key":  "gpt-5=first,gpt-5=second",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := loadServe([]string{
				"--apikey", testAPIKey,
				"--codex-auto-review-model-overrides", value,
			}, noEnv())
			if err == nil {
				t.Fatalf("loadServe() error = nil, want %q rejected", value)
			}
			if !strings.Contains(err.Error(), "codex-auto-review-model-overrides") {
				t.Errorf("error = %q, want key context", err)
			}
		})
	}
}

func TestCodexAutoReviewModelOverridesUsesWholesalePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte(`codex-auto-review-model-overrides = "file-main=file-reviewer"`+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want map[string]string
	}{
		{
			name: "TOML overrides default",
			args: []string{"--config", path},
			want: map[string]string{"file-main": "file-reviewer"},
		},
		{
			name: "env replaces TOML wholesale",
			args: []string{"--config", path},
			env: map[string]string{
				"COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES": "env-main=env-reviewer",
			},
			want: map[string]string{"env-main": "env-reviewer"},
		},
		{
			name: "flag replaces env wholesale",
			args: []string{
				"--config", path,
				"--codex-auto-review-model-overrides", "flag-main=flag-reviewer",
			},
			env: map[string]string{
				"COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES": "env-main=env-reviewer",
			},
			want: map[string]string{"flag-main": "flag-reviewer"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if !reflect.DeepEqual(got.Codex.AutoReviewModelOverrides, tc.want) {
				t.Errorf("AutoReviewModelOverrides = %v, want %v", got.Codex.AutoReviewModelOverrides, tc.want)
			}
		})
	}
}

func TestCodexAutoReviewModelOverridesIsLoggedNormalizedWhenCatalogDisabled(t *testing.T) {
	got, err := loadServe([]string{
		"--apikey", testAPIKey,
		"--codex-catalog-enabled=false",
		"--codex-auto-review-model-overrides", " z-main = z-reviewer , a-main=a-reviewer ",
	}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v, want staged disabled config accepted", err)
	}
	if got.Codex.Enabled {
		t.Fatal("Codex.Enabled = true, want catalog disabled")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("effective config", "config", got)
	out := buf.String()
	want := `config.codex-auto-review-model-overrides="a-main=a-reviewer,z-main=z-reviewer"`
	if !strings.Contains(out, want) {
		t.Errorf("log output missing %q\nfull: %s", want, out)
	}
	if strings.Contains(out, " z-main = z-reviewer ") {
		t.Errorf("log output contains unparsed staging value\nfull: %s", out)
	}
}

func TestCodexConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	document := strings.Join([]string{
		`codex-catalog-enabled = true`,
		`codex-auto-review-model = "reviewer-from-file"`,
		`codex-catalog-override-limits = true`,
	}, "\n")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want CodexConfig
	}{
		{name: "defaults", want: CodexConfig{}},
		{
			name: "TOML overrides defaults",
			args: []string{"--config", path},
			want: CodexConfig{Enabled: true, AutoReviewModel: "reviewer-from-file", OverrideLimits: true},
		},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env: map[string]string{
				"COPILOTD_CODEX_CATALOG_ENABLED":         "false",
				"COPILOTD_CODEX_AUTO_REVIEW_MODEL":       "reviewer-from-env",
				"COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS": "false",
			},
			want: CodexConfig{Enabled: false, AutoReviewModel: "reviewer-from-env", OverrideLimits: false},
		},
		{
			name: "flags override env",
			args: []string{
				"--config", path,
				"--codex-catalog-enabled=true",
				"--codex-auto-review-model", "reviewer-from-flag",
				"--codex-catalog-override-limits=true",
			},
			env: map[string]string{
				"COPILOTD_CODEX_CATALOG_ENABLED":         "false",
				"COPILOTD_CODEX_AUTO_REVIEW_MODEL":       "reviewer-from-env",
				"COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS": "false",
			},
			want: CodexConfig{Enabled: true, AutoReviewModel: "reviewer-from-flag", OverrideLimits: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if !reflect.DeepEqual(got.Codex, tc.want) {
				t.Errorf("Codex = %+v, want %+v", got.Codex, tc.want)
			}
		})
	}
}

func TestCodexConfigRejectsMalformedBooleans(t *testing.T) {
	files := map[string]string{
		"codex-catalog-enabled":         `codex-catalog-enabled = "not-a-bool"`,
		"codex-catalog-override-limits": `codex-catalog-override-limits = "not-a-bool"`,
	}

	for _, key := range []string{"codex-catalog-enabled", "codex-catalog-override-limits"} {
		key := key
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "copilotd.toml")
			if err := os.WriteFile(path, []byte(files[key]+"\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			envKey := envVarName(key)
			for _, tc := range []struct {
				name string
				args []string
				env  map[string]string
			}{
				{name: "flag", args: []string{"--apikey", testAPIKey, "--" + key + "=not-a-bool"}},
				{name: "env", args: []string{"--apikey", testAPIKey}, env: map[string]string{envKey: "not-a-bool"}},
				{name: "TOML", args: []string{"--apikey", testAPIKey, "--config", path}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := loadServe(tc.args, envFunc(tc.env))
					if err == nil {
						t.Fatalf("loadServe() error = nil, want malformed %s rejected", key)
					}
					if !strings.Contains(err.Error(), key) {
						t.Errorf("error = %q, want %s context", err, key)
					}
				})
			}
		})
	}
}

func TestCodexConfigIsInertWhenDisabled(t *testing.T) {
	got, err := loadServe([]string{
		"--apikey", testAPIKey,
		"--codex-catalog-enabled=false",
		"--codex-auto-review-model", "staged-reviewer",
		"--codex-catalog-override-limits=true",
	}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v, want staged disabled config accepted", err)
	}
	want := CodexConfig{AutoReviewModel: "staged-reviewer", OverrideLimits: true}
	if !reflect.DeepEqual(got.Codex, want) {
		t.Errorf("Codex = %+v, want inert staged config %+v", got.Codex, want)
	}
}

func TestCodexCatalogRefreshIntervalPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("codex-catalog-refresh-interval = \"12h\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "default", want: 24 * time.Hour},
		{name: "TOML", args: []string{"--config", path}, want: 12 * time.Hour},
		{
			name: "env over TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_CODEX_CATALOG_REFRESH_INTERVAL": "6h"},
			want: 6 * time.Hour,
		},
		{
			name: "flag over env",
			args: []string{"--config", path, "--codex-catalog-refresh-interval", "3h"},
			env:  map[string]string{"COPILOTD_CODEX_CATALOG_REFRESH_INTERVAL": "6h"},
			want: 3 * time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--apikey", testAPIKey}, tc.args...)
			got, err := loadServe(args, envFunc(tc.env))
			if err != nil {
				t.Fatalf("loadServe: %v", err)
			}
			if got.CodexCatalogRefreshInterval != tc.want {
				t.Errorf("CodexCatalogRefreshInterval = %v, want %v", got.CodexCatalogRefreshInterval, tc.want)
			}
		})
	}
}

func TestCodexCatalogRefreshIntervalRejectsNegativeValues(t *testing.T) {
	_, err := loadServe([]string{
		"--apikey", testAPIKey,
		"--codex-catalog-refresh-interval", "-1s",
	}, noEnv())
	if err == nil || !strings.Contains(err.Error(), "codex-catalog-refresh-interval") {
		t.Fatalf("loadServe error = %v, want refresh interval validation", err)
	}
}

func TestMaxBufferedResponseBytesConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("max-buffered-response-bytes = 11\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want int64
	}{
		{name: "default", want: 33554432},
		{name: "TOML overrides default", args: []string{"--config", path}, want: 11},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_MAX_BUFFERED_RESPONSE_BYTES": "21"},
			want: 21,
		},
		{
			name: "flag overrides env",
			args: []string{"--config", path, "--max-buffered-response-bytes", "31"},
			env:  map[string]string{"COPILOTD_MAX_BUFFERED_RESPONSE_BYTES": "21"},
			want: 31,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.MaxBufferedResponseBytes != tc.want {
				t.Errorf("MaxBufferedResponseBytes = %d, want %d", got.MaxBufferedResponseBytes, tc.want)
			}
		})
	}
}

func TestMaxBufferedResponseBytesRejectsMalformedOrNonPositiveValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("max-buffered-response-bytes = \"not-an-integer\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "malformed flag", args: []string{"--apikey", testAPIKey, "--max-buffered-response-bytes", "not-an-integer"}},
		{name: "malformed env", args: []string{"--apikey", testAPIKey}, env: map[string]string{"COPILOTD_MAX_BUFFERED_RESPONSE_BYTES": "not-an-integer"}},
		{name: "malformed TOML", args: []string{"--apikey", testAPIKey, "--config", path}},
		{name: "zero flag", args: []string{"--apikey", testAPIKey, "--max-buffered-response-bytes", "0"}},
		{name: "negative env", args: []string{"--apikey", testAPIKey}, env: map[string]string{"COPILOTD_MAX_BUFFERED_RESPONSE_BYTES": "-1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadServe(tc.args, envFunc(tc.env))
			if err == nil {
				t.Fatal("loadServe() error = nil, want buffered-response cap rejected")
			}
			if !strings.Contains(err.Error(), "max-buffered-response-bytes") {
				t.Errorf("error = %q, want max-buffered-response-bytes context", err)
			}
		})
	}
}

func TestShimNopEnabledConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("shim-nop-enabled = true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want bool
	}{
		{name: "shim default", want: false},
		{name: "TOML overrides default", args: []string{"--config", path}, want: true},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_SHIM_NOP_ENABLED": "false"},
			want: false,
		},
		{
			name: "flag overrides env",
			args: []string{"--config", path, "--shim-nop-enabled=true"},
			env:  map[string]string{"COPILOTD_SHIM_NOP_ENABLED": "false"},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.ShimNopEnabled != tc.want {
				t.Errorf("ShimNopEnabled = %t, want %t", got.ShimNopEnabled, tc.want)
			}
		})
	}
}

func TestShimNopEnabledRejectsMalformedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("shim-nop-enabled = \"not-a-bool\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "flag", args: []string{"--apikey", testAPIKey, "--shim-nop-enabled=not-a-bool"}},
		{name: "env", args: []string{"--apikey", testAPIKey}, env: map[string]string{"COPILOTD_SHIM_NOP_ENABLED": "not-a-bool"}},
		{name: "TOML", args: []string{"--apikey", testAPIKey, "--config", path}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadServe(tc.args, envFunc(tc.env))
			if err == nil {
				t.Fatal("loadServe() error = nil, want malformed shim toggle rejected")
			}
			if !strings.Contains(err.Error(), "shim-nop-enabled") {
				t.Errorf("error = %q, want shim-nop-enabled context", err)
			}
		})
	}
}

func TestRemovedUpstreamBaseSettingsHaveNoEffect(t *testing.T) {
	t.Run("environment variable", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey}, envFunc(map[string]string{
			"COPILOTD_UPSTREAM_BASE": "https://redirect.example.invalid",
		}))
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if !reflect.DeepEqual(got, defaultConfig()) {
			t.Errorf("loadServe() = %+v, want the default config; removed environment setting must be ignored", got)
		}
	})

	t.Run("TOML setting", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "copilotd.toml")
		if err := os.WriteFile(path, []byte("upstream-base = \"https://redirect.example.invalid\"\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		got, err := loadServe([]string{"--apikey", testAPIKey, "--config", path}, noEnv())
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if !reflect.DeepEqual(got, defaultConfig()) {
			t.Errorf("loadServe() = %+v, want the default config; removed TOML setting must be ignored", got)
		}
	})
}

func TestTimeoutConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("write-timeout = \"11s\"\nresponse-header-timeout = \"12s\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		wantWrite    time.Duration
		wantResponse time.Duration
	}{
		{
			name:         "TOML overrides defaults",
			args:         []string{"--config", path},
			wantWrite:    11 * time.Second,
			wantResponse: 12 * time.Second,
		},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env: map[string]string{
				"COPILOTD_WRITE_TIMEOUT":           "21s",
				"COPILOTD_RESPONSE_HEADER_TIMEOUT": "22s",
			},
			wantWrite:    21 * time.Second,
			wantResponse: 22 * time.Second,
		},
		{
			name: "flags override env",
			args: []string{"--config", path, "--write-timeout", "31s", "--response-header-timeout", "32s"},
			env: map[string]string{
				"COPILOTD_WRITE_TIMEOUT":           "21s",
				"COPILOTD_RESPONSE_HEADER_TIMEOUT": "22s",
			},
			wantWrite:    31 * time.Second,
			wantResponse: 32 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.WriteTimeout != tc.wantWrite {
				t.Errorf("WriteTimeout = %v, want %v", got.WriteTimeout, tc.wantWrite)
			}
			if got.ResponseHeaderTimeout != tc.wantResponse {
				t.Errorf("ResponseHeaderTimeout = %v, want %v", got.ResponseHeaderTimeout, tc.wantResponse)
			}
		})
	}
}

func TestWebSocketHandshakeTimeoutConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("ws-handshake-timeout = \"11s\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "default", want: 10 * time.Second},
		{name: "TOML overrides default", args: []string{"--config", path}, want: 11 * time.Second},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_WS_HANDSHAKE_TIMEOUT": "21s"},
			want: 21 * time.Second,
		},
		{
			name: "flag overrides env",
			args: []string{"--config", path, "--ws-handshake-timeout", "31s"},
			env:  map[string]string{"COPILOTD_WS_HANDSHAKE_TIMEOUT": "21s"},
			want: 31 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.WebSocketHandshakeTimeout != tc.want {
				t.Errorf("WebSocketHandshakeTimeout = %v, want %v", got.WebSocketHandshakeTimeout, tc.want)
			}
		})
	}
}

func TestStreamIdleTimeoutConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("stream-idle-timeout = \"11s\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "default", want: 5 * time.Minute},
		{name: "TOML overrides default", args: []string{"--config", path}, want: 11 * time.Second},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_STREAM_IDLE_TIMEOUT": "21s"},
			want: 21 * time.Second,
		},
		{
			name: "flag overrides env",
			args: []string{"--config", path, "--stream-idle-timeout", "31s"},
			env:  map[string]string{"COPILOTD_STREAM_IDLE_TIMEOUT": "21s"},
			want: 31 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.StreamIdleTimeout != tc.want {
				t.Errorf("StreamIdleTimeout = %v, want %v", got.StreamIdleTimeout, tc.want)
			}
		})
	}
}

func TestStreamKeepaliveIntervalConfigPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	if err := os.WriteFile(path, []byte("stream-keepalive-interval = \"11s\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "default", want: 15 * time.Second},
		{name: "TOML overrides default", args: []string{"--config", path}, want: 11 * time.Second},
		{
			name: "env overrides TOML",
			args: []string{"--config", path},
			env:  map[string]string{"COPILOTD_STREAM_KEEPALIVE_INTERVAL": "21s"},
			want: 21 * time.Second,
		},
		{
			name: "flag overrides env",
			args: []string{"--config", path, "--stream-keepalive-interval", "31s"},
			env:  map[string]string{"COPILOTD_STREAM_KEEPALIVE_INTERVAL": "21s"},
			want: 31 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for key, value := range tc.env {
				env[key] = value
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.StreamKeepaliveInterval != tc.want {
				t.Errorf("StreamKeepaliveInterval = %v, want %v", got.StreamKeepaliveInterval, tc.want)
			}
		})
	}
}

func TestLoadPrecedence(t *testing.T) {
	tokenFile := defaultOAuthTokenFile()

	// impersonationDefaults folds the new §6.7 knob defaults (plus the startup-mint
	// retry default) into a precedence want, since Resolve now populates them; the
	// precedence cases only exercise addr/log/file fields.
	withDefaults := func(c ServeConfig) ServeConfig {
		c.StreamIdleTimeout = 5 * time.Minute
		c.StreamKeepaliveInterval = 15 * time.Second
		c.WriteTimeout = 90 * time.Second
		c.ResponseHeaderTimeout = 600 * time.Second
		c.WebSocketHandshakeTimeout = 10 * time.Second
		c.MaxBufferedResponseBytes = 33554432
		c.CodexCatalogRefreshInterval = 24 * time.Hour
		c.StartupMintRetries = 3
		c.VSCodeVersionFallback = "1.104.1"
		c.PluginVersionFallback = "0.26.7"
		c.CopilotIntegrationID = "vscode-chat"
		c.GithubAPIVersion = "2025-04-01"
		c.ImpersonationRefreshInterval = 24 * time.Hour
		return c
	}

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
		want       ServeConfig
	}{
		{
			name: "env overrides default",
			env:  map[string]string{"COPILOTD_ADDR": "0.0.0.0:9090", "COPILOTD_LOG_LEVEL": "debug"},
			want: withDefaults(ServeConfig{Addr: "0.0.0.0:9090", LogLevel: "debug", LogFormat: "text", ShutdownTimeout: 10 * time.Second, GithubOAuthTokenFile: tokenFile, APIKey: testAPIKey, OutboundTimeout: 600 * time.Second, MaxRequestBytes: 33554432}),
		},
		{
			name: "flag overrides env",
			args: []string{"--addr", "127.0.0.1:7000", "--log-level=error"},
			env:  map[string]string{"COPILOTD_ADDR": "0.0.0.0:9090", "COPILOTD_LOG_LEVEL": "debug"},
			want: withDefaults(ServeConfig{Addr: "127.0.0.1:7000", LogLevel: "error", LogFormat: "text", ShutdownTimeout: 10 * time.Second, GithubOAuthTokenFile: tokenFile, APIKey: testAPIKey, OutboundTimeout: 600 * time.Second, MaxRequestBytes: 33554432}),
		},
		{
			name:      "file under env under flag; file-only keys still apply",
			writeFile: true,
			// --config is supplied per-test in the body; here flag overrides addr,
			// env overrides log-level, the rest come from the file.
			args: []string{"--addr", "127.0.0.1:7000"},
			env:  map[string]string{"COPILOTD_LOG_LEVEL": "error"},
			want: withDefaults(ServeConfig{
				Addr:                 "127.0.0.1:7000",     // flag wins
				LogLevel:             "error",              // env wins over file "warn"
				LogFormat:            "json",               // from file
				LogFile:              "/tmp/from-file.log", // from file
				ShutdownTimeout:      30 * time.Second,     // from file
				GithubOAuthTokenFile: tokenFile,
				APIKey:               testAPIKey,
				OutboundTimeout:      600 * time.Second,
				MaxRequestBytes:      33554432,
			}),
		},
		{
			name:       "config path honored via COPILOTD_CONFIG env",
			writeFile:  true,
			fileViaEnv: true,
			env:        map[string]string{},
			want: withDefaults(ServeConfig{
				Addr:                 "10.0.0.1:1111",
				LogLevel:             "warn",
				LogFormat:            "json",
				LogFile:              "/tmp/from-file.log",
				ShutdownTimeout:      30 * time.Second,
				GithubOAuthTokenFile: tokenFile,
				APIKey:               testAPIKey,
				OutboundTimeout:      600 * time.Second,
				MaxRequestBytes:      33554432,
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			// apikey is required; supply it via env for every case (these tests are
			// about addr/log/file precedence, not the key).
			env["COPILOTD_APIKEY"] = testAPIKey
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

			got, err := loadServe(args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("loadServe() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLoadOAuthTokenFile covers the shared --github-oauth-token-file
// flag: default path, flag override, env override, and flag > env precedence.
// This phase only parses and stores the path; it is never read here.
func TestLoadOAuthTokenFile(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"default", nil, nil, defaultOAuthTokenFile()},
		{"flag override", []string{"--github-oauth-token-file", "/tmp/flag.tok"}, nil, "/tmp/flag.tok"},
		{"env override", nil, map[string]string{"COPILOTD_GITHUB_OAUTH_TOKEN_FILE": "/tmp/env.tok"}, "/tmp/env.tok"},
		{
			name: "flag over env",
			args: []string{"--github-oauth-token-file", "/tmp/flag.tok"},
			env:  map[string]string{"COPILOTD_GITHUB_OAUTH_TOKEN_FILE": "/tmp/env.tok"},
			want: "/tmp/flag.tok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"COPILOTD_APIKEY": testAPIKey}
			for k, v := range tc.env {
				env[k] = v
			}
			got, err := loadServe(tc.args, envFunc(env))
			if err != nil {
				t.Fatalf("loadServe() error = %v", err)
			}
			if got.GithubOAuthTokenFile != tc.want {
				t.Errorf("GithubOAuthTokenFile = %q, want %q", got.GithubOAuthTokenFile, tc.want)
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
		// apikey is required and fails fast when unset.
		{"missing apikey", nil, "apikey"},
		// outbound-timeout / max-request-bytes are validated after apikey passes.
		{"non-positive outbound timeout", []string{"--apikey", testAPIKey, "--outbound-timeout", "0s"}, "outbound-timeout"},
		{"non-positive stream idle timeout", []string{"--apikey", testAPIKey, "--stream-idle-timeout", "0s"}, "stream-idle-timeout"},
		{"non-positive stream keepalive interval", []string{"--apikey", testAPIKey, "--stream-keepalive-interval", "0s"}, "stream-keepalive-interval"},
		{"non-positive write timeout", []string{"--apikey", testAPIKey, "--write-timeout", "0s"}, "write-timeout"},
		{"non-positive response header timeout", []string{"--apikey", testAPIKey, "--response-header-timeout", "0s"}, "response-header-timeout"},
		{"non-positive WebSocket handshake timeout", []string{"--apikey", testAPIKey, "--ws-handshake-timeout", "0s"}, "ws-handshake-timeout"},
		{"non-positive max request bytes", []string{"--apikey", testAPIKey, "--max-request-bytes", "0"}, "max-request-bytes"},
		{"negative startup mint retries", []string{"--apikey", testAPIKey, "--startup-mint-retries", "-1"}, "startup-mint-retries"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadServe(tc.args, noEnv())
			if err == nil {
				t.Fatalf("loadServe() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("loadServe() error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestConfigLogValueEmitsOnlyNonSecretFields(t *testing.T) {
	cfg := ServeConfig{
		Addr:                      "127.0.0.1:8080",
		LogLevel:                  "info",
		LogFormat:                 "text",
		LogFile:                   "/var/log/copilotd.log",
		ShutdownTimeout:           10 * time.Second,
		GithubOAuthTokenFile:      "/home/op/.config/copilotd/github-oauth-token",
		APIKey:                    "super-secret-apikey-value",
		OutboundTimeout:           600 * time.Second,
		StreamIdleTimeout:         90 * time.Second,
		StreamKeepaliveInterval:   15 * time.Second,
		WriteTimeout:              90 * time.Second,
		ResponseHeaderTimeout:     600 * time.Second,
		WebSocketHandshakeTimeout: 12 * time.Second,
		MaxRequestBytes:           33554432,
		MaxBufferedResponseBytes:  16777216,
		ShimNopEnabled:            true,
		GithubOAuthToken:          "gho-super-secret-oauth-value",
		Codex: CodexConfig{
			Enabled:         true,
			AutoReviewModel: "gpt-5.6-luna",
			AutoReviewModelOverrides: map[string]string{
				"gpt-5.6-sol": "gpt-5.4",
				"gpt-5.4":     "gpt-5.4-mini",
			},
			OverrideLimits: true,
		},
		CodexCatalogRefreshInterval:  6 * time.Hour,
		StartupMintRetries:           3,
		VSCodeVersionFallback:        "1.104.1",
		PluginVersionFallback:        "0.26.7",
		CopilotIntegrationID:         "vscode-chat",
		GithubAPIVersion:             "2025-04-01",
		ImpersonationRefreshInterval: 24 * time.Hour,
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
		"config.github-oauth-token-file=/home/op/.config/copilotd/github-oauth-token",
		"config.outbound-timeout=10m0s",
		"config.stream-idle-timeout=1m30s",
		"config.stream-keepalive-interval=15s",
		"config.write-timeout=1m30s",
		"config.response-header-timeout=10m0s",
		"config.ws-handshake-timeout=12s",
		"config.max-request-bytes=33554432",
		"config.max-buffered-response-bytes=16777216",
		"config.shim-nop-enabled=true",
		"config.startup-mint-retries=3",
		"config.vscode-version=1.104.1",
		"config.plugin-version=0.26.7",
		"config.copilot-integration-id=vscode-chat",
		"config.github-api-version=2025-04-01",
		"config.impersonation-refresh-interval=24h0m0s",
		"config.codex-catalog-enabled=true",
		"config.codex-auto-review-model=gpt-5.6-luna",
		`config.codex-auto-review-model-overrides="gpt-5.4=gpt-5.4-mini,gpt-5.6-sol=gpt-5.4"`,
		"config.codex-catalog-override-limits=true",
		"config.codex-catalog-refresh-interval=6h0m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\nfull: %s", want, out)
		}
	}

	// The apikey is a secret: neither its value nor an "apikey" key may appear.
	if strings.Contains(out, "super-secret-apikey-value") || strings.Contains(out, "apikey") {
		t.Errorf("log output must not contain the apikey\nfull: %s", out)
	}

	// The inline GitHub OAuth token is a secret: neither its value nor the
	// "github-oauth-token=" key may appear. (The "github-oauth-token-file=" path key
	// is logged and legitimately shares the prefix, so we match the exact key form.)
	if strings.Contains(out, "gho-super-secret-oauth-value") || strings.Contains(out, "github-oauth-token=") {
		t.Errorf("log output must not contain the inline github-oauth-token\nfull: %s", out)
	}

	if strings.Contains(out, "upstream-base") {
		t.Errorf("log output must not contain the removed upstream-base setting\nfull: %s", out)
	}
	for _, removed := range []string{"editor-version", "editor-plugin-version", "copilot-user-agent"} {
		if strings.Contains(out, removed) {
			t.Errorf("log output must not contain removed %s setting\nfull: %s", removed, out)
		}
	}
}

// TestLoadServeIdentityFields covers the new serve-only identity/impersonation
// settings: defaults, the inline github-oauth-token secret's precedence, and the
// startup-mint-retries + impersonation-knob overrides across flag/env/file.
func TestLoadServeIdentityFields(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.GithubOAuthToken != "" {
			t.Errorf("GithubOAuthToken = %q, want empty by default", got.GithubOAuthToken)
		}
		if got.StartupMintRetries != 3 {
			t.Errorf("StartupMintRetries = %d, want 3", got.StartupMintRetries)
		}
		want := map[string]string{
			"VSCodeVersionFallback": "1.104.1",
			"PluginVersionFallback": "0.26.7",
			"CopilotIntegrationID":  "vscode-chat",
			"GithubAPIVersion":      "2025-04-01",
		}
		gotm := map[string]string{
			"VSCodeVersionFallback": got.VSCodeVersionFallback,
			"PluginVersionFallback": got.PluginVersionFallback,
			"CopilotIntegrationID":  got.CopilotIntegrationID,
			"GithubAPIVersion":      got.GithubAPIVersion,
		}
		for k, v := range want {
			if gotm[k] != v {
				t.Errorf("%s = %q, want %q", k, gotm[k], v)
			}
		}
	})

	t.Run("github-oauth-token flag over env", func(t *testing.T) {
		got, err := loadServe(
			[]string{"--apikey", testAPIKey, "--github-oauth-token", "gho-from-flag"},
			envFunc(map[string]string{"COPILOTD_GITHUB_OAUTH_TOKEN": "gho-from-env"}),
		)
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.GithubOAuthToken != "gho-from-flag" {
			t.Errorf("GithubOAuthToken = %q, want gho-from-flag", got.GithubOAuthToken)
		}
	})

	t.Run("startup-mint-retries and knobs via env", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey}, envFunc(map[string]string{
			"COPILOTD_STARTUP_MINT_RETRIES":   "5",
			"COPILOTD_COPILOT_INTEGRATION_ID": "vscode",
			"COPILOTD_VSCODE_VERSION":         "9.9.9",
			"COPILOTD_GITHUB_API_VERSION":     "2099-01-01",
		}))
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.StartupMintRetries != 5 {
			t.Errorf("StartupMintRetries = %d, want 5", got.StartupMintRetries)
		}
		if got.CopilotIntegrationID != "vscode" || got.VSCodeVersionFallback != "9.9.9" || got.GithubAPIVersion != "2099-01-01" {
			t.Errorf("knob overrides not applied: %+v", got)
		}
	})

	t.Run("zero startup-mint-retries is valid", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey, "--startup-mint-retries", "0"}, noEnv())
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.StartupMintRetries != 0 {
			t.Errorf("StartupMintRetries = %d, want 0", got.StartupMintRetries)
		}
	})
}

func TestLoadServeImpersonationConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.VSCodeVersionFallback != "1.104.1" {
			t.Errorf("VSCodeVersionFallback = %q, want 1.104.1", got.VSCodeVersionFallback)
		}
		if got.PluginVersionFallback != "0.26.7" {
			t.Errorf("PluginVersionFallback = %q, want 0.26.7", got.PluginVersionFallback)
		}
		if got.ImpersonationRefreshInterval != 24*time.Hour {
			t.Errorf("ImpersonationRefreshInterval = %v, want 24h", got.ImpersonationRefreshInterval)
		}
	})

	t.Run("flags override env", func(t *testing.T) {
		got, err := loadServe([]string{
			"--apikey", testAPIKey,
			"--vscode-version", "1.130.0",
			"--plugin-version", "0.50.0",
			"--impersonation-refresh-interval", "6h",
		}, envFunc(map[string]string{
			"COPILOTD_VSCODE_VERSION":                 "1.129.0",
			"COPILOTD_PLUGIN_VERSION":                 "0.49.0",
			"COPILOTD_IMPERSONATION_REFRESH_INTERVAL": "12h",
		}))
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.VSCodeVersionFallback != "1.130.0" || got.PluginVersionFallback != "0.50.0" || got.ImpersonationRefreshInterval != 6*time.Hour {
			t.Errorf("impersonation config = %+v, want flag values", got)
		}
	})

	t.Run("env overrides defaults", func(t *testing.T) {
		got, err := loadServe([]string{"--apikey", testAPIKey}, envFunc(map[string]string{
			"COPILOTD_VSCODE_VERSION":                 "1.129.0",
			"COPILOTD_PLUGIN_VERSION":                 "0.49.0",
			"COPILOTD_IMPERSONATION_REFRESH_INTERVAL": "0s",
		}))
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.VSCodeVersionFallback != "1.129.0" || got.PluginVersionFallback != "0.49.0" || got.ImpersonationRefreshInterval != 0 {
			t.Errorf("impersonation config = %+v, want env values", got)
		}
	})

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "empty VS Code fallback", args: []string{"--vscode-version", ""}, want: "vscode-version"},
		{name: "empty plugin fallback", args: []string{"--plugin-version", ""}, want: "plugin-version"},
		{name: "prefixed VS Code fallback", args: []string{"--vscode-version", "vscode/1.2.3"}, want: "vscode-version"},
		{name: "prefixed plugin fallback", args: []string{"--plugin-version", "copilot-chat/1.2.3"}, want: "plugin-version"},
		{name: "non-version VS Code fallback", args: []string{"--vscode-version", "banana"}, want: "vscode-version"},
		{name: "non-version plugin fallback", args: []string{"--plugin-version", "banana"}, want: "plugin-version"},
		{name: "slash suffix in VS Code fallback", args: []string{"--vscode-version", "1.2.3/garbage"}, want: "vscode-version"},
		{name: "whitespace in plugin fallback", args: []string{"--plugin-version", "1.2.3 beta"}, want: "plugin-version"},
		{name: "control character in VS Code fallback", args: []string{"--vscode-version", "1.2.3\nInjected: true"}, want: "vscode-version"},
		{name: "empty prerelease in plugin fallback", args: []string{"--plugin-version", "1.2.3-"}, want: "plugin-version"},
		{name: "negative refresh interval", args: []string{"--impersonation-refresh-interval", "-1s"}, want: "impersonation-refresh-interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--apikey", testAPIKey}, tc.args...)
			_, err := loadServe(args, noEnv())
			if err == nil {
				t.Fatal("loadServe() error = nil, want validation failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want %q context", err, tc.want)
			}
		})
	}

	t.Run("version suffixes remain accepted", func(t *testing.T) {
		got, err := loadServe([]string{
			"--apikey", testAPIKey,
			"--vscode-version", "1.130.0-insider",
			"--plugin-version", "0.50.0+build.1",
		}, noEnv())
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.VSCodeVersionFallback != "1.130.0-insider" || got.PluginVersionFallback != "0.50.0+build.1" {
			t.Errorf("version fallbacks = (%q, %q), want accepted suffixes", got.VSCodeVersionFallback, got.PluginVersionFallback)
		}
	})

	for _, oldFlag := range []string{"editor-version", "editor-plugin-version", "copilot-user-agent"} {
		t.Run("removed "+oldFlag, func(t *testing.T) {
			_, err := loadServe([]string{"--apikey", testAPIKey, "--" + oldFlag, "obsolete"}, noEnv())
			if err == nil {
				t.Fatalf("loadServe() accepted removed --%s", oldFlag)
			}
		})
	}
}

// loadLogin builds the login flag set the way the command tree does, parses args,
// and resolves. It mirrors production wiring so the precedence/validation tests
// exercise the same code path.
func loadLogin(args []string, lookupEnv func(string) (string, bool)) (LoginConfig, error) {
	login := ff.NewFlagSet("login")
	lf := RegisterLogin(login)
	if err := ff.Parse(login, args); err != nil {
		return LoginConfig{}, fmt.Errorf("parse flags: %w", err)
	}
	return lf.Resolve(lookupEnv)
}

func TestLoadLoginDefaults(t *testing.T) {
	got, err := loadLogin(nil, noEnv())
	if err != nil {
		t.Fatalf("loadLogin() error = %v", err)
	}
	want := LoginConfig{
		LogLevel:             "info",
		LogFormat:            "text",
		LogFile:              "",
		GithubOAuthTokenFile: defaultOAuthTokenFile(),
		GithubClientID:       "Iv1.b507a08c87ecfe98",
		GithubScope:          "read:user",
	}
	if got != want {
		t.Errorf("loadLogin() = %+v, want %+v", got, want)
	}
}

func TestServeAndLoginResolveIndependentCommonFlags(t *testing.T) {
	serveConfig := filepath.Join(t.TempDir(), "serve.toml")
	if err := os.WriteFile(serveConfig, []byte("unknown-key = \"ignored\"\n"), 0o600); err != nil {
		t.Fatalf("write serve config: %v", err)
	}
	serveFS := ff.NewFlagSet("serve")
	serveFlags := RegisterServe(serveFS)
	if err := ff.Parse(serveFS, []string{
		"--apikey", testAPIKey,
		"--log-level", "debug",
		"--log-format", "json",
		"--log-file", "/tmp/serve.log",
		"--config", serveConfig,
		"--github-oauth-token-file", "/tmp/serve-token",
	}); err != nil {
		t.Fatalf("parse serve flags: %v", err)
	}

	loginConfig := filepath.Join(t.TempDir(), "login.toml")
	if err := os.WriteFile(loginConfig, []byte("other-unknown-key = \"ignored\"\n"), 0o600); err != nil {
		t.Fatalf("write login config: %v", err)
	}
	loginFS := ff.NewFlagSet("login")
	loginFlags := RegisterLogin(loginFS)
	if err := ff.Parse(loginFS, []string{
		"--log-level", "error",
		"--log-format", "json",
		"--log-file", "/tmp/login.log",
		"--config", loginConfig,
		"--github-oauth-token-file", "/tmp/login-token",
	}); err != nil {
		t.Fatalf("parse login flags: %v", err)
	}

	serve, err := serveFlags.Resolve(noEnv())
	if err != nil {
		t.Fatalf("resolve serve flags: %v", err)
	}
	login, err := loginFlags.Resolve(noEnv())
	if err != nil {
		t.Fatalf("resolve login flags: %v", err)
	}

	if serve.LogLevel != "debug" || serve.LogFormat != "json" || serve.LogFile != "/tmp/serve.log" || serve.GithubOAuthTokenFile != "/tmp/serve-token" {
		t.Errorf("serve common flags = %q/%q/%q/%q, want debug/json//tmp/serve.log//tmp/serve-token", serve.LogLevel, serve.LogFormat, serve.LogFile, serve.GithubOAuthTokenFile)
	}
	if login.LogLevel != "error" || login.LogFormat != "json" || login.LogFile != "/tmp/login.log" || login.GithubOAuthTokenFile != "/tmp/login-token" {
		t.Errorf("login common flags = %q/%q/%q/%q, want error/json//tmp/login.log//tmp/login-token", login.LogLevel, login.LogFormat, login.LogFile, login.GithubOAuthTokenFile)
	}
}

func TestGlobalTOMLProjectsOntoOperationalCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilotd.toml")
	document := strings.Join([]string{
		`log-level = "warn"`,
		`addr = "127.0.0.1:9191"`,
		`apikey = "from-global-document"`,
		`github-client-id = "client-from-global-document"`,
		`github-scope = "scope:from-global-document"`,
		`unknown-key = "ignored"`,
	}, "\n")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	serve, err := loadServe([]string{"--config", path}, noEnv())
	if err != nil {
		t.Fatalf("load serve from global document: %v", err)
	}
	if serve.LogLevel != "warn" || serve.Addr != "127.0.0.1:9191" || serve.APIKey != "from-global-document" {
		t.Errorf("serve projection = %+v, want shared and serve keys from global document", serve)
	}

	login, err := loadLogin([]string{"--config", path}, noEnv())
	if err != nil {
		t.Fatalf("load login from global document: %v", err)
	}
	if login.LogLevel != "warn" || login.GithubClientID != "client-from-global-document" || login.GithubScope != "scope:from-global-document" {
		t.Errorf("login projection = %+v, want shared and login keys from global document", login)
	}
}

func TestLoadLoginPrecedence(t *testing.T) {
	// A TOML file setting every login-resolvable key; env and flags override
	// subsets so we observe flags > env > file > default.
	toml := strings.Join([]string{
		`log-level = "warn"`,
		`github-oauth-token-file = "/tmp/from-file.tok"`,
		`github-client-id = "id-from-file"`,
		`github-scope = "scope:from-file"`,
	}, "\n")

	t.Run("env over file, flag over env, file-only key applies", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "copilotd.toml")
		if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
			t.Fatalf("write toml: %v", err)
		}
		got, err := loadLogin(
			// flag wins for client-id and the config path; the rest come from env/file
			[]string{"--config", path, "--github-client-id", "id-from-flag"},
			envFunc(map[string]string{
				"COPILOTD_GITHUB_SCOPE": "scope:from-env", // env over file
			}),
		)
		if err != nil {
			t.Fatalf("loadLogin() error = %v", err)
		}
		want := LoginConfig{
			LogLevel:             "warn",               // from file (file-only key)
			LogFormat:            "text",               // default
			GithubOAuthTokenFile: "/tmp/from-file.tok", // from file
			GithubClientID:       "id-from-flag",       // flag wins
			GithubScope:          "scope:from-env",     // env over file
		}
		if got != want {
			t.Errorf("loadLogin() = %+v, want %+v", got, want)
		}
	})

	t.Run("shared --github-oauth-token-file flag over env", func(t *testing.T) {
		got, err := loadLogin(
			[]string{"--github-oauth-token-file", "/tmp/flag.tok"},
			envFunc(map[string]string{"COPILOTD_GITHUB_OAUTH_TOKEN_FILE": "/tmp/env.tok"}),
		)
		if err != nil {
			t.Fatalf("loadLogin() error = %v", err)
		}
		if got.GithubOAuthTokenFile != "/tmp/flag.tok" {
			t.Errorf("GithubOAuthTokenFile = %q, want /tmp/flag.tok (flag over env)", got.GithubOAuthTokenFile)
		}
	})

	t.Run("client-id and scope via env over default", func(t *testing.T) {
		got, err := loadLogin(nil, envFunc(map[string]string{
			"COPILOTD_GITHUB_CLIENT_ID": "id-env",
			"COPILOTD_GITHUB_SCOPE":     "repo",
		}))
		if err != nil {
			t.Fatalf("loadLogin() error = %v", err)
		}
		if got.GithubClientID != "id-env" || got.GithubScope != "repo" {
			t.Errorf("client-id/scope = %q/%q, want id-env/repo", got.GithubClientID, got.GithubScope)
		}
	})
}

func TestLoadLoginValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantSub string
	}{
		{"empty client-id", []string{"--github-client-id", ""}, nil, "github-client-id"},
		{"empty scope", []string{"--github-scope", ""}, nil, "github-scope"},
		{"whitespace client-id via env", nil, map[string]string{"COPILOTD_GITHUB_CLIENT_ID": "   "}, "github-client-id"},
		{"bad log level", []string{"--log-level", "trace"}, nil, "log-level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadLogin(tc.args, envFunc(tc.env))
			if err == nil {
				t.Fatalf("loadLogin() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("loadLogin() error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoginConfigLogValueEmitsAllFields(t *testing.T) {
	cfg := LoginConfig{
		LogLevel:             "info",
		LogFormat:            "text",
		LogFile:              "/var/log/copilotd.log",
		GithubOAuthTokenFile: "/home/op/.config/copilotd/github-oauth-token",
		GithubClientID:       "Iv1.b507a08c87ecfe98",
		GithubScope:          "read:user",
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("effective config", "config", cfg)
	out := buf.String()
	for _, want := range []string{
		"config.log-level=info",
		"config.log-format=text",
		"config.log-file=/var/log/copilotd.log",
		"config.github-oauth-token-file=/home/op/.config/copilotd/github-oauth-token",
		"config.github-client-id=Iv1.b507a08c87ecfe98",
		"config.github-scope=read:user",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\nfull: %s", want, out)
		}
	}
}
