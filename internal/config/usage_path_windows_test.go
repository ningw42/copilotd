//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func expectedDefaultUsageDBPath() string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "copilotd", "usage.db")
	}
	return filepath.Join("copilotd", "usage.db")
}

func TestRegisterServeResolvesWindowsLocalUsageDBDefaultAndFallback(t *testing.T) {
	t.Run("LOCALAPPDATA wins over roaming APPDATA", func(t *testing.T) {
		local := filepath.Join(t.TempDir(), "Local")
		roaming := filepath.Join(t.TempDir(), "Roaming")
		t.Setenv("LOCALAPPDATA", local)
		t.Setenv("APPDATA", roaming)
		cfg, err := loadServe([]string{"--apikey", testAPIKey}, noEnv())
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(local, "copilotd", "usage.db")
		if cfg.UsageDBPath != want {
			t.Errorf("resolved UsageDBPath = %q, want local %q (not roaming %q)", cfg.UsageDBPath, want, roaming)
		}
	})

	t.Run("missing LOCALAPPDATA falls back relative", func(t *testing.T) {
		t.Setenv("LOCALAPPDATA", "")
		t.Setenv("APPDATA", filepath.Join(t.TempDir(), "Roaming"))
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
