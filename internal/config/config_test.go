package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// loadServe builds the serve flag set the way the command tree does (root and
// serve flags on one set for the pure loader), parses args, and resolves. It is
// the test seam that keeps the Phase 0 precedence/validation tests intact after
// the split into RegisterServe + Resolve.
func loadServe(args []string, lookupEnv func(string) (string, bool)) (ServeConfig, error) {
	fs := ff.NewFlagSet("copilotd")
	f := RegisterServe(fs, fs)
	if err := ff.Parse(fs, args); err != nil {
		return ServeConfig{}, fmt.Errorf("parse flags: %w", err)
	}
	return f.Resolve(lookupEnv)
}

func defaultConfig() ServeConfig {
	return ServeConfig{
		Addr:                 "127.0.0.1:8080",
		LogLevel:             "info",
		LogFormat:            "text",
		LogFile:              "",
		ShutdownTimeout:      10 * time.Second,
		GithubOAuthTokenFile: defaultOAuthTokenFile(),
		APIKey:               testAPIKey,
		OutboundTimeout:      600 * time.Second,
		MaxRequestBytes:      33554432,
		StartupMintRetries:   3,
		CopilotIntegrationID: "vscode-chat",
		EditorVersion:        "vscode/1.104.1",
		EditorPluginVersion:  "copilot-chat/0.26.7",
		CopilotUserAgent:     "GitHubCopilotChat/0.26.7",
		GithubAPIVersion:     "2025-04-01",
	}
}

func TestLoadDefaults(t *testing.T) {
	got, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
	if err != nil {
		t.Fatalf("loadServe() error = %v", err)
	}
	if got != defaultConfig() {
		t.Errorf("loadServe() = %+v, want %+v", got, defaultConfig())
	}
}

func TestLoadPrecedence(t *testing.T) {
	tokenFile := defaultOAuthTokenFile()

	// impersonationDefaults folds the new §6.7 knob defaults (plus the startup-mint
	// retry default) into a precedence want, since Resolve now populates them; the
	// precedence cases only exercise addr/log/file fields.
	withDefaults := func(c ServeConfig) ServeConfig {
		c.StartupMintRetries = 3
		c.CopilotIntegrationID = "vscode-chat"
		c.EditorVersion = "vscode/1.104.1"
		c.EditorPluginVersion = "copilot-chat/0.26.7"
		c.CopilotUserAgent = "GitHubCopilotChat/0.26.7"
		c.GithubAPIVersion = "2025-04-01"
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
			if got != tc.want {
				t.Errorf("loadServe() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLoadOAuthTokenFile covers the new root-inherited --github-oauth-token-file
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
		Addr:                 "127.0.0.1:8080",
		LogLevel:             "info",
		LogFormat:            "text",
		LogFile:              "/var/log/copilotd.log",
		ShutdownTimeout:      10 * time.Second,
		GithubOAuthTokenFile: "/home/op/.config/copilotd/github-oauth-token",
		APIKey:               "super-secret-apikey-value",
		UpstreamBase:         "https://upstream.example.invalid",
		OutboundTimeout:      600 * time.Second,
		MaxRequestBytes:      33554432,
		GithubOAuthToken:     "gho-super-secret-oauth-value",
		StartupMintRetries:   3,
		CopilotIntegrationID: "vscode-chat",
		EditorVersion:        "vscode/1.104.1",
		EditorPluginVersion:  "copilot-chat/0.26.7",
		CopilotUserAgent:     "GitHubCopilotChat/0.26.7",
		GithubAPIVersion:     "2025-04-01",
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
		"config.upstream-base=https://upstream.example.invalid",
		"config.outbound-timeout=10m0s",
		"config.max-request-bytes=33554432",
		"config.startup-mint-retries=3",
		"config.copilot-integration-id=vscode-chat",
		"config.editor-version=vscode/1.104.1",
		"config.editor-plugin-version=copilot-chat/0.26.7",
		"config.copilot-user-agent=GitHubCopilotChat/0.26.7",
		"config.github-api-version=2025-04-01",
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
			"CopilotIntegrationID": "vscode-chat",
			"EditorVersion":        "vscode/1.104.1",
			"EditorPluginVersion":  "copilot-chat/0.26.7",
			"CopilotUserAgent":     "GitHubCopilotChat/0.26.7",
			"GithubAPIVersion":     "2025-04-01",
		}
		gotm := map[string]string{
			"CopilotIntegrationID": got.CopilotIntegrationID,
			"EditorVersion":        got.EditorVersion,
			"EditorPluginVersion":  got.EditorPluginVersion,
			"CopilotUserAgent":     got.CopilotUserAgent,
			"GithubAPIVersion":     got.GithubAPIVersion,
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
			"COPILOTD_EDITOR_VERSION":         "vscode/9.9.9",
			"COPILOTD_GITHUB_API_VERSION":     "2099-01-01",
		}))
		if err != nil {
			t.Fatalf("loadServe() error = %v", err)
		}
		if got.StartupMintRetries != 5 {
			t.Errorf("StartupMintRetries = %d, want 5", got.StartupMintRetries)
		}
		if got.CopilotIntegrationID != "vscode" || got.EditorVersion != "vscode/9.9.9" || got.GithubAPIVersion != "2099-01-01" {
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

// loadLogin builds the login flag tree the way the command tree does: root flags
// (declared by RegisterServe) shared with a login flag set that carries the two
// login-specific flags, then parses args (against login, which inherits root) and
// resolves. It mirrors production wiring so the precedence/validation tests
// exercise the same code path.
func loadLogin(args []string, lookupEnv func(string) (string, bool)) (LoginConfig, error) {
	root := ff.NewFlagSet("copilotd")
	serve := ff.NewFlagSet("serve").SetParent(root)
	_ = RegisterServe(root, serve) // declares the shared root-inherited flags
	login := ff.NewFlagSet("login").SetParent(root)
	lf := RegisterLogin(root, login)
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

	t.Run("inherited --github-oauth-token-file flag over env", func(t *testing.T) {
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
