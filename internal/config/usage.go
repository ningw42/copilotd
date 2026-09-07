package config

import (
	"time"

	"github.com/peterbourgon/ff/v4"
)

// UsageConfig contains only one-shot report client settings, never daemon
// credentials or database paths. Nil optional values preserve omission.
type UsageConfig struct {
	Endpoint string
	Period   string
	Since    string
	Until    string
	Timezone *string
	Surface  string
	Model    *string
	Details  bool
	JSON     bool
	Timeout  time.Duration
}
type UsageFlags struct {
	fs         *ff.FlagSet
	specs      []spec[UsageConfig]
	configPath *configPathField[UsageConfig]
}

func RegisterUsage(fs *ff.FlagSet) *UsageFlags {
	path := &configPathField[UsageConfig]{}
	specs := []spec[UsageConfig]{
		stringField("endpoint", "http://127.0.0.1:8080", func(c *UsageConfig) *string { return &c.Endpoint }, required, "daemon HTTP(S) base URL (path prefix preserved)"),
		stringField("period", "day", func(c *UsageConfig) *string { return &c.Period }, oneOf([]string{"day", "week", "month", "year"}), "calendar grouping: day, week, month, year (does not change range)"),
		stringField("since", "", func(c *UsageConfig) *string { return &c.Since }, nil, "inclusive YYYY-MM-DD (omitted: current month's first day in requested zone)"),
		stringField("until", "", func(c *UsageConfig) *string { return &c.Until }, nil, "exclusive YYYY-MM-DD (omitted: next month's first day in requested zone)"),
		optionalStringField("timezone", func(c *UsageConfig) **string { return &c.Timezone }, "named Area/City or UTC (omitted: terminal-local on supported Unix; native Windows requires explicit)"),
		stringField("surface", "all", func(c *UsageConfig) *string { return &c.Surface }, oneOf([]string{"all", "anthropic", "openai"}), "native Surface selection: all, anthropic, openai"),
		optionalStringField("model", func(c *UsageConfig) **string { return &c.Model }, "exact Reported model (not yet supported)"),
		boolField("details", false, func(c *UsageConfig) *bool { return &c.Details }, "secondary native tables (not yet supported)"),
		boolField("json", false, func(c *UsageConfig) *bool { return &c.JSON }, "validated JSON output (not yet supported)"),
		durationField("timeout", 15*time.Second, inSeconds, func(c *UsageConfig) *time.Duration { return &c.Timeout }, positive, "overall HTTP request/read timeout"),
		path,
	}
	for _, s := range specs {
		s.register(fs)
	}
	return &UsageFlags{fs: fs, specs: specs, configPath: path}
}
func (f *UsageFlags) Resolve(lookupEnv func(string) (string, bool)) (UsageConfig, error) {
	path := resolveConfigPath(setFlags(f.fs), f.configPath.flagValue(), lookupEnv)
	var cfg UsageConfig
	if err := resolve(f.specs, f.fs, &cfg, path, lookupEnv); err != nil {
		return UsageConfig{}, err
	}
	return cfg, nil
}
