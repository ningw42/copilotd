package config

import (
	"os"
	"path/filepath"
	"reflect"
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
