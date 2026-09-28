package report

import (
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestPricingProvenanceNormalizesCachedSuccessToUTCWithoutMutation(t *testing.T) {
	captured := time.Date(2026, 9, 7, 12, 34, 56, 789, time.FixedZone("report-daemon", 9*60*60))
	status := pricing.SnapshotStatus{
		Source:      "fetched",
		Version:     "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
		LastSuccess: &captured,
	}

	got := pricingProvenance(status)
	if got.Source != status.Source || got.Version != status.Version || got.LastSuccess == nil {
		t.Fatalf("pricing provenance = %+v, want fetched source status", got)
	}
	_, wireOffset := got.LastSuccess.Zone()
	if wireOffset != 0 || !got.LastSuccess.Equal(captured) {
		t.Fatalf("wire successful-fetch time = %s (offset %d), want UTC instant equal to %s", got.LastSuccess.Format(time.RFC3339Nano), wireOffset, captured.Format(time.RFC3339Nano))
	}
	if got.LastSuccess == status.LastSuccess {
		t.Fatal("pricing provenance retained the cache status time pointer")
	}
	_, retainedOffset := status.LastSuccess.Zone()
	if retainedOffset != 9*60*60 || status.LastSuccess != &captured {
		t.Fatalf("source successful-fetch representation changed: time=%s offset=%d", status.LastSuccess.Format(time.RFC3339Nano), retainedOffset)
	}
}
