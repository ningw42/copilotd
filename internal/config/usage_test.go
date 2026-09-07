package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peterbourgon/ff/v4"
)

func loadUsage(args []string, env map[string]string) (UsageConfig, error) {
	fs := ff.NewFlagSet("usage")
	flags := RegisterUsage(fs)
	if err := ff.Parse(fs, args); err != nil {
		return UsageConfig{}, err
	}
	return flags.Resolve(envFunc(env))
}
func TestUsageTimezonePresencePrecedenceAndEagerParsing(t *testing.T) {
	for _, tc := range []struct {
		name, toml    string
		flags         []string
		env           map[string]string
		want          *string
		errorContains string
	}{
		{name: "omission remains nil even with OS TZ", env: map[string]string{"TZ": "Europe/Berlin"}},
		{name: "file timezone", toml: "timezone = 'Europe/Berlin'", want: timezoneValue("Europe/Berlin")},
		{name: "environment over file", toml: "timezone = 'Europe/Berlin'", env: map[string]string{"COPILOTD_TIMEZONE": "US/Eastern"}, want: timezoneValue("US/Eastern")},
		{name: "flag over environment and file", toml: "timezone = 'Europe/Berlin'", env: map[string]string{"COPILOTD_TIMEZONE": "US/Eastern"}, flags: []string{"--timezone", "UTC"}, want: timezoneValue("UTC")},
		{name: "empty file is present", toml: "timezone = ''", errorContains: "timezone must not be explicitly empty"},
		{name: "empty environment overrides file", toml: "timezone = 'Europe/Berlin'", env: map[string]string{"COPILOTD_TIMEZONE": ""}, errorContains: "timezone must not be explicitly empty"},
		{name: "empty flag overrides environment", env: map[string]string{"COPILOTD_TIMEZONE": "UTC"}, flags: []string{"--timezone", ""}, errorContains: "timezone must not be explicitly empty"},
		// Empty strings parse normally; the engine validates the resolved value.
		{name: "valid flag replaces parsed empty lower values", toml: "timezone = ''", env: map[string]string{"COPILOTD_TIMEZONE": ""}, flags: []string{"--timezone", "Etc/UTC"}, want: timezoneValue("Etc/UTC")},
		{name: "malformed TOML cannot be masked", toml: "timezone = '", flags: []string{"--timezone", "UTC"}, errorContains: "config"},
		{name: "malformed lower file duration", toml: "timeout = 'bad'", flags: []string{"--timeout", "2s", "--timezone", "UTC"}, errorContains: "from config file"},
		{name: "malformed lower environment duration", env: map[string]string{"COPILOTD_TIMEOUT": "bad"}, flags: []string{"--timeout", "2s", "--timezone", "UTC"}, errorContains: "from env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, tc.flags...)
			if tc.toml != "" {
				path := filepath.Join(t.TempDir(), "usage.toml")
				if err := os.WriteFile(path, []byte(tc.toml), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--config", path)
			}
			got, err := loadUsage(args, tc.env)
			if tc.errorContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errorContains) {
					t.Fatalf("error=%v, want %s", err, tc.errorContains)
				}
			} else if err != nil || !reflect.DeepEqual(got.Timezone, tc.want) {
				t.Fatalf("timezone=%v error=%v, want %v", got.Timezone, err, tc.want)
			}
		})
	}
}
func timezoneValue(value string) *string { return &value }

func TestUsageConfigurationIsIsolatedAndPresenceAware(t *testing.T) {
	got, err := loadUsage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := UsageConfig{Endpoint: "http://127.0.0.1:8080", Period: "day", Surface: "all", Timeout: 15 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults: %+v", got)
	}
	for _, flag := range []string{"--apikey", "--usage-db-path", "--github-oauth-token", "--log-level", "--addr"} {
		if _, err := loadUsage([]string{flag, "x"}, nil); err == nil {
			t.Errorf("accepted unrelated flag %s", flag)
		}
	}
	path := filepath.Join(t.TempDir(), "shared.toml")
	if err := os.WriteFile(path, []byte("timezone = 'Europe/Berlin'\nmodel = ' file '\nperiod = 'month'\napikey = 'irrelevant'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = loadUsage([]string{"--config", path, "--timezone", "UTC", "--period", "day"}, map[string]string{"COPILOTD_TIMEZONE": "US/Eastern", "COPILOTD_MODEL": " env "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone == nil || *got.Timezone != "UTC" || got.Model == nil || *got.Model != " env " || got.Period != "day" {
		t.Fatalf("precedence/presence: %+v", got)
	}
	for _, key := range []string{"timezone", "model"} {
		if _, err := loadUsage([]string{"--" + key, ""}, nil); err == nil {
			t.Errorf("explicit empty %s accepted", key)
		}
	}
	if _, err := loadUsage([]string{"--timeout", "2s"}, map[string]string{"COPILOTD_TIMEOUT": "invalid"}); err == nil {
		t.Fatal("malformed lower duration overlay masked")
	}
}
