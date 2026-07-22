package cache

import (
	"context"
	"sync"
	"time"
)

const primeTimeout = 5 * time.Second

type entry interface {
	prime(context.Context) error
	run(context.Context)
	observe() (Status, bool)
}

// Registry aggregates the collective lifecycle and observation operations for
// the process's cached values.
type Registry struct {
	mu           sync.RWMutex
	entries      []entry
	primeTimeout time.Duration
}

// NewRegistry constructs an empty cache registry.
func NewRegistry() *Registry { return &Registry{primeTimeout: primeTimeout} }

// Register adds a cached value to the registry.
func (r *Registry) Register(value entry) {
	r.mu.Lock()
	r.entries = append(r.entries, value)
	r.mu.Unlock()
}

// Prime performs one startup attempt for each refresh-enabled cached value.
func (r *Registry) Prime(ctx context.Context) {
	timeout := r.primeTimeout
	if timeout <= 0 {
		timeout = primeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entries := r.snapshot()
	done := make(chan struct{}, len(entries))
	for _, value := range entries {
		go func() {
			_ = value.prime(ctx)
			done <- struct{}{}
		}()
	}
	for range entries {
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
	}
}

// Start launches each registered cached value's independent refresh loop.
func (r *Registry) Start(ctx context.Context) {
	for _, value := range r.snapshot() {
		go value.run(ctx)
	}
}

// Observe collects a fresh status from every entry that elects to publish.
func (r *Registry) Observe() []Status {
	entries := r.snapshot()
	statuses := make([]Status, 0, len(entries))
	for _, value := range entries {
		if status, publish := value.observe(); publish {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func (r *Registry) snapshot() []entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]entry(nil), r.entries...)
}
