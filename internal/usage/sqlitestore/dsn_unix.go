//go:build !windows

package sqlitestore

import (
	"net/url"
	"path/filepath"
)

func sqliteFileURL(path string) *url.URL {
	return &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
}
