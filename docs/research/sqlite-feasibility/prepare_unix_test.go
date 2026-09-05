//go:build !windows

package sqliteprobe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestPreparePrivateDatabaseRefusesUnsafeParentAndDestination(t *testing.T) {
	t.Run("shared parent", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatalf("make parent unsafe: %v", err)
		}
		_, err := PreparePrivateDatabase(filepath.Join(parent, "usage.db"))
		if err == nil || !strings.Contains(err.Error(), "parent mode") {
			t.Fatalf("prepare error = %v, want unsafe parent refusal", err)
		}
		info, statErr := os.Stat(parent)
		if statErr != nil {
			t.Fatalf("stat refused parent: %v", statErr)
		}
		if got := info.Mode().Perm(); got != 0o777 {
			t.Errorf("refused parent was chmodded to %04o, want unchanged 0777", got)
		}
	})

	t.Run("nonregular destination", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		target := filepath.Join(parent, "target")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatalf("write symlink target: %v", err)
		}
		path := filepath.Join(parent, "usage.db")
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		_, err := PreparePrivateDatabase(path)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("prepare error = %v, want nonregular destination refusal", err)
		}
		contents, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("read symlink target: %v", readErr)
		}
		if string(contents) != "preserve" {
			t.Errorf("symlink target = %q, want preserved", contents)
		}
	})

	t.Run("unsafe existing file mode", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatalf("create parent: %v", err)
		}
		path := filepath.Join(parent, "usage.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write database: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("make database unsafe: %v", err)
		}
		_, err := PreparePrivateDatabase(path)
		if err == nil || !strings.Contains(err.Error(), "database mode") {
			t.Fatalf("prepare error = %v, want unsafe database refusal", err)
		}
	})
}

func TestPreparePrivateDatabaseConcurrentCreatorsUseExclusiveCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	const creators = 16
	start := make(chan struct{})
	results := make(chan bool, creators)
	errors := make(chan error, creators)
	var wait sync.WaitGroup
	for range creators {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			created, err := PreparePrivateDatabase(path)
			if err != nil {
				errors <- err
				return
			}
			results <- created
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("concurrent prepare: %v", err)
	}
	var createdCount, resultCount int
	for created := range results {
		resultCount++
		if created {
			createdCount++
		}
	}
	if resultCount != creators {
		t.Fatalf("successful creators = %d, want %d", resultCount, creators)
	}
	if createdCount != 1 {
		t.Errorf("new-file winners = %d, want exactly 1", createdCount)
	}
}

func TestWALSidecarsStayInsidePrivateParentUnderPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	admission, err := Admit(context.Background(), path, AdmissionOptions{
		Migrations: []string{`CREATE TABLE evidence(value TEXT NOT NULL)`},
	})
	if err != nil {
		t.Fatalf("admit WAL database: %v", err)
	}
	defer admission.Close()
	if _, err := admission.Conn.ExecContext(context.Background(), `INSERT INTO evidence(value) VALUES ('sidecars')`); err != nil {
		t.Fatalf("write WAL row: %v", err)
	}

	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(candidate), err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s is not regular: %s", filepath.Base(candidate), info.Mode())
		}
		t.Logf("observed Linux mode %s=%04o (privacy contract is parent 0700)", filepath.Base(candidate), info.Mode().Perm())
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat private parent: %v", err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("private parent mode = %04o, want 0700", got)
	}
}

func TestPreparePrivateDatabaseCreatesPrivateParentAndFileUnderPermissiveUmask(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	path := filepath.Join(t.TempDir(), "private", "usage.db")
	created, err := PreparePrivateDatabase(path)
	if err != nil {
		t.Fatalf("prepare private database: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}

	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("parent mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("database mode = %04o, want 0600", got)
	}
}
