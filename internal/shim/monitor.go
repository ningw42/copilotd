package shim

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
)

// Clock is the Hook overrun monitor's monotonic time and callback-timer seam.
// It is intentionally distinct from the SSE transport clock: Hook overrun
// monitoring is shared by SSE and WebSocket transports.
// Tests replace it with a deterministic fake shared across both transports.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) Timer
}

// Timer is the reusable callback timer owned by one Watchdog.
type Timer interface {
	Stop() bool
	Reset(time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, callback func()) Timer {
	return time.AfterFunc(d, callback)
}

// MonitorOption configures a Monitor test seam.
type MonitorOption func(*Monitor)

// WithClock replaces the monitor's time source. Production uses the process
// monotonic clock; deterministic tests supply a manually advanced clock.
func WithClock(clock Clock) MonitorOption {
	return func(m *Monitor) { m.clock = clock }
}

// Monitor owns the process-level Hook overrun record contract. One reusable
// Watchdog is created for each synchronous SSE adapter or WebSocket direction.
type Monitor struct {
	logger    *slog.Logger
	threshold time.Duration
	clock     Clock
}

// NewMonitor constructs Hook overrun monitoring from its required Component
// logger and the resolved global threshold. A non-positive threshold disables
// watchdog construction; configuration rejects negative operator values.
func NewMonitor(logger *slog.Logger, threshold time.Duration, options ...MonitorOption) *Monitor {
	monitor := &Monitor{logger: logger, threshold: threshold, clock: realClock{}}
	for _, configure := range options {
		configure(monitor)
	}
	return monitor
}

type overrunRecorder interface {
	Increment()
}

type hookRole string

const (
	hookEventTransform         hookRole = "event_transform"
	hookStreamFinalize         hookRole = "stream_finalize"
	hookClientMessageTransform hookRole = "client_message_transform"
	hookServerMessageTransform hookRole = "server_message_transform"
)

type hookState string

const (
	hookStateInFlight hookState = "in_flight"
	hookStateReturned hookState = "returned"
	hookStatePanicked hookState = "panicked"
)

type hookPublication struct {
	ctx       context.Context
	shim      string
	hook      hookRole
	state     hookState
	elapsed   time.Duration
	threshold time.Duration
}

// Watchdog monitors sequential invocations in one synchronous transport
// direction. It allocates one callback timer at construction and reuses it. If
// a future caller violates the single-flight transport invariant, that
// invocation runs unmonitored rather than letting observability change its
// behavior.
type Watchdog struct {
	monitor  *Monitor
	recorder overrunRecorder
	timer    Timer
	logCtx   context.Context

	// mu protects invocation state only. publishMu preserves crossing-before-
	// ending order without holding mu across logger or recorder I/O.
	mu        sync.Mutex
	publishMu sync.Mutex
	active    bool
	crossed   bool
	started   time.Time
	shim      string
	hook      hookRole
}

// NewWatchdog returns a reusable watchdog bound to one correlated logging
// context, or nil when monitoring is disabled. The nil receiver fast path
// invokes hooks directly.
func (m *Monitor) NewWatchdog(logCtx context.Context, recorder overrunRecorder) *Watchdog {
	if m == nil || m.threshold <= 0 {
		return nil
	}
	watchdog := &Watchdog{monitor: m, recorder: recorder, logCtx: logCtx}
	watchdog.timer = m.clock.AfterFunc(m.threshold, watchdog.publishCrossing)
	watchdog.timer.Stop()
	return watchdog
}

// Invoke observes one named post-commit hook call without changing its
// execution. If invoke panics, the ending state is recorded when applicable and
// the exact recovered value is re-panicked for the transport recovery boundary.
func (w *Watchdog) Invoke(shimName string, hook hookRole, invoke func()) {
	if w == nil || !w.begin(shimName, hook) {
		invoke()
		return
	}

	returned := false
	defer func() {
		panicValue := recover()
		if returned {
			w.finish(hookStateReturned)
			return
		}
		if panicValue == nil {
			// runtime.Goexit runs deferred functions but is not a panic. Returning
			// from this defer preserves the goroutine exit instead of converting
			// it into panic(nil).
			w.abandon()
			return
		}
		w.finish(hookStatePanicked)
		panic(panicValue)
	}()
	invoke()
	returned = true
}

func (w *Watchdog) begin(shimName string, hook hookRole) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		return false
	}
	w.active = true
	w.crossed = false
	w.started = w.monitor.clock.Now()
	w.shim = shimName
	w.hook = hook
	w.timer.Reset(w.monitor.threshold)
	return true
}

func (w *Watchdog) publishCrossing() {
	w.mu.Lock()
	if !w.active || w.crossed {
		w.mu.Unlock()
		return
	}
	now := w.monitor.clock.Now()
	// time.Timer.Reset may let an already queued callback from the previous
	// invocation run concurrently with the newly armed timer. Its old callback
	// must not publish against the new invocation before the new deadline.
	if now.Sub(w.started) < w.monitor.threshold {
		w.mu.Unlock()
		return
	}

	// Acquire publication ownership before exposing crossed=true. finish can
	// then snapshot state promptly and wait only on publication ordering, never
	// on the state mutex while the logger or recorder is blocked.
	w.publishMu.Lock()
	w.crossed = true
	publication := w.publication(hookStateInFlight, now)
	w.mu.Unlock()

	w.log(publication)
	if w.recorder != nil {
		w.recorder.Increment()
	}
	w.publishMu.Unlock()
}

func (w *Watchdog) finish(state hookState) {
	w.mu.Lock()
	if !w.active {
		w.mu.Unlock()
		return
	}
	w.timer.Stop()
	if !w.crossed {
		w.active = false
		w.mu.Unlock()
		return
	}
	publication := w.publication(state, w.monitor.clock.Now())
	w.active = false
	w.mu.Unlock()

	w.publishMu.Lock()
	w.log(publication)
	w.publishMu.Unlock()
}

func (w *Watchdog) abandon() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.timer.Stop()
	w.active = false
}

func (w *Watchdog) publication(state hookState, now time.Time) hookPublication {
	return hookPublication{
		ctx:       w.logCtx,
		shim:      w.shim,
		hook:      w.hook,
		state:     state,
		elapsed:   now.Sub(w.started),
		threshold: w.monitor.threshold,
	}
}

func (w *Watchdog) log(publication hookPublication) {
	w.monitor.logger.LogAttrs(publication.ctx, slog.LevelWarn, "shim hook overrun",
		slog.String(logging.ShimKey, publication.shim),
		slog.String(logging.HookKey, string(publication.hook)),
		slog.String(logging.HookStateKey, string(publication.state)),
		slog.Duration(logging.DurationKey, publication.elapsed),
		slog.Duration(logging.ThresholdKey, publication.threshold),
	)
}
