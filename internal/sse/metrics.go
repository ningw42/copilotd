package sse

import "sync/atomic"

// FallbackCounter records frames whose type identification had to fall back
// from an absent or empty event line to the data.type path.
type FallbackCounter struct {
	count atomic.Uint64
}

// NewFallbackCounter returns an empty fallback-fired counter.
func NewFallbackCounter() *FallbackCounter { return &FallbackCounter{} }

// Increment records one use of the data.type fallback path.
func (c *FallbackCounter) Increment() { c.count.Add(1) }

// Count returns the number of fallback attempts observed so far.
func (c *FallbackCounter) Count() uint64 { return c.count.Load() }
