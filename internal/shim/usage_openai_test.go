package shim

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

type memoryUsageSink struct {
	mu    sync.Mutex
	turns []usage.Turn
}

func (s *memoryUsageSink) Record(turn usage.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, turn)
}

func (s *memoryUsageSink) snapshot() []usage.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.Turn(nil), s.turns...)
}

func enabledOpenAIUsageStream(ctx context.Context, sink usage.Sink) sse.FrameTransformer {
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	return registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses).StreamAdapter(ctx, nil)
}

func TestOpenAIUsageMeterRecordsRecordedSSECompletionWithoutChangingFrames(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "usage", "openai-responses-sse.recorded.sse"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	sink := &memoryUsageSink{}
	constructionCtx := logging.WithRequestID(context.Background(), "sse-inbound-correlation")
	adapter := enabledOpenAIUsageStream(constructionCtx, sink)
	reader := sse.NewReader(file, nil)
	before := time.Now()
	for {
		frame, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got := adapter.Transform(context.Background(), frame)
		if !reflect.DeepEqual(got, []sse.Frame{frame}) {
			t.Fatalf("Transform(%q) = %#v, want exact original frame %#v", frame.Type, got, frame)
		}
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %d, want 1", len(turns))
	}
	turn := turns[0]
	after := time.Now()
	if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "sse-inbound-correlation" ||
		turn.ResponseID != "resp_redacted_recorded_sse" || turn.Model != "gpt-5.6-sol" ||
		turn.Transport != usage.TransportSSE || turn.TurnIndex != 0 {
		t.Errorf("Turn envelope = %+v", turn)
	}
	native, ok := turn.Usage.(usage.OpenAIUsage)
	if !ok {
		t.Fatalf("Usage = %T, want usage.OpenAIUsage", turn.Usage)
	}
	if native.InputTokens != 12 || native.OutputTokens != 20 || pointerValue(native.CachedTokens) != 0 ||
		pointerValue(native.CacheWriteTokens) != 0 || pointerValue(native.ReasoningTokens) != 12 ||
		pointerValue(native.TotalTokens) != 32 {
		t.Errorf("native usage = %+v", native)
	}
}

func TestOpenAIUsageMeterJoinsRepeatedCRLFDataFieldsWithoutChangingTheFrame(t *testing.T) {
	sink := &memoryUsageSink{}
	adapter := enabledOpenAIUsageStream(context.Background(), sink)
	frame := sse.Frame{
		Type: "response.completed",
		Raw:  []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata:\t\"response\":{\"id\":\"resp-multiline\",\"model\":\"reported-model\",\"status\":\"completed\",\r\ndata: \"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\r\n\r\n"),
	}

	got := adapter.Transform(context.Background(), frame)

	if !reflect.DeepEqual(got, []sse.Frame{frame}) {
		t.Fatalf("Transform() = %#v, want exact original frame %#v", got, frame)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].ResponseID != "resp-multiline" || turns[0].Model != "reported-model" ||
		turns[0].Transport != usage.TransportSSE || turns[0].TurnIndex != 0 {
		t.Fatalf("recorded Turns = %+v", turns)
	}
	native := turns[0].Usage.(usage.OpenAIUsage)
	if native.InputTokens != 3 || native.OutputTokens != 4 {
		t.Errorf("native usage = %+v", native)
	}
}

func TestOpenAIUsageMeterDeclinesNonqualifyingSSEFramesExactly(t *testing.T) {
	tests := []struct {
		name string
		kind string
		raw  string
	}{
		{name: "missing terminal", kind: "response.created", raw: "event: response.created\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"},
		{name: "absent data", kind: "response.completed", raw: "event: response.completed\nid: opaque\n\n"},
		{name: "empty data", kind: "response.completed", raw: "event: response.completed\ndata:\n\n"},
		{name: "malformed data", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\n\n"},
		{name: "type conflict", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.failed\"}\n\n"},
		{name: "missing response", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
		{name: "null response", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":null}\n\n"},
		{name: "missing response id", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"},
		{name: "empty response model", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"},
		{name: "missing response status", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"},
		{name: "nested response incomplete", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"incomplete\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"},
		{name: "missing core count", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"output_tokens\":2}}}\n\n"},
		{name: "invalid core count", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":-1,\"output_tokens\":2}}}\n\n"},
		{name: "invalid optional count", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":1.0}}}\n\n"},
		{name: "invalid details object", kind: "response.completed", raw: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"input_tokens_details\":[]}}}\n\n"},
		{name: "failed terminal", kind: "response.failed", raw: "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n"},
		{name: "incomplete terminal", kind: "response.incomplete", raw: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n"},
		{name: "error terminal", kind: "error", raw: "event: error\ndata: {\"type\":\"error\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &memoryUsageSink{}
			adapter := enabledOpenAIUsageStream(context.Background(), sink)
			frame := sse.Frame{Type: test.kind, Raw: []byte(test.raw)}

			got := adapter.Transform(context.Background(), frame)

			if !reflect.DeepEqual(got, []sse.Frame{frame}) {
				t.Errorf("Transform() = %#v, want exact original frame %#v", got, frame)
			}
			if turns := sink.snapshot(); len(turns) != 0 {
				t.Errorf("declined frame recorded Turns: %+v", turns)
			}
		})
	}
}

func TestOpenAIUsageMeterKeepsSSECompletionsSelfContainedAcrossInterleavingAndDuplicates(t *testing.T) {
	sink := &memoryUsageSink{}
	constructionCtx := logging.WithRequestID(context.Background(), "captured-once")
	adapter := enabledOpenAIUsageStream(constructionCtx, sink)
	frames := []sse.Frame{
		{Type: "response.failed", Raw: []byte("event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-a\",\"model\":\"model-a\",\"status\":\"completed\",\"usage\":{\"input_tokens\":41}}}\n\n")},
		{Type: "response.incomplete", Raw: []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-b\",\"model\":\"model-b\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":8}}}\n\n")},
		{Type: "error", Raw: []byte("event: error\ndata: {\"type\":\"error\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-a\",\"model\":\"model-a\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-b\",\"model\":\"model-b-duplicate\",\"status\":\"completed\",\"usage\":{\"input_tokens\":70,\"output_tokens\":80}}}\n\n")},
	}
	for _, frame := range frames {
		hookCtx := logging.WithRequestID(context.Background(), "must-not-replace-construction-correlation")
		if got := adapter.Transform(hookCtx, frame); !reflect.DeepEqual(got, []sse.Frame{frame}) {
			t.Fatalf("Transform(%q) = %#v, want exact original frame %#v", frame.Type, got, frame)
		}
	}

	turns := sink.snapshot()
	if len(turns) != 3 {
		t.Fatalf("recorded Turns = %+v, want three self-contained completions", turns)
	}
	wantIDs := []string{"resp-b", "resp-a", "resp-b"}
	wantModels := []string{"model-b", "model-a", "model-b-duplicate"}
	wantCounts := [][2]int64{{7, 8}, {1, 2}, {70, 80}}
	for i, turn := range turns {
		native := turn.Usage.(usage.OpenAIUsage)
		if turn.RequestID != "captured-once" || turn.ResponseID != wantIDs[i] || turn.Model != wantModels[i] ||
			turn.Transport != usage.TransportSSE || turn.TurnIndex != i ||
			native.InputTokens != wantCounts[i][0] || native.OutputTokens != wantCounts[i][1] {
			t.Errorf("Turn %d = %+v usage=%+v", i, turn, native)
		}
	}
}

func TestOpenAIUsageMeterAdvancesDuplicateSSESubmissionOrdinalWhenSinkDrops(t *testing.T) {
	sink := &dropFirstUsageSink{}
	adapter := enabledOpenAIUsageStream(context.Background(), sink)
	frame := sse.Frame{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-duplicate\",\"model\":\"reported\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")}

	for range 2 {
		if got := adapter.Transform(context.Background(), frame); !reflect.DeepEqual(got, []sse.Frame{frame}) {
			t.Fatalf("Transform() = %#v, want exact original frame %#v", got, frame)
		}
	}

	turns := sink.kept.snapshot()
	if sink.calls != 2 {
		t.Fatalf("Sink.Record calls = %d, want one per duplicate", sink.calls)
	}
	if len(turns) != 1 || turns[0].ResponseID != "resp-duplicate" || turns[0].TurnIndex != 1 || turns[0].Transport != usage.TransportSSE {
		t.Fatalf("retained Turns = %+v, want second duplicate submission at ordinal one", turns)
	}
}

func TestOpenAIUsageMeterObservesUpstreamBasisInsideItemIDStabilizer(t *testing.T) {
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[1].Enabled = true
	registry[len(registry)-1].Enabled = true
	if registry[1].Name != "responses-item-id-stabilizer" || registry[len(registry)-1].Name != "usage-meter" {
		t.Fatalf("registry order = %+v, want stabilizer outer and meter innermost", registry)
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).StreamAdapter(context.Background(), nil)
	added := sse.Frame{Type: "response.output_item.added", Raw: []byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"item-upstream-first\"}}\n\n")}
	if got := adapter.Transform(context.Background(), added); !reflect.DeepEqual(got, []sse.Frame{added}) {
		t.Fatalf("first item frame = %#v, want verbatim %#v", got, added)
	}
	completed := sse.Frame{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-upstream\",\"model\":\"model-upstream\",\"status\":\"completed\",\"output\":[{\"id\":\"item-upstream-churned\",\"content\":[ 1, 2 ]}],\"usage\":{\"input_tokens\":13,\"output_tokens\":21}}}\n\n")}

	got := adapter.Transform(context.Background(), completed)

	if len(got) != 1 || reflect.DeepEqual(got[0], completed) {
		t.Fatalf("stabilized completion = %#v, want one output-id-altered frame", got)
	}
	payload, present := got[0].Data()
	if !present {
		t.Fatal("stabilized completion lost data")
	}
	output := responseOutput(t, payload)
	if id := rawString(t, rawObject(t, output[0])["id"]); id != "item-upstream-first" {
		t.Errorf("stabilized output item id = %q, want first upstream id", id)
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %+v", turns)
	}
	native := turns[0].Usage.(usage.OpenAIUsage)
	if turns[0].ResponseID != "response-upstream" || turns[0].Model != "model-upstream" ||
		turns[0].Transport != usage.TransportSSE || native.InputTokens != 13 || native.OutputTokens != 21 {
		t.Errorf("upstream-basis Turn = %+v usage=%+v", turns[0], native)
	}
}

func TestOpenAIUsageMeterRecordsRecordedBufferedCompletionWithoutChangingBody(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "usage", "openai-responses-buffered.recorded.json"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &memoryUsageSink{}
	ctx := logging.WithRequestID(context.Background(), "inbound-correlation")
	meter := newOpenAIUsageMeter(ctx, sink)
	body := &Body{Bytes: append([]byte(nil), fixture...)}
	before := time.Now()

	if err := meter.TransformBuffered(context.Background(), body); err != nil {
		t.Fatalf("TransformBuffered: %v", err)
	}
	if !reflect.DeepEqual(body.Bytes, fixture) {
		t.Fatalf("buffered payload changed:\n got: %q\nwant: %q", body.Bytes, fixture)
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %d, want 1", len(turns))
	}
	turn := turns[0]
	after := time.Now()
	if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "inbound-correlation" || turn.ResponseID != "resp_redacted_recorded_buffered" ||
		turn.Model != "gpt-5.6-sol" || turn.Transport != usage.TransportBuffered || turn.TurnIndex != 0 {
		t.Errorf("Turn envelope = %+v", turn)
	}
	native, ok := turn.Usage.(usage.OpenAIUsage)
	if !ok {
		t.Fatalf("Usage = %T, want usage.OpenAIUsage", turn.Usage)
	}
	if native.InputTokens != 12 || native.OutputTokens != 6 || pointerValue(native.CachedTokens) != 0 ||
		pointerValue(native.CacheWriteTokens) != 0 || pointerValue(native.ReasoningTokens) != 0 ||
		pointerValue(native.TotalTokens) != 18 {
		t.Errorf("native usage = %+v", native)
	}
}

func TestOpenAIUsageMeterPreservesMissingOptionalCountsAndReportedZero(t *testing.T) {
	sink := &memoryUsageSink{}
	meter := newOpenAIUsageMeter(context.Background(), sink)
	bodies := [][]byte{
		[]byte(`{"id":"resp-null","model":"reported","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":null},"output_tokens_details":null,"total_tokens":null}}`),
		[]byte(`{"id":"resp-zero","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}`),
	}
	for _, raw := range bodies {
		body := &Body{Bytes: append([]byte(nil), raw...)}
		if err := meter.TransformBuffered(context.Background(), body); err != nil || !reflect.DeepEqual(body.Bytes, raw) {
			t.Fatalf("TransformBuffered = %v body %q, want nil and unchanged %q", err, body.Bytes, raw)
		}
	}
	turns := sink.snapshot()
	if len(turns) != 2 || turns[0].RequestID != "" || turns[0].TurnIndex != 0 || turns[1].TurnIndex != 1 {
		t.Fatalf("Turn envelopes = %+v", turns)
	}
	missing := turns[0].Usage.(usage.OpenAIUsage)
	if missing.CachedTokens != nil || missing.CacheWriteTokens != nil || missing.ReasoningTokens != nil || missing.TotalTokens != nil {
		t.Errorf("missing/null optionals = %+v, want nil", missing)
	}
	reported := turns[1].Usage.(usage.OpenAIUsage)
	if reported.CachedTokens == nil || reported.CacheWriteTokens == nil || reported.ReasoningTokens == nil || reported.TotalTokens == nil {
		t.Errorf("reported zeros = %+v, want non-nil pointers", reported)
	}
}

func TestOpenAIUsageMeterDeclinesInvalidAndIncompleteCandidatesWithoutErrorOrMutation(t *testing.T) {
	tests := map[string]string{
		"malformed":           `{"id":`,
		"wrong object type":   `[]`,
		"missing id":          `{"model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`,
		"empty model":         `{"id":"r","model":"","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`,
		"incomplete":          `{"id":"r","model":"m","status":"incomplete","usage":{"input_tokens":1,"output_tokens":2}}`,
		"missing required":    `{"id":"r","model":"m","status":"completed","usage":{"output_tokens":2}}`,
		"null required":       `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":null,"output_tokens":2}}`,
		"wrong required type": `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":"1","output_tokens":2}}`,
		"fractional required": `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":1.0,"output_tokens":2}}`,
		"negative required":   `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":-1,"output_tokens":2}}`,
		"overflow required":   `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":9223372036854775808,"output_tokens":2}}`,
		"wrong optional type": `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":"3"}}`,
		"negative optional":   `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":-1}}}`,
		"overflow optional":   `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":9223372036854775808}}}`,
		"wrong detail object": `{"id":"r","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":[]}}`,
	}
	for name, literal := range tests {
		t.Run(name, func(t *testing.T) {
			sink := &memoryUsageSink{}
			meter := newOpenAIUsageMeter(context.Background(), sink)
			raw := []byte(literal)
			body := &Body{Bytes: append([]byte(nil), raw...)}
			if err := meter.TransformBuffered(context.Background(), body); err != nil {
				t.Fatalf("TransformBuffered error = %v, want nil decline", err)
			}
			if !reflect.DeepEqual(body.Bytes, raw) {
				t.Errorf("declined body = %q, want unchanged %q", body.Bytes, raw)
			}
			if turns := sink.snapshot(); len(turns) != 0 {
				t.Errorf("declined candidate recorded %+v", turns)
			}
		})
	}
}

func TestOpenAIUsageMeterDoesNotAddCrossFieldArithmeticValidation(t *testing.T) {
	sink := &memoryUsageSink{}
	meter := newOpenAIUsageMeter(context.Background(), sink)
	body := &Body{Bytes: []byte(`{"id":"resp-native","model":"reported","status":"completed","usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":9},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":7},"total_tokens":1}}`)}

	if err := meter.TransformBuffered(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if turns := sink.snapshot(); len(turns) != 1 {
		t.Fatalf("native subset report recorded %d Turns, want 1 without consistency rejection", len(turns))
	}
}

func TestCanonicalRegistryKeepsUsageMeterExactLastWithOnlyOpenAISSEActive(t *testing.T) {
	withoutSink := CanonicalRegistry(nil)
	if len(withoutSink) != 3 || withoutSink[2].Name != "usage-meter" || withoutSink[2].Enabled {
		t.Fatalf("CanonicalRegistry(nil) = %+v, want disabled usage-meter last", withoutSink)
	}
	sink := &memoryUsageSink{}
	withSink := CanonicalRegistry(sink)
	registration := withSink[len(withSink)-1]
	if registration.Name != "usage-meter" || registration.Enabled || registration.Scope == nil {
		t.Fatalf("usage registration = %+v", registration)
	}
	for _, tc := range []struct {
		name    string
		surface endpoint.Surface
		route   endpoint.Route
		want    bool
	}{
		{name: "Anthropic Messages", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicMessages, want: true},
		{name: "OpenAI Responses", surface: endpoint.OpenAI, route: endpoint.RouteOpenAIResponses, want: true},
		{name: "Anthropic count tokens", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicCountTokens},
		{name: "Anthropic catalog source", surface: endpoint.Anthropic, route: endpoint.RouteModels},
		{name: "Anthropic with Responses route", surface: endpoint.Anthropic, route: endpoint.RouteOpenAIResponses},
		{name: "OpenAI with Messages route", surface: endpoint.OpenAI, route: endpoint.RouteAnthropicMessages},
		{name: "OpenAI catalog source", surface: endpoint.OpenAI, route: endpoint.RouteModels},
		{name: "GitHub Copilot support", surface: endpoint.GitHubCopilot, route: endpoint.RouteModels},
		{name: "GitHub Copilot with Responses route", surface: endpoint.GitHubCopilot, route: endpoint.RouteOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registration.Scope(tc.surface, tc.route); got != tc.want {
				t.Errorf("Scope(%q, %q) = %t, want %t", tc.surface, tc.route, got, tc.want)
			}
		})
	}

	instances := []struct {
		name    string
		value   any
		wantSSE bool
	}{
		{name: "Anthropic remains buffered only", value: registration.New(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages)},
		{name: "OpenAI adds SSE", value: registration.New(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses), wantSSE: true},
	}
	for _, instance := range instances {
		t.Run(instance.name, func(t *testing.T) {
			if _, ok := instance.value.(BufferedTransformer); !ok {
				t.Fatalf("usage-meter instance = %T, want BufferedTransformer", instance.value)
			}
			if _, ok := instance.value.(EventTransformer); ok != instance.wantSSE {
				t.Fatalf("usage-meter instance %T EventTransformer = %t, want %t", instance.value, ok, instance.wantSSE)
			}
			if _, ok := instance.value.(ServerMessageTransformer); ok {
				t.Fatalf("usage-meter instance = %T, issue #199 must not install WebSocket hooks", instance.value)
			}
			if _, ok := instance.value.(ClientMessageTransformer); ok {
				t.Fatalf("usage-meter instance = %T, issue #199 must not install client-message hooks", instance.value)
			}
			if _, ok := instance.value.(StreamFinalizer); ok {
				t.Fatalf("usage-meter instance = %T, issue #199 must not install finalizers", instance.value)
			}
			if _, ok := instance.value.(PreludeTransformer); ok {
				t.Fatalf("usage-meter instance = %T, issue #199 must not install Prelude hooks", instance.value)
			}
		})
	}
}

func pointerValue(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
