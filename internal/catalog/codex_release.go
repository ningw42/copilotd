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
	requiredStrings := []struct {
		name  string
		value string
	}{
		{name: "release.repository", value: record.Release.Repository},
		{name: "release.tag", value: record.Release.Tag},
		{name: "release.peeled_commit", value: record.Release.PeeledCommit},
		{name: "release.audit_date", value: record.Release.AuditDate},
		{name: "release.published_at", value: record.Release.PublishedAt},
		{name: "release.tag_object", value: record.Release.TagObject},
		{name: "models.source_path", value: record.Models.SourcePath},
		{name: "models.git_blob", value: record.Models.GitBlob},
		{name: "models.sha256", value: record.Models.SHA256},
		{name: "models.audited_bundled_default", value: record.Models.AuditedBundledDefault},
		{name: "manifest.asset_name", value: record.Manifest.AssetName},
		{name: "manifest.sha256", value: record.Manifest.SHA256},
		{name: "executable_audit.asset_name", value: record.ExecutableAudit.AssetName},
		{name: "executable_audit.archive_sha256", value: record.ExecutableAudit.ArchiveSHA256},
		{name: "executable_audit.executable_sha256", value: record.ExecutableAudit.ExecutableSHA256},
	}
	for _, field := range requiredStrings {
		if field.value == "" {
			panic(fmt.Sprintf("decode embedded Codex release record: %s is empty", field.name))
		}
	}
	if !isStableCodexReleaseTag(record.Release.Tag) {
		panic(fmt.Sprintf("decode embedded Codex release record: release.tag %q is not stable", record.Release.Tag))
	}
	if !isGitCommitSHA(record.Release.PeeledCommit) {
		panic(fmt.Sprintf("decode embedded Codex release record: release.peeled_commit %q is invalid", record.Release.PeeledCommit))
	}
	if record.Release.GitHubReleaseID <= 0 || record.Manifest.GitHubAssetID <= 0 || record.Models.Size <= 0 {
		panic("decode embedded Codex release record: numeric identity is not positive")
	}
	return record
}
