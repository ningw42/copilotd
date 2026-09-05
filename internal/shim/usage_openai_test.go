package shim

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
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

func TestOpenAIUsageMeterRecordsRecordedBufferedCompletionWithoutChangingBody(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "usage", "openai-responses-buffered.recorded.json"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &memoryUsageSink{}
	ctx := logging.WithRequestID(context.Background(), "inbound-correlation")
	meter := newOpenAIUsageMeter(ctx, sink, usage.TransportBuffered)
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
	meter := newOpenAIUsageMeter(context.Background(), sink, usage.TransportBuffered)
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
			meter := newOpenAIUsageMeter(context.Background(), sink, usage.TransportBuffered)
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
	meter := newOpenAIUsageMeter(context.Background(), sink, usage.TransportBuffered)
	body := &Body{Bytes: []byte(`{"id":"resp-native","model":"reported","status":"completed","usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":8,"cache_write_tokens":9},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":7},"total_tokens":1}}`)}

	if err := meter.TransformBuffered(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if turns := sink.snapshot(); len(turns) != 1 {
		t.Fatalf("native subset report recorded %d Turns, want 1 without consistency rejection", len(turns))
	}
}

func TestCanonicalRegistryKeepsBufferedOpenAIUsageMeterLastAndDisabledWithoutSink(t *testing.T) {
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
	if !registration.Scope(endpoint.OpenAI, endpoint.RouteOpenAIResponses) ||
		registration.Scope(endpoint.Anthropic, endpoint.RouteAnthropicMessages) ||
		registration.Scope(endpoint.OpenAI, endpoint.RouteModels) {
		t.Error("usage-meter scope is not buffered OpenAI Responses-only for issue #197")
	}
	instance := registration.New(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	if _, ok := instance.(BufferedTransformer); !ok {
		t.Fatalf("usage-meter instance = %T, want BufferedTransformer", instance)
	}
	if _, ok := instance.(EventTransformer); ok {
		t.Fatalf("usage-meter instance = %T, issue #197 must not install SSE hooks", instance)
	}
	if _, ok := instance.(ServerMessageTransformer); ok {
		t.Fatalf("usage-meter instance = %T, issue #197 must not install WebSocket hooks", instance)
	}
}

func pointerValue(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}
