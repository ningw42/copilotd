package identity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- WRITE: atomic, 0600, parent dirs ---------------------------------------

func TestWriteTokenFileMode0600AndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// A nested path proves parent directories are created.
	path := filepath.Join(dir, "nested", "sub", "github-oauth-token")
	const token = "gho_secret_value"

	if err := WriteTokenFile(path, token); err != nil {
		t.Fatalf("WriteTokenFile() error = %v", err)
	}

	// Exact bytes are preserved on disk (write does not trim).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != token {
		t.Errorf("on-disk token = %q, want %q", data, token)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %#o, want 0600", perm)
		}
	}

	// No temp files were left behind in the target directory (atomic rename).
	targetDir := filepath.Dir(path)
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q in %q", e.Name(), targetDir)
		}
	}
}

func TestWriteTokenFileOverwritesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github-oauth-token")
	if err := WriteTokenFile(path, "first"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteTokenFile(path, "second"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "second" {
		t.Errorf("token = %q, want the overwritten value %q", got, "second")
	}
}

// --- READ: trim, whitespace-as-empty, missing, too-open refusal -------------

func TestReadTokenFileTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	if err := WriteTokenFile(path, "  gho_padded\n\t"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "gho_padded" {
		t.Errorf("token = %q, want trimmed %q", got, "gho_padded")
	}
}

func TestReadTokenFileWhitespaceOnlyIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	if err := WriteTokenFile(path, "   \n\t "); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "" {
		t.Errorf("token = %q, want empty (whitespace-only counts as absent)", got)
	}
}

func TestReadTokenFileMissingReportsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := ReadTokenFile(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestReadTokenFileRefusesTooOpenPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit enforcement is Unix-only (best-effort on Windows)")
	}
	path := filepath.Join(t.TempDir(), "tok")
	// Write directly with group/other-readable perms, bypassing WriteTokenFile.
	if err := os.WriteFile(path, []byte("gho_secret"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := ReadTokenFile(path)
	if !errors.Is(err, ErrTokenFileTooOpen) {
		t.Fatalf("error = %v, want ErrTokenFileTooOpen", err)
	}
	if !strings.Contains(err.Error(), "0600") {
		t.Errorf("error %q should mention the expected 0600 mode", err)
	}
}

func TestReadTokenFileAccepts0400(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit enforcement is Unix-only")
	}
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte("gho_secret"), 0o400); err != nil { // stricter than 0600
		t.Fatalf("write: %v", err)
	}
	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "gho_secret" {
		t.Errorf("token = %q, want gho_secret", got)
	}
}

// --- RESOLVE: precedence, empty-as-absent, fail-fast, perms propagation ------

func TestResolveOAuthTokenPrecedence(t *testing.T) {
	fileWith := func(t *testing.T, token string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tok")
		if err := WriteTokenFile(path, token); err != nil {
			t.Fatalf("seed token file: %v", err)
		}
		return path
	}

	t.Run("inline wins over file", func(t *testing.T) {
		got, err := ResolveOAuthToken("inline-token", fileWith(t, "file-token"))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != "inline-token" {
			t.Errorf("token = %q, want inline-token", got)
		}
	})

	t.Run("file used when inline absent", func(t *testing.T) {
		got, err := ResolveOAuthToken("", fileWith(t, "file-token"))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != "file-token" {
			t.Errorf("token = %q, want file-token", got)
		}
	})

	t.Run("whitespace-only inline is absent, file used", func(t *testing.T) {
		got, err := ResolveOAuthToken("   \n", fileWith(t, "file-token"))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != "file-token" {
			t.Errorf("token = %q, want file-token (whitespace inline is absent)", got)
		}
	})

	t.Run("both absent -> fail-fast with login message", func(t *testing.T) {
		_, err := ResolveOAuthToken("", fileWith(t, "   "))
		if !errors.Is(err, ErrNoOAuthToken) {
			t.Fatalf("error = %v, want ErrNoOAuthToken", err)
		}
		if !strings.Contains(err.Error(), "copilotd login") {
			t.Errorf("error %q should tell the operator to run `copilotd login`", err)
		}
	})

	t.Run("missing file and no inline -> fail-fast", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope")
		_, err := ResolveOAuthToken("", missing)
		if !errors.Is(err, ErrNoOAuthToken) {
			t.Errorf("error = %v, want ErrNoOAuthToken for a missing file", err)
		}
	})

	t.Run("empty file path and no inline -> fail-fast", func(t *testing.T) {
		_, err := ResolveOAuthToken("", "")
		if !errors.Is(err, ErrNoOAuthToken) {
			t.Errorf("error = %v, want ErrNoOAuthToken", err)
		}
	})

	t.Run("too-open file propagates, does not fall through to login", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission-bit enforcement is Unix-only")
		}
		path := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(path, []byte("gho_secret"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := ResolveOAuthToken("", path)
		if !errors.Is(err, ErrTokenFileTooOpen) {
			t.Fatalf("error = %v, want ErrTokenFileTooOpen (fail-closed)", err)
		}
		if errors.Is(err, ErrNoOAuthToken) {
			t.Errorf("a too-open file must NOT be reported as absent")
		}
	})
}
