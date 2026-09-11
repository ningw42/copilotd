package report

import (
	"context"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/pricing"
)

type testPricingSource struct{}

var testPricingSnapshot = func() *pricing.Snapshot {
	snapshot, err := pricing.ParseSnapshot(context.Background(), []byte(`{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`))
	if err != nil {
		panic(err)
	}
	return snapshot
}()

func (testPricingSource) Current(ctx context.Context, limit pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if testPricingSnapshot.IdentityBytes() > limit.MaxIdentityBytes {
		return nil, pricing.SnapshotStatus{}, pricing.ErrProjectionLimit
	}
	return testPricingSnapshot, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:empty-test-prices"}, nil
}

func newReporterForTest(path string) *Reporter {
	return New(path, testPricingSource{})
}

func TestNewUsesPublishedReadLimits(t *testing.T) {
	want := readLimits{
		maxModelBytes:         1 << 20,
		maxRows:               1_000_000,
		maxGroups:             10_000,
		maxDistinctModelBytes: 1 << 20,
	}
	if got := newReporterForTest("/unused").limits; got != want {
		t.Fatalf("production read limits = %+v, want %+v", got, want)
	}
}

// NewReadLimitsForTest is compiled only into this package's test binary. It
// keeps the real SQLite read and Query interface while shrinking policy limits
// so boundary behavior does not require production-scale fixtures.
func NewReadLimitsForTest(path string, source pricing.Source, maxRows, maxGroups, maxDistinctModelBytes int) *Reporter {
	reader := New(path, source)
	reader.limits.maxRows = maxRows
	reader.limits.maxGroups = maxGroups
	reader.limits.maxDistinctModelBytes = maxDistinctModelBytes
	return reader
}

// NotifyAfterExaminedTurnForTest installs deterministic scan coordination
// without adding a production interface or replacing the real SQLite read.
func NotifyAfterExaminedTurnForTest(reader *Reporter, notify func(int)) {
	reader.afterExaminedTurn = notify
}

// SetNowForTest fixes Query's captured report clock while retaining the public
// Query seam and real SQLite read.
func SetNowForTest(reader *Reporter, now time.Time) {
	reader.now = func() time.Time { return now }
}
