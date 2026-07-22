package cache

import (
	"context"
	"testing"
	"time"
)

func TestRegistryPrimeUsesFiveSecondOverallDeadline(t *testing.T) {
	t.Parallel()

	if primeTimeout != 5*time.Second {
		t.Fatalf("prime timeout = %v, want 5s", primeTimeout)
	}
	entry := &deadlineEntry{canceled: make(chan struct{})}
	registry := NewRegistry()
	registry.primeTimeout = 20 * time.Millisecond
	registry.Register(entry)
	started := time.Now()

	registry.Prime(context.Background())

	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Prime returned after %v, want the accelerated overall deadline", elapsed)
	}
	select {
	case <-entry.canceled:
	case <-time.After(time.Second):
		t.Fatal("Prime deadline did not cancel the entry context")
	}
}

type deadlineEntry struct {
	canceled chan struct{}
}

func (e *deadlineEntry) prime(ctx context.Context) error {
	<-ctx.Done()
	close(e.canceled)
	return ctx.Err()
}

func (*deadlineEntry) run(context.Context)     {}
func (*deadlineEntry) observe() (Status, bool) { return Status{}, false }
