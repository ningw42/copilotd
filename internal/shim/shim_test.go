package shim

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
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

type eventTransformFunc func(context.Context, sse.Frame) ([]sse.Frame, error)

func (f eventTransformFunc) TransformEvent(ctx context.Context, frame sse.Frame) ([]sse.Frame, error) {
	return f(ctx, frame)
}

type finalizingEventShim struct {
	transform func(context.Context, sse.Frame) ([]sse.Frame, error)
	finalize  func(context.Context) ([]sse.Frame, error)
}

type streamFinalizerFunc func(context.Context) ([]sse.Frame, error)

func (f streamFinalizerFunc) Finalize(ctx context.Context) ([]sse.Frame, error) {
	return f(ctx)
}

func (s *finalizingEventShim) TransformEvent(ctx context.Context, frame sse.Frame) ([]sse.Frame, error) {
	return s.transform(ctx, frame)
}

func (s *finalizingEventShim) Finalize(ctx context.Context) ([]sse.Frame, error) {
	return s.finalize(ctx)
}

func TestStreamAdapterFoldsInnerToOuterWithFanout(t *testing.T) {
	wrap := func(name string) eventTransformFunc {
		return func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
			return []sse.Frame{{Type: frame.Type, Raw: []byte(name + "(" + string(frame.Raw) + ")")}}, nil
		}
	}
	registry := Registry{
		{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return wrap("outer") }},
		{Name: "fanout", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return eventTransformFunc(func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
				return []sse.Frame{
					{Type: frame.Type, Raw: []byte("left(" + string(frame.Raw) + ")")},
					{Type: frame.Type, Raw: []byte("right(" + string(frame.Raw) + ")")},
				}, nil
			})
		}},
		{Name: "inner", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return wrap("inner") }},
	}
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

	frames, err := adapter.Transform(context.Background(), sse.Frame{Type: "delta", Raw: []byte("seed")})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
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
		transform: func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
			held = append(held, frame)
			return nil, nil
		},
		finalize: func(context.Context) ([]sse.Frame, error) { return held, nil },
	}
	outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
		return []sse.Frame{{Type: frame.Type, Raw: []byte("altered(" + string(frame.Raw) + ")")}}, nil
	})
	registry := Registry{
		{Name: "outer-alter", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
		{Name: "inner-hold", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
	}
	adapter := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

	frames, err := adapter.Transform(context.Background(), sse.Frame{Type: "message_stop", Raw: []byte("X")})
	if err != nil || len(frames) != 0 {
		t.Fatalf("Transform() = %#v, %v, want held frame", frames, err)
	}
	frames, err = adapter.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	want := []sse.Frame{{Type: "message_stop", Raw: []byte("altered(X)")}}
	if !reflect.DeepEqual(frames, want) {
		t.Errorf("final frames = %#v, want %#v", frames, want)
	}
}

func TestStreamAdapterFinalizeErrorRetainsOnlyFullyComposedFrames(t *testing.T) {
	t.Run("outer finalize error retains inner terminal after full traversal", func(t *testing.T) {
		shimErr := errors.New("outer finalize failed")
		held := []sse.Frame{}
		outer := &finalizingEventShim{
			transform: func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
				return []sse.Frame{{Type: frame.Type, Raw: []byte("outer(" + string(frame.Raw) + ")")}}, nil
			},
			finalize: func(context.Context) ([]sse.Frame, error) { return nil, shimErr },
		}
		inner := &finalizingEventShim{
			transform: func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
				held = append(held, frame)
				return nil, nil
			},
			finalize: func(context.Context) ([]sse.Frame, error) { return held, nil },
		}
		adapter := (Registry{
			{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
			{Name: "inner", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
		}).NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()
		terminal := sse.Frame{Type: "message_stop", Raw: []byte("terminal")}
		if frames, err := adapter.Transform(context.Background(), terminal); err != nil || len(frames) != 0 {
			t.Fatalf("Transform() = %#v, %v, want held terminal", frames, err)
		}

		frames, err := adapter.Finalize(context.Background())
		if !errors.Is(err, shimErr) {
			t.Fatalf("Finalize error = %v, want %v", err, shimErr)
		}
		want := []sse.Frame{{Type: "message_stop", Raw: []byte("outer(terminal)")}}
		if !reflect.DeepEqual(frames, want) {
			t.Errorf("retained frames = %#v, want fully composed %#v", frames, want)
		}
	})

	t.Run("middle finalize error discards frame before outer event hook", func(t *testing.T) {
		shimErr := errors.New("middle finalize failed")
		var outerSaw []sse.Frame
		outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
			outerSaw = append(outerSaw, frame)
			return []sse.Frame{frame}, nil
		})
		middle := streamFinalizerFunc(func(context.Context) ([]sse.Frame, error) {
			return []sse.Frame{{Type: "delta", Raw: []byte("partially-composed-secret")}}, shimErr
		})
		inner := streamFinalizerFunc(func(context.Context) ([]sse.Frame, error) { return nil, nil })
		adapter := (Registry{
			{Name: "outer-A", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
			{Name: "middle-B", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return middle }},
			{Name: "inner-C", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
		}).NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

		frames, err := adapter.Finalize(context.Background())
		if !errors.Is(err, shimErr) {
			t.Fatalf("Finalize error = %v, want %v", err, shimErr)
		}
		if len(frames) != 0 {
			t.Errorf("retained frames = %#v, want partially composed frame discarded", frames)
		}
		if len(outerSaw) != 0 {
			t.Errorf("outer hook saw %#v after middle failure, want no skipped traversal", outerSaw)
		}
	})

	t.Run("outer event error retains same-call output after full traversal", func(t *testing.T) {
		shimErr := errors.New("outer event failed after output")
		outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
			return []sse.Frame{{Type: frame.Type, Raw: []byte("fully(" + string(frame.Raw) + ")")}}, shimErr
		})
		inner := streamFinalizerFunc(func(context.Context) ([]sse.Frame, error) {
			return []sse.Frame{{Type: "delta", Raw: []byte("inner")}}, nil
		})
		adapter := (Registry{
			{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
			{Name: "inner", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return inner }},
		}).NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()

		frames, err := adapter.Finalize(context.Background())
		if !errors.Is(err, shimErr) {
			t.Fatalf("Finalize error = %v, want %v", err, shimErr)
		}
		want := []sse.Frame{{Type: "delta", Raw: []byte("fully(inner)")}}
		if !reflect.DeepEqual(frames, want) {
			t.Errorf("retained frames = %#v, want outermost same-call output %#v", frames, want)
		}
	})
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
			return streamFinalizerFunc(func(context.Context) ([]sse.Frame, error) { return nil, nil })
		},
	}}
	if adapter := finalizerOnly.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter(); adapter == nil {
		t.Error("finalizer-only StreamAdapter() = nil, want composed transformer")
	}

	outerCalls := 0
	outer := eventTransformFunc(func(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
		outerCalls++
		return []sse.Frame{frame}, nil
	})
	hold := eventTransformFunc(func(context.Context, sse.Frame) ([]sse.Frame, error) { return nil, nil })
	adapter := (Registry{
		{Name: "outer", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return outer }},
		{Name: "inner-hold", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return hold }},
	}).NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages).StreamAdapter()
	frames, err := adapter.Transform(ctx, sse.Frame{Type: "delta", Raw: []byte("held")})
	if err != nil || len(frames) != 0 || outerCalls != 0 {
		t.Errorf("held Transform() = %#v, %v, outer calls %d; want no output/calls", frames, err, outerCalls)
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

func TestCanonicalRegistryShipsDisabledNop(t *testing.T) {
	registry := CanonicalRegistry()
	if len(registry) != 1 || registry[0].Name != "nop" || registry[0].Enabled {
		t.Fatalf("CanonicalRegistry() = %+v, want one disabled nop", registry)
	}
	if registry[0].New == nil {
		t.Fatal("nop registration has nil factory")
	}
}
