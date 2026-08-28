// Package config loads and validates copilotd's runtime configuration.
//
// Configuration is split by operational subcommand. Serve and login each own an
// independent flag set containing the same five common operational flags plus
// their command-specific flags. Env lookup is injected so precedence and
// validation stay pure and table-testable. Precedence is flags > env > TOML file
// > default.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peterbourgon/ff/v4"
)

// Defaults for every configurable value. The bind default is loopback so this
// credential-handling proxy is not network-exposed until an operator opts in.
const (
	defaultAddr            = "127.0.0.1:8080"
	defaultLogLevel        = "info"
	defaultLogFormat       = "text"
	defaultShutdownTimeout = 10 * time.Second

	// The timeout defaults separately bound buffered response completion, stream
	// silence, individual downstream writes, and time-to-first-byte. The
	// request and opt-in buffered-response caps (32 MiB each) are generous enough
	// for multi-image base64 while guarding against pathological bodies.
	//
	// Timeouts are written in seconds and refresh intervals in hours, matching
	// the durationUnit each row declares below so a constant, its --help default,
	// and its CONFIGURATION.md row all read alike. Overrides still accept any Go
	// duration form.
	defaultOutboundTimeout                             = 600 * time.Second
	defaultStreamIdleTimeout                           = 600 * time.Second
	defaultStreamKeepaliveInterval                     = 15 * time.Second
	defaultWriteTimeout                                = 90 * time.Second
	defaultResponseHeaderTimeout                       = 600 * time.Second
	defaultWebSocketHandshakeTimeout                   = 10 * time.Second
	defaultMaxRequestBytes                             = 33554432
	defaultMaxBufferedResponseBytes                    = 33554432
	defaultAnthropicCatalogModelIDNormalizationEnabled = false
	defaultShimNopEnabled                              = false
	defaultShimResponsesItemIDStabilizerEnabled        = false
	defaultCodexCatalogEnabled                         = false
	defaultCodexAutoReviewModel                        = ""
	defaultCodexOverrideLimits                         = false
	defaultCodexCatalogRefreshInterval                 = 24 * time.Hour

	// defaultStartupMintRetries bounds the transient-failure retries of the boot
	// warm-up mint (total attempts = 1 + N); auth-class failures short-circuit.
	defaultStartupMintRetries = 3

	// Impersonation defaults present copilotd to Copilot as the VS Code Copilot
	// client so upstream client/user-agent allowlist checks pass. The two bare
	// versions are fallbacks for runtime discovery; main derives the exact header
	// values from them. The discovery interval controls the runtime orchestration
	// and zero disables discovery.
	defaultCopilotIntegrationID         = "vscode-chat"
	defaultVSCodeVersionFallback        = "1.104.1"
	defaultPluginVersionFallback        = "0.26.7"
	defaultGithubAPIVersion             = "2025-04-01"
	defaultImpersonationRefreshInterval = 24 * time.Hour

	// Device-flow defaults for `copilotd login` (§9.3). The client id is the
	// public VS Code Copilot OAuth app; it is overridable so a GitHub Enterprise
	// Server deployment can point login at its own OAuth app.
	defaultGithubClientID = "Iv1.b507a08c87ecfe98"
	defaultGithubScope    = "read:user"

	// envPrefix is prepended (with an underscore) to the upper-cased flag name
	// to form the environment variable, e.g. --log-level -> COPILOTD_LOG_LEVEL.
	envPrefix = "COPILOTD"
)

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"text", "json"}
)

// ServeConfig is the resolved, validated configuration for `copilotd serve`. It
// carries the common operational fields (logging, config-selected values, and
// the GitHub OAuth token file path) plus serve-specific settings.
type ServeConfig struct {
	Addr            string
	LogLevel        string
	LogFormat       string
	LogFile         string // empty = stderr
	ShutdownTimeout time.Duration

	// GithubOAuthTokenFile is the path to the raw GitHub OAuth token file. This
	// phase only parses and stores it; reading/writing the file lands later. It
	// is a path, not the secret itself, so it is safe to log.
	GithubOAuthTokenFile string

	// APIKey is the required inbound secret clients present (Authorization: Bearer
	// or x-api-key). It is a secret — omitted from LogValue (redaction by
	// construction) and validated non-empty so serve fails fast before binding.
	APIKey string

	// OutboundTimeout is the total backstop for a buffered upstream response.
	OutboundTimeout time.Duration

	// StreamIdleTimeout bounds genuine upstream silence on a streaming response.
	StreamIdleTimeout time.Duration

	// StreamKeepaliveInterval bounds an idle gap before an OpenAI keepalive.
	StreamKeepaliveInterval time.Duration

	// WriteTimeout bounds each individual downstream write.
	WriteTimeout time.Duration

	// ResponseHeaderTimeout bounds the wait for upstream response headers.
	ResponseHeaderTimeout time.Duration

	// WebSocketHandshakeTimeout bounds the upstream WebSocket dial only.
	WebSocketHandshakeTimeout time.Duration

	// MaxRequestBytes caps an inbound request body; an over-limit body yields 413.
	MaxRequestBytes int64

	// MaxBufferedResponseBytes caps an upstream response only when a buffered
	// response shim is active; an over-limit body yields 413 before commit.
	MaxBufferedResponseBytes int64

	// AnthropicCatalogModelIDNormalizationEnabled controls the opt-in rewrite of
	// dots in Anthropic-vendored Claude model IDs to hyphens.
	AnthropicCatalogModelIDNormalizationEnabled bool

	// ShimNopEnabled controls the canonical no-op shim. It is disabled by
	// default, like the shim-defined default in the canonical registry.
	ShimNopEnabled bool

	// ShimResponsesItemIDStabilizerEnabled controls the opt-in OpenAI Responses
	// item-id stabilizer shim. It is disabled by default.
	ShimResponsesItemIDStabilizerEnabled bool

	// The Codex settings control the opt-in client-shaped Codex catalog and its
	// overlays. They are non-secret and remain valid but inert while the catalog
	// is disabled.
	CodexCatalogEnabled           bool
	CodexAutoReviewModel          string
	CodexAutoReviewModelOverrides map[string]string
	CodexOverrideLimits           bool

	// CodexCatalogRefreshInterval controls best-effort refresh of Codex's
	// models.json cached value. Zero pins the embedded floor.
	CodexCatalogRefreshInterval time.Duration

	// GithubOAuthToken is the inline GitHub OAuth token; when present it takes
	// precedence over the GitHub OAuth token file (resolution lands in #12). It is
	// a secret — omitted from LogValue (redaction by construction). This phase only
	// stores it.
	GithubOAuthToken string

	// StartupMintRetries bounds the transient-failure retries of the startup mint
	// (total attempts = 1 + N). Auth-class failures short-circuit regardless.
	StartupMintRetries int

	// The impersonation fallbacks and static identifiers (§6.7). Non-secret;
	// logged normally. Runtime discovery will replace the bare-version fallbacks
	// when it succeeds; the cadence is parsed and stored but remains inert here.
	VSCodeVersionFallback        string
	PluginVersionFallback        string
	CopilotIntegrationID         string
	GithubAPIVersion             string
	ImpersonationRefreshInterval time.Duration
}

// LogValue implements slog.LogValuer. A read-only descriptor view rebuilt from
// the shared declaration emits non-secret fields only, so APIKey and the inline
// GitHub OAuth token are redacted by construction.
func (c ServeConfig) LogValue() slog.Value {
	specs, _ := serveSpecs()
	attrs := appendSpecLogAttrs(make([]slog.Attr, 0, len(specs)), specs, &c)
	return slog.GroupValue(attrs...)
}

func appendSpecLogAttrs[C any](attrs []slog.Attr, specs []spec[C], target *C) []slog.Attr {
	for _, s := range specs {
		if attr, include := s.logAttr(target); include {
			attrs = append(attrs, attr)
		}
	}
	return attrs
}

func formatAutoReviewModelOverrides(overrides map[string]string) string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+overrides[key])
	}
	return strings.Join(pairs, ",")
}

// commonTargets supplies command-local accessors for the shared operational
// settings while preserving the flat ServeConfig and LoginConfig layouts.
type commonTargets[C any] struct {
	logLevel, logFormat, logFile, githubOAuthTokenFile func(*C) *string
}

// commonFields declares the five shared rows in their help-mandated order:
// log-level, log-format, log-file, config, github-oauth-token-file. The config
// path row is registration-only because it selects the file resolved below.
func commonFields[C any](targets commonTargets[C]) ([]spec[C], *configPathField[C]) {
	configPath := &configPathField[C]{}
	return []spec[C]{
		stringField("log-level", defaultLogLevel, targets.logLevel, oneOf(validLogLevels), "log level: debug|info|warn|error"),
		stringField("log-format", defaultLogFormat, targets.logFormat, oneOf(validLogFormats), "log format: text|json"),
		stringField("log-file", "", targets.logFile, nil, "log file path (empty = stderr)"),
		configPath,
		stringField("github-oauth-token-file", defaultOAuthTokenFile(), targets.githubOAuthTokenFile, nil, "path to the raw GitHub OAuth token file"),
	}, configPath
}

type codexAutoReviewModelOverridesFlagValue struct {
	stored *map[string]string
}

func (v *codexAutoReviewModelOverridesFlagValue) String() string {
	if v == nil || v.stored == nil {
		return ""
	}
	return formatAutoReviewModelOverrides(*v.stored)
}

func (v *codexAutoReviewModelOverridesFlagValue) Set(raw string) error {
	parsed, err := parseAutoReviewModelOverrides(raw)
	if err != nil {
		return err
	}
	*v.stored = parsed
	return nil
}

func registerCodexAutoReviewModelOverrides(fs *ff.FlagSet, name string, def map[string]string, usage string) *map[string]string {
	stored := def
	if _, err := fs.AddFlag(ff.FlagConfig{
		LongName:    name,
		Usage:       usage,
		Value:       &codexAutoReviewModelOverridesFlagValue{stored: &stored},
		Placeholder: "STRING",
	}); err != nil {
		panic(err)
	}
	return &stored
}

func codexAutoReviewModelOverridesField() spec[ServeConfig] {
	return &field[ServeConfig, map[string]string]{
		name:  "codex-auto-review-model-overrides",
		usage: "per-main-model reviewer overrides (main=reviewer,...)",
		get:   func(c *ServeConfig) *map[string]string { return &c.CodexAutoReviewModelOverrides },
		parse: parseAutoReviewModelOverrides,
		reg:   registerCodexAutoReviewModelOverrides,
		logf: func(key string, value map[string]string) slog.Attr {
			return slog.String(key, formatAutoReviewModelOverrides(value))
		},
	}
}

// serveSpecs declares every serve setting once, in registration order. Each
// registered handle retains one instance for resolution; LogValue creates a
// read-only view from this same declaration. The common rows remain first in
// their help-mandated order.
func serveSpecs() ([]spec[ServeConfig], *configPathField[ServeConfig]) {
	common, configPath := commonFields(commonTargets[ServeConfig]{
		logLevel:             func(c *ServeConfig) *string { return &c.LogLevel },
		logFormat:            func(c *ServeConfig) *string { return &c.LogFormat },
		logFile:              func(c *ServeConfig) *string { return &c.LogFile },
		githubOAuthTokenFile: func(c *ServeConfig) *string { return &c.GithubOAuthTokenFile },
	})
	serveSpecific := []spec[ServeConfig]{
		stringField("addr", defaultAddr, func(c *ServeConfig) *string { return &c.Addr }, validAddr, "bind address (host:port)"),
		durationField("shutdown-timeout", defaultShutdownTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.ShutdownTimeout }, positive, "graceful shutdown grace period"),
		secretStringField("apikey", func(c *ServeConfig) *string { return &c.APIKey }, required, "required inbound API key clients must present (secret)"),
		durationField("outbound-timeout", defaultOutboundTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.OutboundTimeout }, positive, "buffered upstream response timeout"),
		durationField("stream-idle-timeout", defaultStreamIdleTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.StreamIdleTimeout }, positive, "upstream stream idle timeout"),
		durationField("stream-keepalive-interval", defaultStreamKeepaliveInterval, inSeconds, func(c *ServeConfig) *time.Duration { return &c.StreamKeepaliveInterval }, positive, "OpenAI stream keepalive interval"),
		durationField("write-timeout", defaultWriteTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.WriteTimeout }, positive, "per-write downstream timeout"),
		durationField("response-header-timeout", defaultResponseHeaderTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.ResponseHeaderTimeout }, positive, "upstream response-header timeout"),
		durationField("ws-handshake-timeout", defaultWebSocketHandshakeTimeout, inSeconds, func(c *ServeConfig) *time.Duration { return &c.WebSocketHandshakeTimeout }, positive, "upstream WebSocket handshake timeout"),
		int64Field("max-request-bytes", defaultMaxRequestBytes, func(c *ServeConfig) *int64 { return &c.MaxRequestBytes }, positive, "maximum inbound request body size in bytes"),
		int64Field("max-buffered-response-bytes", defaultMaxBufferedResponseBytes, func(c *ServeConfig) *int64 { return &c.MaxBufferedResponseBytes }, positive, "maximum buffered upstream response body size in bytes"),
		boolField("anthropic-catalog-model-id-normalization-enabled", defaultAnthropicCatalogModelIDNormalizationEnabled, func(c *ServeConfig) *bool { return &c.AnthropicCatalogModelIDNormalizationEnabled }, "normalize Anthropic-vendored Claude model IDs to hyphenated slugs (opt-in)"),
		boolField("shim-nop-enabled", defaultShimNopEnabled, func(c *ServeConfig) *bool { return &c.ShimNopEnabled }, "enable the canonical no-op shim"),
		boolField("shim-responses-item-id-stabilizer-enabled", defaultShimResponsesItemIDStabilizerEnabled, func(c *ServeConfig) *bool { return &c.ShimResponsesItemIDStabilizerEnabled }, "stabilize churning OpenAI Responses item ids (opt-in)"),
		boolField("codex-catalog-enabled", defaultCodexCatalogEnabled, func(c *ServeConfig) *bool { return &c.CodexCatalogEnabled }, "enable the Codex client-shaped catalog"),
		stringField("codex-auto-review-model", defaultCodexAutoReviewModel, func(c *ServeConfig) *string { return &c.CodexAutoReviewModel }, nil, "reviewer model injected into the Codex catalog"),
		codexAutoReviewModelOverridesField(),
		boolField("codex-catalog-override-limits", defaultCodexOverrideLimits, func(c *ServeConfig) *bool { return &c.CodexOverrideLimits }, "override Codex catalog limits with live Copilot limits"),
		durationField("codex-catalog-refresh-interval", defaultCodexCatalogRefreshInterval, inHours, func(c *ServeConfig) *time.Duration { return &c.CodexCatalogRefreshInterval }, nonNegative, "Codex models.json refresh cadence (0 pins the embedded floor)"),
		secretStringField("github-oauth-token", func(c *ServeConfig) *string { return &c.GithubOAuthToken }, nil, "inline GitHub OAuth token (secret; precedence over the GitHub OAuth token file)"),
		intField("startup-mint-retries", defaultStartupMintRetries, func(c *ServeConfig) *int { return &c.StartupMintRetries }, nonNegative, "transient startup-mint retries (total attempts = 1 + N)"),
		stringField("vscode-version", defaultVSCodeVersionFallback, func(c *ServeConfig) *string { return &c.VSCodeVersionFallback }, bareVersion, "impersonation: bare VS Code version fallback"),
		stringField("plugin-version", defaultPluginVersionFallback, func(c *ServeConfig) *string { return &c.PluginVersionFallback }, bareVersion, "impersonation: bare Copilot Chat version fallback"),
		stringField("copilot-integration-id", defaultCopilotIntegrationID, func(c *ServeConfig) *string { return &c.CopilotIntegrationID }, nil, "impersonation: Copilot-Integration-Id header value"),
		stringField("github-api-version", defaultGithubAPIVersion, func(c *ServeConfig) *string { return &c.GithubAPIVersion }, nil, "impersonation: X-GitHub-Api-Version header value"),
		durationField("impersonation-refresh-interval", defaultImpersonationRefreshInterval, inHours, func(c *ServeConfig) *time.Duration { return &c.ImpersonationRefreshInterval }, nonNegative, "impersonation version re-discovery cadence (0 disables discovery)"),
	}
	return append(common, serveSpecific...), configPath
}

// ServeFlags bundles the parsed flag pointers for `copilotd serve`. It is an
// opaque handle produced by RegisterServe and consumed by Resolve.
type ServeFlags struct {
	fs         *ff.FlagSet
	specs      []spec[ServeConfig]
	configPath *configPathField[ServeConfig]
}

// RegisterServe declares the common operational flags first, followed by the
// serve-specific flags, on a single command-local flag set.
func RegisterServe(fs *ff.FlagSet) *ServeFlags {
	specs, configPath := serveSpecs()
	f := &ServeFlags{fs: fs, specs: specs, configPath: configPath}
	for _, s := range f.specs {
		s.register(fs)
	}

	return f
}

// Resolve layers env and TOML file over the parsed flags (precedence
// flags > env > file > default) and validates, returning the ServeConfig.
// Invalid configuration returns an error with no usable config, so callers fail
// fast before binding a listener.
//
// The env layer is applied by hand (rather than via ff's own env support)
// because ff reads the OS environment directly; injecting lookupEnv keeps Resolve
// pure and testable.
func (f *ServeFlags) Resolve(lookupEnv func(string) (string, bool)) (ServeConfig, error) {
	set := setFlags(f.fs)
	path := resolveConfigPath(set, f.configPath.flagValue(), lookupEnv)
	var cfg ServeConfig
	err := resolve(
		f.specs,
		f.fs,
		&cfg,
		path,
		lookupEnv,
	)
	if err != nil {
		return ServeConfig{}, err
	}
	return cfg, nil
}

func parseAutoReviewModelOverrides(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	overrides := make(map[string]string)
	for _, segment := range strings.Split(raw, ",") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		pair := strings.SplitN(segment, "=", 2)
		if len(pair) != 2 {
			return nil, fmt.Errorf("invalid codex-auto-review-model-overrides segment %q: expected main=reviewer", segment)
		}
		mainModel := strings.TrimSpace(pair[0])
		reviewerModel := strings.TrimSpace(pair[1])
		if mainModel == "" {
			return nil, fmt.Errorf("invalid codex-auto-review-model-overrides segment %q: main model is empty", segment)
		}
		if reviewerModel == "" {
			return nil, fmt.Errorf("invalid codex-auto-review-model-overrides segment %q: reviewer model is empty", segment)
		}
		if _, exists := overrides[mainModel]; exists {
			return nil, fmt.Errorf("invalid codex-auto-review-model-overrides: duplicate main model %q", mainModel)
		}
		overrides[mainModel] = reviewerModel
	}
	if len(overrides) == 0 {
		return nil, nil
	}
	return overrides, nil
}

// defaultOAuthTokenFile is the default path to the GitHub OAuth token file:
// <os.UserConfigDir()>/copilotd/github-oauth-token. If the user config dir cannot
// be determined it falls back to a relative path, so flag registration never
// fails.
func defaultOAuthTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join("copilotd", "github-oauth-token")
	}
	return filepath.Join(dir, "copilotd", "github-oauth-token")
}

// setFlags returns the set of long flag names that were explicitly set on the
// command line, used to drive the flag layer of precedence.
func setFlags(fs *ff.FlagSet) map[string]bool {
	set := make(map[string]bool)
	_ = fs.WalkFlags(func(f ff.Flag) error {
		if name, ok := f.GetLongName(); ok && f.IsSet() {
			set[name] = true
		}
		return nil
	})
	return set
}

// resolveConfigPath picks the TOML file path with flag > env precedence.
func resolveConfigPath(set map[string]bool, flagVal string, lookupEnv func(string) (string, bool)) string {
	if set["config"] {
		return flagVal
	}
	if v, ok := lookupEnv(envVarName("config")); ok {
		return v
	}
	return ""
}

// envVarName maps a canonical key to its environment variable, following the
// same convention ff uses: "log-level" -> "COPILOTD_LOG_LEVEL".
func envVarName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// LoginConfig is the resolved, validated configuration for `copilotd login`. It
// carries the common operational logging fields and the GitHub OAuth token file
// write target, plus the two device-flow knobs. None of its fields is a secret, so
// LogValue enumerates them all.
type LoginConfig struct {
	LogLevel  string
	LogFormat string
	LogFile   string // empty = stderr

	// GithubOAuthTokenFile is the path login writes the raw GitHub OAuth token to
	// (the same command-local setting serve reads). It is a path, not the secret.
	GithubOAuthTokenFile string

	// GithubClientID is the device-flow OAuth app client id; GithubScope is the
	// requested scope. Both are non-secret knobs, validated non-empty.
	GithubClientID string
	GithubScope    string
}

// LogValue implements slog.LogValuer. Every login field is non-secret, so all
// are enumerated; the token itself is never held by LoginConfig.
func (c LoginConfig) LogValue() slog.Value {
	specs := loginSpecs()
	attrs := appendSpecLogAttrs(make([]slog.Attr, 0, len(specs.order)), specs.order, &c)
	return slog.GroupValue(attrs...)
}

type loginSpecTable struct {
	order      []spec[LoginConfig]
	configPath *configPathField[LoginConfig]
}

// loginSpecs declares the shared operational settings first, followed by the
// two login-specific settings, preserving registration, resolution, validation,
// and logging order from one set of descriptors.
func loginSpecs() loginSpecTable {
	common, configPath := commonFields(commonTargets[LoginConfig]{
		logLevel:             func(c *LoginConfig) *string { return &c.LogLevel },
		logFormat:            func(c *LoginConfig) *string { return &c.LogFormat },
		logFile:              func(c *LoginConfig) *string { return &c.LogFile },
		githubOAuthTokenFile: func(c *LoginConfig) *string { return &c.GithubOAuthTokenFile },
	})
	loginSpecific := []spec[LoginConfig]{
		stringField("github-client-id", defaultGithubClientID, func(c *LoginConfig) *string { return &c.GithubClientID }, required, "device-flow OAuth app client id (override for GitHub Enterprise Server)"),
		stringField("github-scope", defaultGithubScope, func(c *LoginConfig) *string { return &c.GithubScope }, required, "device-flow OAuth scope"),
	}
	return loginSpecTable{
		order:      append(common, loginSpecific...),
		configPath: configPath,
	}
}

// LoginFlags is the opaque registered descriptor handle for `copilotd login`.
type LoginFlags struct {
	fs    *ff.FlagSet
	specs loginSpecTable
}

// RegisterLogin declares the common operational flags first, followed by the
// login-specific flags, on a single command-local flag set.
func RegisterLogin(fs *ff.FlagSet) *LoginFlags {
	f := &LoginFlags{fs: fs, specs: loginSpecs()}
	for _, s := range f.specs.order {
		s.register(fs)
	}
	return f
}

// Resolve layers env and TOML over the parsed flags (precedence
// flags > env > file > default) and validates, returning the LoginConfig.
func (f *LoginFlags) Resolve(lookupEnv func(string) (string, bool)) (LoginConfig, error) {
	set := setFlags(f.fs)
	path := resolveConfigPath(set, f.specs.configPath.flagValue(), lookupEnv)
	cfg := LoginConfig{}
	if err := resolve(f.specs.order, f.fs, &cfg, path, lookupEnv); err != nil {
		return LoginConfig{}, err
	}
	return cfg, nil
}
