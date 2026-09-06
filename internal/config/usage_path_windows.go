//go:build windows

package config

import (
	"os"
	"path/filepath"
)

func defaultUsageDBPath() string {
	// os.UserConfigDir returns roaming AppData on Windows. Usage databases must
	// default to the local application-data root instead.
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		return filepath.Join("copilotd", "usage.db")
	}
	return filepath.Join(dir, "copilotd", "usage.db")
}
