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

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close pre-created usage database %q: %w", path, closeErr)
		}
		return validatePrivateMainFile(path)
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("pre-create usage database %q: %w", path, err)
	}
	return validatePrivateMainFile(path)
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

func validatePrivateMainFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect usage database %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("usage database %q is not a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("usage database %q has permissions %04o, want 0600", path, got)
	}
	return nil
}
