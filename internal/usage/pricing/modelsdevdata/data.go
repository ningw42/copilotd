// Package modelsdevdata embeds the audited models.dev license and the
// independently recorded artifact and source identities.
package modelsdevdata

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Record separates the identity of bytes fetched from the live artifact URL
// from the source revision used to audit the schema and license.
type Record struct {
	Artifact struct {
		URL        string `json:"url"`
		ObservedAt string `json:"observed_at"`
		Size       int    `json:"size"`
		SHA256     string `json:"sha256"`
	} `json:"artifact"`
	SourceAudit struct {
		Repository    string `json:"repository"`
		AuditedAt     string `json:"audited_at"`
		Commit        string `json:"commit"`
		SchemaPath    string `json:"schema_path"`
		LicensePath   string `json:"license_path"`
		LicenseSHA256 string `json:"license_sha256"`
	} `json:"source_audit"`
}

//go:embed LICENSE
var license []byte

//go:embed identity.json
var identityJSON []byte

var identity = mustIdentity(identityJSON)

// License returns a copy of the applicable vendored license.
func License() []byte { return append([]byte(nil), license...) }

// Identity returns the fetched artifact and source-audit identities.
func Identity() Record { return identity }

func mustIdentity(raw []byte) Record {
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		panic(fmt.Sprintf("decode embedded models.dev identity: %v", err))
	}
	return record
}
