package sse

import "sync/atomic"

type counter struct {
	count atomic.Uint64
}

func (c *counter) increment()    { c.count.Add(1) }
func (c *counter) value() uint64 { return c.count.Load() }

// FallbackCounter records frames whose type identification had to fall back
// from an absent or empty event line to the data.type path.
type FallbackCounter struct {
	counter counter
}

// NewFallbackCounter returns an empty fallback-fired counter.
func NewFallbackCounter() *FallbackCounter { return &FallbackCounter{} }

// Increment records one use of the data.type fallback path.
func (c *FallbackCounter) Increment() { c.counter.increment() }

// Count returns the number of fallback attempts observed so far.
func (c *FallbackCounter) Count() uint64 { return c.counter.value() }

// SuppressedShimErrorCounter records FrameTransformer failures hidden from the
// wire by the post-terminal no-double-up rule.
type SuppressedShimErrorCounter struct {
	counter counter
}

// NewSuppressedShimErrorCounter returns an empty suppressed-error counter.
func NewSuppressedShimErrorCounter() *SuppressedShimErrorCounter {
	return &SuppressedShimErrorCounter{}
}

// Increment records one post-terminal FrameTransformer failure.
func (c *SuppressedShimErrorCounter) Increment() { c.counter.increment() }

// Count returns the number of suppressed post-terminal failures observed.
func (c *SuppressedShimErrorCounter) Count() uint64 { return c.counter.value() }
