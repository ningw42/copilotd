package shim

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/sse"
)

type surfaceRouteRecorder struct {
	surface endpoint.Surface
	route   endpoint.Route
	calls   int
}

type wrappingShim struct {
	name   string
	events *[]string
}

var (
	_ RequestTransformer = (*wrappingShim)(nil)
	_ PreludeTransformer = (*wrappingShim)(nil)
)

func (s *wrappingShim) TransformRequest(_ context.Context, r *Request) error {
	*s.events = append(*s.events, "request:"+s.name)
	r.Body = []byte(s.name + "(" + string(r.Body) + ")")
	r.Header = http.Header{"X-Request-Order": {s.name + "(" + r.Header.Get("X-Request-Order") + ")"}}
	return nil
}

func (s *wrappingShim) TransformPrelude(_ context.Context, p *Prelude) error {
	*s.events = append(*s.events, "prelude:"+s.name)
	p.Status++
	p.Header = http.Header{"X-Prelude-Order": {s.name + "(" + p.Header.Get("X-Prelude-Order") + ")"}}
	return nil
}

type streamOutcomeObserverFunc func(string, sse.Outcome)

func (f streamOutcomeObserverFunc) ObserveStreamOutcome(surface string, outcome sse.Outcome) {
	f(surface, outcome)
}

func TestChainConstructionMarksHookOverrunCountApplicable(t *testing.T) {
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	_ = (Registry(nil)).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	publication := summary.Finish(requestsummary.ResponseResult{})
	assertHookOverrunsAttr(t, publication, 0)
}

func TestChainConstructsEnabledShimsOnceWithSurfaceAndRoute(t *testing.T) {
	ctx := context.Background()
	disabledCalls := 0
	recorded := surfaceRouteRecorder{}
	registry := Registry{
		{
			Name:    "disabled",
			Enabled: false,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				disabledCalls++
				return NopShim{}
			},
		},
		{
			Name:    "enabled",
			Enabled: true,
			New: func(_ context.Context, surface endpoint.Surface, route endpoint.Route) any {
				recorded.surface = surface
				recorded.route = route
				recorded.calls++
				return NopShim{}
			},
		},
	}

	_ = registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if disabledCalls != 0 {
		t.Fatalf("disabled constructor calls = %d, want 0", disabledCalls)
	}
	if recorded.calls != 1 || recorded.surface != endpoint.OpenAI || recorded.route != "/responses" {
		t.Fatalf("enabled constructor = %+v, want one call for OpenAI /responses", recorded)
	}
}

func TestChainConstructsEnabledShimOnlyWithinItsScope(t *testing.T) {
	ctx := context.Background()
	scopeCalls := surfaceRouteRecorder{}
	constructorCalls := 0
	registry := Registry{{
		Name:    "scoped",
		Enabled: true,
		Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
			scopeCalls.surface = surface
			scopeCalls.route = route
			scopeCalls.calls++
			return surface == endpoint.OpenAI && route == endpoint.RouteOpenAIResponses
		},
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			constructorCalls++
			return NopShim{}
		},
	}}

	_ = registry.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)

	if scopeCalls.calls != 1 || scopeCalls.surface != endpoint.Anthropic || scopeCalls.route != endpoint.RouteAnthropicMessages {
		t.Fatalf("scope predicate = %+v, want one call for Anthropic /v1/messages", scopeCalls)
	}
	if constructorCalls != 0 {
		t.Fatalf("out-of-scope constructor calls = %d, want 0", constructorCalls)
	}

	_ = registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if scopeCalls.calls != 2 || scopeCalls.surface != endpoint.OpenAI || scopeCalls.route != endpoint.RouteOpenAIResponses {
		t.Fatalf("scope predicate after matching Surface/Route pair = %+v, want second call for OpenAI /responses", scopeCalls)
	}
	if constructorCalls != 1 {
		t.Fatalf("in-scope constructor calls = %d, want 1", constructorCalls)
	}
}

func TestChainRunsRequestOutwardAndPreludeInward(t *testing.T) {
	ctx := context.Background()
	events := []string{}
	registry := Registry{}
	for _, name := range []string{"one", "two", "three"} {
		name := name
		registry = append(registry, Registration{
			Name:    name,
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return &wrappingShim{name: name, events: &events}
			},
		})
	}
	chain := registry.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)

	header, body, err := chain.RunRequest(ctx, "model=x%2Fy", http.Header{"X-Request-Order": {"seed"}}, []byte("seed"))
	if err != nil {
		t.Fatalf("RunRequest: %v", err)
	}
	status, prelude, err := chain.RunPrelude(ctx, http.StatusOK, http.Header{"X-Prelude-Order": {"seed"}})
	if err != nil {
		t.Fatalf("RunPrelude: %v", err)
	}

	if got, want := string(body), "three(two(one(seed)))"; got != want {
		t.Errorf("request body = %q, want %q", got, want)
	}
	if got, want := header.Get("X-Request-Order"), "three(two(one(seed)))"; got != want {
		t.Errorf("request header = %q, want %q", got, want)
	}
	if got, want := prelude.Get("X-Prelude-Order"), "one(two(three(seed)))"; got != want {
		t.Errorf("prelude header = %q, want %q", got, want)
	}
	if status != http.StatusOK+3 {
		t.Errorf("prelude status = %d, want %d", status, http.StatusOK+3)
	}
	wantEvents := []string{
		"request:one", "request:two", "request:three",
		"prelude:three", "prelude:two", "prelude:one",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

type queryReadingShim struct {
	query string
}

var _ RequestTransformer = (*queryReadingShim)(nil)

func (s *queryReadingShim) TransformRequest(_ context.Context, r *Request) error {
	s.query = r.Query()
	r.Header = http.Header{"X-Reassigned": {"yes"}}
	r.Body = []byte("reassigned")
	return nil
}

func TestRunRequestOwnsCarrierAndReturnsWholeFieldReassignments(t *testing.T) {
	instance := &queryReadingShim{}
	chain := (Registry{{
		Name:    "reader",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return instance
		},
	}}).NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages)

	header, body, err := chain.RunRequest(context.Background(), "q=a%2Fb", http.Header{"X-Old": {"value"}}, []byte("old"))
	if err != nil {
		t.Fatalf("RunRequest: %v", err)
	}
	if instance.query != "q=a%2Fb" {
		t.Errorf("Query() = %q, want exact query", instance.query)
	}
	if header.Get("X-Reassigned") != "yes" || string(body) != "reassigned" {
		t.Errorf("returned header/body = %v, %q, want whole-field reassignments", header, body)
	}
}

type bufferedWrappingShim struct {
	name   string
	events *[]string
}

var _ BufferedTransformer = (*bufferedWrappingShim)(nil)

func (s *bufferedWrappingShim) TransformBuffered(_ context.Context, b *Body) error {
	*s.events = append(*s.events, "buffered:"+s.name)
	b.Bytes = []byte(s.name + "(" + string(b.Bytes) + ")")
	return nil
}

func TestChainRunsBufferedResponseInwardAndOwnsCarrier(t *testing.T) {
	events := []string{}
	registry := Registry{}
	for _, name := range []string{"one", "two", "three"} {
		name := name
		registry = append(registry, Registration{
			Name:    name,
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return &bufferedWrappingShim{name: name, events: &events}
			},
		})
	}
	chain := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages)

	body, err := chain.RunBuffered(context.Background(), []byte("seed"))
	if err != nil {
		t.Fatalf("RunBuffered: %v", err)
	}

	if got, want := string(body), "one(two(three(seed)))"; got != want {
		t.Errorf("buffered body = %q, want %q", got, want)
	}
	wantEvents := []string{"buffered:three", "buffered:two", "buffered:one"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

func TestChainBuffersResponseOnlyWhenEnabledInstanceImplementsHook(t *testing.T) {
	ctx := context.Background()
	withoutHook := (Registry{{
		Name:    "nop",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return NopShim{}
		},
	}}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)
	withHook := (Registry{{
		Name:    "buffered",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return &bufferedWrappingShim{}
		},
	}}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)

	if withoutHook.HasBufferedTransformer() {
		t.Error("nop chain unexpectedly opts into buffering")
	}
	if !withHook.HasBufferedTransformer() {
		t.Error("buffered hook presence did not opt into buffering")
	}
}

type eventTransformFunc func(context.Context, sse.Frame) []sse.Frame

func (f eventTransformFunc) TransformEvent(ctx context.Context, frame sse.Frame) []sse.Frame {
	return f(ctx, frame)
}

type finalizingEventShim struct {
	transform func(context.Context, sse.Frame) []sse.Frame
	finalize  func(context.Context) []sse.Frame
}

type streamFinalizerFunc func(context.Context) []sse.Frame

func (f streamFinalizerFunc) Finalize(ctx context.Context) []sse.Frame {
	return f(ctx)
}

func (s *finalizingEventShim) TransformEvent(ctx context.Context, frame sse.Frame) []sse.Frame {
	return s.transform(ctx, frame)
}

func (s *finalizingEventShim) Finalize(ctx context.Context) []sse.Frame {
	return s.finalize(ctx)
}

func TestStreamAdapterMonitorsDirectEventTransform(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	chain := (Registry{{
		Name:    "slow-event",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
				clock.Advance(time.Second)
				return []sse.Frame{frame}
			})
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	adapter := chain.StreamAdapter(ctx, monitor)

	got := adapter.Transform(ctx, sse.Frame{Type: "delta", Raw: []byte("verbatim")})

	if len(got) != 1 || string(got[0].Raw) != "verbatim" {
		t.Fatalf("Transform() = %#v, want unchanged frame", got)
	}
	records := logs.snapshot()
	if len(records) != 2 || recordedHookState(records[0]) != hookStateInFlight || recordedHookState(records[1]) != hookStateReturned {
		t.Fatalf("event-transform records = %#v, want in_flight/returned pair", records)
	}
	for i, record := range records {
		if record.attrs[logging.ShimKey].String() != "slow-event" || record.attrs[logging.HookKey].String() != "event_transform" {
			t.Errorf("record %d identity = shim:%q hook:%q", i, record.attrs[logging.ShimKey].String(), record.attrs[logging.HookKey].String())
		}
	}
	publication := summary.Finish(requestsummary.ResponseResult{})
	assertHookOverrunsAttr(t, publication, 1)
}

func TestStreamAdapterContinuesMonitoringAfterRequestCancellation(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, cancel := context.WithCancel(context.Background())
	ctx, summary := requestsummary.Begin(ctx, streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	entered := make(chan struct{})
	release := make(chan struct{})
	chain := (Registry{{
		Name:    "canceled-sse",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
				close(entered)
				<-release
				return []sse.Frame{frame}
			})
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	adapter := chain.StreamAdapter(ctx, monitor)
	done := make(chan struct{})
	go func() {
		adapter.Transform(ctx, sse.Frame{Raw: []byte("unchanged")})
		close(done)
	}()
	<-entered
	cancel()
	clock.Advance(time.Second)
	close(release)
	<-done

	records := logs.snapshot()
	if len(records) != 2 || recordedHookState(records[0]) != hookStateInFlight || recordedHookState(records[1]) != hookStateReturned {
		t.Fatalf("canceled SSE records = %#v, want crossing/ending pair", records)
	}
	assertHookOverrunsAttr(t, summary.Finish(requestsummary.ResponseResult{}), 1)
}

func TestStreamAdapterMonitoredPanicKeepsValueAndAttribution(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	panicValue := &struct{ label string }{label: "original"}
	chain := (Registry{{
		Name:    "panicking-event",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(context.Context, sse.Frame) []sse.Frame {
				clock.Advance(time.Second)
				panic(panicValue)
			})
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		chain.StreamAdapter(ctx, monitor).Transform(ctx, sse.Frame{})
	}()

	if recovered != panicValue {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, panicValue)
	}
	records := logs.snapshot()
	if len(records) != 2 || recordedHookState(records[1]) != hookStatePanicked || records[1].attrs[logging.ShimKey].String() != "panicking-event" {
		t.Fatalf("panic records = %#v, want attributed in_flight/panicked pair", records)
	}
	publication := summary.Finish(requestsummary.ResponseResult{})
	assertHookOverrunsAttr(t, publication, 1)
}

func TestStreamAdapterConfiguredZeroDisablesMonitoring(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), 0, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	chain := (Registry{{
		Name:    "unmonitored-event",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
				clock.Advance(10 * time.Second)
				return []sse.Frame{frame}
			})
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	frames := chain.StreamAdapter(ctx, monitor).Transform(ctx, sse.Frame{Raw: []byte("verbatim")})
	publication := summary.Finish(requestsummary.ResponseResult{})

	if len(frames) != 1 || string(frames[0].Raw) != "verbatim" {
		t.Fatalf("disabled Transform() = %#v, want unchanged frame", frames)
	}
	if clock.created != 0 || len(logs.snapshot()) != 0 {
		t.Fatalf("disabled SSE path created %d timers and %d records, want none", clock.created, len(logs.snapshot()))
	}
	assertHookOverrunsAttr(t, publication, 0)
}

func TestStreamAdapterFoldsInnerToOuterWithFanout(t *testing.T) {
	wrap := func(name string) eventTransformFunc {
		return func(_ context.Context, frame sse.Frame) []sse.Frame {
			return []sse.Frame{{Type: frame.Type, Raw: []byte(name + "(" + string(frame.Raw) + ")")}}
		}
	}
	registry := Registry{
		{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return wrap("outer") }},
		{Name: "fanout", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
				return []sse.Frame{
					{Type: frame.Type, Raw: []byte("left(" + string(frame.Raw) + ")")},
					{Type: frame.Type, Raw: []byte("right(" + string(frame.Raw) + ")")},
				}
			})
		}},
		{Name: "inner", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return wrap("inner") }},
	}
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(context.Background(), nil)

	frames := adapter.Transform(context.Background(), sse.Frame{Type: "delta", Raw: []byte("seed")})
	want := []sse.Frame{
		{Type: "delta", Raw: []byte("outer(left(inner(seed)))")},
		{Type: "delta", Raw: []byte("outer(right(inner(seed)))")},
	}
	if !reflect.DeepEqual(frames, want) {
		t.Errorf("frames = %#v, want %#v", frames, want)
	}
}

func TestStreamAdapterMonitorsFinalizerAndFinalizationSweepTransforms(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
		clock.Advance(time.Second)
		return []sse.Frame{frame}
	})
	inner := streamFinalizerFunc(func(context.Context) []sse.Frame {
		clock.Advance(time.Second)
		return []sse.Frame{{Type: "message_stop", Raw: []byte("terminal")}}
	})
	adapter := (Registry{
		{Name: "outer-transform", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
		{Name: "inner-finalize", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
	}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(ctx, monitor)

	frames := adapter.Finalize(ctx)

	if len(frames) != 1 || string(frames[0].Raw) != "terminal" {
		t.Fatalf("Finalize() = %#v, want terminal frame", frames)
	}
	records := logs.snapshot()
	if len(records) != 4 {
		t.Fatalf("finalization records = %d, want two crossing/ending pairs", len(records))
	}
	want := []struct {
		shim, hook string
		state      hookState
	}{
		{shim: "inner-finalize", hook: "stream_finalize", state: hookStateInFlight},
		{shim: "inner-finalize", hook: "stream_finalize", state: hookStateReturned},
		{shim: "outer-transform", hook: "event_transform", state: hookStateInFlight},
		{shim: "outer-transform", hook: "event_transform", state: hookStateReturned},
	}
	for i, expected := range want {
		if got := records[i]; got.attrs[logging.ShimKey].String() != expected.shim || got.attrs[logging.HookKey].String() != expected.hook || recordedHookState(got) != expected.state {
			t.Errorf("record %d = %#v, want shim=%s hook=%s state=%s", i, got, expected.shim, expected.hook, expected.state)
		}
	}
	publication := summary.Finish(requestsummary.ResponseResult{})
	assertHookOverrunsAttr(t, publication, 2)
}

func TestStreamAdapterRetransformsInnerFinalizeOutputThroughOuterHooks(t *testing.T) {
	held := []sse.Frame{}
	inner := &finalizingEventShim{
		transform: func(_ context.Context, frame sse.Frame) []sse.Frame {
			held = append(held, frame)
			return nil
		},
		finalize: func(context.Context) []sse.Frame { return held },
	}
	outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
		return []sse.Frame{{Type: frame.Type, Raw: []byte("altered(" + string(frame.Raw) + ")")}}
	})
	registry := Registry{
		{Name: "outer-alter", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
		{Name: "inner-hold", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
	}
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(context.Background(), nil)

	frames := adapter.Transform(context.Background(), sse.Frame{Type: "message_stop", Raw: []byte("X")})
	if len(frames) != 0 {
		t.Fatalf("Transform() = %#v, want held frame", frames)
	}
	frames = adapter.Finalize(context.Background())
	want := []sse.Frame{{Type: "message_stop", Raw: []byte("altered(X)")}}
	if !reflect.DeepEqual(frames, want) {
		t.Errorf("final frames = %#v, want %#v", frames, want)
	}
}

func TestNilPostCommitAdaptersBypassEnabledMonitor(t *testing.T) {
	clock := newFakeMonitorClock()
	monitor := NewMonitor(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, WithClock(clock))
	ctx := context.Background()
	chain := (Registry{{
		Name:    "no-post-commit-hooks",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return NopShim{}
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if adapter := chain.StreamAdapter(ctx, monitor); adapter != nil {
		t.Errorf("StreamAdapter() = %T, want nil", adapter)
	}
	if adapter := chain.WSClientAdapter(ctx, monitor); adapter != nil {
		t.Errorf("WSClientAdapter() = %T, want nil", adapter)
	}
	if adapter := chain.WSServerAdapter(ctx, monitor); adapter != nil {
		t.Errorf("WSServerAdapter() = %T, want nil", adapter)
	}
	if clock.created != 0 {
		t.Fatalf("nil adapters created %d watchdog timers, want 0", clock.created)
	}
}

func TestStreamAdapterSelectionAndHoldSemantics(t *testing.T) {
	ctx := context.Background()
	if adapter := (Registry{{Name: "nop", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return NopShim{} }}}).
		NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(ctx, nil); adapter != nil {
		t.Errorf("nop StreamAdapter() = %T, want nil fast path", adapter)
	}
	finalizerOnly := Registry{{
		Name:    "finalizer-only",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return streamFinalizerFunc(func(context.Context) []sse.Frame { return nil })
		},
	}}
	if adapter := finalizerOnly.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(ctx, nil); adapter == nil {
		t.Error("finalizer-only StreamAdapter() = nil, want composed transformer")
	}

	outerCalls := 0
	outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) []sse.Frame {
		outerCalls++
		return []sse.Frame{frame}
	})
	hold := eventTransformFunc(func(context.Context, sse.Frame) []sse.Frame { return nil })
	adapter := (Registry{
		{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
		{Name: "inner-hold", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return hold }},
	}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(ctx, nil)
	frames := adapter.Transform(ctx, sse.Frame{Type: "delta", Raw: []byte("held")})
	if len(frames) != 0 || outerCalls != 0 {
		t.Errorf("held Transform() = %#v, outer calls %d; want no output/calls", frames, outerCalls)
	}
}

func TestNopShimImplementsNoHooks(t *testing.T) {
	nop := any(NopShim{})
	if _, ok := nop.(RequestTransformer); ok {
		t.Error("NopShim unexpectedly implements RequestTransformer")
	}
	if _, ok := nop.(PreludeTransformer); ok {
		t.Error("NopShim unexpectedly implements PreludeTransformer")
	}
	if _, ok := nop.(BufferedTransformer); ok {
		t.Error("NopShim unexpectedly implements BufferedTransformer")
	}
	if _, ok := nop.(EventTransformer); ok {
		t.Error("NopShim unexpectedly implements EventTransformer")
	}
	if _, ok := nop.(StreamFinalizer); ok {
		t.Error("NopShim unexpectedly implements StreamFinalizer")
	}
}

type allPostCommitHooks struct{}

func (allPostCommitHooks) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	return []sse.Frame{frame}
}

func (allPostCommitHooks) Finalize(context.Context) []sse.Frame { return nil }

func (allPostCommitHooks) TransformClientMessage(context.Context, *Message) bool { return true }

func (allPostCommitHooks) TransformServerMessage(context.Context, *Message) bool { return true }

type monitoredWebSocketHooks struct {
	clock     *fakeMonitorClock
	clientCtx context.Context
	serverCtx context.Context
}

func (h *monitoredWebSocketHooks) TransformClientMessage(ctx context.Context, _ *Message) bool {
	h.clientCtx = ctx
	h.clock.Advance(time.Second)
	return true
}

func (h *monitoredWebSocketHooks) TransformServerMessage(ctx context.Context, _ *Message) bool {
	h.serverCtx = ctx
	h.clock.Advance(time.Second)
	return true
}

func TestWebSocketAdaptersMonitorBothDirectionsWithSharedRecorder(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	requestCtx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	executionCtx := context.WithValue(context.Background(), struct{ name string }{"context"}, "process-rooted execution")
	logCtx := context.WithValue(requestCtx, struct{ name string }{"context"}, "correlated response")
	hooks := &monitoredWebSocketHooks{clock: clock}
	chain := (Registry{{
		Name:    "both-directions",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return hooks
		},
	}}).NewChain(requestCtx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if emit := chain.WSClientAdapter(logCtx, monitor)(executionCtx, &Message{}); !emit {
		t.Fatal("client adapter dropped message")
	}
	if emit := chain.WSServerAdapter(logCtx, monitor)(executionCtx, &Message{}); !emit {
		t.Fatal("server adapter dropped message")
	}

	if hooks.clientCtx != executionCtx || hooks.serverCtx != executionCtx {
		t.Fatal("monitor replaced the process-rooted hook execution context")
	}
	records := logs.snapshot()
	if len(records) != 4 {
		t.Fatalf("WebSocket overrun records = %d, want two pairs", len(records))
	}
	wantHooks := []string{"client_message_transform", "client_message_transform", "server_message_transform", "server_message_transform"}
	for i, want := range wantHooks {
		if got := records[i].attrs[logging.HookKey].String(); got != want {
			t.Errorf("record %d hook = %q, want %q", i, got, want)
		}
	}
	publication := summary.Finish(requestsummary.ResponseResult{})
	assertHookOverrunsAttr(t, publication, 2)
}

type blockingWebSocketHooks struct {
	entered chan string
	release chan struct{}
}

func (h *blockingWebSocketHooks) TransformClientMessage(context.Context, *Message) bool {
	h.entered <- "client"
	<-h.release
	return true
}

func (h *blockingWebSocketHooks) TransformServerMessage(context.Context, *Message) bool {
	h.entered <- "server"
	<-h.release
	return true
}

func TestWebSocketDirectionsCountConcurrentOverrunsOnSharedRecorder(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	hooks := &blockingWebSocketHooks{entered: make(chan string, 2), release: make(chan struct{})}
	chain := (Registry{{
		Name:    "concurrent-directions",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return hooks
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	client := chain.WSClientAdapter(ctx, monitor)
	server := chain.WSServerAdapter(ctx, monitor)

	var calls sync.WaitGroup
	calls.Add(2)
	go func() {
		defer calls.Done()
		client(context.Background(), &Message{})
	}()
	go func() {
		defer calls.Done()
		server(context.Background(), &Message{})
	}()
	<-hooks.entered
	<-hooks.entered
	clock.Advance(time.Second)
	close(hooks.release)
	calls.Wait()
	publication := summary.Finish(requestsummary.ResponseResult{})

	assertHookOverrunsAttr(t, publication, 2)
	if got := len(logs.snapshot()); got != 4 {
		t.Fatalf("concurrent directional records = %d, want two pairs", got)
	}
}

func TestWebSocketAdapterContinuesMonitoringAfterServerDrain(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), time.Second, WithClock(clock))
	requestCtx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	drainCtx, startDrain := context.WithCancel(context.Background())
	hooks := &blockingWebSocketHooks{entered: make(chan string, 1), release: make(chan struct{})}
	chain := (Registry{{
		Name:    "draining-websocket",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return hooks
		},
	}}).NewChain(requestCtx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	adapter := chain.WSClientAdapter(requestCtx, monitor)
	done := make(chan struct{})
	go func() {
		adapter(drainCtx, &Message{})
		close(done)
	}()
	<-hooks.entered
	startDrain()
	clock.Advance(time.Second)
	close(hooks.release)
	<-done

	records := logs.snapshot()
	if len(records) != 2 || recordedHookState(records[0]) != hookStateInFlight || recordedHookState(records[1]) != hookStateReturned {
		t.Fatalf("draining WebSocket records = %#v, want crossing/ending pair", records)
	}
	assertHookOverrunsAttr(t, summary.Finish(requestsummary.ResponseResult{}), 1)
}

func TestWebSocketAdaptersConfiguredZeroDisableMonitoring(t *testing.T) {
	clock := newFakeMonitorClock()
	logs := &hookLogRecorder{minimum: slog.LevelDebug}
	monitor := NewMonitor(slog.New(logs), 0, WithClock(clock))
	ctx, summary := requestsummary.Begin(context.Background(), streamOutcomeObserverFunc(func(string, sse.Outcome) {}))
	hooks := &monitoredWebSocketHooks{clock: clock}
	chain := (Registry{{
		Name:    "disabled-monitoring",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return hooks
		},
	}}).NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if !chain.WSClientAdapter(ctx, monitor)(context.Background(), &Message{}) ||
		!chain.WSServerAdapter(ctx, monitor)(context.Background(), &Message{}) {
		t.Fatal("disabled monitoring changed WebSocket emission")
	}
	publication := summary.Finish(requestsummary.ResponseResult{})

	if clock.created != 0 || len(logs.snapshot()) != 0 {
		t.Fatalf("disabled WebSocket path created %d timers and %d records, want none", clock.created, len(logs.snapshot()))
	}
	assertHookOverrunsAttr(t, publication, 0)
}

func TestPostCommitAdaptersRetainExactRegistrationIdentity(t *testing.T) {
	chain := (Registry{
		{Name: "first", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return allPostCommitHooks{} }},
		{Name: "second", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return allPostCommitHooks{} }},
	}).NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if got := []string{chain.instances[0].name, chain.instances[1].name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("Chain registration names = %v, want [first second]", got)
	}
	stream := chain.StreamAdapter(context.Background(), nil).(*sseAdapter)
	if got := []string{stream.instances[0].name, stream.instances[1].name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("SSE participant names = %v, want [first second]", got)
	}
	clients := chain.clientMessageParticipants()
	if got := []string{clients[0].name, clients[1].name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("client-message participant names = %v, want [first second]", got)
	}
	servers := chain.serverMessageParticipants()
	if got := []string{servers[0].name, servers[1].name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("server-message participant names = %v, want [first second]", got)
	}
}

func TestCanonicalRegistryNamesAreNonEmptyAndUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i, registration := range CanonicalRegistry() {
		if registration.Name == "" {
			t.Errorf("CanonicalRegistry()[%d] has an empty name", i)
		}
		if _, duplicate := seen[registration.Name]; duplicate {
			t.Errorf("CanonicalRegistry()[%d] repeats name %q", i, registration.Name)
		}
		seen[registration.Name] = struct{}{}
	}
}

func TestCanonicalRegistryShipsDisabledNop(t *testing.T) {
	registry := CanonicalRegistry()
	if len(registry) < 1 || registry[0].Name != "nop" || registry[0].Enabled {
		t.Fatalf("CanonicalRegistry()[0] = %+v, want disabled nop first", registry)
	}
	if registry[0].Scope != nil {
		t.Fatal("nop registration unexpectedly gained Surface/Route scope")
	}
	if registry[0].New == nil {
		t.Fatal("nop registration has nil factory")
	}
}

func TestCanonicalRegistryShipsDisabledResponsesItemIDStabilizerScopedToOpenAIResponses(t *testing.T) {
	registry := CanonicalRegistry()
	if len(registry) != 2 {
		t.Fatalf("len(CanonicalRegistry()) = %d, want nop and responses item-id stabilizer", len(registry))
	}
	registration := registry[1]
	if registration.Name != "responses-item-id-stabilizer" || registration.Enabled {
		t.Fatalf("CanonicalRegistry()[1] = %+v, want disabled responses-item-id-stabilizer", registration)
	}
	if registration.Scope == nil {
		t.Fatal("responses-item-id-stabilizer registration has nil scope")
	}
	for _, tc := range []struct {
		name    string
		surface endpoint.Surface
		route   endpoint.Route
		want    bool
	}{
		{name: "OpenAI Responses", surface: endpoint.OpenAI, route: endpoint.RouteOpenAIResponses, want: true},
		{name: "Anthropic Messages", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicMessages},
		{name: "OpenAI catalog", surface: endpoint.OpenAI, route: endpoint.RouteModels},
		{name: "GitHub Copilot Responses route", surface: endpoint.GitHubCopilot, route: endpoint.RouteOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registration.Scope(tc.surface, tc.route); got != tc.want {
				t.Errorf("Scope(%q, %q) = %t, want %t", tc.surface, tc.route, got, tc.want)
			}
		})
	}
	if _, ok := registration.New(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).(*responsesItemIDStabilizer); !ok {
		t.Fatal("responses-item-id-stabilizer factory did not construct a stabilizer")
	}
}

func TestResponsesItemIDStabilizerRegistrationSelectsOnlyResponsesTransports(t *testing.T) {
	registry := CanonicalRegistry()
	registry[1].Enabled = true
	originalNew := registry[1].New
	constructorCalls := 0
	registry[1].New = func(ctx context.Context, surface endpoint.Surface, route endpoint.Route) any {
		constructorCalls++
		return originalNew(ctx, surface, route)
	}
	for _, tc := range []struct {
		name                 string
		surface              endpoint.Surface
		route                endpoint.Route
		wantConstructorCalls int
		wantStreamAdapter    bool
		wantWSServerAdapter  bool
	}{
		{name: "out of scope", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicMessages},
		{name: "in scope", surface: endpoint.OpenAI, route: endpoint.RouteOpenAIResponses, wantConstructorCalls: 1, wantStreamAdapter: true, wantWSServerAdapter: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := registry.NewChain(context.Background(), tc.surface, tc.route)
			if constructorCalls != tc.wantConstructorCalls {
				t.Fatalf("stabilizer constructor calls = %d, want %d", constructorCalls, tc.wantConstructorCalls)
			}
			if got := chain.StreamAdapter(context.Background(), nil) != nil; got != tc.wantStreamAdapter {
				t.Errorf("StreamAdapter() presence = %t, want %t", got, tc.wantStreamAdapter)
			}
			if chain.WSClientAdapter(context.Background(), nil) != nil {
				t.Error("WSClientAdapter() is non-nil, want server-direction-only stabilizer")
			}
			if got := chain.WSServerAdapter(context.Background(), nil) != nil; got != tc.wantWSServerAdapter {
				t.Errorf("WSServerAdapter() presence = %t, want %t", got, tc.wantWSServerAdapter)
			}
		})
	}
}
