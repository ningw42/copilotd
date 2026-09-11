package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/pricing/modelsdevdata"
)

func TestEmbeddedArtifactMatchesRecordedIdentity(t *testing.T) {
	t.Parallel()

	identity := modelsdevdata.Identity().Artifact
	if len(embeddedArtifact) != identity.Size {
		t.Fatalf("artifact size = %d, want %d", len(embeddedArtifact), identity.Size)
	}
	sum := sha256.Sum256(embeddedArtifact)
	if got := hex.EncodeToString(sum[:]); got != identity.SHA256 {
		t.Fatalf("artifact SHA-256 = %s, want %s", got, identity.SHA256)
	}
}
