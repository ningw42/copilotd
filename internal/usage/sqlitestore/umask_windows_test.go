//go:build windows

package sqlitestore_test

func setUmask(int) int { return 0 }
