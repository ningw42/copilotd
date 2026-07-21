package impersonation

import "regexp"

var bareVersionPattern = regexp.MustCompile(`\A\d+\.\d+\.\d+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\z`)

// IsBareVersion reports whether value is a major.minor.patch version with
// optional semver-style prerelease and build suffixes.
func IsBareVersion(value string) bool {
	return bareVersionPattern.MatchString(value)
}
