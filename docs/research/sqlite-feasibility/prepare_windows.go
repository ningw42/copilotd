//go:build windows

package sqliteprobe

import "os"

// Go's Unix-like mode bits do not establish or prove a Windows ACL. This probe
// can only perform best-effort exclusive creation and regular-file validation.
func validatePrivateParent(string) error { return nil }

func validateDatabasePermissions(string, os.FileInfo) error { return nil }
