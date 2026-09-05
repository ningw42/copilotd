//go:build !windows && !darwin

package config

import (
	"path/filepath"
	"testing"
)

func TestRegisterServeResolvesXDGUsageDBDefaultAndFallback(t *testing.T) {
	t.Run("user config directory", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "xdg-config")
		t.Setenv("XDG_CONFIG_HOME", base)
		cfg, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(base, "copilotd", "usage.db")
		if cfg.UsageDBPath != want {
			t.Errorf("resolved UsageDBPath = %q, want %q", cfg.UsageDBPath, want)
		}
	})

	t.Run("unresolved base falls back relative", func(t *testing.T) {
		// os.UserConfigDir rejects a non-absolute XDG_CONFIG_HOME. Resolve must
		// still return a usable relative setting without touching the filesystem.
		t.Setenv("XDG_CONFIG_HOME", "relative-config-home")
		cfg, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join("copilotd", "usage.db")
		if cfg.UsageDBPath != want {
			t.Errorf("fallback UsageDBPath = %q, want %q", cfg.UsageDBPath, want)
		}
	})
}
