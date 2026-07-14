// Package config loads and validates copilotd's runtime configuration.
//
// Configuration is split by subcommand: a set of root-inherited flags (logging,
// the config-file path, and the OAuth-token-file path) shared by every verb, and
// serve-specific flags layered on top. RegisterServe declares both groups onto
// ff flag sets so the command tree in main can bind them; ServeFlags.Resolve then
// layers env and TOML over the parsed flags and validates. Env lookup is injected
// so precedence and validation stay pure and table-testable. Precedence is
// flags > env > TOML file > default.
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

	// defaultOutboundTimeout bounds each upstream call via a per-request context
	// deadline (not a blunt client timeout); generous so a slow completion is not
	// killed. defaultMaxRequestBytes (32 MiB) is a safety rail against pathological
	// bodies, generous enough for multi-image base64.
	defaultOutboundTimeout = 600 * time.Second
	defaultMaxRequestBytes = 33554432

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

// ServeConfig is the resolved, validated configuration for `copilotd serve`. It
// carries the root-inherited fields (logging, OAuth-token-file path) plus the
// serve-specific bind address and shutdown timeout.
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

	// UpstreamBase overrides the Copilot base URL. Empty means identity resolves
	// it from the token exchange's endpoints.api (a later slice); it is stored now.
	UpstreamBase string

	// OutboundTimeout bounds each upstream call via a per-request context deadline.
	OutboundTimeout time.Duration

	// MaxRequestBytes caps an inbound request body; an over-limit body yields 413.
	MaxRequestBytes int64

	// GithubOAuthToken is the inline GitHub OAuth token; when present it takes
	// precedence over the token file (resolution lands in #12). It is a secret —
	// omitted from LogValue (redaction by construction). This phase only stores it.
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
		slog.String("upstream-base", c.UpstreamBase),
		slog.Duration("outbound-timeout", c.OutboundTimeout),
		slog.Int64("max-request-bytes", c.MaxRequestBytes),
		slog.Int("startup-mint-retries", c.StartupMintRetries),
		slog.String("copilot-integration-id", c.CopilotIntegrationID),
		slog.String("editor-version", c.EditorVersion),
		slog.String("editor-plugin-version", c.EditorPluginVersion),
		slog.String("copilot-user-agent", c.CopilotUserAgent),
		slog.String("github-api-version", c.GithubAPIVersion),
	)
}

// ServeFlags bundles the parsed flag pointers for `copilotd serve` together with
// the flag sets they live on, so Resolve can inspect which flags were explicitly
// set. It is an opaque handle produced by RegisterServe and consumed by Resolve.
type ServeFlags struct {
	rootFS  *ff.FlagSet
	serveFS *ff.FlagSet

	// root-inherited
	logLevel       *string
	logFormat      *string
	logFile        *string
	configPath     *string
	oauthTokenFile *string

	// serve-specific
	addr            *string
	shutdownTimeout *time.Duration
	apikey          *string
	upstreamBase    *string
	outboundTimeout *time.Duration
	maxRequestBytes *int64

	githubOAuthToken     *string
	startupMintRetries   *int
	copilotIntegrationID *string
	editorVersion        *string
	editorPluginVersion  *string
	copilotUserAgent     *string
	githubAPIVersion     *string
}

// RegisterServe declares the root-inherited flags on root and the serve-specific
// flags on serve (which is expected to have root as its parent, so serve inherits
// them). It returns a handle whose Resolve method produces the ServeConfig after
// the command tree parses. The same root flag set is inherited by the other verbs
// (login/help/version); only serve resolves a full config in this phase.
func RegisterServe(root, serve *ff.FlagSet) *ServeFlags {
	f := &ServeFlags{rootFS: root, serveFS: serve}

	// Root-inherited flags, shared by every subcommand (§9.1).
	f.logLevel = root.StringLong("log-level", defaultLogLevel, "log level: debug|info|warn|error")
	f.logFormat = root.StringLong("log-format", defaultLogFormat, "log format: text|json")
	f.logFile = root.StringLong("log-file", "", "log file path (empty = stderr)")
	f.configPath = root.StringLong("config", "", "path to an optional TOML config file")
	f.oauthTokenFile = root.StringLong("github-oauth-token-file", defaultOAuthTokenFile(), "path to the raw GitHub OAuth token file")
	// Defined so `--help` lists it and parsing never rejects it; the actual
	// short-circuit happens in main via VersionRequested, before the tree parses.
	_ = root.BoolLong("version", "print build version and exit")

	// Serve-specific flags (§9.2).
	f.addr = serve.StringLong("addr", defaultAddr, "bind address (host:port)")
	f.shutdownTimeout = serve.DurationLong("shutdown-timeout", defaultShutdownTimeout, "graceful shutdown grace period")
	f.apikey = serve.StringLong("apikey", "", "required inbound API key clients must present (secret)")
	f.upstreamBase = serve.StringLong("upstream-base", "", "override the upstream base URL (empty = resolved from the token exchange)")
	f.outboundTimeout = serve.DurationLong("outbound-timeout", defaultOutboundTimeout, "per-request upstream timeout")
	f.maxRequestBytes = serve.Int64Long("max-request-bytes", defaultMaxRequestBytes, "maximum inbound request body size in bytes")

	f.githubOAuthToken = serve.StringLong("github-oauth-token", "", "inline GitHub OAuth token (secret; precedence over the token file)")
	f.startupMintRetries = serve.IntLong("startup-mint-retries", defaultStartupMintRetries, "transient startup-mint retries (total attempts = 1 + N)")
	f.copilotIntegrationID = serve.StringLong("copilot-integration-id", defaultCopilotIntegrationID, "impersonation: Copilot-Integration-Id header value")
	f.editorVersion = serve.StringLong("editor-version", defaultEditorVersion, "impersonation: Editor-Version header value")
	f.editorPluginVersion = serve.StringLong("editor-plugin-version", defaultEditorPluginVersion, "impersonation: Editor-Plugin-Version header value")
	f.copilotUserAgent = serve.StringLong("copilot-user-agent", defaultCopilotUserAgent, "impersonation: User-Agent header value")
	f.githubAPIVersion = serve.StringLong("github-api-version", defaultGithubAPIVersion, "impersonation: X-GitHub-Api-Version header value")

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
	set := setFlags(f.rootFS)
	for name := range setFlags(f.serveFS) {
		set[name] = true
	}

	cfg := ServeConfig{
		Addr:                 defaultAddr,
		LogLevel:             defaultLogLevel,
		LogFormat:            defaultLogFormat,
		LogFile:              "",
		ShutdownTimeout:      defaultShutdownTimeout,
		GithubOAuthTokenFile: defaultOAuthTokenFile(),
		OutboundTimeout:      defaultOutboundTimeout,
		MaxRequestBytes:      defaultMaxRequestBytes,
		StartupMintRetries:   defaultStartupMintRetries,
		CopilotIntegrationID: defaultCopilotIntegrationID,
		EditorVersion:        defaultEditorVersion,
		EditorPluginVersion:  defaultEditorPluginVersion,
		CopilotUserAgent:     defaultCopilotUserAgent,
		GithubAPIVersion:     defaultGithubAPIVersion,
	}

	// file layer (lowest precedence above defaults)
	if path := resolveConfigPath(set, *f.configPath, lookupEnv); path != "" {
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
	if set["log-level"] {
		cfg.LogLevel = *f.logLevel
	}
	if set["log-format"] {
		cfg.LogFormat = *f.logFormat
	}
	if set["log-file"] {
		cfg.LogFile = *f.logFile
	}
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = *f.shutdownTimeout
	}
	if set["github-oauth-token-file"] {
		cfg.GithubOAuthTokenFile = *f.oauthTokenFile
	}
	if set["apikey"] {
		cfg.APIKey = *f.apikey
	}
	if set["upstream-base"] {
		cfg.UpstreamBase = *f.upstreamBase
	}
	if set["outbound-timeout"] {
		cfg.OutboundTimeout = *f.outboundTimeout
	}
	if set["max-request-bytes"] {
		cfg.MaxRequestBytes = *f.maxRequestBytes
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

// VersionRequested reports whether --version appears in args. It is a cheap
// pre-scan so main can print build info and exit even when the rest of the
// configuration would fail to load.
func VersionRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break // end of flags
		}
		if a == "--version" || a == "-version" {
			return true
		}
	}
	return false
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
	if v, ok := get("upstream-base"); ok {
		cfg.UpstreamBase = v
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
	if v, ok := get("max-request-bytes"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid max-request-bytes %q from %s: %w", v, source, err)
		}
		cfg.MaxRequestBytes = n
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
	if c.MaxRequestBytes <= 0 {
		return fmt.Errorf("invalid max-request-bytes %d: must be positive", c.MaxRequestBytes)
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
// carries the root-inherited logging fields and the OAuth-token-file write
// target, plus the two device-flow knobs. None of its fields is a secret, so
// LogValue enumerates them all.
type LoginConfig struct {
	LogLevel  string
	LogFormat string
	LogFile   string // empty = stderr

	// GithubOAuthTokenFile is the path login writes the raw GitHub OAuth token to
	// (the same root-inherited path serve reads). It is a path, not the secret.
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

// LoginFlags bundles the parsed flag sets for `copilotd login`. The two
// login-specific flags are held as typed pointers; the root-inherited flags
// (logging, config path, OAuth-token-file) are declared by RegisterServe on the
// shared root set and read by name from loginFS, which walks to its parent.
type LoginFlags struct {
	rootFS  *ff.FlagSet
	loginFS *ff.FlagSet

	githubClientID *string
	githubScope    *string
}

// RegisterLogin declares the login-specific flags (github-client-id,
// github-scope) on login. The root-inherited flags (log-*, config,
// github-oauth-token-file) are declared once by RegisterServe on root, which
// login inherits via SetParent; RegisterLogin does not re-declare them. It
// mirrors RegisterServe: the returned handle's Resolve layers env and TOML over
// the parsed flags and validates.
func RegisterLogin(root, login *ff.FlagSet) *LoginFlags {
	f := &LoginFlags{rootFS: root, loginFS: login}
	f.githubClientID = login.StringLong("github-client-id", defaultGithubClientID, "device-flow OAuth app client id (override for GitHub Enterprise Server)")
	f.githubScope = login.StringLong("github-scope", defaultGithubScope, "device-flow OAuth scope")
	return f
}

// Resolve layers env and TOML over the parsed flags (precedence
// flags > env > file > default) and validates, returning the LoginConfig.
func (f *LoginFlags) Resolve(lookupEnv func(string) (string, bool)) (LoginConfig, error) {
	// setFlags on loginFS walks to the parent root set, so a root-inherited flag
	// set on the command line (e.g. --github-oauth-token-file) is detected too.
	set := setFlags(f.loginFS)

	cfg := LoginConfig{
		LogLevel:             defaultLogLevel,
		LogFormat:            defaultLogFormat,
		LogFile:              "",
		GithubOAuthTokenFile: defaultOAuthTokenFile(),
		GithubClientID:       defaultGithubClientID,
		GithubScope:          defaultGithubScope,
	}

	// file layer (lowest precedence above defaults)
	if path := resolveConfigPath(set, flagString(f.loginFS, "config"), lookupEnv); path != "" {
		if err := applyFileLogin(&cfg, path); err != nil {
			return LoginConfig{}, err
		}
	}
	// env layer
	applyEnvLogin(&cfg, lookupEnv)
	// flag layer (highest precedence)
	if set["log-level"] {
		cfg.LogLevel = flagString(f.loginFS, "log-level")
	}
	if set["log-format"] {
		cfg.LogFormat = flagString(f.loginFS, "log-format")
	}
	if set["log-file"] {
		cfg.LogFile = flagString(f.loginFS, "log-file")
	}
	if set["github-oauth-token-file"] {
		cfg.GithubOAuthTokenFile = flagString(f.loginFS, "github-oauth-token-file")
	}
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

// flagString returns the current string value of the named flag (its parsed
// value, or its default if unset), reading through parent flag sets. It returns
// "" for an unknown name.
func flagString(fs *ff.FlagSet, name string) string {
	if fl, ok := fs.GetFlag(name); ok {
		return fl.GetValue()
	}
	return ""
}
