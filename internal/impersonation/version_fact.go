package impersonation

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type source string

const (
	sourceDiscovered source = "discovered"
	sourceFallback   source = "fallback"
)

type snapshot struct {
	source      source
	lastSuccess time.Time
	lastAttempt time.Time
	lastErr     string
}

type versionFact struct {
	fallback string
	discover func(context.Context) (string, error)
	clock    func() time.Time
	logger   *slog.Logger

	attemptMu sync.Mutex
	mu        sync.RWMutex
	value     string
	state     snapshot
}

type option func(*versionFact)

func withClock(clock func() time.Time) option {
	return func(fact *versionFact) {
		if clock != nil {
			fact.clock = clock
		}
	}
}

func withLogger(logger *slog.Logger) option {
	return func(fact *versionFact) {
		if logger != nil {
			fact.logger = logger
		}
	}
}

func newVersionFact(fallback string, discover func(context.Context) (string, error), opts ...option) *versionFact {
	fact := &versionFact{
		fallback: fallback,
		discover: discover,
		clock:    time.Now,
		logger:   slog.Default(),
		state: snapshot{
			source: sourceFallback,
		},
	}
	for _, configure := range opts {
		configure(fact)
	}
	return fact
}

func (f *versionFact) current() (string, snapshot) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.state.source == sourceFallback {
		return f.fallback, f.state
	}
	return f.value, f.state
}

func (f *versionFact) attemptDiscovery(ctx context.Context) error {
	f.attemptMu.Lock()
	defer f.attemptMu.Unlock()

	value, err := f.discover(ctx)
	now := f.clock()

	f.mu.Lock()
	f.state.lastAttempt = now
	if err != nil {
		f.state.lastErr = err.Error()
		f.mu.Unlock()
		f.logger.Warn("impersonation version discovery failed", "error", err)
		return err
	}
	f.value = value
	f.state.source = sourceDiscovered
	f.state.lastSuccess = now
	f.state.lastErr = ""
	f.mu.Unlock()

	f.logger.Debug("impersonation version discovery succeeded", "version", value)
	return nil
}

func (f *versionFact) run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = f.attemptDiscovery(ctx)
		}
	}
}
