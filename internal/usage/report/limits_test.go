package report

import "testing"

func TestNewUsesPublishedReadLimits(t *testing.T) {
	want := readLimits{
		maxModelBytes:         1 << 20,
		maxRows:               1_000_000,
		maxGroups:             10_000,
		maxDistinctModelBytes: 1 << 20,
	}
	if got := New("/unused").limits; got != want {
		t.Fatalf("production read limits = %+v, want %+v", got, want)
	}
}

// NewReadLimitsForTest is compiled only into this package's test binary. It
// keeps the real SQLite read and Query interface while shrinking policy limits
// so boundary behavior does not require production-scale fixtures.
func NewReadLimitsForTest(path string, maxRows, maxGroups, maxDistinctModelBytes int) *Reporter {
	reader := New(path)
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
