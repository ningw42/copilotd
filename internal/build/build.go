// Package build carries version metadata injected at link time.
//
// The variables default to placeholder values suitable for a bare
// `go build` / `go run`; release builds override them via
// `-ldflags -X github.com/ningw42/copilotd/internal/build.<Var>=…`.
// This package deliberately has no dependencies.
package build

// Build metadata, overridable at link time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String renders the build metadata as "version (commit, date)".
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
