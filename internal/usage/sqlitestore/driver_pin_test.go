package sqlitestore

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The modernc.org/sqlite driver is transpiled against one specific
// modernc.org/libc and asks importers to select exactly the version its own
// go.mod requires ("Fragile modernc.org/libc dependency" in the driver's
// package documentation). A blanket `go get -u all` raises libc past that
// version without failing the build, so these literals are an upgrade-sensitive
// review gate: bump the driver with `go get modernc.org/sqlite@<version>`, then
// update both together.
const (
	pinnedDriverVersion = "v1.62.0"
	requiredLibcVersion = "v1.80.1"
)

func TestLibcMatchesTheVersionTheDriverRequires(t *testing.T) {
	root, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Fatalf("read root go.mod: %v", err)
	}
	if got := requiredVersion(t, root, "modernc.org/sqlite"); got != pinnedDriverVersion {
		t.Errorf("root go.mod selects modernc.org/sqlite %s, want %s; after a driver bump, set requiredLibcVersion to the modernc.org/libc version in the driver's own go.mod", got, pinnedDriverVersion)
	}
	if got := requiredVersion(t, root, "modernc.org/libc"); got != requiredLibcVersion {
		t.Errorf("root go.mod selects modernc.org/libc %s, want %s, the version modernc.org/sqlite %s requires", got, requiredLibcVersion, pinnedDriverVersion)
	}

	// The literals above are only as good as their last review. Where the
	// driver's own go.mod is readable, check them against it. A vendored build
	// has no module go.mod files, so there the literals stand alone.
	driver, ok := driverGoMod(t)
	if !ok {
		return
	}
	if got := requiredVersion(t, driver, "modernc.org/libc"); got != requiredLibcVersion {
		t.Errorf("modernc.org/sqlite %s requires modernc.org/libc %s, but requiredLibcVersion is %s", pinnedDriverVersion, got, requiredLibcVersion)
	}
}

// requiredVersion returns the version a go.mod requires for module. It reads
// only the plain `module version` requirement form both files use.
func requiredVersion(t *testing.T, gomod []byte, module string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^\s*(?:require\s+)?` + regexp.QuoteMeta(module) + `\s+(v\S+)`)
	match := pattern.FindSubmatch(gomod)
	if match == nil {
		t.Fatalf("go.mod has no requirement on %s", module)
	}
	return string(match[1])
}

// driverGoMod reads the selected driver's own go.mod from the module cache. It
// reports false when the go command or that file is unavailable.
func driverGoMod(t *testing.T) ([]byte, bool) {
	t.Helper()
	goCommand, err := exec.LookPath("go")
	if err != nil {
		t.Logf("driver go.mod not checked: %v", err)
		return nil, false
	}
	output, err := exec.Command(goCommand, "list", "-m", "-f", "{{.GoMod}}", "modernc.org/sqlite").Output()
	path := strings.TrimSpace(string(output))
	if err != nil || path == "" {
		t.Logf("driver go.mod not checked: module cache location unavailable (vendored build?): %v", err)
		return nil, false
	}
	driver, err := os.ReadFile(path)
	if err != nil {
		t.Logf("driver go.mod not checked: %v", err)
		return nil, false
	}
	return driver, true
}
