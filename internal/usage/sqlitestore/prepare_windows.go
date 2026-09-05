//go:build windows

package sqlitestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Windows creation is best effort. Go FileMode bits neither configure nor prove
// the inherited ACL protecting the main database or SQLite sidecars.
func preparePrivateDatabase(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create usage database parent %q: %w", parent, err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect usage database parent %q: %w", parent, err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return fmt.Errorf("usage database parent %q is not a directory", parent)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close pre-created usage database %q: %w", path, closeErr)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("pre-create usage database %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect usage database %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("usage database %q is not a regular file", path)
	}
	return nil
}
