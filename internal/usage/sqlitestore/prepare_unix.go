//go:build !windows

package sqlitestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func preparePrivateDatabase(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	switch {
	case err == nil:
		if err := validatePrivateParent(parent, info); err != nil {
			return err
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create usage database parent %q: %w", parent, err)
		}
		info, err = os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("inspect created usage database parent %q: %w", parent, err)
		}
		if err := validatePrivateParent(parent, info); err != nil {
			return err
		}
	default:
		return fmt.Errorf("inspect usage database parent %q: %w", parent, err)
	}

	info, err = prepareMainFile(path)
	if err != nil {
		return err
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("usage database %q has permissions %04o, want 0600", path, got)
	}
	return nil
}

func validatePrivateParent(path string, info fs.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("usage database parent %q is not a regular directory", path)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		return fmt.Errorf("usage database parent %q has permissions %04o, want 0700", path, got)
	}
	return nil
}
