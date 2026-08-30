// Package bareversion recognizes the unprefixed semantic-version shape shared
// by configuration validation and impersonation discovery.
package bareversion

import "regexp"

var pattern = regexp.MustCompile(`\A\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\z`)

// Valid reports whether value is major.minor.patch with optional semver-style
// prerelease and build suffixes.
func Valid(value string) bool {
	return pattern.MatchString(value)
}
