//go:build !windows

package sqliteprobe

import (
	"fmt"
	"os"
)

func validatePrivateParent(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private parent: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("database parent is not a directory: %s", path)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return fmt.Errorf("database parent mode is %04o, want 0700: %s", got, path)
	}
	return nil
}

func validateDatabasePermissions(path string, info os.FileInfo) error {
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("database mode is %04o, want 0600: %s", got, path)
	}
	return nil
}
