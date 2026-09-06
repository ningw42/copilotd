//go:build windows

package sqlitestore

import (
	"fmt"
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

	_, err = prepareMainFile(path)
	return err
}
