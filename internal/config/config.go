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
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/fftoml"
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
	defaultOutboundTimeout          = 600 * time.Second
	defaultStreamIdleTimeout        = 5 * time.Minute
	defaultStreamKeepaliveInterval  = 15 * time.Second
	defaultWriteTimeout             = 90 * time.Second
	defaultResponseHeaderTimeout    = 600 * time.Second
	defaultMaxRequestBytes          = 33554432
	defaultMaxBufferedResponseBytes = 33554432
	defaultShimNopEnabled           = false
	defaultCodexCatalogEnabled      = false
	defaultCodexAutoReviewModel     = ""
	defaultCodexOverrideLimits      = false

	// defaultStartupMintRetries bounds the transient-failure retries of the boot
	// warm-up mint (total attempts = 1 + N); auth-class failures short-circuit.
	defaultStartupMintRetries = 3

	// Impersonation defaults present copilotd to Copilot as the VS Code Copilot
	// client so upstream client/user-agent allowlist checks pass. They are knobs
	// because they are version-sensitive (§6.7); the identity layer applies them to
	// both the token exchange and every inference request.
	defaultCopilotIntegrationID = "vscode-chat"
	defaultEditorVersion        = "vscode/1.104.1"
	defaultEditorPluginVersion  = "copilot-chat/0.26.7"
	defaultCopilotUserAgent     = "GitHubCopilotChat/0.26.7"
	defaultGithubAPIVersion     = "2025-04-01"

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

// CodexConfig is the resolved configuration consumed by the Codex catalog
// renderer. Its operator-facing keys remain flat alongside the other serve
// settings, while this value gives the server one cohesive value to thread to
// the catalog renderer.
type CodexConfig struct {
	Enabled         bool
	AutoReviewModel string
	OverrideLimits  bool
}

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

	// MaxRequestBytes caps an inbound request body; an over-limit body yields 413.
	MaxRequestBytes int64

	// MaxBufferedResponseBytes caps an upstream response only when a buffered
	// response shim is active; an over-limit body yields 413 before commit.
	MaxBufferedResponseBytes int64

	// ShimNopEnabled controls the canonical no-op shim. It is disabled by
	// default, like the shim-defined default in the canonical registry.
	ShimNopEnabled bool

	// Codex controls the opt-in client-shaped Codex catalog and its overlays.
	// These settings are non-secret and remain valid but inert while the catalog
	// is disabled.
	Codex CodexConfig

	// GithubOAuthToken is the inline GitHub OAuth token; when present it takes
	// precedence over the GitHub OAuth token file (resolution lands in #12). It is
	// a secret — omitted from LogValue (redaction by construction). This phase only
	// stores it.
	GithubOAuthToken string

	// StartupMintRetries bounds the transient-failure retries of the startup mint
	// (total attempts = 1 + N). Auth-class failures short-circuit regardless.
	StartupMintRetries int

	// The impersonation header knobs (§6.7). Non-secret; logged normally. The
	// identity Manager builds an http.Header from these and applies it to the token
	// exchange and every inference request.
	CopilotIntegrationID string
	EditorVersion        string
	EditorPluginVersion  string
	CopilotUserAgent     string
	GithubAPIVersion     string
}

// LogValue implements slog.LogValuer. It enumerates the non-secret fields
// explicitly, so redaction is by construction: any secret field (e.g. APIKey) is
// not logged unless it is deliberately added here.
func (c ServeConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.Addr),
		slog.String("log-level", c.LogLevel),
		slog.String("log-format", c.LogFormat),
		slog.String("log-file", c.LogFile),
		slog.Duration("shutdown-timeout", c.ShutdownTimeout),
		slog.String("github-oauth-token-file", c.GithubOAuthTokenFile),
		slog.Duration("outbound-timeout", c.OutboundTimeout),
		slog.Duration("stream-idle-timeout", c.StreamIdleTimeout),
		slog.Duration("stream-keepalive-interval", c.StreamKeepaliveInterval),
		slog.Duration("write-timeout", c.WriteTimeout),
		slog.Duration("response-header-timeout", c.ResponseHeaderTimeout),
		slog.Int64("max-request-bytes", c.MaxRequestBytes),
		slog.Int64("max-buffered-response-bytes", c.MaxBufferedResponseBytes),
		slog.Bool("shim-nop-enabled", c.ShimNopEnabled),
		slog.Bool("codex-catalog-enabled", c.Codex.Enabled),
		slog.String("codex-auto-review-model", c.Codex.AutoReviewModel),
		slog.Bool("codex-catalog-override-limits", c.Codex.OverrideLimits),
		slog.Int("startup-mint-retries", c.StartupMintRetries),
		slog.String("copilot-integration-id", c.CopilotIntegrationID),
		slog.String("editor-version", c.EditorVersion),
		slog.String("editor-plugin-version", c.EditorPluginVersion),
		slog.String("copilot-user-agent", c.CopilotUserAgent),
		slog.String("github-api-version", c.GithubAPIVersion),
	)
}

// commonFlags is a command-local registration of the five operational settings
// shared by serve and login. Each call creates fresh flag instances, preventing
// parse/reset state from leaking between commands.
type commonFlags struct {
	logLevel             *string
	logFormat            *string
	logFile              *string
	configPath           *string
	githubOAuthTokenFile *string
}

func registerCommon(fs *ff.FlagSet) commonFlags {
	return commonFlags{
		logLevel:             fs.StringLong("log-level", defaultLogLevel, "log level: debug|info|warn|error"),
		logFormat:            fs.StringLong("log-format", defaultLogFormat, "log format: text|json"),
		logFile:              fs.StringLong("log-file", "", "log file path (empty = stderr)"),
		configPath:           fs.StringLong("config", "", "path to an optional TOML config file"),
		githubOAuthTokenFile: fs.StringLong("github-oauth-token-file", defaultOAuthTokenFile(), "path to the raw GitHub OAuth token file"),
	}
}

func (f commonFlags) resolvedConfigPath(set map[string]bool, lookupEnv func(string) (string, bool)) string {
	return resolveConfigPath(set, *f.configPath, lookupEnv)
}

func (f commonFlags) applyFlagValues(set map[string]bool, logLevel, logFormat, logFile, githubOAuthTokenFile *string) {
	if set["log-level"] {
		*logLevel = *f.logLevel
	}
	if set["log-format"] {
		*logFormat = *f.logFormat
	}
	if set["log-file"] {
		*logFile = *f.logFile
	}
	if set["github-oauth-token-file"] {
		*githubOAuthTokenFile = *f.githubOAuthTokenFile
	}
}

// ServeFlags bundles the parsed flag pointers for `copilotd serve`. It is an
// opaque handle produced by RegisterServe and consumed by Resolve.
type ServeFlags struct {
	common commonFlags
	fs     *ff.FlagSet

	// serve-specific
	addr                     *string
	shutdownTimeout          *time.Duration
	apikey                   *string
	outboundTimeout          *time.Duration
	streamIdleTimeout        *time.Duration
	streamKeepaliveInterval  *time.Duration
	writeTimeout             *time.Duration
	responseHeaderTimeout    *time.Duration
	maxRequestBytes          *int64
	maxBufferedResponseBytes *int64
	shimNopEnabled           *bool
	codexCatalogEnabled      *bool
	codexAutoReviewModel     *string
	codexOverrideLimits      *bool

	githubOAuthToken     *string
	startupMintRetries   *int
	copilotIntegrationID *string
	editorVersion        *string
	editorPluginVersion  *string
	copilotUserAgent     *string
	githubAPIVersion     *string
}

// RegisterServe declares the common operational flags first, followed by the
// serve-specific flags, on a single command-local flag set.
func RegisterServe(fs *ff.FlagSet) *ServeFlags {
	f := &ServeFlags{common: registerCommon(fs), fs: fs}

	// Serve-specific flags (§9.2).
	f.addr = fs.StringLong("addr", defaultAddr, "bind address (host:port)")
	f.shutdownTimeout = fs.DurationLong("shutdown-timeout", defaultShutdownTimeout, "graceful shutdown grace period")
	f.apikey = fs.StringLong("apikey", "", "required inbound API key clients must present (secret)")
	f.outboundTimeout = fs.DurationLong("outbound-timeout", defaultOutboundTimeout, "buffered upstream response timeout")
	f.streamIdleTimeout = fs.DurationLong("stream-idle-timeout", defaultStreamIdleTimeout, "upstream stream idle timeout")
	f.streamKeepaliveInterval = fs.DurationLong("stream-keepalive-interval", defaultStreamKeepaliveInterval, "OpenAI stream keepalive interval")
	f.writeTimeout = fs.DurationLong("write-timeout", defaultWriteTimeout, "per-write downstream timeout")
	f.responseHeaderTimeout = fs.DurationLong("response-header-timeout", defaultResponseHeaderTimeout, "upstream response-header timeout")
	f.maxRequestBytes = fs.Int64Long("max-request-bytes", defaultMaxRequestBytes, "maximum inbound request body size in bytes")
	f.maxBufferedResponseBytes = fs.Int64Long("max-buffered-response-bytes", defaultMaxBufferedResponseBytes, "maximum buffered upstream response body size in bytes")
	f.shimNopEnabled = fs.BoolLongDefault("shim-nop-enabled", defaultShimNopEnabled, "enable the canonical no-op shim")
	f.codexCatalogEnabled = fs.BoolLongDefault("codex-catalog-enabled", defaultCodexCatalogEnabled, "enable the Codex client-shaped catalog")
	f.codexAutoReviewModel = fs.StringLong("codex-auto-review-model", defaultCodexAutoReviewModel, "reviewer model injected into the Codex catalog")
	f.codexOverrideLimits = fs.BoolLongDefault("codex-catalog-override-limits", defaultCodexOverrideLimits, "override Codex catalog limits with live Copilot limits")

	f.githubOAuthToken = fs.StringLong("github-oauth-token", "", "inline GitHub OAuth token (secret; precedence over the GitHub OAuth token file)")
	f.startupMintRetries = fs.IntLong("startup-mint-retries", defaultStartupMintRetries, "transient startup-mint retries (total attempts = 1 + N)")
	f.copilotIntegrationID = fs.StringLong("copilot-integration-id", defaultCopilotIntegrationID, "impersonation: Copilot-Integration-Id header value")
	f.editorVersion = fs.StringLong("editor-version", defaultEditorVersion, "impersonation: Editor-Version header value")
	f.editorPluginVersion = fs.StringLong("editor-plugin-version", defaultEditorPluginVersion, "impersonation: Editor-Plugin-Version header value")
	f.copilotUserAgent = fs.StringLong("copilot-user-agent", defaultCopilotUserAgent, "impersonation: User-Agent header value")
	f.githubAPIVersion = fs.StringLong("github-api-version", defaultGithubAPIVersion, "impersonation: X-GitHub-Api-Version header value")

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

	cfg := ServeConfig{
		Addr:                     defaultAddr,
		LogLevel:                 defaultLogLevel,
		LogFormat:                defaultLogFormat,
		LogFile:                  "",
		ShutdownTimeout:          defaultShutdownTimeout,
		GithubOAuthTokenFile:     defaultOAuthTokenFile(),
		OutboundTimeout:          defaultOutboundTimeout,
		StreamIdleTimeout:        defaultStreamIdleTimeout,
		StreamKeepaliveInterval:  defaultStreamKeepaliveInterval,
		WriteTimeout:             defaultWriteTimeout,
		ResponseHeaderTimeout:    defaultResponseHeaderTimeout,
		MaxRequestBytes:          defaultMaxRequestBytes,
		MaxBufferedResponseBytes: defaultMaxBufferedResponseBytes,
		ShimNopEnabled:           defaultShimNopEnabled,
		Codex: CodexConfig{
			Enabled:         defaultCodexCatalogEnabled,
			AutoReviewModel: defaultCodexAutoReviewModel,
			OverrideLimits:  defaultCodexOverrideLimits,
		},
		StartupMintRetries:   defaultStartupMintRetries,
		CopilotIntegrationID: defaultCopilotIntegrationID,
		EditorVersion:        defaultEditorVersion,
		EditorPluginVersion:  defaultEditorPluginVersion,
		CopilotUserAgent:     defaultCopilotUserAgent,
		GithubAPIVersion:     defaultGithubAPIVersion,
	}

	// file layer (lowest precedence above defaults)
	if path := f.common.resolvedConfigPath(set, lookupEnv); path != "" {
		if err := applyFile(&cfg, path); err != nil {
			return ServeConfig{}, err
		}
	}
	// env layer
	if err := applyEnv(&cfg, lookupEnv); err != nil {
		return ServeConfig{}, err
	}
	// flag layer (highest precedence)
	if set["addr"] {
		cfg.Addr = *f.addr
	}
	f.common.applyFlagValues(set, &cfg.LogLevel, &cfg.LogFormat, &cfg.LogFile, &cfg.GithubOAuthTokenFile)
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = *f.shutdownTimeout
	}
	if set["apikey"] {
		cfg.APIKey = *f.apikey
	}
	if set["outbound-timeout"] {
		cfg.OutboundTimeout = *f.outboundTimeout
	}
	if set["stream-idle-timeout"] {
		cfg.StreamIdleTimeout = *f.streamIdleTimeout
	}
	if set["stream-keepalive-interval"] {
		cfg.StreamKeepaliveInterval = *f.streamKeepaliveInterval
	}
	if set["write-timeout"] {
		cfg.WriteTimeout = *f.writeTimeout
	}
	if set["response-header-timeout"] {
		cfg.ResponseHeaderTimeout = *f.responseHeaderTimeout
	}
	if set["max-request-bytes"] {
		cfg.MaxRequestBytes = *f.maxRequestBytes
	}
	if set["max-buffered-response-bytes"] {
		cfg.MaxBufferedResponseBytes = *f.maxBufferedResponseBytes
	}
	if set["shim-nop-enabled"] {
		cfg.ShimNopEnabled = *f.shimNopEnabled
	}
	if set["codex-catalog-enabled"] {
		cfg.Codex.Enabled = *f.codexCatalogEnabled
	}
	if set["codex-auto-review-model"] {
		cfg.Codex.AutoReviewModel = *f.codexAutoReviewModel
	}
	if set["codex-catalog-override-limits"] {
		cfg.Codex.OverrideLimits = *f.codexOverrideLimits
	}
	if set["github-oauth-token"] {
		cfg.GithubOAuthToken = *f.githubOAuthToken
	}
	if set["startup-mint-retries"] {
		cfg.StartupMintRetries = *f.startupMintRetries
	}
	if set["copilot-integration-id"] {
		cfg.CopilotIntegrationID = *f.copilotIntegrationID
	}
	if set["editor-version"] {
		cfg.EditorVersion = *f.editorVersion
	}
	if set["editor-plugin-version"] {
		cfg.EditorPluginVersion = *f.editorPluginVersion
	}
	if set["copilot-user-agent"] {
		cfg.CopilotUserAgent = *f.copilotUserAgent
	}
	if set["github-api-version"] {
		cfg.GithubAPIVersion = *f.githubAPIVersion
	}

	if err := cfg.validate(); err != nil {
		return ServeConfig{}, err
	}
	return cfg, nil
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

// overlay applies present values from a source getter onto cfg, keyed by the
// canonical (TOML) key names. Only keys the getter reports present are applied;
// absent keys keep their prior value. source names the layer for error text.
func overlay(cfg *ServeConfig, source string, get func(key string) (string, bool)) error {
	if v, ok := get("addr"); ok {
		cfg.Addr = v
	}
	if v, ok := get("log-level"); ok {
		cfg.LogLevel = v
	}
	if v, ok := get("log-format"); ok {
		cfg.LogFormat = v
	}
	if v, ok := get("log-file"); ok {
		cfg.LogFile = v
	}
	if v, ok := get("github-oauth-token-file"); ok {
		cfg.GithubOAuthTokenFile = v
	}
	if v, ok := get("apikey"); ok {
		cfg.APIKey = v
	}
	if v, ok := get("github-oauth-token"); ok {
		cfg.GithubOAuthToken = v
	}
	if v, ok := get("copilot-integration-id"); ok {
		cfg.CopilotIntegrationID = v
	}
	if v, ok := get("editor-version"); ok {
		cfg.EditorVersion = v
	}
	if v, ok := get("editor-plugin-version"); ok {
		cfg.EditorPluginVersion = v
	}
	if v, ok := get("copilot-user-agent"); ok {
		cfg.CopilotUserAgent = v
	}
	if v, ok := get("github-api-version"); ok {
		cfg.GithubAPIVersion = v
	}
	if v, ok := get("shutdown-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid shutdown-timeout %q from %s: %w", v, source, err)
		}
		cfg.ShutdownTimeout = d
	}
	if v, ok := get("outbound-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid outbound-timeout %q from %s: %w", v, source, err)
		}
		cfg.OutboundTimeout = d
	}
	if v, ok := get("stream-idle-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid stream-idle-timeout %q from %s: %w", v, source, err)
		}
		cfg.StreamIdleTimeout = d
	}
	if v, ok := get("stream-keepalive-interval"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid stream-keepalive-interval %q from %s: %w", v, source, err)
		}
		cfg.StreamKeepaliveInterval = d
	}
	if v, ok := get("write-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid write-timeout %q from %s: %w", v, source, err)
		}
		cfg.WriteTimeout = d
	}
	if v, ok := get("response-header-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid response-header-timeout %q from %s: %w", v, source, err)
		}
		cfg.ResponseHeaderTimeout = d
	}
	if v, ok := get("max-request-bytes"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid max-request-bytes %q from %s: %w", v, source, err)
		}
		cfg.MaxRequestBytes = n
	}
	if v, ok := get("max-buffered-response-bytes"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid max-buffered-response-bytes %q from %s: %w", v, source, err)
		}
		cfg.MaxBufferedResponseBytes = n
	}
	if v, ok := get("shim-nop-enabled"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid shim-nop-enabled %q from %s: %w", v, source, err)
		}
		cfg.ShimNopEnabled = enabled
	}
	if v, ok := get("codex-catalog-enabled"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid codex-catalog-enabled %q from %s: %w", v, source, err)
		}
		cfg.Codex.Enabled = enabled
	}
	if v, ok := get("codex-auto-review-model"); ok {
		cfg.Codex.AutoReviewModel = v
	}
	if v, ok := get("codex-catalog-override-limits"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid codex-catalog-override-limits %q from %s: %w", v, source, err)
		}
		cfg.Codex.OverrideLimits = enabled
	}
	if v, ok := get("startup-mint-retries"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid startup-mint-retries %q from %s: %w", v, source, err)
		}
		cfg.StartupMintRetries = n
	}
	return nil
}

// applyFile overlays values found in the TOML file onto cfg.
func applyFile(cfg *ServeConfig, path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer fh.Close()

	values := make(map[string]string)
	if err := fftoml.Parse(fh, func(name, value string) error {
		values[name] = value
		return nil
	}); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	return overlay(cfg, "config file", func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	})
}

// applyEnv overlays COPILOTD_* environment values onto cfg via the injected
// lookup, mapping each canonical key to its environment variable name.
func applyEnv(cfg *ServeConfig, lookupEnv func(string) (string, bool)) error {
	return overlay(cfg, "env", func(key string) (string, bool) {
		return lookupEnv(envVarName(key))
	})
}

// envVarName maps a canonical key to its environment variable, following the
// same convention ff uses: "log-level" -> "COPILOTD_LOG_LEVEL".
func envVarName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

func (c ServeConfig) validate() error {
	if err := validateAddr(c.Addr); err != nil {
		return err
	}
	if !slices.Contains(validLogLevels, c.LogLevel) {
		return fmt.Errorf("invalid log-level %q: must be one of debug, info, warn, error", c.LogLevel)
	}
	if !slices.Contains(validLogFormats, c.LogFormat) {
		return fmt.Errorf("invalid log-format %q: must be one of text, json", c.LogFormat)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("invalid shutdown-timeout %v: must be positive", c.ShutdownTimeout)
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("apikey is required: set --apikey, COPILOTD_APIKEY, or apikey in the config file")
	}
	if c.OutboundTimeout <= 0 {
		return fmt.Errorf("invalid outbound-timeout %v: must be positive", c.OutboundTimeout)
	}
	if c.StreamIdleTimeout <= 0 {
		return fmt.Errorf("invalid stream-idle-timeout %v: must be positive", c.StreamIdleTimeout)
	}
	if c.StreamKeepaliveInterval <= 0 {
		return fmt.Errorf("invalid stream-keepalive-interval %v: must be positive", c.StreamKeepaliveInterval)
	}
	if c.WriteTimeout <= 0 {
		return fmt.Errorf("invalid write-timeout %v: must be positive", c.WriteTimeout)
	}
	if c.ResponseHeaderTimeout <= 0 {
		return fmt.Errorf("invalid response-header-timeout %v: must be positive", c.ResponseHeaderTimeout)
	}
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("invalid max-request-bytes %d: must be positive", c.MaxRequestBytes)
	}
	if c.MaxBufferedResponseBytes <= 0 {
		return fmt.Errorf("invalid max-buffered-response-bytes %d: must be positive", c.MaxBufferedResponseBytes)
	}
	if c.StartupMintRetries < 0 {
		return fmt.Errorf("invalid startup-mint-retries %d: must be >= 0", c.StartupMintRetries)
	}
	return nil
}

func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("invalid addr %q: port must be an integer in [0,65535]", addr)
	}
	return nil
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
	return slog.GroupValue(
		slog.String("log-level", c.LogLevel),
		slog.String("log-format", c.LogFormat),
		slog.String("log-file", c.LogFile),
		slog.String("github-oauth-token-file", c.GithubOAuthTokenFile),
		slog.String("github-client-id", c.GithubClientID),
		slog.String("github-scope", c.GithubScope),
	)
}

// LoginFlags bundles the parsed flag pointers for `copilotd login`.
type LoginFlags struct {
	common commonFlags
	fs     *ff.FlagSet

	githubClientID *string
	githubScope    *string
}

// RegisterLogin declares the common operational flags first, followed by the
// login-specific flags, on a single command-local flag set.
func RegisterLogin(fs *ff.FlagSet) *LoginFlags {
	f := &LoginFlags{common: registerCommon(fs), fs: fs}
	f.githubClientID = fs.StringLong("github-client-id", defaultGithubClientID, "device-flow OAuth app client id (override for GitHub Enterprise Server)")
	f.githubScope = fs.StringLong("github-scope", defaultGithubScope, "device-flow OAuth scope")
	return f
}

// Resolve layers env and TOML over the parsed flags (precedence
// flags > env > file > default) and validates, returning the LoginConfig.
func (f *LoginFlags) Resolve(lookupEnv func(string) (string, bool)) (LoginConfig, error) {
	set := setFlags(f.fs)

	cfg := LoginConfig{
		LogLevel:             defaultLogLevel,
		LogFormat:            defaultLogFormat,
		LogFile:              "",
		GithubOAuthTokenFile: defaultOAuthTokenFile(),
		GithubClientID:       defaultGithubClientID,
		GithubScope:          defaultGithubScope,
	}

	// file layer (lowest precedence above defaults)
	if path := f.common.resolvedConfigPath(set, lookupEnv); path != "" {
		if err := applyFileLogin(&cfg, path); err != nil {
			return LoginConfig{}, err
		}
	}
	// env layer
	applyEnvLogin(&cfg, lookupEnv)
	// flag layer (highest precedence)
	f.common.applyFlagValues(set, &cfg.LogLevel, &cfg.LogFormat, &cfg.LogFile, &cfg.GithubOAuthTokenFile)
	if set["github-client-id"] {
		cfg.GithubClientID = *f.githubClientID
	}
	if set["github-scope"] {
		cfg.GithubScope = *f.githubScope
	}

	if err := cfg.validate(); err != nil {
		return LoginConfig{}, err
	}
	return cfg, nil
}

func (c LoginConfig) validate() error {
	if !slices.Contains(validLogLevels, c.LogLevel) {
		return fmt.Errorf("invalid log-level %q: must be one of debug, info, warn, error", c.LogLevel)
	}
	if !slices.Contains(validLogFormats, c.LogFormat) {
		return fmt.Errorf("invalid log-format %q: must be one of text, json", c.LogFormat)
	}
	if strings.TrimSpace(c.GithubClientID) == "" {
		return fmt.Errorf("github-client-id is required: set --github-client-id, COPILOTD_GITHUB_CLIENT_ID, or github-client-id in the config file")
	}
	if strings.TrimSpace(c.GithubScope) == "" {
		return fmt.Errorf("github-scope is required: set --github-scope, COPILOTD_GITHUB_SCOPE, or github-scope in the config file")
	}
	return nil
}

// overlayLogin applies present values from a source getter onto cfg. Every login
// key is a string, so unlike serve's overlay it needs no per-field parsing.
func overlayLogin(cfg *LoginConfig, get func(key string) (string, bool)) {
	if v, ok := get("log-level"); ok {
		cfg.LogLevel = v
	}
	if v, ok := get("log-format"); ok {
		cfg.LogFormat = v
	}
	if v, ok := get("log-file"); ok {
		cfg.LogFile = v
	}
	if v, ok := get("github-oauth-token-file"); ok {
		cfg.GithubOAuthTokenFile = v
	}
	if v, ok := get("github-client-id"); ok {
		cfg.GithubClientID = v
	}
	if v, ok := get("github-scope"); ok {
		cfg.GithubScope = v
	}
}

// applyFileLogin overlays values found in the TOML file onto cfg.
func applyFileLogin(cfg *LoginConfig, path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer fh.Close()

	values := make(map[string]string)
	if err := fftoml.Parse(fh, func(name, value string) error {
		values[name] = value
		return nil
	}); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	overlayLogin(cfg, func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	})
	return nil
}

// applyEnvLogin overlays COPILOTD_* environment values onto cfg.
func applyEnvLogin(cfg *LoginConfig, lookupEnv func(string) (string, bool)) {
	overlayLogin(cfg, func(key string) (string, bool) {
		return lookupEnv(envVarName(key))
	})
}
