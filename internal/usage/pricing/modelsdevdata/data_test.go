package modelsdevdata_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/pricing/modelsdevdata"
)

func TestVendoredArtifactHasRecordedIndependentIdentity(t *testing.T) {
	t.Parallel()

	identity := modelsdevdata.Identity()
	if identity.Artifact.URL != "https://models.dev/api.json" ||
		identity.Artifact.ObservedAt != "2026-09-11T11:57:10Z" ||
		identity.Artifact.Size != 4584629 ||
		identity.Artifact.SHA256 != "db685655368231dce789b060e493a21899cb861c9930cc52521795776ab6e3b5" {
		t.Fatalf("artifact identity = %#v, want recorded fetched URL/time/size/SHA", identity.Artifact)
	}
	artifact := modelsdevdata.Artifact()
	if len(artifact) != identity.Artifact.Size {
		t.Fatalf("artifact size = %d, want %d", len(artifact), identity.Artifact.Size)
	}
	sum := sha256.Sum256(artifact)
	if got := hex.EncodeToString(sum[:]); got != identity.Artifact.SHA256 {
		t.Fatalf("artifact SHA-256 = %s, want %s", got, identity.Artifact.SHA256)
	}

	if identity.SourceAudit.Repository != "https://github.com/anomalyco/models.dev" ||
		identity.SourceAudit.AuditedAt != "2026-09-11T11:57:03Z" ||
		identity.SourceAudit.Commit != "b8244af8bd1ca8c0e60103f058854d5510a2534d" ||
		identity.SourceAudit.SchemaPath != "packages/core/src/schema.ts" ||
		identity.SourceAudit.LicensePath != "LICENSE" ||
		identity.SourceAudit.LicenseSHA256 != "dc2dc41c9fea2fd3c41c21c6f484844cab367800b88ff24781f980ad3a9d160a" {
		t.Fatalf("source audit identity = %#v, want separately recorded source/license audit", identity.SourceAudit)
	}
	licenseSum := sha256.Sum256(modelsdevdata.License())
	if got := hex.EncodeToString(licenseSum[:]); got != identity.SourceAudit.LicenseSHA256 {
		t.Fatalf("license SHA-256 = %s, want %s", got, identity.SourceAudit.LicenseSHA256)
	}
}
