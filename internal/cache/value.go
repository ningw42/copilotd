// Package cache provides the concurrency-safe engine for in-memory cached values.
package cache

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type source string

const (
	sourceFallback source = "fallback"
	sourceFetched  source = "fetched"
)

// Cacheable is the static recipe for one cached value.
type Cacheable[V any] struct {
	Fallback        V
	FallbackVersion string
	TTL             time.Duration
	Version         func(context.Context) (string, error)
	Fetch           func(context.Context) (V, string, error)
	Hash            func(V) string
	Validate        func(V) error
	Name            string
}

// Status is the non-secret freshness record for a cached value.
type Status struct {
	Name    string
	Source  string
	Version string
	// LastSuccess is the last successful content fetch. A Version-only peek
	// records an attempt but does not advance content freshness.
	LastSuccess *time.Time
}

// Option customizes a Value's runtime seams.
type Option func(*options)

type options struct {
	clock     func() time.Time
	logger    *slog.Logger
	newTicker func(time.Duration) Ticker
}

// WithClock replaces the wall clock used to timestamp refresh attempts and
// successful content fetches.
func WithClock(clock func() time.Time) Option {
	return func(opts *options) {
		if clock != nil {
			opts.clock = clock
		}
	}
}

// WithLogger replaces the logger used for refresh outcomes.
func WithLogger(logger *slog.Logger) Option {
	return func(opts *options) {
		if logger != nil {
			opts.logger = logger
		}
	}
}

// Ticker is the narrow time.Ticker behavior exposed for deterministic refresh
// loop tests.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// WithTicker replaces the per-value ticker factory. Each Run call obtains its
// own ticker from this factory.
func WithTicker(newTicker func(time.Duration) Ticker) Option {
	return func(opts *options) {
		if newTicker != nil {
			opts.newTicker = newTicker
		}
	}
}

// Value runs a Cacheable recipe and serves its current value. A Value contains
// mutexes and must not be copied after first use.
type Value[V any] struct {
	src       Cacheable[V]
	clock     func() time.Time
	logger    *slog.Logger
	newTicker func(time.Duration) Ticker
	floorHash string

	attemptMu sync.Mutex
	mu        sync.RWMutex

	value          V
	currentHash    string
	currentSource  source
	currentVersion string
	// Attempt details stay internal: raw errors may contain upstream URLs and
	// must never enter Status or the unauthenticated readiness response.
	lastAttempt time.Time
	lastErr     error
	lastSuccess time.Time
}

// New constructs a cached value. Refreshing remains inert until the value is
// primed through a Registry or Run is called.
func New[V any](src Cacheable[V], configure ...Option) *Value[V] {
	if src.Hash == nil {
		panic("cache: nil Hash")
	}
	if src.Fetch == nil {
		panic("cache: nil Fetch")
	}
	opts := options{clock: time.Now, logger: slog.Default(), newTicker: newRealTicker}
	for _, option := range configure {
		option(&opts)
	}
	floorHash := src.Hash(src.Fallback)
	return &Value[V]{
		src:            src,
		clock:          opts.clock,
		logger:         opts.logger,
		newTicker:      opts.newTicker,
		floorHash:      floorHash,
		currentHash:    floorHash,
		currentSource:  sourceFallback,
		currentVersion: src.FallbackVersion,
	}
}

// Current returns the effective value and a fresh non-secret status snapshot.
func (v *Value[V]) Current() (V, Status) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	value := v.value
	if v.currentSource == sourceFallback {
		value = v.src.Fallback
	}
	status := Status{
		Name:    v.src.Name,
		Source:  string(v.currentSource),
		Version: v.currentVersion,
	}
	if !v.lastSuccess.IsZero() {
		lastSuccess := v.lastSuccess
		status.LastSuccess = &lastSuccess
	}
	return value, status
}

func (v *Value[V]) prime(ctx context.Context) error {
	if v.src.TTL <= 0 {
		return nil
	}
	return v.attempt(ctx)
}

func (v *Value[V]) attempt(ctx context.Context) error {
	v.attemptMu.Lock()
	defer v.attemptMu.Unlock()

	if v.src.Version != nil {
		version, err := v.src.Version(ctx)
		if err != nil {
			return v.failed(ctx, err)
		}
		v.mu.Lock()
		if version == v.currentVersion {
			v.recordAttemptLocked(v.clock(), nil)
			v.mu.Unlock()
			v.succeeded(ctx, version)
			return nil
		}
		v.mu.Unlock()
	}

	value, version, err := v.src.Fetch(ctx)
	if err != nil {
		return v.failed(ctx, err)
	}
	hash := v.src.Hash(value)

	v.mu.RLock()
	currentHash := v.currentHash
	v.mu.RUnlock()
	if hash == currentHash {
		now := v.clock()
		v.mu.Lock()
		v.currentVersion = version
		v.recordAttemptLocked(now, nil)
		v.lastSuccess = now
		v.mu.Unlock()
		v.succeeded(ctx, version)
		return nil
	}
	if hash == v.floorHash {
		now := v.clock()
		var zero V
		v.mu.Lock()
		v.value = zero
		v.currentHash = v.floorHash
		v.currentSource = sourceFallback
		v.currentVersion = version
		v.recordAttemptLocked(now, nil)
		v.lastSuccess = now
		v.mu.Unlock()
		v.succeeded(ctx, version)
		return nil
	}

	if v.src.Validate != nil {
		if err := v.src.Validate(value); err != nil {
			return v.failed(ctx, err)
		}
	}
	now := v.clock()

	v.mu.Lock()
	v.value = value
	v.currentHash = hash
	v.currentSource = sourceFetched
	v.currentVersion = version
	v.recordAttemptLocked(now, nil)
	v.lastSuccess = now
	v.mu.Unlock()
	v.succeeded(ctx, version)
	return nil
}

func (v *Value[V]) succeeded(ctx context.Context, version string) {
	v.logger.DebugContext(ctx, "cached value refresh succeeded",
		slog.String("cached_value", v.src.Name), slog.String("version", version))
}

func (v *Value[V]) failed(ctx context.Context, err error) error {
	v.mu.Lock()
	v.recordAttemptLocked(v.clock(), err)
	v.mu.Unlock()
	v.logger.WarnContext(ctx, "cached value refresh failed",
		slog.String("cached_value", v.src.Name), slog.Any("error", err))
	return err
}

func (v *Value[V]) recordAttemptLocked(at time.Time, err error) {
	v.lastAttempt = at
	v.lastErr = err
}

// Run refreshes the value on its configured cadence until context cancellation.
// It deliberately waits for the first tick because Registry.Prime owns the
// startup attempt.
func (v *Value[V]) Run(ctx context.Context) {
	if v.src.TTL <= 0 {
		return
	}
	ticker := v.newTicker(v.src.TTL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			_ = v.attempt(ctx)
		}
	}
}

func (v *Value[V]) run(ctx context.Context) { v.Run(ctx) }

func (v *Value[V]) observe() (Status, bool) {
	_, status := v.Current()
	return status, status.Name != ""
}

type realTicker struct{ *time.Ticker }

func newRealTicker(interval time.Duration) Ticker {
	return realTicker{Ticker: time.NewTicker(interval)}
}

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }
