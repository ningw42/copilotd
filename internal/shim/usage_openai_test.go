package shim

import (
	"bytes"
	"context"
	"encoding/json"
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

func enabledOpenAIUsageWSServer(ctx context.Context, sink usage.Sink) MessageTransform {
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	return registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter(ctx, nil)
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
	if turn.OpenAIServiceTier != nil {
		t.Errorf("recorded fixture OpenAIServiceTier = %q, want unavailable", *turn.OpenAIServiceTier)
	}
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
	// Synthetic interleaved completions exercise per-completion tier ownership;
	// no recorded fixture is modified.
	frames := []sse.Frame{
		{Type: "response.failed", Raw: []byte("event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-a\",\"model\":\"model-a\",\"status\":\"completed\",\"usage\":{\"input_tokens\":41}}}\n\n")},
		{Type: "response.incomplete", Raw: []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-b\",\"model\":\"model-b\",\"status\":\"completed\",\"service_tier\":\"priority\",\"usage\":{\"input_tokens\":7,\"output_tokens\":8}}}\n\n")},
		{Type: "error", Raw: []byte("event: error\ndata: {\"type\":\"error\"}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-a\",\"model\":\"model-a\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")},
		{Type: "response.completed", Raw: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-b\",\"model\":\"model-b-duplicate\",\"status\":\"completed\",\"service_tier\":\"flex\",\"usage\":{\"input_tokens\":70,\"output_tokens\":80}}}\n\n")},
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
	wantTiers := []*string{stringPointer("priority"), nil, stringPointer("flex")}
	for i, turn := range turns {
		native := turn.Usage.(usage.OpenAIUsage)
		if turn.RequestID != "captured-once" || turn.ResponseID != wantIDs[i] || turn.Model != wantModels[i] ||
			turn.Transport != usage.TransportSSE || turn.TurnIndex != i ||
			!reflect.DeepEqual(turn.OpenAIServiceTier, wantTiers[i]) ||
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

func TestOpenAIUsageMeterRecordsRecordedWebSocketCompletionWithoutChangingMessages(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "usage", "openai-responses-websocket.recorded.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	fixture = bytes.TrimSuffix(fixture, []byte("\n"))
	sink := &memoryUsageSink{}
	constructionCtx := logging.WithRequestID(context.Background(), "websocket-handshake-correlation")
	adapter := enabledOpenAIUsageWSServer(constructionCtx, sink)
	before := time.Now()
	for _, kind := range []MessageKind{MessageText, MessageBinary} {
		message := &Message{Kind: kind, Data: append([]byte(nil), fixture...)}
		executionCtx := logging.WithRequestID(context.Background(), "must-not-replace-handshake-correlation")

		if emit := adapter(executionCtx, message); !emit {
			t.Fatalf("TransformServerMessage(%v) returned emit=false", kind)
		}
		if message.Kind != kind || !bytes.Equal(message.Data, fixture) {
			t.Fatalf("TransformServerMessage(%v) changed Message to kind=%v data=%q", kind, message.Kind, message.Data)
		}
	}

	turns := sink.snapshot()
	if len(turns) != 2 {
		t.Fatalf("recorded Turns = %d, want one for each qualifying duplicate Message", len(turns))
	}
	after := time.Now()
	for index, turn := range turns {
		if turn.OpenAIServiceTier != nil {
			t.Errorf("Turn %d recorded fixture OpenAIServiceTier = %q, want unavailable", index, *turn.OpenAIServiceTier)
		}
		if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "websocket-handshake-correlation" ||
			turn.ResponseID != "resp_redacted_recorded_websocket" || turn.Model != "gpt-5.6-sol" ||
			turn.Transport != usage.TransportWebSocket || turn.TurnIndex != index {
			t.Errorf("Turn %d envelope = %+v", index, turn)
		}
		native, ok := turn.Usage.(usage.OpenAIUsage)
		if !ok {
			t.Fatalf("Turn %d Usage = %T, want usage.OpenAIUsage", index, turn.Usage)
		}
		if native.InputTokens != 12 || native.OutputTokens != 20 || pointerValue(native.CachedTokens) != 0 ||
			pointerValue(native.CacheWriteTokens) != 0 || pointerValue(native.ReasoningTokens) != 12 ||
			pointerValue(native.TotalTokens) != 32 {
			t.Errorf("Turn %d native usage = %+v", index, native)
		}
	}
}

func TestOpenAIUsageMeterKeepsWebSocketCompletionsSelfContainedAcrossInvalidAndTerminalMessages(t *testing.T) {
	sink := &memoryUsageSink{}
	adapter := enabledOpenAIUsageWSServer(context.Background(), sink)
	// Synthetic WebSocket literals cover independent present/missing tier
	// evidence alongside malformed and terminal Messages.
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"created","model":"created-model","status":"in_progress","usage":{"input_tokens":99,"output_tokens":99}}}`),
		[]byte(`{"type":"vendor.completed","response":{"id":"wrong-event-type","model":"bad","status":"completed","usage":{"input_tokens":99,"output_tokens":99}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"malformed"`),
		[]byte(`{"type":"response.completed","response":{"id":"missing-core","model":"bad","status":"completed","usage":{"input_tokens":41}}}`),
		[]byte(`{"type":"response.failed","response":{"id":"failed","model":"bad","status":"failed","usage":{"input_tokens":1,"output_tokens":2}}}`),
		[]byte(`{"type":"response.incomplete","response":{"id":"incomplete","model":"bad","status":"incomplete","usage":{"input_tokens":3,"output_tokens":4}}}`),
		[]byte(`{"type":"error","error":{"message":"session event"}}`),
		[]byte(`{"type":"response.completed","response":{"id":"invalid-optional","model":"bad","status":"completed","usage":{"input_tokens":5,"output_tokens":6,"total_tokens":"11"}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-a","model":"reported-a","status":"completed","service_tier":"priority","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":null},"output_tokens_details":null,"total_tokens":null}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"negative-core","model":"bad","status":"completed","usage":{"input_tokens":-1,"output_tokens":2}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-b","model":"reported-b","status":"completed","usage":{"input_tokens":7,"output_tokens":8,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-b","model":"reported-b-duplicate","status":"completed","service_tier":"fast","usage":{"input_tokens":70,"output_tokens":80}}}`),
	}
	for index, payload := range payloads {
		kind := MessageText
		if index%2 == 1 {
			kind = MessageBinary
		}
		message := &Message{Kind: kind, Data: append([]byte(nil), payload...)}
		executionCtx := logging.WithRequestID(context.Background(), "must-not-backfill-missing-construction-correlation")
		if emit := adapter(executionCtx, message); !emit {
			t.Fatalf("Message %d returned emit=false", index)
		}
		if message.Kind != kind || !bytes.Equal(message.Data, payload) {
			t.Fatalf("Message %d changed to kind=%v data=%q", index, message.Kind, message.Data)
		}
	}

	turns := sink.snapshot()
	if len(turns) != 3 {
		t.Fatalf("recorded Turns = %+v, want only three independently valid completions", turns)
	}
	wantIDs := []string{"resp-a", "resp-b", "resp-b"}
	wantModels := []string{"reported-a", "reported-b", "reported-b-duplicate"}
	wantCounts := [][2]int64{{0, 0}, {7, 8}, {70, 80}}
	wantTiers := []*string{stringPointer("priority"), nil, stringPointer("fast")}
	for index, turn := range turns {
		native := turn.Usage.(usage.OpenAIUsage)
		if turn.RequestID != "" || turn.ResponseID != wantIDs[index] || turn.Model != wantModels[index] ||
			turn.Transport != usage.TransportWebSocket || turn.TurnIndex != index ||
			!reflect.DeepEqual(turn.OpenAIServiceTier, wantTiers[index]) ||
			native.InputTokens != wantCounts[index][0] || native.OutputTokens != wantCounts[index][1] {
			t.Errorf("Turn %d = %+v usage=%+v", index, turn, native)
		}
	}
	missing := turns[0].Usage.(usage.OpenAIUsage)
	if missing.CachedTokens != nil || missing.CacheWriteTokens != nil || missing.ReasoningTokens != nil || missing.TotalTokens != nil {
		t.Errorf("omitted/null optional counts = %+v, want nil", missing)
	}
	reportedZero := turns[1].Usage.(usage.OpenAIUsage)
	if reportedZero.CachedTokens == nil || reportedZero.CacheWriteTokens == nil || reportedZero.ReasoningTokens == nil || reportedZero.TotalTokens == nil {
		t.Errorf("reported zero optional counts = %+v, want non-nil pointers", reportedZero)
	}
}

func TestOpenAIUsageMeterAdvancesDuplicateWebSocketSubmissionOrdinalWhenSinkDrops(t *testing.T) {
	sink := &dropFirstUsageSink{}
	adapter := enabledOpenAIUsageWSServer(context.Background(), sink)
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-duplicate","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)

	for range 2 {
		message := &Message{Kind: MessageText, Data: append([]byte(nil), payload...)}
		if emit := adapter(context.Background(), message); !emit || message.Kind != MessageText || !bytes.Equal(message.Data, payload) {
			t.Fatalf("TransformServerMessage() = emit:%t Message:%+v, want true and exact input", emit, message)
		}
	}

	turns := sink.kept.snapshot()
	if sink.calls != 2 {
		t.Fatalf("Sink.Record calls = %d, want one per qualifying duplicate", sink.calls)
	}
	if len(turns) != 1 || turns[0].ResponseID != "resp-duplicate" || turns[0].TurnIndex != 1 || turns[0].Transport != usage.TransportWebSocket {
		t.Fatalf("retained Turns = %+v, want second duplicate submission at ordinal one", turns)
	}
}

type lastUsageSink struct {
	calls int
	last  usage.Turn
}

func (s *lastUsageSink) Record(turn usage.Turn) {
	s.calls++
	s.last = turn
}

func TestOpenAIUsageMeterHandlesSustainedWebSocketTurnsWithoutHistoryDependence(t *testing.T) {
	const completions = 4096
	sink := &lastUsageSink{}
	adapter := enabledOpenAIUsageWSServer(context.Background(), sink)
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-repeated","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
	for index := range completions {
		message := &Message{Kind: MessageBinary, Data: append([]byte(nil), payload...)}
		if emit := adapter(context.Background(), message); !emit || message.Kind != MessageBinary || !bytes.Equal(message.Data, payload) {
			t.Fatalf("completion %d did not pass through exactly", index)
		}
	}
	if sink.calls != completions || sink.last.ResponseID != "resp-repeated" || sink.last.TurnIndex != completions-1 || sink.last.Transport != usage.TransportWebSocket {
		t.Fatalf("sustained submissions = calls:%d last:%+v", sink.calls, sink.last)
	}
}

func TestOpenAIUsageMeterObservesUpstreamBasisInsideItemIDStabilizerWebSocket(t *testing.T) {
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[1].Enabled = true
	registry[len(registry)-1].Enabled = true
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter(context.Background(), nil)
	addedPayload := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item-upstream-first"}}`)
	added := &Message{Kind: MessageText, Data: append([]byte(nil), addedPayload...)}
	if emit := adapter(context.Background(), added); !emit || added.Kind != MessageText || !bytes.Equal(added.Data, addedPayload) {
		t.Fatalf("first upstream item Message = emit:%t %+v, want exact input", emit, added)
	}
	completionPayload := []byte(`{"type":"response.completed","response":{"id":"response-upstream","model":"model-upstream","status":"completed","output":[{"id":"item-upstream-churned","content":[1,2]}],"usage":{"input_tokens":13,"output_tokens":21}}}`)
	completion := &Message{Kind: MessageBinary, Data: append([]byte(nil), completionPayload...)}

	if emit := adapter(context.Background(), completion); !emit {
		t.Fatal("completion returned emit=false")
	}
	if completion.Kind != MessageBinary || bytes.Equal(completion.Data, completionPayload) {
		t.Fatalf("stabilized completion = kind:%v data:%s, want binary with output-id alteration", completion.Kind, completion.Data)
	}
	var want, got map[string]any
	if err := json.Unmarshal(completionPayload, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(completion.Data, &got); err != nil {
		t.Fatal(err)
	}
	wantResponse := want["response"].(map[string]any)
	wantOutput := wantResponse["output"].([]any)
	wantOutput[0].(map[string]any)["id"] = "item-upstream-first"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stabilizer changed fields beyond the output id:\n got: %#v\nwant: %#v", got, want)
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %+v", turns)
	}
	native := turns[0].Usage.(usage.OpenAIUsage)
	if turns[0].ResponseID != "response-upstream" || turns[0].Model != "model-upstream" ||
		turns[0].Transport != usage.TransportWebSocket || native.InputTokens != 13 || native.OutputTokens != 21 {
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
	if turn.OpenAIServiceTier != nil {
		t.Errorf("recorded fixture OpenAIServiceTier = %q, want unavailable", *turn.OpenAIServiceTier)
	}
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

func TestCanonicalRegistryKeepsUsageMeterExactLastWithOpenAIStreamingHooksActive(t *testing.T) {
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
		wantWS  bool
	}{
		{name: "Anthropic adds SSE without WebSocket", value: registration.New(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages), wantSSE: true},
		{name: "OpenAI adds SSE and WebSocket", value: registration.New(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses), wantSSE: true, wantWS: true},
	}
	for _, instance := range instances {
		t.Run(instance.name, func(t *testing.T) {
			if _, ok := instance.value.(RequestTransformer); !ok {
				t.Fatalf("usage-meter instance = %T, want RequestTransformer", instance.value)
			}
			if _, ok := instance.value.(BufferedTransformer); !ok {
				t.Fatalf("usage-meter instance = %T, want BufferedTransformer", instance.value)
			}
			if _, ok := instance.value.(EventTransformer); ok != instance.wantSSE {
				t.Fatalf("usage-meter instance %T EventTransformer = %t, want %t", instance.value, ok, instance.wantSSE)
			}
			if _, ok := instance.value.(ServerMessageTransformer); ok != instance.wantWS {
				t.Fatalf("usage-meter instance %T ServerMessageTransformer = %t, want %t", instance.value, ok, instance.wantWS)
			}
			if _, ok := instance.value.(ClientMessageTransformer); ok {
				t.Fatalf("usage-meter instance = %T, must not install client-message hooks", instance.value)
			}
			if _, ok := instance.value.(StreamFinalizer); ok {
				t.Fatalf("usage-meter instance = %T, must not install finalizers", instance.value)
			}
			if _, ok := instance.value.(PreludeTransformer); ok {
				t.Fatalf("usage-meter instance = %T, must not install Prelude hooks", instance.value)
			}
		})
	}
}

type syntheticOpenAITransportCase struct {
	name      string
	transport usage.Transport
	kind      MessageKind
}

func syntheticOpenAITransportCases() []syntheticOpenAITransportCase {
	return []syntheticOpenAITransportCase{
		{name: "buffered", transport: usage.TransportBuffered},
		{name: "SSE", transport: usage.TransportSSE},
		{name: "WebSocket text", transport: usage.TransportWebSocket, kind: MessageText},
		{name: "WebSocket binary", transport: usage.TransportWebSocket, kind: MessageBinary},
	}
}

func TestOpenAIUsageMeterServiceTierEvidenceMatrixAcrossTransports(t *testing.T) {
	// These synthetic member literals exercise the provider contract; recorded
	// Copilot fixtures remain unchanged and continue to represent absent evidence.
	tests := []struct {
		name    string
		members []byte
		want    *string
	}{
		{name: "absent"},
		{name: "null", members: []byte(`"service_tier":null,`)},
		{name: "empty", members: []byte(`"service_tier":"",`), want: stringPointer("")},
		{name: "known default", members: []byte(`"service_tier":"default",`), want: stringPointer("default")},
		{name: "known priority", members: []byte(`"service_tier":"priority",`), want: stringPointer("priority")},
		{name: "future", members: []byte(`"service_tier":"turbo-next",`), want: stringPointer("turbo-next")},
		{name: "whitespace control NUL and Unicode", members: []byte(`"service_tier":"  \t\u0000雪\n  ",`), want: stringPointer("  \t\x00雪\n  ")},
		{name: "escaped string", members: []byte(`"service_tier":"\u0070riority\/x",`), want: stringPointer("priority/x")},
		{name: "escaped exact key", members: []byte(`"\u0073ervice_tier":"fast",`), want: stringPointer("fast")},
		{name: "nested and differently cased only", members: []byte(`"metadata":{"service_tier":"nested"},"Service_Tier":"cased",`)},
		{name: "number", members: []byte(`"service_tier":42,`)},
		{name: "boolean", members: []byte(`"service_tier":true,`)},
		{name: "array", members: []byte(`"service_tier":["priority"],`)},
		{name: "object", members: []byte(`"service_tier":{"value":"priority"},`)},
		{name: "identical duplicate", members: []byte(`"service_tier":"priority","service_tier":"priority",`)},
		{name: "conflicting duplicate", members: []byte(`"service_tier":"priority","service_tier":"default",`)},
		{name: "escaped equivalent duplicate", members: []byte(`"service_tier":"priority","\u0073ervice_tier":"priority",`)},
		{name: "invalid UTF-8", members: []byte("\"service_tier\":\"bad-\xff\",")},
		{name: "lone high surrogate", members: []byte(`"service_tier":"\uD800",`)},
		{name: "lone low surrogate", members: []byte(`"service_tier":"\uDC00",`)},
		{name: "valid surrogate pair", members: []byte(`"service_tier":"\uD83D\uDE80",`), want: stringPointer("🚀")},
		{name: "genuine literal replacement character", members: []byte(`"service_tier":"�",`), want: stringPointer("�")},
		{name: "genuine escaped replacement character", members: []byte(`"service_tier":"\uFFFD",`), want: stringPointer("�")},
		{name: "literal backslash-u text", members: []byte(`"service_tier":"\\uD800",`), want: stringPointer(`\uD800`)},
		{name: "unrelated lone surrogate", members: []byte(`"service_tier":"priority","unrelated":"\uD800",`), want: stringPointer("priority")},
		{name: "unrelated invalid UTF-8", members: []byte("\"service_tier\":\"priority\",\"unrelated\":\"\xff\","), want: stringPointer("priority")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := syntheticOpenAIResponse(tc.members)
			for _, transport := range syntheticOpenAITransportCases() {
				t.Run(transport.name, func(t *testing.T) {
					turn := observeSyntheticOpenAICompletions(t, transport, response)[0]
					if !reflect.DeepEqual(turn.OpenAIServiceTier, tc.want) {
						t.Errorf("OpenAIServiceTier = %v, want %v", turn.OpenAIServiceTier, tc.want)
					}
					native := turn.Usage.(usage.OpenAIUsage)
					if native.InputTokens != 1 || native.OutputTokens != 2 {
						t.Errorf("malformed optional evidence changed native usage: %+v", native)
					}
				})
			}
		})
	}
}

func TestOpenAIUsageMeterIgnoresEarlierEventTierEvidence(t *testing.T) {
	// Synthetic earlier-event literals prove that only the completed Response is
	// evidence; no recorded fixture is augmented with a tier.
	t.Run("SSE", func(t *testing.T) {
		sink := &memoryUsageSink{}
		adapter := enabledOpenAIUsageStream(context.Background(), sink)
		frames := []sse.Frame{
			{
				Type: "response.created",
				Raw:  []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"service_tier\":\"priority\"}}\n\n"),
			},
			{
				Type: "response.completed",
				Raw:  []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-tier\",\"model\":\"reported\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"),
			},
		}
		for _, frame := range frames {
			want := sse.Frame{Type: frame.Type, Raw: bytes.Clone(frame.Raw)}
			if got := adapter.Transform(context.Background(), frame); !reflect.DeepEqual(got, []sse.Frame{want}) {
				t.Fatalf("frame changed: got=%#v want=%#v", got, want)
			}
			clear(frame.Raw)
		}
		turns := sink.snapshot()
		if len(turns) != 1 || turns[0].OpenAIServiceTier != nil {
			t.Fatalf("Turns = %+v, want completion-local unavailable tier", turns)
		}
	})

	t.Run("WebSocket", func(t *testing.T) {
		sink := &memoryUsageSink{}
		adapter := enabledOpenAIUsageWSServer(context.Background(), sink)
		messages := []Message{
			{Kind: MessageText, Data: []byte(`{"type":"response.created","response":{"service_tier":"priority"}}`)},
			{Kind: MessageBinary, Data: syntheticOpenAICompletedEvent(syntheticOpenAIResponse(nil))},
		}
		for index := range messages {
			message := &messages[index]
			wantKind, wantData := message.Kind, bytes.Clone(message.Data)
			if emit := adapter(context.Background(), message); !emit || message.Kind != wantKind || !bytes.Equal(message.Data, wantData) {
				t.Fatalf("Message %d changed: emit=%t got=%+v want kind=%v data=%q", index, emit, message, wantKind, wantData)
			}
			clear(message.Data)
		}
		turns := sink.snapshot()
		if len(turns) != 1 || turns[0].OpenAIServiceTier != nil {
			t.Fatalf("Turns = %+v, want completion-local unavailable tier", turns)
		}
	})
}

func TestOpenAIUsageMeterOwnsTierEvidenceAcrossInputReuseAndLaterCompletions(t *testing.T) {
	// Synthetic completions deliberately transition from present to absent tier
	// evidence while each submitted transport buffer is cleared after observation.
	first := syntheticOpenAIResponse([]byte(`"service_tier":"priority",`))
	second := syntheticOpenAIResponse(nil)
	for _, transport := range syntheticOpenAITransportCases() {
		t.Run(transport.name, func(t *testing.T) {
			turns := observeSyntheticOpenAICompletions(t, transport, first, second)
			if len(turns) != 2 || turns[0].OpenAIServiceTier == nil || *turns[0].OpenAIServiceTier != "priority" ||
				turns[1].OpenAIServiceTier != nil || turns[0].TurnIndex != 0 || turns[1].TurnIndex != 1 {
				t.Fatalf("immutable completion evidence = %+v", turns)
			}
		})
	}
}

func syntheticOpenAIResponse(members []byte) []byte {
	response := append([]byte(`{"id":"resp-tier","model":"reported","status":"completed",`), members...)
	return append(response, []byte(`"usage":{"input_tokens":1,"output_tokens":2}}`)...)
}

func syntheticOpenAICompletedEvent(response []byte) []byte {
	payload := append([]byte(`{"type":"response.completed","response":`), response...)
	return append(payload, '}')
}

func observeSyntheticOpenAICompletions(t *testing.T, transport syntheticOpenAITransportCase, responses ...[]byte) []usage.Turn {
	t.Helper()
	sink := &memoryUsageSink{}
	ctx := context.Background()
	var chain *Chain
	var stream sse.FrameTransformer
	var server MessageTransform
	switch transport.transport {
	case usage.TransportBuffered:
		registry := CanonicalRegistry(sink)
		registry[len(registry)-1].Enabled = true
		chain = registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	case usage.TransportSSE:
		stream = enabledOpenAIUsageStream(ctx, sink)
	case usage.TransportWebSocket:
		server = enabledOpenAIUsageWSServer(ctx, sink)
	default:
		t.Fatalf("unsupported synthetic transport %q", transport.transport)
	}

	for _, response := range responses {
		switch transport.transport {
		case usage.TransportBuffered:
			input, want := bytes.Clone(response), bytes.Clone(response)
			got, err := chain.RunBuffered(ctx, input)
			if err != nil || !bytes.Equal(got, want) || &got[0] != &input[0] {
				t.Fatalf("buffered completion changed: got=%q want=%q err=%v", got, want, err)
			}
			clear(input)
		case usage.TransportSSE:
			payload := syntheticOpenAICompletedEvent(response)
			frame := sse.Frame{Type: "response.completed", Raw: append(append([]byte("event: response.completed\ndata: "), payload...), '\n', '\n')}
			want := sse.Frame{Type: frame.Type, Raw: bytes.Clone(frame.Raw)}
			if got := stream.Transform(ctx, frame); !reflect.DeepEqual(got, []sse.Frame{want}) {
				t.Fatalf("SSE completion changed: got=%#v want=%#v", got, want)
			}
			clear(frame.Raw)
		case usage.TransportWebSocket:
			input := bytes.Clone(syntheticOpenAICompletedEvent(response))
			message := &Message{Kind: transport.kind, Data: input}
			want := bytes.Clone(input)
			if emit := server(ctx, message); !emit || message.Kind != transport.kind || !bytes.Equal(message.Data, want) || &message.Data[0] != &input[0] {
				t.Fatalf("WebSocket completion changed: emit=%t message=%+v want kind=%v data=%q", emit, message, transport.kind, want)
			}
			clear(input)
		}
	}

	turns := sink.snapshot()
	if len(turns) != len(responses) {
		t.Fatalf("Turns = %+v, want %d", turns, len(responses))
	}
	for index, turn := range turns {
		if turn.Transport != transport.transport || turn.TurnIndex != index {
			t.Fatalf("Turn %d = %+v, want transport %s and ordinal %d", index, turn, transport.transport, index)
		}
	}
	return turns
}

func pointerValue(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
