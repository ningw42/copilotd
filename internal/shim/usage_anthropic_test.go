package shim

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
)

func enabledAnthropicUsageChain(ctx context.Context, sink usage.Sink) *Chain {
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	return registry.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)
}

func TestAnthropicUsageMeterRecordsGeneratedBufferedMessageWithoutChangingBody(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "usage", "anthropic-messages-buffered.synthetic.json"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &memoryUsageSink{}
	constructionCtx := logging.WithRequestID(context.Background(), "anthropic-inbound-correlation")
	chain := enabledAnthropicUsageChain(constructionCtx, sink)
	body := append([]byte(nil), fixture...)
	before := time.Now()

	got, err := chain.RunBuffered(logging.WithRequestID(context.Background(), "later-context-id"), body)
	if err != nil {
		t.Fatalf("RunBuffered: %v", err)
	}
	if !reflect.DeepEqual(got, fixture) || &got[0] != &body[0] {
		t.Fatalf("buffered payload changed or was replaced:\n got: %q\nwant: %q", got, fixture)
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %d, want 1", len(turns))
	}
	turn := turns[0]
	after := time.Now()
	if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "anthropic-inbound-correlation" ||
		turn.ResponseID != "msg_redacted_synthetic_buffered" || turn.Model != "claude-synthetic" ||
		turn.Transport != usage.TransportBuffered || turn.TurnIndex != 0 {
		t.Errorf("Turn envelope = %+v", turn)
	}
	native, ok := turn.Usage.(usage.AnthropicUsage)
	if !ok {
		t.Fatalf("Usage = %T, want usage.AnthropicUsage", turn.Usage)
	}
	if native.InputTokens != 12 || native.OutputTokens != 9 ||
		pointerValue(native.CacheCreationInputTokens) != 2000 || pointerValue(native.CacheReadInputTokens) != 6000 ||
		pointerValue(native.Ephemeral5mInputTokens) != 750 || pointerValue(native.Ephemeral1hInputTokens) != 1250 ||
		pointerValue(native.ThinkingTokens) != 4 {
		t.Errorf("native usage = %+v", native)
	}
}

func TestAnthropicUsageMeterPreservesNullZeroAndImmutableSnapshotsForReusedIDs(t *testing.T) {
	sink := &memoryUsageSink{}
	chain := enabledAnthropicUsageChain(context.Background(), sink)
	bodies := [][]byte{
		[]byte(`{"id":"msg-reused","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},"output_tokens_details":{"thinking_tokens":7}}}`),
		[]byte(`{"id":"msg-reused","type":"message","model":"reported","stop_reason":"max_tokens","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":0}}}`),
		[]byte(`{"id":"msg-reused","type":"message","model":"reported","stop_reason":"tool_use","usage":{"input_tokens":8,"output_tokens":9,"cache_creation_input_tokens":null,"cache_creation":null,"output_tokens_details":{"thinking_tokens":null}}}`),
	}
	for _, raw := range bodies {
		got, err := chain.RunBuffered(context.Background(), raw)
		if err != nil || !reflect.DeepEqual(got, raw) || &got[0] != &raw[0] {
			t.Fatalf("RunBuffered = %v body %q, want nil and same unchanged slice %q", err, got, raw)
		}
	}

	turns := sink.snapshot()
	if len(turns) != 3 {
		t.Fatalf("recorded Turns = %d, want 3", len(turns))
	}
	for i, turn := range turns {
		if turn.RequestID != "" || turn.ResponseID != "msg-reused" || turn.TurnIndex != i {
			t.Errorf("Turn %d envelope = %+v", i, turn)
		}
	}
	first := turns[0].Usage.(usage.AnthropicUsage)
	if pointerValue(first.CacheCreationInputTokens) != 3 || pointerValue(first.CacheReadInputTokens) != 4 ||
		pointerValue(first.Ephemeral5mInputTokens) != 5 || pointerValue(first.Ephemeral1hInputTokens) != 6 ||
		pointerValue(first.ThinkingTokens) != 7 {
		t.Errorf("first immutable snapshot changed after later records: %+v", first)
	}
	zero := turns[1].Usage.(usage.AnthropicUsage)
	if zero.InputTokens != 0 || zero.OutputTokens != 0 || zero.CacheCreationInputTokens == nil ||
		zero.CacheReadInputTokens == nil || zero.Ephemeral5mInputTokens == nil ||
		zero.Ephemeral1hInputTokens == nil || zero.ThinkingTokens == nil {
		t.Errorf("reported zeros = %+v, want required zeros and non-nil optional pointers", zero)
	}
	missing := turns[2].Usage.(usage.AnthropicUsage)
	if missing.CacheCreationInputTokens != nil || missing.CacheReadInputTokens != nil ||
		missing.Ephemeral5mInputTokens != nil || missing.Ephemeral1hInputTokens != nil || missing.ThinkingTokens != nil {
		t.Errorf("missing/null optionals = %+v, want nil", missing)
	}
}

func TestAnthropicUsageMeterDeclinesInvalidAndIncompleteCandidatesWithoutErrorOrMutation(t *testing.T) {
	tests := map[string]string{
		"malformed":                  `{"id":`,
		"wrong top-level type":       `[]`,
		"missing message type":       `{"id":"m","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		"wrong message type":         `{"id":"m","type":"error","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		"empty message id":           `{"id":"","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		"empty model":                `{"id":"m","type":"message","model":"","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
		"missing stop reason":        `{"id":"m","type":"message","model":"x","usage":{"input_tokens":1,"output_tokens":2}}`,
		"null stop reason":           `{"id":"m","type":"message","model":"x","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":2}}`,
		"empty stop reason":          `{"id":"m","type":"message","model":"x","stop_reason":"","usage":{"input_tokens":1,"output_tokens":2}}`,
		"wrong stop reason type":     `{"id":"m","type":"message","model":"x","stop_reason":7,"usage":{"input_tokens":1,"output_tokens":2}}`,
		"missing usage":              `{"id":"m","type":"message","model":"x","stop_reason":"end_turn"}`,
		"wrong usage object":         `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":[]}`,
		"missing required":           `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"output_tokens":2}}`,
		"null required":              `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":null,"output_tokens":2}}`,
		"wrong required type":        `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":"1","output_tokens":2}}`,
		"fractional required":        `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1.0,"output_tokens":2}}`,
		"negative required":          `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":-1,"output_tokens":2}}`,
		"overflow required":          `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":9223372036854775808,"output_tokens":2}}`,
		"wrong output type":          `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":"2"}}`,
		"wrong cache creation count": `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":"3"}}`,
		"negative cache read count":  `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":-1}}`,
		"wrong cache detail object":  `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation":[]}}`,
		"overflow five minute count": `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":9223372036854775808}}}`,
		"fractional one hour count":  `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_1h_input_tokens":1.0}}}`,
		"wrong output detail object": `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"output_tokens_details":[]}}`,
		"wrong thinking count":       `{"id":"m","type":"message","model":"x","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"thinking_tokens":"1"}}}`,
	}
	for name, literal := range tests {
		t.Run(name, func(t *testing.T) {
			sink := &memoryUsageSink{}
			chain := enabledAnthropicUsageChain(context.Background(), sink)
			raw := []byte(literal)
			got, err := chain.RunBuffered(context.Background(), raw)
			if err != nil {
				t.Fatalf("RunBuffered error = %v, want nil decline", err)
			}
			if !reflect.DeepEqual(got, raw) || &got[0] != &raw[0] {
				t.Errorf("declined body changed or was replaced: got %q want %q", got, raw)
			}
			if turns := sink.snapshot(); len(turns) != 0 {
				t.Errorf("declined candidate recorded %+v", turns)
			}
		})
	}
}

func TestAnthropicUsageMeterAcceptsUnknownStopReasonAndFutureFieldsWithoutArithmeticChecks(t *testing.T) {
	sink := &memoryUsageSink{}
	chain := enabledAnthropicUsageChain(context.Background(), sink)
	raw := []byte(`{"id":"msg-native","type":"message","model":"reported","stop_reason":"provider_future_reason","future_top":{"kept":true},"usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":8,"cache_creation":{"ephemeral_5m_input_tokens":9,"ephemeral_1h_input_tokens":10,"future_ttl":11},"output_tokens_details":{"thinking_tokens":7,"future_output":12},"future_usage":13}}`)

	got, err := chain.RunBuffered(context.Background(), raw)
	if err != nil || !reflect.DeepEqual(got, raw) || &got[0] != &raw[0] {
		t.Fatalf("RunBuffered = %v body %q, want nil and same unchanged slice %q", err, got, raw)
	}
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("native report with future fields recorded %d Turns, want 1", len(turns))
	}
	native := turns[0].Usage.(usage.AnthropicUsage)
	if native.InputTokens != 1 || native.OutputTokens != 2 || pointerValue(native.CacheCreationInputTokens) != 3 ||
		pointerValue(native.CacheReadInputTokens) != 8 || pointerValue(native.Ephemeral5mInputTokens) != 9 ||
		pointerValue(native.Ephemeral1hInputTokens) != 10 || pointerValue(native.ThinkingTokens) != 7 {
		t.Errorf("native report was normalized or rejected: %+v", native)
	}
}

func TestAnthropicUsageMeterInvalidCandidateDoesNotPoisonTheNextBody(t *testing.T) {
	sink := &memoryUsageSink{}
	chain := enabledAnthropicUsageChain(context.Background(), sink)
	invalid := []byte(`{"id":"bad","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":"3"}}`)
	valid := []byte(`{"id":"good","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":5}}`)

	if _, err := chain.RunBuffered(context.Background(), invalid); err != nil {
		t.Fatalf("invalid candidate returned error: %v", err)
	}
	if _, err := chain.RunBuffered(context.Background(), valid); err != nil {
		t.Fatalf("valid candidate returned error: %v", err)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].ResponseID != "good" || turns[0].TurnIndex != 0 {
		t.Fatalf("recorded Turns = %+v, want only the later valid candidate at ordinal zero", turns)
	}
}

type dropFirstUsageSink struct {
	calls int
	kept  memoryUsageSink
}

func (s *dropFirstUsageSink) Record(turn usage.Turn) {
	s.calls++
	if s.calls == 1 {
		return
	}
	s.kept.Record(turn)
}

func TestAnthropicUsageMeterAdvancesSubmissionOrdinalWhenSinkDrops(t *testing.T) {
	sink := &dropFirstUsageSink{}
	chain := enabledAnthropicUsageChain(context.Background(), sink)
	for _, id := range []string{"dropped", "kept"} {
		raw := []byte(`{"id":"` + id + `","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`)
		if _, err := chain.RunBuffered(context.Background(), raw); err != nil {
			t.Fatalf("RunBuffered(%q): %v", id, err)
		}
	}

	turns := sink.kept.snapshot()
	if s := sink.calls; s != 2 {
		t.Fatalf("Sink.Record calls = %d, want 2", s)
	}
	if len(turns) != 1 || turns[0].ResponseID != "kept" || turns[0].TurnIndex != 1 {
		t.Fatalf("retained Turns = %+v, want second submission at ordinal one", turns)
	}
}
