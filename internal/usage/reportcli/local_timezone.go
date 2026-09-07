package reportcli

import (
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
)

// Platform, environment, and filesystem are process-local external dependencies.
// Fixtures stay private; Run's public interface exposes no resolver controls.
type localTimezoneSystem struct {
	goos      string
	lookupEnv func(string) (string, bool)
	readlink  func(string) (string, error)
	lstat     func(string) (os.FileInfo, error)
	open      func(string) (*os.File, error)
}

func processTimezoneSystem() *localTimezoneSystem {
	return &localTimezoneSystem{goos: runtime.GOOS, lookupEnv: os.LookupEnv, readlink: os.Readlink, lstat: os.Lstat, open: os.Open}
}

// Reasons are bounded, code-owned text, never OS errors or configuration bytes.
func localTimezoneError(reason string) error {
	return fmt.Errorf("cannot determine a named local timezone (%s); pass --timezone Area/City (or --timezone UTC)", reason)
}

func (s *localTimezoneSystem) discover() (string, error) {
	if s.goos == "windows" {
		return "", localTimezoneError("native Windows requires an explicit timezone")
	}
	tzdir, _ := s.lookupEnv("TZDIR")
	zoneinfo, _ := s.lookupEnv("ZONEINFO")
	if tzdir != "" || zoneinfo != "" {
		return "", localTimezoneError("custom TZDIR or ZONEINFO")
	}
	name, present := s.lookupEnv("TZ")
	if !present {
		return s.fileTimezone("/etc/localtime")
	}
	if name == "" {
		return "UTC", nil
	}
	name = strings.TrimPrefix(name, ":")
	if strings.HasPrefix(name, "/") {
		return s.fileTimezone(name)
	}
	if strings.HasPrefix(name, "right/") || strings.HasPrefix(name, "posix/") {
		return "", localTimezoneError("unsupported right/posix zoneinfo subtree")
	}
	if _, err := report.LoadTimezone(name); err != nil {
		return "", localTimezoneError("invalid or unsupported TZ")
	}
	return name, nil
}

const (
	maxTimezoneSymlinks  = 40
	maxTimezoneFileBytes = 1 << 20
)

type timezoneObservation struct {
	info os.FileInfo
	link string
}
type timezoneWalk struct {
	system       *localTimezoneSystem
	observations map[string]timezoneObservation
	unverifiable bool
}

func sameTimezoneFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// Recheck the path evidence, not just the final filename: a changed system link
// or root alias must not label a different configuration with the old name.
func (w *timezoneWalk) consistent() bool {
	if w.unverifiable {
		return false
	}
	for name, previous := range w.observations {
		info, err := w.system.lstat(name)
		if previous.info == nil && os.IsNotExist(err) {
			continue
		}
		if err != nil || !sameTimezoneFile(previous.info, info) {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := w.system.readlink(name)
			if err != nil || link != previous.link {
				return false
			}
		}
	}
	return true
}

// Walk each component ourselves: letting the OS follow directory links, or
// EvalSymlinks' independent bound, would bypass our report discovery hop limit.
func (w *timezoneWalk) walk(name string) (string, []string, error) {
	paths := []string{name}
	pending := strings.Split(name, "/")
	resolved := "/"
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = path.Dir(resolved)
			continue
		}
		next := path.Join(resolved, part)
		info, err := w.system.lstat(next)
		if err != nil {
			if previous, ok := w.observations[next]; ok && previous.info != nil {
				w.unverifiable = true
			}
			if os.IsNotExist(err) {
				w.observations[next] = timezoneObservation{}
			} else {
				w.unverifiable = true
			}
			return "", nil, localTimezoneError("missing or unreadable timezone path")
		}
		observation := timezoneObservation{info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			observation.link, err = w.system.readlink(next)
			if err != nil {
				w.unverifiable = true
				return "", nil, localTimezoneError("unreadable timezone symlink")
			}
		}
		if previous, ok := w.observations[next]; ok && (!sameTimezoneFile(previous.info, info) || previous.link != observation.link) {
			w.unverifiable = true
			return "", nil, localTimezoneError("timezone path changed during discovery")
		}
		w.observations[next] = observation
		if info.Mode()&os.ModeSymlink != 0 {
			// Capture the name before following this link, now that preceding
			// relative components are resolved. Otherwise a ../ prefix could
			// hide a named alias and silently turn it into its target's name.
			paths = append(paths, strings.TrimSuffix(next+"/"+strings.Join(pending, "/"), "/"))
			hops++
			if hops > maxTimezoneSymlinks {
				w.unverifiable = true
				return "", nil, localTimezoneError("symlink loop or more than 40 links")
			}
			target := observation.link
			if strings.HasPrefix(target, "/") {
				resolved = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
			paths = append(paths, strings.TrimSuffix(resolved, "/")+"/"+strings.TrimLeft(strings.Join(pending, "/"), "/"))
			continue
		}
		if len(pending) > 0 && !info.IsDir() {
			w.unverifiable = true
			return "", nil, localTimezoneError("non-directory timezone path")
		}
		resolved = next
	}
	return resolved, append(paths, resolved), nil
}

func (s *localTimezoneSystem) fileTimezone(filename string) (string, error) {
	walk := timezoneWalk{system: s, observations: map[string]timezoneObservation{}}
	target, paths, err := walk.walk(filename)
	if err != nil {
		return "", err
	}
	roots := []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo"}
	if s.goos == "darwin" {
		roots = []string{"/var/db/timezone/zoneinfo", "/usr/share/zoneinfo"}
		// Apple's documented prefix ends in zoneinfo, including versioned
		// layouts. This heuristic is intentionally never used on Linux.
		for _, observed := range paths {
			if index := strings.LastIndex(observed, "/zoneinfo/"); index >= 0 {
				roots = append(roots, observed[:index+len("/zoneinfo")])
			}
		}
	}
	for _, root := range roots {
		if resolved, _, err := walk.walk(root); err == nil {
			roots = append(roots, resolved)
		}
	}
	candidates := map[string]bool{}
	for _, observed := range paths {
		for _, root := range roots {
			if name, found := strings.CutPrefix(observed, root+"/"); found {
				if strings.HasPrefix(name, "right/") || strings.HasPrefix(name, "posix/") {
					return "", localTimezoneError("unsupported right/posix zoneinfo subtree")
				}
				if _, err := report.LoadTimezone(name); err == nil {
					candidates[name] = true
				}
			}
		}
	}
	if len(candidates) != 1 {
		return "", localTimezoneError("unidentifiable or ambiguous zone file")
	}
	var name string
	for candidate := range candidates {
		name = candidate
	}
	observed := walk.observations[target].info
	if observed == nil || !observed.Mode().IsRegular() {
		return "", localTimezoneError("not a regular TZif file")
	}
	file, err := s.open(target)
	if err != nil {
		return "", localTimezoneError("unreadable zone file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameTimezoneFile(observed, opened) {
		return "", localTimezoneError("zone file changed during discovery")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTimezoneFileBytes+1))
	if err != nil || len(data) > maxTimezoneFileBytes {
		return "", localTimezoneError("unreadable or oversized TZif file")
	}
	// This only verifies TZif evidence. These rules are not used to aggregate,
	// reverse-match an identity, or override the daemon's calendar rules.
	if _, err := time.LoadLocationFromTZData(name, data); err != nil {
		return "", localTimezoneError("not a readable TZif file")
	}
	finished, err := file.Stat()
	if err != nil || !sameTimezoneFile(opened, finished) || !walk.consistent() {
		return "", localTimezoneError("timezone paths changed or could not be verified")
	}
	return name, nil
}
