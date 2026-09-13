// Package windowszonesdata owns the pinned CLDR Windows-zone candidate data
// used for representative native Windows report-timezone selection.
package windowszonesdata

//go:generate go run ../../../../scripts/generate-windows-zones

// Candidates returns a caller-owned copy of CLDR's ordered candidates.
func Candidates(key, territory string) []string {
	byTerritory := generatedCandidates[key]
	return append([]string(nil), byTerritory[territory]...)
}
