package shim

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
)

// Clock is the Hook overrun monitor's monotonic time and callback-timer seam.
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

const (
	hookStateInFlight = "in_flight"
	hookStateReturned = "returned"
	hookStatePanicked = "panicked"
)

// Watchdog monitors sequential invocations in one synchronous transport
// direction. It allocates one callback timer at construction and reuses it.
type Watchdog struct {
	monitor  *Monitor
	recorder overrunRecorder
	timer    Timer

	mu      sync.Mutex
	active  bool
	crossed bool
	started time.Time
	shim    string
	hook    hookRole
	logCtx  context.Context
}

// NewWatchdog returns a reusable watchdog, or nil when monitoring is disabled.
// The nil receiver fast path invokes hooks directly.
func (m *Monitor) NewWatchdog(recorder overrunRecorder) *Watchdog {
	if m == nil || m.threshold <= 0 {
		return nil
	}
	watchdog := &Watchdog{monitor: m, recorder: recorder}
	watchdog.timer = m.clock.AfterFunc(m.threshold, watchdog.publishCrossing)
	watchdog.timer.Stop()
	return watchdog
}

// Invoke observes one named post-commit hook call without changing its
// execution. If invoke panics, the ending state is recorded when applicable and
// the exact recovered value is re-panicked for the transport recovery boundary.
func (w *Watchdog) Invoke(logCtx context.Context, shimName string, hook hookRole, invoke func()) {
	if w == nil {
		invoke()
		return
	}

	w.begin(logCtx, shimName, hook)
	returned := false
	defer func() {
		panicValue := recover()
		state := hookStatePanicked
		if returned {
			state = hookStateReturned
		}
		w.finish(state)
		if !returned {
			panic(panicValue)
		}
	}()
	invoke()
	returned = true
}

func (w *Watchdog) begin(logCtx context.Context, shimName string, hook hookRole) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		panic("shim: concurrent watchdog invocation")
	}
	w.active = true
	w.crossed = false
	w.started = w.monitor.clock.Now()
	w.shim = shimName
	w.hook = hook
	w.logCtx = logCtx
	w.timer.Reset(w.monitor.threshold)
}

func (w *Watchdog) publishCrossing() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active || w.crossed {
		return
	}
	now := w.monitor.clock.Now()
	// time.Timer.Reset may let an already queued callback from the previous
	// invocation run concurrently with the newly armed timer. Its old callback
	// must not publish against the new invocation before the new deadline.
	if now.Sub(w.started) < w.monitor.threshold {
		return
	}
	w.crossed = true
	w.log(hookStateInFlight, now.Sub(w.started))
	if w.recorder != nil {
		w.recorder.Increment()
	}
}

func (w *Watchdog) finish(state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.timer.Stop()
	if w.crossed {
		w.log(state, w.monitor.clock.Now().Sub(w.started))
	}
	w.active = false
	w.logCtx = nil
}

func (w *Watchdog) log(state string, elapsed time.Duration) {
	w.monitor.logger.LogAttrs(w.logCtx, slog.LevelWarn, "shim hook overrun",
		slog.String(logging.ShimKey, w.shim),
		slog.String(logging.HookKey, string(w.hook)),
		slog.String(logging.HookStateKey, state),
		slog.Duration(logging.DurationKey, elapsed),
		slog.Duration(logging.ThresholdKey, w.monitor.threshold),
	)
}
