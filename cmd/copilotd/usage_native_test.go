package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Public OS process/filesystem prerequisites for native verification. No test
// infers native execution from GOOS/GOARCH cross-compilation settings alone.
func TestUsageNativeRuntime(t *testing.T) {
	t.Logf("test process runtime: GOOS=%s GOARCH=%s version=%s", runtime.GOOS, runtime.GOARCH, runtime.Version())
	if want := os.Getenv("COPILOTD_TEST_GO_VERSION"); want != "" && runtime.Version() != want {
		t.Fatalf("test process toolchain %s, want %s", runtime.Version(), want)
	}
	if want := os.Getenv("COPILOTD_TEST_NATIVE_TARGET"); want != "" {
		if got := runtime.GOOS + "/" + runtime.GOARCH; got != want {
			t.Fatalf("test process is %s, want native %s", got, want)
		}
		// The same os.Symlink API used by discovery and path-policy fixtures must
		// work for both files and directories. Windows privilege failure is a
		// certification blocker, never permission to count skips as coverage.
		root := t.TempDir()
		for _, directory := range []bool{false, true} {
			target := filepath.Join(root, "file")
			if directory {
				target = filepath.Join(root, "directory")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			link := target + "-link"
			if err := os.Symlink(target, link); err != nil {
				t.Fatalf("native symlink preflight directory=%t: %v", directory, err)
			}
			info, err := os.Stat(link)
			if err != nil || info.IsDir() != directory {
				t.Fatalf("symlink target: %v", err)
			}
		}
		t.Log("native file and directory os.Symlink preflight passed")
	}
}
