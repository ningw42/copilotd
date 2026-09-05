//go:build windows

package sqlitestore

import (
	"net/url"
	"path/filepath"
	"strings"
)

func sqliteFileURL(path string) *url.URL {
	slash := filepath.ToSlash(path)
	if strings.HasPrefix(slash, "//") {
		hostAndPath := strings.TrimPrefix(slash, "//")
		host, rest, _ := strings.Cut(hostAndPath, "/")
		return &url.URL{Scheme: "file", Host: host, Path: "/" + rest}
	}
	// Absolute drive paths need the leading URI slash: C:/dir/db becomes
	// file:///C:/dir/db, not file://C:/dir/db (where C: would be a host).
	return &url.URL{Scheme: "file", Path: "/" + slash}
}
