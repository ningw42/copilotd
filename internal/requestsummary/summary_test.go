package requestsummary_test

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/sse"
)

type observerFunc func(string, sse.Outcome)

func (f observerFunc) ObserveStreamOutcome(surface string, outcome sse.Outcome) {
	f(surface, outcome)
}

func assertPanics(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("call did not panic")
		}
	}()
	f()
}

func TestBeginReturnsChildContextAndBasePublication(t *testing.T) {
	type markerKey struct{}
	base := context.WithValue(context.Background(), markerKey{}, "base")
	ctx, summary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {
		t.Fatal("observed a stream without a stream result")
	}))

	if ctx == base {
		t.Fatal("Begin returned its input context, want a child context")
	}
	if got := ctx.Value(markerKey{}); got != "base" {
		t.Fatalf("child context marker = %v, want base", got)
	}
	if summary == nil {
		t.Fatal("Begin returned a nil Summary")
	}

	publication := summary.Finish(requestsummary.ResponseResult{
		Method:   "POST",
		Status:   201,
		Bytes:    42,
		Duration: 1500 * time.Millisecond,
	})
	if publication.Context != base {
		t.Fatal("publication did not use the context supplied to Begin")
	}
	if publication.Level != slog.LevelInfo {
		t.Fatalf("publication level = %s, want INFO", publication.Level)
	}
	want := []slog.Attr{
		slog.String(logging.MethodKey, "POST"),
		slog.Int(logging.StatusKey, 201),
		slog.Int64(logging.BytesKey, 42),
		slog.Duration(logging.DurationKey, 1500*time.Millisecond),
	}
	if !reflect.DeepEqual(publication.Attrs, want) {
		t.Fatalf("publication attrs = %#v, want %#v", publication.Attrs, want)
	}
}

func TestContextProducersCannotRetrieveMutableSummaryState(t *testing.T) {
	summaryType := reflect.TypeOf(requestsummary.Summary{})
	for i := 0; i < summaryType.NumField(); i++ {
		field := summaryType.Field(i)
		if field.IsExported() {
			t.Errorf("Summary exposes mutable field %q", field.Name)
		}
	}

	pointerType := reflect.PointerTo(summaryType)
	if pointerType.NumMethod() != 1 || pointerType.Method(0).Name != "Finish" {
		t.Fatalf("*Summary exported methods = %v, want only Finish", exportedMethodNames(pointerType))
	}
}

func exportedMethodNames(typ reflect.Type) []string {
	names := make([]string, typ.NumMethod())
	for i := range names {
		names[i] = typ.Method(i).Name
	}
	return names
}

func TestBeginRequiresNonNilDependencies(t *testing.T) {
	observer := observerFunc(func(string, sse.Outcome) {})
	t.Run("context", func(t *testing.T) {
		assertPanics(t, func() {
			requestsummary.Begin(nil, observer)
		})
	})
	t.Run("stream observer", func(t *testing.T) {
		assertPanics(t, func() {
			requestsummary.Begin(context.Background(), nil)
		})
	})
}

func TestRecordOperationsWithoutSummaryAreNoOps(t *testing.T) {
	ctx := context.Background()
	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: ctx, Probe: true})
	requestsummary.RecordCorrelation(ctx, context.WithValue(ctx, struct{}{}, "correlated"))
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{
		Surface: "openai", Outcome: sse.OutcomeClean, Frames: 1, Fallbacks: 2,
	})
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeOpenAI)
	requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{
		Terminal: requestsummary.WebSocketClientClosed, CloseCode: 1000,
		MsgsC2U: 1, MsgsU2C: 2, BytesC2U: 3, BytesU2C: 4,
	})
	requestsummary.NewHookOverrunRecorder(ctx).Increment()

	if matched, ok := requestsummary.MatchedContext(ctx); ok || matched != nil {
		t.Fatalf("MatchedContext without a summary = (%v, %v), want (nil, false)", matched, ok)
	}
}

func TestBindingIsFirstWriteWins(t *testing.T) {
	base := context.Background()
	ctx, summary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {}))
	first := context.WithValue(base, struct{ name string }{"binding"}, "first")
	second := context.WithValue(base, struct{ name string }{"binding"}, "second")

	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: first})
	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: second, Probe: true})

	matched, ok := requestsummary.MatchedContext(ctx)
	if !ok || matched != first {
		t.Fatalf("MatchedContext = (%v, %v), want first binding", matched, ok)
	}
	publication := summary.Finish(requestsummary.ResponseResult{Status: 503})
	if publication.Context != first {
		t.Fatal("publication did not use the first binding context")
	}
	if publication.Level != slog.LevelWarn {
		t.Fatalf("publication level = %s, want WARN from non-probe 503", publication.Level)
	}
}

func TestCorrelationIsFirstWriteWinsAndTakesPublicationPrecedence(t *testing.T) {
	base := context.Background()
	ctx, summary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {}))
	binding := context.WithValue(base, struct{ name string }{"scope"}, "binding")
	first, cancel := context.WithCancel(binding)
	cancel()
	second := context.WithValue(binding, struct{ name string }{"correlation"}, "second")

	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: binding})
	requestsummary.RecordCorrelation(ctx, first)
	requestsummary.RecordCorrelation(ctx, second)

	matched, ok := requestsummary.MatchedContext(ctx)
	if !ok || matched != binding {
		t.Fatalf("MatchedContext = (%v, %v), want binding rather than correlation", matched, ok)
	}
	publication := summary.Finish(requestsummary.ResponseResult{})
	if publication.Context != first {
		t.Fatal("publication did not prefer the first correlation context")
	}
	if publication.Context.Err() != context.Canceled {
		t.Fatalf("selected correlation Err = %v, want context.Canceled", publication.Context.Err())
	}

	correlationOnlyContext, correlationOnlySummary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {}))
	requestsummary.RecordCorrelation(correlationOnlyContext, first)
	if matched, ok := requestsummary.MatchedContext(correlationOnlyContext); ok || matched != nil {
		t.Fatalf("correlation-only MatchedContext = (%v, %v), want (nil, false)", matched, ok)
	}
	correlationOnlySummary.Finish(requestsummary.ResponseResult{})
}

func TestMatchedContextIsAvailableOnlyBeforeFinish(t *testing.T) {
	base := context.Background()
	ctx, summary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {}))
	binding := context.WithValue(base, struct{ name string }{"binding"}, "matched")
	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: binding})

	if matched, ok := requestsummary.MatchedContext(ctx); !ok || matched != binding {
		t.Fatalf("MatchedContext before Finish = (%v, %v), want binding", matched, ok)
	}
	summary.Finish(requestsummary.ResponseResult{})
	if matched, ok := requestsummary.MatchedContext(ctx); ok || matched != nil {
		t.Fatalf("MatchedContext after Finish = (%v, %v), want (nil, false)", matched, ok)
	}
}

func TestStreamAndValidCatalogShapeReplaceEarlierValues(t *testing.T) {
	type observation struct {
		surface string
		outcome sse.Outcome
	}
	var observations []observation
	ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(surface string, outcome sse.Outcome) {
		observations = append(observations, observation{surface: surface, outcome: outcome})
	}))

	requestsummary.RecordStream(ctx, requestsummary.StreamResult{
		Surface: "anthropic", Outcome: sse.OutcomeClean, Frames: 2, Fallbacks: 1,
	})
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{
		Surface: "future-surface", Outcome: sse.Outcome("future_outcome"), Frames: -2, Fallbacks: -3,
	})
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeOpenAI)
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShape("invalid"))
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeCodex)
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShape("still_invalid"))

	publication := summary.Finish(requestsummary.ResponseResult{
		Method: "FUTURE", Status: -9, Bytes: -10, Duration: -time.Second,
	})
	wantAttrs := []slog.Attr{
		slog.String(logging.MethodKey, "FUTURE"),
		slog.Int(logging.StatusKey, -9),
		slog.Int64(logging.BytesKey, -10),
		slog.Duration(logging.DurationKey, -time.Second),
		slog.String(logging.OutcomeKey, "future_outcome"),
		slog.Int(logging.FramesKey, -2),
		slog.Int(logging.FallbacksKey, -3),
		slog.String(logging.CatalogShapeKey, "codex"),
	}
	if !reflect.DeepEqual(publication.Attrs, wantAttrs) {
		t.Fatalf("publication attrs = %#v, want %#v", publication.Attrs, wantAttrs)
	}
	wantObservations := []observation{{surface: "future-surface", outcome: sse.Outcome("future_outcome")}}
	if !reflect.DeepEqual(observations, wantObservations) {
		t.Fatalf("stream observations = %#v, want %#v", observations, wantObservations)
	}
	if publication.Level != slog.LevelInfo {
		t.Fatalf("unknown stream outcome level = %s, want INFO", publication.Level)
	}
}

func TestWebSocketIsFirstWriteWinsAndPreservesScalars(t *testing.T) {
	ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
	requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{
		Terminal:  requestsummary.WebSocketTerminal("future_terminal"),
		CloseCode: -1,
		MsgsC2U:   -2,
		MsgsU2C:   -3,
		BytesC2U:  -4,
		BytesU2C:  -5,
	})
	requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{
		Terminal:  requestsummary.WebSocketError,
		CloseCode: 1011,
		MsgsC2U:   2,
		MsgsU2C:   3,
		BytesC2U:  4,
		BytesU2C:  5,
	})

	publication := summary.Finish(requestsummary.ResponseResult{})
	wantAttrs := []slog.Attr{
		slog.String(logging.MethodKey, ""),
		slog.Int(logging.StatusKey, 0),
		slog.Int64(logging.BytesKey, 0),
		slog.Duration(logging.DurationKey, 0),
		slog.String(logging.TerminalReasonKey, "future_terminal"),
		slog.Int(logging.CloseCodeKey, -1),
		slog.Int64(logging.MsgsC2UKey, -2),
		slog.Int64(logging.MsgsU2CKey, -3),
		slog.Int64(logging.BytesC2UKey, -4),
		slog.Int64(logging.BytesU2CKey, -5),
	}
	if !reflect.DeepEqual(publication.Attrs, wantAttrs) {
		t.Fatalf("publication attrs = %#v, want %#v", publication.Attrs, wantAttrs)
	}
	if publication.Level != slog.LevelInfo {
		t.Fatalf("unknown terminal level = %s, want INFO", publication.Level)
	}
}

func TestFinishAppliesLevelPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		probe     bool
		status    int
		stream    sse.Outcome
		webSocket requestsummary.WebSocketTerminal
		want      slog.Level
	}{
		{name: "normal", status: 200, want: slog.LevelInfo},
		{name: "server failure", status: 500, want: slog.LevelWarn},
		{name: "probe overrides every abnormality", probe: true, status: 503, stream: sse.OutcomeSynthesized, webSocket: requestsummary.WebSocketError, want: slog.LevelDebug},
		{name: "synthesized stream", stream: sse.OutcomeSynthesized, want: slog.LevelWarn},
		{name: "stalled stream", stream: sse.OutcomeStall, want: slog.LevelWarn},
		{name: "upstream stream error", stream: sse.OutcomeUpstreamError, want: slog.LevelWarn},
		{name: "shim stream error", stream: sse.OutcomeShimError, want: slog.LevelWarn},
		{name: "clean stream", stream: sse.OutcomeClean, want: slog.LevelInfo},
		{name: "client cancellation", stream: sse.OutcomeClientCancel, want: slog.LevelInfo},
		{name: "unknown stream outcome", stream: sse.Outcome("future"), want: slog.LevelInfo},
		{name: "websocket error", webSocket: requestsummary.WebSocketError, want: slog.LevelWarn},
		{name: "client closed websocket", webSocket: requestsummary.WebSocketClientClosed, want: slog.LevelInfo},
		{name: "upstream closed websocket", webSocket: requestsummary.WebSocketUpstreamClosed, want: slog.LevelInfo},
		{name: "unknown websocket terminal", webSocket: requestsummary.WebSocketTerminal("future"), want: slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
			if tt.probe {
				requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: ctx, Probe: true})
			}
			if tt.stream != "" {
				requestsummary.RecordStream(ctx, requestsummary.StreamResult{Outcome: tt.stream})
			}
			if tt.webSocket != "" {
				requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{Terminal: tt.webSocket})
			}

			publication := summary.Finish(requestsummary.ResponseResult{Status: tt.status})
			if publication.Level != tt.want {
				t.Fatalf("level = %s, want %s", publication.Level, tt.want)
			}
			if publication.Level == slog.LevelError {
				t.Fatal("access publication must never use ERROR")
			}
		})
	}
}

func TestFinishIsIdempotentWithFreshCallerOwnedAttrs(t *testing.T) {
	observations := 0
	base := context.Background()
	ctx, summary := requestsummary.Begin(base, observerFunc(func(string, sse.Outcome) {
		observations++
	}))
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{
		Surface: "openai", Outcome: sse.OutcomeClean, Frames: 3, Fallbacks: 1,
	})

	first := summary.Finish(requestsummary.ResponseResult{
		Method: "POST", Status: 201, Bytes: 12, Duration: time.Second,
	})
	wantAttrs := append([]slog.Attr(nil), first.Attrs...)
	first.Attrs[0] = slog.String(logging.MethodKey, "mutated by caller")

	second := summary.Finish(requestsummary.ResponseResult{
		Method: "DELETE", Status: 503, Bytes: 99, Duration: 2 * time.Second,
	})
	if second.Context != base || second.Level != slog.LevelInfo {
		t.Fatalf("second publication context/level = (%v, %s), want first publication", second.Context, second.Level)
	}
	if !reflect.DeepEqual(second.Attrs, wantAttrs) {
		t.Fatalf("second attrs = %#v, want unmodified first attrs %#v", second.Attrs, wantAttrs)
	}
	if observations != 1 {
		t.Fatalf("stream observations = %d, want exactly 1", observations)
	}
}

func TestRecordsAfterFinishAreIgnored(t *testing.T) {
	observations := 0
	ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {
		observations++
	}))
	first := summary.Finish(requestsummary.ResponseResult{Method: "GET", Status: 204})

	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: context.WithValue(ctx, struct{}{}, "late"), Probe: true})
	requestsummary.RecordCorrelation(ctx, context.WithValue(ctx, struct{}{}, "late correlation"))
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{Surface: "openai", Outcome: sse.OutcomeStall, Frames: 9, Fallbacks: 8})
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeCodex)
	requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{Terminal: requestsummary.WebSocketError, CloseCode: 1011})

	if matched, ok := requestsummary.MatchedContext(ctx); ok || matched != nil {
		t.Fatalf("late binding became visible as (%v, %v)", matched, ok)
	}
	second := summary.Finish(requestsummary.ResponseResult{Method: "POST", Status: 503})
	if first.Context != second.Context || first.Level != second.Level || !reflect.DeepEqual(first.Attrs, second.Attrs) {
		t.Fatalf("late records changed publication: first=%#v second=%#v", first, second)
	}
	if observations != 0 {
		t.Fatalf("late stream caused %d observations, want 0", observations)
	}
}

func TestCatalogShapeIsAnIndependentOptionalGroup(t *testing.T) {
	baseAttrs := []slog.Attr{
		slog.String(logging.MethodKey, "GET"),
		slog.Int(logging.StatusKey, 200),
		slog.Int64(logging.BytesKey, 0),
		slog.Duration(logging.DurationKey, 0),
	}
	tests := []struct {
		name  string
		shape requestsummary.CatalogShape
		want  []slog.Attr
	}{
		{name: "invalid shape omitted", shape: requestsummary.CatalogShape("invalid"), want: baseAttrs},
		{name: "valid shape present alone", shape: requestsummary.CatalogShapeCodex, want: append(append([]slog.Attr(nil), baseAttrs...), slog.String(logging.CatalogShapeKey, "codex"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
			requestsummary.RecordCatalogShape(ctx, tt.shape)
			publication := summary.Finish(requestsummary.ResponseResult{Method: "GET", Status: 200})
			if !reflect.DeepEqual(publication.Attrs, tt.want) {
				t.Fatalf("publication attrs = %#v, want %#v", publication.Attrs, tt.want)
			}
		})
	}
}

func TestFinishOrdersEveryAttributeGroup(t *testing.T) {
	ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{
		Surface: "openai", Outcome: sse.OutcomeClean, Frames: 7, Fallbacks: 2,
	})
	requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeOpenAI)
	requestsummary.RecordWebSocket(ctx, requestsummary.WebSocketResult{
		Terminal:  requestsummary.WebSocketUpstreamClosed,
		CloseCode: 1001,
		MsgsC2U:   3,
		MsgsU2C:   4,
		BytesC2U:  30,
		BytesU2C:  40,
	})
	overruns := requestsummary.NewHookOverrunRecorder(ctx)
	overruns.Increment()
	overruns.Increment()

	publication := summary.Finish(requestsummary.ResponseResult{
		Method: "GET", Status: 202, Bytes: 80, Duration: 3 * time.Second,
	})
	want := []slog.Attr{
		slog.String(logging.MethodKey, "GET"),
		slog.Int(logging.StatusKey, 202),
		slog.Int64(logging.BytesKey, 80),
		slog.Duration(logging.DurationKey, 3*time.Second),
		slog.String(logging.OutcomeKey, "clean"),
		slog.Int(logging.FramesKey, 7),
		slog.Int(logging.FallbacksKey, 2),
		slog.String(logging.CatalogShapeKey, "openai"),
		slog.String(logging.TerminalReasonKey, "upstream_closed"),
		slog.Int(logging.CloseCodeKey, 1001),
		slog.Int64(logging.MsgsC2UKey, 3),
		slog.Int64(logging.MsgsU2CKey, 4),
		slog.Int64(logging.BytesC2UKey, 30),
		slog.Int64(logging.BytesU2CKey, 40),
		slog.Int(logging.HookOverrunsKey, 2),
	}
	if !reflect.DeepEqual(publication.Attrs, want) {
		t.Fatalf("publication attrs = %#v, want %#v", publication.Attrs, want)
	}
}

func TestStreamObserverCanReenterSummaryWithoutDeadlock(t *testing.T) {
	var (
		ctx       context.Context
		summary   *requestsummary.Summary
		reentered requestsummary.Publication
		observed  int
	)
	ctx, summary = requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {
		observed++
		requestsummary.RecordCatalogShape(ctx, requestsummary.CatalogShapeCodex)
		reentered = summary.Finish(requestsummary.ResponseResult{Method: "reentrant", Status: 503})
	}))
	requestsummary.RecordBinding(ctx, requestsummary.Binding{Context: ctx, Probe: true})
	requestsummary.RecordStream(ctx, requestsummary.StreamResult{Surface: "openai", Outcome: sse.OutcomeStall})

	done := make(chan requestsummary.Publication, 1)
	go func() {
		done <- summary.Finish(requestsummary.ResponseResult{Method: "GET", Status: 200})
	}()

	var publication requestsummary.Publication
	select {
	case publication = <-done:
	case <-time.After(time.Second):
		t.Fatal("Finish deadlocked while the stream observer re-entered the summary")
	}
	if observed != 1 {
		t.Fatalf("probe stream observations = %d, want 1", observed)
	}
	if publication.Level != slog.LevelDebug {
		t.Fatalf("probe publication level = %s, want DEBUG", publication.Level)
	}
	if reentered.Context != publication.Context || reentered.Level != publication.Level || !reflect.DeepEqual(reentered.Attrs, publication.Attrs) {
		t.Fatalf("reentrant Finish = %#v, want cached publication %#v", reentered, publication)
	}
}

func TestHookOverrunRecorderGatesAndCountsTerminalSummary(t *testing.T) {
	_, baseSummary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
	basePublication := baseSummary.Finish(requestsummary.ResponseResult{})
	for _, attr := range basePublication.Attrs {
		if attr.Key == logging.HookOverrunsKey {
			t.Fatal("summary without a constructed Chain published hook_overruns")
		}
	}

	zeroCtx, zeroSummary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
	_ = requestsummary.NewHookOverrunRecorder(zeroCtx)
	zeroPublication := zeroSummary.Finish(requestsummary.ResponseResult{})
	if got := zeroPublication.Attrs[len(zeroPublication.Attrs)-1]; !got.Equal(slog.Int(logging.HookOverrunsKey, 0)) {
		t.Fatalf("applicable zero attr = %#v, want hook_overruns=0", got)
	}

	countCtx, countSummary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
	recorder := requestsummary.NewHookOverrunRecorder(countCtx)
	const increments = 100
	var wg sync.WaitGroup
	wg.Add(increments)
	for range increments {
		go func() {
			defer wg.Done()
			recorder.Increment()
		}()
	}
	wg.Wait()
	countPublication := countSummary.Finish(requestsummary.ResponseResult{})
	if got := countPublication.Attrs[len(countPublication.Attrs)-1]; !got.Equal(slog.Int(logging.HookOverrunsKey, increments)) {
		t.Fatalf("count attr = %#v, want hook_overruns=%d", got, increments)
	}

	recorder.Increment()
	latePublication := countSummary.Finish(requestsummary.ResponseResult{})
	if !reflect.DeepEqual(latePublication.Attrs, countPublication.Attrs) {
		t.Fatalf("late increment changed publication: before=%#v after=%#v", countPublication.Attrs, latePublication.Attrs)
	}
}

func TestRecordAndFinishLinearizeConcurrently(t *testing.T) {
	for i := 0; i < 250; i++ {
		ctx, summary := requestsummary.Begin(context.Background(), observerFunc(func(string, sse.Outcome) {}))
		result := requestsummary.WebSocketResult{
			Terminal:  requestsummary.WebSocketClientClosed,
			CloseCode: 1000 + i,
			MsgsC2U:   int64(i + 1),
			MsgsU2C:   int64(i + 2),
			BytesC2U:  int64(i + 3),
			BytesU2C:  int64(i + 4),
		}
		start := make(chan struct{})
		publications := make(chan requestsummary.Publication, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			requestsummary.RecordWebSocket(ctx, result)
		}()
		go func() {
			defer wg.Done()
			<-start
			publications <- summary.Finish(requestsummary.ResponseResult{Method: "GET", Status: 200})
		}()
		close(start)
		wg.Wait()
		publication := <-publications

		if got := len(publication.Attrs); got != 4 && got != 10 {
			t.Fatalf("iteration %d: attr count = %d, want complete omission or complete WebSocket group", i, got)
		}
		if len(publication.Attrs) == 10 {
			wantWebSocket := []slog.Attr{
				slog.String(logging.TerminalReasonKey, "client_closed"),
				slog.Int(logging.CloseCodeKey, result.CloseCode),
				slog.Int64(logging.MsgsC2UKey, result.MsgsC2U),
				slog.Int64(logging.MsgsU2CKey, result.MsgsU2C),
				slog.Int64(logging.BytesC2UKey, result.BytesC2U),
				slog.Int64(logging.BytesU2CKey, result.BytesU2C),
			}
			if !reflect.DeepEqual(publication.Attrs[4:], wantWebSocket) {
				t.Fatalf("iteration %d: torn WebSocket group = %#v, want %#v", i, publication.Attrs[4:], wantWebSocket)
			}
		}
		repeated := summary.Finish(requestsummary.ResponseResult{Method: "late", Status: 503})
		if repeated.Context != publication.Context || repeated.Level != publication.Level || !reflect.DeepEqual(repeated.Attrs, publication.Attrs) {
			t.Fatalf("iteration %d: concurrent publication was not stable", i)
		}
	}
}
