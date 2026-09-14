package modelsdevdata_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/pricing/modelsdevdata"
)

func TestManifestHasRecordedIndependentIdentities(t *testing.T) {
	t.Parallel()

	identity := modelsdevdata.Identity()
	if identity.Artifact.URL != "https://models.dev/api.json" ||
		identity.Artifact.Size <= 0 {
		t.Fatalf("artifact identity = %#v, want models.dev URL and positive byte size", identity.Artifact)
	}
	if identity.SourceAudit.Repository != "https://github.com/anomalyco/models.dev" ||
		identity.SourceAudit.SchemaPath != "packages/core/src/schema.ts" ||
		identity.SourceAudit.LicensePath != "LICENSE" {
		t.Fatalf("source audit identity = %#v, want models.dev repository and schema/license paths", identity.SourceAudit)
	}
	for _, field := range []struct {
		name, value string
	}{
		{"artifact.observed_at", identity.Artifact.ObservedAt},
		{"source_audit.audited_at", identity.SourceAudit.AuditedAt},
	} {
		if _, err := time.Parse(time.RFC3339, field.value); err != nil {
			t.Errorf("%s = %q, want RFC3339 observation time: %v", field.name, field.value, err)
		}
	}
	for _, field := range []struct {
		name, value string
		bytes       int
	}{
		{"artifact.sha256", identity.Artifact.SHA256, sha256.Size},
		{"source_audit.commit", identity.SourceAudit.Commit, 20},
		{"source_audit.license_sha256", identity.SourceAudit.LicenseSHA256, sha256.Size},
	} {
		decoded, err := hex.DecodeString(field.value)
		if err != nil || len(decoded) != field.bytes {
			t.Errorf("%s = %q, want %d-byte hex identity", field.name, field.value, field.bytes)
		}
	}
	licenseSum := sha256.Sum256(modelsdevdata.License())
	if got := hex.EncodeToString(licenseSum[:]); got != identity.SourceAudit.LicenseSHA256 {
		t.Fatalf("license SHA-256 = %s, want %s", got, identity.SourceAudit.LicenseSHA256)
	}
}
