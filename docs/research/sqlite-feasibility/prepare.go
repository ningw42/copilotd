// Package sqliteprobe is a disposable feasibility probe for copilotd issue #195.
// It is not a production persistence package.
package sqliteprobe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PreparePrivateDatabase creates path without truncation or validates the
// existing regular file. The containing directory is created private when
// missing and refused when its platform-specific safety checks fail.
func PreparePrivateDatabase(path string) (created bool, err error) {
	if path == "" {
		return false, errors.New("database path is empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, fmt.Errorf("create private parent: %w", err)
	}
	if err := validatePrivateParent(parent); err != nil {
		return false, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return true, fmt.Errorf("close new database file: %w", closeErr)
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("create database exclusively: %w", err)
	}
	if err := validateExistingDatabase(path); err != nil {
		return false, err
	}
	return false, nil
}

func validateExistingDatabase(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("existing database is not a regular file: %s", path)
	}
	if err := validateDatabasePermissions(path, info); err != nil {
		return err
	}
	return nil
}
