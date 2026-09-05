package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

type codexReleaseRecord struct {
	Release struct {
		Repository      string `json:"repository"`
		Tag             string `json:"tag"`
		PeeledCommit    string `json:"peeled_commit"`
		AuditDate       string `json:"audit_date"`
		GitHubReleaseID int64  `json:"github_release_id"`
		PublishedAt     string `json:"published_at"`
		TagObject       string `json:"tag_object"`
	} `json:"release"`
	Models struct {
		SourcePath            string `json:"source_path"`
		GitBlob               string `json:"git_blob"`
		SHA256                string `json:"sha256"`
		Size                  int    `json:"size"`
		AuditedBundledDefault string `json:"audited_bundled_default"`
	} `json:"models"`
	Manifest struct {
		AssetName     string `json:"asset_name"`
		GitHubAssetID int64  `json:"github_asset_id"`
		SHA256        string `json:"sha256"`
	} `json:"manifest"`
	ExecutableAudit struct {
		AssetName        string `json:"asset_name"`
		ArchiveSHA256    string `json:"archive_sha256"`
		ExecutableSHA256 string `json:"executable_sha256"`
	} `json:"executable_audit"`
}

//go:embed codexdata/release.json
var embeddedCodexReleaseJSON []byte

var embeddedCodexRelease = mustDecodeCodexReleaseRecord(embeddedCodexReleaseJSON)

func mustDecodeCodexReleaseRecord(recordJSON []byte) codexReleaseRecord {
	var record codexReleaseRecord
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		panic(fmt.Sprintf("decode embedded Codex release record: %v", err))
	}
	// Only the stable tag is a runtime prerequisite. Documentary audit fields
	// and artifact identities are checked at the vendored identity test seam.
	if !isStableCodexReleaseTag(record.Release.Tag) {
		panic(fmt.Sprintf("decode embedded Codex release record: release.tag %q is not stable", record.Release.Tag))
	}
	return record
}
