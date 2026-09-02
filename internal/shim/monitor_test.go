package shim

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/sse"
)

type fakeMonitorClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeMonitorTimer
	created int
}

type fakeMonitorTimer struct {
	clock    *fakeMonitorClock
	deadline time.Time
	callback func()
	active   bool
}

func newFakeMonitorClock() *fakeMonitorClock {
	return &fakeMonitorClock{now: time.Unix(1_000, 0)}
}

func (c *fakeMonitorClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeMonitorClock) AfterFunc(d time.Duration, callback func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeMonitorTimer{clock: c, deadline: c.now.Add(d), callback: callback, active: true}
	c.timers = append(c.timers, timer)
	c.created++
	return timer
}

func (c *fakeMonitorClock) Advance(d time.Duration) {
	for _, callback := range c.advanceAndTakeCallbacks(d) {
		callback()
	}
}

func (c *fakeMonitorClock) advanceAndTakeCallbacks(d time.Duration) []func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	var callbacks []func()
	for _, timer := range c.timers {
		if timer.active && !timer.deadline.After(c.now) {
			timer.active = false
			callbacks = append(callbacks, timer.callback)
		}
	}
	return callbacks
}

func (t *fakeMonitorTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *fakeMonitorTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	return wasActive
}

type recordedHookLog struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

type hookLogRecorder struct {
	mu      sync.Mutex
	minimum slog.Level
	records []recordedHookLog
}

func (h *hookLogRecorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minimum
}

func (h *hookLogRecorder) Handle(_ context.Context, record slog.Record) error {
	entry := recordedHookLog{level: record.Level, msg: record.Message, attrs: make(map[string]slog.Value)}
	record.Attrs(func(attr slog.Attr) bool {
		entry.attrs[attr.Key] = attr.Value
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, entry)
	h.mu.Unlock()
	return nil
}

func (h *hookLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *hookLogRecorder) WithGroup(string) slog.Handler      { return h }

func (h *hookLogRecorder) snapshot() []recordedHookLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedHookLog(nil), h.records...)
}

type countingOverrunRecorder struct {
	mu    sync.Mutex
	count int
}

func (r *countingOverrunRecorder) Increment() {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
}

func (r *countingOverrunRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

func recordedHookState(record recordedHookLog) hookState {
	return hookState(record.attrs["hook_state"].String())
}

func TestWatchdogReportsCrossingAndReturn(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	counts := &countingOverrunRecorder{}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	watchdog := monitor.NewWatchdog(counts)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		watchdog.Invoke(context.Background(), "named-shim", hookEventTransform, func() {
			close(started)
			<-release
		})
		close(done)
	}()
	<-started

	clock.Advance(1250 * time.Millisecond)
	clock.Advance(250 * time.Millisecond)
	close(release)
	<-done

	records := logs.snapshot()
	if len(records) != 2 {
		t.Fatalf("Hook overrun records = %d, want crossing and ending", len(records))
	}
	for i, state := range []string{"in_flight", "returned"} {
		record := records[i]
		if len(record.attrs) != 5 {
			t.Errorf("record %d attributes = %v, want only shim/hook/hook_state/duration/threshold", i, record.attrs)
		}
		if record.level != slog.LevelWarn || record.msg != "shim hook overrun" {
			t.Errorf("record %d level/message = (%s, %q), want (WARN, %q)", i, record.level, record.msg, "shim hook overrun")
		}
		if got := record.attrs["shim"].String(); got != "named-shim" {
			t.Errorf("record %d shim = %q, want named-shim", i, got)
		}
		if got := record.attrs["hook"].String(); got != "event_transform" {
			t.Errorf("record %d hook = %q, want event_transform", i, got)
		}
		if got := record.attrs["hook_state"].String(); got != state {
			t.Errorf("record %d hook_state = %q, want %q", i, got, state)
		}
		if got := record.attrs["threshold"].Duration(); got != time.Second {
			t.Errorf("record %d threshold = %v, want 1s", i, got)
		}
	}
	if got := records[0].attrs["duration"].Duration(); got != 1250*time.Millisecond {
		t.Errorf("crossing duration = %v, want actual publication delay 1.25s", got)
	}
	if got := records[1].attrs["duration"].Duration(); got != 1500*time.Millisecond {
		t.Errorf("ending duration = %v, want total 1.5s", got)
	}
	if counts.Count() != 1 {
		t.Fatalf("overrun count = %d, want 1", counts.Count())
	}
}

func TestWatchdogReportsPanicThenRepanicsSameValue(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	counts := &countingOverrunRecorder{}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(counts)
	panicValue := &struct{ label string }{label: "same panic"}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		watchdog.Invoke(context.Background(), "panicking-shim", hookStreamFinalize, func() {
			clock.Advance(time.Second)
			panic(panicValue)
		})
	}()

	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original value %#v", recovered, panicValue)
	}
	records := logs.snapshot()
	if len(records) != 2 || recordedHookState(records[0]) != hookStateInFlight || recordedHookState(records[1]) != hookStatePanicked {
		t.Fatalf("panic records = %#v, want in_flight/panicked pair", records)
	}
	if got := records[1].attrs["hook"].String(); got != "stream_finalize" {
		t.Fatalf("panic hook = %q, want stream_finalize", got)
	}
	if counts.Count() != 1 {
		t.Fatalf("panic overrun count = %d, want 1", counts.Count())
	}
}

func TestWatchdogPreservesRuntimeGoexit(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(&countingOverrunRecorder{})
	outcome := make(chan string, 1)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				outcome <- "panicked"
				return
			}
			outcome <- "goexit"
		}()
		watchdog.Invoke(context.Background(), "goexit", hookEventTransform, runtime.Goexit)
		outcome <- "returned"
	}()

	if got := <-outcome; got != "goexit" {
		t.Fatalf("runtime.Goexit outcome = %q, want preserved goroutine exit", got)
	}
	if records := logs.snapshot(); len(records) != 0 {
		t.Fatalf("runtime.Goexit records = %#v, want no fabricated ending", records)
	}
}

func TestWatchdogReportsEverySequentialOverrun(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	counts := &countingOverrunRecorder{}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(counts)

	for range 3 {
		watchdog.Invoke(context.Background(), "repeat", hookClientMessageTransform, func() {
			clock.Advance(time.Second)
		})
	}

	if got := len(logs.snapshot()); got != 6 {
		t.Fatalf("records for three overruns = %d, want three complete pairs", got)
	}
	if counts.Count() != 3 {
		t.Fatalf("count for three overruns = %d, want 3", counts.Count())
	}
}

func TestWatchdogFastPathsAndTimerReuse(t *testing.T) {
	t.Run("zero threshold bypasses clock and records", func(t *testing.T) {
		clock := newFakeMonitorClock()
		logs := &hookLogRecorder{minimum: slog.LevelDebug}
		counts := &countingOverrunRecorder{}
		watchdog := NewMonitor(slog.New(logs), 0, WithClock(clock)).NewWatchdog(counts)
		called := false

		watchdog.Invoke(context.Background(), "disabled", hookEventTransform, func() { called = true })

		if !called || clock.created != 0 || len(logs.snapshot()) != 0 || counts.Count() != 0 {
			t.Fatalf("disabled path = called:%t timers:%d logs:%d count:%d, want direct call only", called, clock.created, len(logs.snapshot()), counts.Count())
		}
	})

	t.Run("ordinary invocations reuse one timer", func(t *testing.T) {
		clock := newFakeMonitorClock()
		logs := &hookLogRecorder{minimum: slog.LevelDebug}
		watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(&countingOverrunRecorder{})

		for range 3 {
			watchdog.Invoke(context.Background(), "quick", hookClientMessageTransform, func() {
				clock.Advance(100 * time.Millisecond)
			})
		}

		if clock.created != 1 {
			t.Fatalf("timer allocations = %d, want one reusable watchdog timer", clock.created)
		}
		if records := logs.snapshot(); len(records) != 0 {
			t.Fatalf("quick invocation records = %#v, want none", records)
		}
	})
}

func TestWatchdogCountsWhenWarnIsFilteredAfterRequestCancellation(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelError}
	counts := &countingOverrunRecorder{}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(counts)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		watchdog.Invoke(ctx, "canceled-context", hookServerMessageTransform, func() {
			close(started)
			<-release
		})
		close(done)
	}()
	<-started
	cancel()
	clock.Advance(time.Second)
	close(release)
	<-done

	if records := logs.snapshot(); len(records) != 0 {
		t.Fatalf("filtered handler captured records = %#v, want none", records)
	}
	if counts.Count() != 1 {
		t.Fatalf("filtered-Warn overrun count = %d, want 1", counts.Count())
	}
}

func TestWatchdogSurvivesServerDrainWhileInvocationStillRuns(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(&countingOverrunRecorder{})
	drainCtx, startDrain := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		watchdog.Invoke(context.Background(), "draining", hookClientMessageTransform, func() {
			close(started)
			<-release
			_ = drainCtx.Err()
		})
		close(done)
	}()
	<-started
	startDrain()
	clock.Advance(time.Second)

	records := logs.snapshot()
	if len(records) != 1 || recordedHookState(records[0]) != hookStateInFlight {
		t.Fatalf("records after server drain = %#v, want crossing while hook remains in flight", records)
	}
	close(release)
	<-done
	if records = logs.snapshot(); len(records) != 2 || recordedHookState(records[1]) != hookStateReturned {
		t.Fatalf("records after drained hook returns = %#v, want complete pair", records)
	}
}

func TestStaleTimerCallbackCannotCrossNextInvocationEarly(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	counts := &countingOverrunRecorder{}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(counts)

	invoke := func() (chan struct{}, chan struct{}) {
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			watchdog.Invoke(context.Background(), "reused", hookEventTransform, func() {
				close(started)
				<-release
			})
			close(done)
		}()
		<-started
		return release, done
	}

	firstRelease, firstDone := invoke()
	staleCallbacks := clock.advanceAndTakeCallbacks(time.Second)
	if len(staleCallbacks) != 1 {
		t.Fatalf("due callbacks = %d, want one captured callback", len(staleCallbacks))
	}
	close(firstRelease)
	<-firstDone

	secondRelease, secondDone := invoke()
	staleCallbacks[0]()
	close(secondRelease)
	<-secondDone

	if records := logs.snapshot(); len(records) != 0 {
		t.Fatalf("stale timer callback reported the next invocation early: %#v", records)
	}
	if counts.Count() != 0 {
		t.Fatalf("stale timer callback count = %d, want 0", counts.Count())
	}
}

func TestWatchdogThresholdCompletionRaceIsAbsentOrPaired(t *testing.T) {
	for iteration := 0; iteration < 250; iteration++ {
		clock := newFakeMonitorClock()
		logs := &hookLogRecorder{minimum: slog.LevelDebug}
		watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(&countingOverrunRecorder{})
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			watchdog.Invoke(context.Background(), "racing", hookEventTransform, func() {
				close(started)
				<-release
			})
			close(done)
		}()
		<-started

		var race sync.WaitGroup
		race.Add(2)
		go func() {
			defer race.Done()
			clock.Advance(time.Second)
		}()
		go func() {
			defer race.Done()
			close(release)
		}()
		race.Wait()
		<-done

		records := logs.snapshot()
		if len(records) != 0 && len(records) != 2 {
			t.Fatalf("iteration %d: race emitted %d records, want absent or pair", iteration, len(records))
		}
		if len(records) == 2 && (recordedHookState(records[0]) != hookStateInFlight || recordedHookState(records[1]) != hookStateReturned) {
			t.Fatalf("iteration %d: paired states = %q/%q, want in_flight/returned", iteration, recordedHookState(records[0]), recordedHookState(records[1]))
		}
	}
}

type preCommitAdvancingShim struct{ clock *fakeMonitorClock }

func (s preCommitAdvancingShim) TransformRequest(context.Context, *Request) error {
	s.clock.Advance(2 * time.Second)
	return nil
}

func (s preCommitAdvancingShim) TransformPrelude(context.Context, *Prelude) error {
	s.clock.Advance(2 * time.Second)
	return nil
}

func (s preCommitAdvancingShim) TransformBuffered(context.Context, *Body) error {
	s.clock.Advance(2 * time.Second)
	return nil
}

func TestPreCommitHooksAreNotMonitored(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	_ = NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	chain := (Registry{{
		Name:    "pre-commit-only",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return preCommitAdvancingShim{clock: clock}
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if _, _, err := chain.RunRequest(ctx, "", nil, nil); err != nil {
		t.Fatalf("RunRequest() error = %v", err)
	}
	if _, _, err := chain.RunPrelude(ctx, 200, nil); err != nil {
		t.Fatalf("RunPrelude() error = %v", err)
	}
	if _, err := chain.RunBuffered(ctx, nil); err != nil {
		t.Fatalf("RunBuffered() error = %v", err)
	}
	publication := summary.Finish(requestsummary.ResponseResult{})

	if clock.created != 0 || len(logs.snapshot()) != 0 {
		t.Fatalf("pre-commit path created %d timers and %d records, want none", clock.created, len(logs.snapshot()))
	}
	if got := publication.Attrs[len(publication.Attrs)-1].Value.Int64(); got != 0 {
		t.Fatalf("pre-commit hook_overruns = %d, want 0", got)
	}
}

func TestPermanentOverrunPublishesOneWarningAndWarningExecutionReturns(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	counts := &countingOverrunRecorder{}
	watchdog := NewMonitor(slog.New(logs), time.Second, WithClock(clock)).NewWatchdog(counts)
	started := make(chan struct{})
	stuck := make(chan struct{})
	done := make(chan struct{})
	go func() {
		watchdog.Invoke(context.Background(), "stuck", hookEventTransform, func() {
			close(started)
			<-stuck
		})
		close(done)
	}()
	<-started

	advanceReturned := make(chan struct{})
	go func() {
		clock.Advance(time.Second)
		close(advanceReturned)
	}()
	<-advanceReturned
	clock.Advance(10 * time.Second)

	records := logs.snapshot()
	if len(records) != 1 || recordedHookState(records[0]) != hookStateInFlight {
		t.Fatalf("permanent overrun records = %#v, want one in_flight warning", records)
	}
	if counts.Count() != 1 {
		t.Fatalf("permanent overrun count = %d, want 1", counts.Count())
	}
	close(stuck)
	<-done
}
