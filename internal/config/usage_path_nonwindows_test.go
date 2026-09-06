//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

func expectedDefaultUsageDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join("copilotd", "usage.db")
	}
	return filepath.Join(dir, "copilotd", "usage.db")
}
