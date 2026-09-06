//go:build !windows

package sqlitestore_test

import "syscall"

func setUmask(mask int) int { return syscall.Umask(mask) }
