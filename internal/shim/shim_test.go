package shim

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"testing"

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
	want := slog.Int(logging.HookOverrunsKey, 0)
	if len(publication.Attrs) == 0 || !publication.Attrs[len(publication.Attrs)-1].Equal(want) {
		t.Fatalf("last publication attr = %#v, want %#v", publication.Attrs, want)
	}
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
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

	frames := adapter.Transform(context.Background(), sse.Frame{Type: "delta", Raw: []byte("seed")})
	want := []sse.Frame{
		{Type: "delta", Raw: []byte("outer(left(inner(seed)))")},
		{Type: "delta", Raw: []byte("outer(right(inner(seed)))")},
	}
	if !reflect.DeepEqual(frames, want) {
		t.Errorf("frames = %#v, want %#v", frames, want)
	}
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
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

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

func TestStreamAdapterSelectionAndHoldSemantics(t *testing.T) {
	ctx := context.Background()
	if adapter := (Registry{{Name: "nop", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return NopShim{} }}}).
		NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(); adapter != nil {
		t.Errorf("nop StreamAdapter() = %T, want nil fast path", adapter)
	}
	finalizerOnly := Registry{{
		Name:    "finalizer-only",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return streamFinalizerFunc(func(context.Context) []sse.Frame { return nil })
		},
	}}
	if adapter := finalizerOnly.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(); adapter == nil {
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
	}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()
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

func TestPostCommitAdaptersRetainExactRegistrationIdentity(t *testing.T) {
	chain := (Registry{
		{Name: "first", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return allPostCommitHooks{} }},
		{Name: "second", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return allPostCommitHooks{} }},
	}).NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if got := []string{chain.instances[0].name, chain.instances[1].name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("Chain registration names = %v, want [first second]", got)
	}
	stream := chain.StreamAdapter().(*sseAdapter)
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
			if got := chain.StreamAdapter() != nil; got != tc.wantStreamAdapter {
				t.Errorf("StreamAdapter() presence = %t, want %t", got, tc.wantStreamAdapter)
			}
			if chain.WSClientAdapter() != nil {
				t.Error("WSClientAdapter() is non-nil, want server-direction-only stabilizer")
			}
			if got := chain.WSServerAdapter() != nil; got != tc.wantWSServerAdapter {
				t.Errorf("WSServerAdapter() presence = %t, want %t", got, tc.wantWSServerAdapter)
			}
		})
	}
}
