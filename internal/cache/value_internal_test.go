package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValueRecordsEveryAttemptOutcomeInternally(t *testing.T) {
	t.Parallel()

	failure := errors.New("upstream unavailable")
	now := time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)
	attempts := 0
	value := New(Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			attempts++
			if attempts == 1 {
				return "", "", failure
			}
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
	}, WithClock(func() time.Time { return now }))
	registry := NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	value.mu.RLock()
	failedAt, failedErr := value.lastAttempt, value.lastErr
	value.mu.RUnlock()
	if !failedAt.Equal(now) || !errors.Is(failedErr, failure) {
		t.Fatalf("failed outcome = (%v, %v), want (%v, %v)", failedAt, failedErr, now, failure)
	}
	_, failedStatus := value.Current()
	if failedStatus.LastSuccess != nil {
		t.Fatalf("public status last success = %v after failure, want nil", failedStatus.LastSuccess)
	}

	now = now.Add(time.Hour)
	registry.Prime(context.Background())
	value.mu.RLock()
	succeededAt, succeededErr := value.lastAttempt, value.lastErr
	value.mu.RUnlock()
	if !succeededAt.Equal(now) || succeededErr != nil {
		t.Fatalf("successful outcome = (%v, %v), want (%v, nil)", succeededAt, succeededErr, now)
	}
	_, succeededStatus := value.Current()
	if succeededStatus.LastSuccess == nil || !succeededStatus.LastSuccess.Equal(now) {
		t.Fatalf("public status last success = %v, want %v", succeededStatus.LastSuccess, now)
	}
}
