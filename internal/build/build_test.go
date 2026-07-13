package build

import "testing"

func TestStringComposesMetadata(t *testing.T) {
	// Save and restore the package globals so the test is hermetic.
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "1.2.3", "abcdef0", "2026-07-13T00:00:00Z"

	got := String()
	want := "1.2.3 (abcdef0, 2026-07-13T00:00:00Z)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
