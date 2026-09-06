//go:build darwin

package config

import (
	"path/filepath"
	"testing"
)

func TestRegisterServeResolvesDarwinUsageDBDefaultAndFallback(t *testing.T) {
	t.Run("user application support directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		cfg, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, "Library", "Application Support", "copilotd", "usage.db")
		if cfg.UsageDBPath != want {
			t.Errorf("resolved UsageDBPath = %q, want %q", cfg.UsageDBPath, want)
		}
	})

	t.Run("unresolved HOME falls back relative", func(t *testing.T) {
		t.Setenv("HOME", "")
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
