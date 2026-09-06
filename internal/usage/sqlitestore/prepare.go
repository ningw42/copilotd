package sqlitestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

func prepareMainFile(path string) (fs.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("close pre-created usage database %q: %w", path, closeErr)
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("pre-create usage database %q: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect usage database %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("usage database %q is not a regular file", path)
	}
	return info, nil
}
