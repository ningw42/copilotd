// Package windowszonesdata owns the pinned CLDR Windows-zone candidate data
// used for representative native Windows report-timezone selection.
package windowszonesdata

//go:generate go run ../../../../scripts/generate-windows-zones

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// IdentityRecord identifies the immutable CLDR source and applicable license.
type IdentityRecord struct {
	Repository string `json:"repository"`
	Release    string `json:"release"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	Tree       string `json:"tree"`
	Source     struct {
		Path    string `json:"path"`
		GitBlob string `json:"git_blob"`
		Size    int    `json:"size"`
		SHA256  string `json:"sha256"`
	} `json:"source"`
	License struct {
		Path    string `json:"path"`
		GitBlob string `json:"git_blob"`
		Size    int    `json:"size"`
		SHA256  string `json:"sha256"`
		SPDX    string `json:"spdx"`
	} `json:"license"`
}

//go:embed identity.json
var identityJSON []byte

//go:embed LICENSE
var license []byte

var identity = mustIdentity(identityJSON)

// Identity returns the pinned source and license identity.
func Identity() IdentityRecord { return identity }

// License returns a copy of the vendored Unicode license.
func License() []byte { return append([]byte(nil), license...) }

// Candidates returns a caller-owned copy of CLDR's ordered candidates.
func Candidates(key, territory string) []string {
	byTerritory := generatedCandidates[key]
	return append([]string(nil), byTerritory[territory]...)
}

func mustIdentity(raw []byte) IdentityRecord {
	var record IdentityRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		panic(fmt.Sprintf("decode embedded CLDR Windows-zone identity: %v", err))
	}
	return record
}
