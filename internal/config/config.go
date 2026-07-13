// Package config loads and validates copilotd's runtime configuration.
//
// Load is a pure function: command-line args and an environment lookup are
// injected, so precedence and validation are table-testable without touching
// global or OS state. Precedence is flags > env > TOML file > default.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
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

	// envPrefix is prepended (with an underscore) to the upper-cased flag name
	// to form the environment variable, e.g. --log-level -> COPILOTD_LOG_LEVEL.
	envPrefix = "COPILOTD"
)

var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"text", "json"}
)

// Config is the resolved, validated configuration.
type Config struct {
	Addr            string
	LogLevel        string
	LogFormat       string
	LogFile         string // empty = stderr
	ShutdownTimeout time.Duration
}

// LogValue implements slog.LogValuer. It enumerates the non-secret fields
// explicitly, so redaction is by construction: any future secret field is not
// logged unless it is deliberately added here.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.Addr),
		slog.String("log-level", c.LogLevel),
		slog.String("log-format", c.LogFormat),
		slog.String("log-file", c.LogFile),
		slog.Duration("shutdown-timeout", c.ShutdownTimeout),
	)
}

// Load resolves configuration from args and env, layering flags > env > TOML
// file > default, then validates. Invalid configuration returns an error with
// no usable Config, so callers fail fast before binding a listener.
//
// The env layer is applied by hand (rather than via ff's own env support)
// because ff reads the OS environment directly; injecting lookupEnv keeps Load
// pure and testable.
func Load(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	fs := ff.NewFlagSet("copilotd")
	var (
		addr            = fs.StringLong("addr", defaultAddr, "bind address (host:port)")
		logLevel        = fs.StringLong("log-level", defaultLogLevel, "log level: debug|info|warn|error")
		logFormat       = fs.StringLong("log-format", defaultLogFormat, "log format: text|json")
		logFile         = fs.StringLong("log-file", "", "log file path (empty = stderr)")
		shutdownTimeout = fs.DurationLong("shutdown-timeout", defaultShutdownTimeout, "graceful shutdown grace period")
		configPath      = fs.StringLong("config", "", "path to an optional TOML config file")
	)
	// Defined so `--help` lists it and parsing never rejects it; the actual
	// short-circuit happens in main via VersionRequested, before Load runs.
	_ = fs.BoolLong("version", "print build version and exit")

	if err := ff.Parse(fs, args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}
	set := setFlags(fs)

	cfg := Config{
		Addr:            defaultAddr,
		LogLevel:        defaultLogLevel,
		LogFormat:       defaultLogFormat,
		LogFile:         "",
		ShutdownTimeout: defaultShutdownTimeout,
	}

	// file layer (lowest precedence above defaults)
	if path := resolveConfigPath(set, *configPath, lookupEnv); path != "" {
		if err := applyFile(&cfg, path); err != nil {
			return Config{}, err
		}
	}
	// env layer
	if err := applyEnv(&cfg, lookupEnv); err != nil {
		return Config{}, err
	}
	// flag layer (highest precedence)
	if set["addr"] {
		cfg.Addr = *addr
	}
	if set["log-level"] {
		cfg.LogLevel = *logLevel
	}
	if set["log-format"] {
		cfg.LogFormat = *logFormat
	}
	if set["log-file"] {
		cfg.LogFile = *logFile
	}
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = *shutdownTimeout
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
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
func overlay(cfg *Config, source string, get func(key string) (string, bool)) error {
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
	if v, ok := get("shutdown-timeout"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid shutdown-timeout %q from %s: %w", v, source, err)
		}
		cfg.ShutdownTimeout = d
	}
	return nil
}

// applyFile overlays values found in the TOML file onto cfg.
func applyFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file: %w", err)
	}
	defer f.Close()

	values := make(map[string]string)
	if err := fftoml.Parse(f, func(name, value string) error {
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
func applyEnv(cfg *Config, lookupEnv func(string) (string, bool)) error {
	return overlay(cfg, "env", func(key string) (string, bool) {
		return lookupEnv(envVarName(key))
	})
}

// envVarName maps a canonical key to its environment variable, following the
// same convention ff uses: "log-level" -> "COPILOTD_LOG_LEVEL".
func envVarName(key string) string {
	return envPrefix + "_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

func (c Config) validate() error {
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
