package identity

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNoOAuthToken is returned by ResolveOAuthToken when no source yields a
// non-empty GitHub OAuth token. Its message tells the operator to run
// `copilotd login`; callers use it to fail fast before binding a listener.
var ErrNoOAuthToken = errors.New("no GitHub OAuth token found: set --github-oauth-token / COPILOTD_GITHUB_OAUTH_TOKEN, or run `copilotd login` to obtain one")

// ErrTokenFileTooOpen is returned by ReadTokenFile when the token file's Unix
// permission bits are broader than 0600 (any group/other bit set). Reading a
// secret from a world- or group-accessible file is refused (fail-closed,
// ssh-style) rather than silently accepted.
var ErrTokenFileTooOpen = errors.New("token file permissions too open")

// tokenFileTempPattern names the temp file used for the atomic write. The dot
// prefix keeps it inconspicuous; CreateTemp fills in the random middle.
const tokenFileTempPattern = ".oauth-token-*.tmp"

// WriteTokenFile writes the raw GitHub OAuth token to path atomically with owner-
// only (0600) permissions, creating parent directories (0700) as needed. It
// writes a temp file in the same directory, fsync-free, then renames it over the
// target so a reader never observes a partially written secret. The token bytes
// are written verbatim (no trimming); trimming is a read-side concern.
//
// This is the single write path shared by `copilotd login` (#13) and tests.
//
// On Windows the 0600 mode maps only to the read-only bit (Go's os.Chmod
// limitation); enforcement there is best-effort, mirroring the read-side caveat.
func WriteTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token file directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, tokenFileTempPattern)
	if err != nil {
		return fmt.Errorf("create temp token file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	renamed := false
	// Clean up the temp file on every error path; a no-op once renamed away.
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	// CreateTemp already makes the file 0600 on Unix, but set it explicitly so the
	// guarantee does not depend on the platform default.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set token file permissions: %w", err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace token file %q: %w", path, err)
	}
	renamed = true
	return nil
}

// ReadTokenFile reads the raw GitHub OAuth token from path and returns it with
// surrounding whitespace trimmed; a whitespace-only file yields "" (treated as
// absent by ResolveOAuthToken). A missing file returns an error satisfying
// errors.Is(err, fs.ErrNotExist), so callers can distinguish absent from broken.
//
// On Unix, if the file's permission bits are broader than 0600 (any group/other
// bit set) it refuses with ErrTokenFileTooOpen — fail-closed for a secret. On
// Windows the bit check is skipped (best-effort; Go cannot represent Unix modes
// there), a deliberate documented caveat.
func ReadTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err // includes fs.ErrNotExist; the caller decides what to do
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return "", fmt.Errorf("%w: %q has mode %#o, want 0600 or stricter (fix: chmod 600 %q)",
				ErrTokenFileTooOpen, path, perm, path)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ResolveOAuthToken applies the source precedence for the GitHub OAuth token
// (§6.5): the inline value wins over the token file. A present-but-empty
// (whitespace-only) inline value OR token file counts as absent. If neither
// yields a non-empty token it returns ErrNoOAuthToken (fail-fast, run
// `copilotd login`). A token file that exists but is refused for too-open
// permissions propagates that error rather than falling through to
// ErrNoOAuthToken — a broken secret is not the same as a missing one.
//
// This is a purely local read; it performs no network call.
func ResolveOAuthToken(inline, tokenFilePath string) (string, error) {
	if t := strings.TrimSpace(inline); t != "" {
		return t, nil
	}
	if tokenFilePath != "" {
		tok, err := ReadTokenFile(tokenFilePath)
		switch {
		case err == nil:
			if tok != "" {
				return tok, nil
			}
			// Present but empty: fall through to the no-source error.
		case errors.Is(err, fs.ErrNotExist):
			// Missing file: fall through to the no-source error.
		default:
			// Too-open perms or an unreadable file: fail closed, do not fall through.
			return "", err
		}
	}
	return "", ErrNoOAuthToken
}
