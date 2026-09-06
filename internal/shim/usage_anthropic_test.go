package shim

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

func enabledAnthropicUsageChain(ctx context.Context, sink usage.Sink) *Chain {
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	return registry.NewChain(ctx, endpoint.Anthropic, endpoint.RouteAnthropicMessages)
}

func enabledAnthropicUsageStream(ctx context.Context, sink usage.Sink) sse.FrameTransformer {
	return enabledAnthropicUsageChain(ctx, sink).StreamAdapter(ctx, nil)
}

func TestAnthropicUsageMeterAccumulatesGeneratedSSEUsageWithoutChangingFrames(t *testing.T) {
	tests := []struct {
		name                     string
		fixture                  string
		messageID                string
		input, output            int64
		cacheCreation, cacheRead int64
		ephemeral5m, ephemeral1h int64
		thinking                 int64
	}{
		{
			name: "cumulative last reports", fixture: "anthropic-messages-sse-cumulative.synthetic.sse",
			messageID: "msg_redacted_synthetic_cumulative", input: 12, output: 9,
			cacheCreation: 2000, cacheRead: 6000, ephemeral5m: 750, ephemeral1h: 1250, thinking: 4,
		},
		{
			name: "usage completed by later delta", fixture: "anthropic-messages-sse-late-usage.synthetic.sse",
			messageID: "msg_redacted_synthetic_late_usage", ephemeral5m: -1, ephemeral1h: -1, thinking: -1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.Open(filepath.Join("testdata", "usage", test.fixture))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			sink := &memoryUsageSink{}
			adapter := enabledAnthropicUsageStream(logging.WithRequestID(context.Background(), "anthropic-sse-correlation"), sink)
			reader := sse.NewReader(file, nil)
			for {
				frame, err := reader.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				hookCtx := logging.WithRequestID(context.Background(), "must-not-replace-construction-correlation")
				if got := adapter.Transform(hookCtx, frame); !reflect.DeepEqual(got, []sse.Frame{frame}) {
					t.Fatalf("Transform(%q) = %#v, want exact original frame %#v", frame.Type, got, frame)
				}
			}
			turns := sink.snapshot()
			if len(turns) != 1 {
				t.Fatalf("recorded Turns = %+v, want one generated completion", turns)
			}
			turn := turns[0]
			if turn.RequestID != "anthropic-sse-correlation" || turn.ResponseID != test.messageID ||
				turn.Model != "claude-synthetic" || turn.Transport != usage.TransportSSE || turn.TurnIndex != 0 {
				t.Errorf("Turn envelope = %+v", turn)
			}
			native := turn.Usage.(usage.AnthropicUsage)
			if native.InputTokens != test.input || native.OutputTokens != test.output ||
				pointerValue(native.CacheCreationInputTokens) != test.cacheCreation ||
				pointerValue(native.CacheReadInputTokens) != test.cacheRead ||
				pointerValue(native.Ephemeral5mInputTokens) != test.ephemeral5m ||
				pointerValue(native.Ephemeral1hInputTokens) != test.ephemeral1h ||
				pointerValue(native.ThinkingTokens) != test.thinking {
				t.Errorf("native usage = %+v", native)
			}
			if test.fixture == "anthropic-messages-sse-late-usage.synthetic.sse" &&
				(native.CacheCreationInputTokens == nil || native.CacheReadInputTokens == nil ||
					native.Ephemeral5mInputTokens != nil || native.Ephemeral1hInputTokens != nil || native.ThinkingTokens != nil) {
				t.Errorf("late usage nil/zero distinction = %+v", native)
			}
		})
	}
}

func anthropicSSEFrame(kind, payload string) sse.Frame {
	return sse.Frame{Type: kind, Raw: []byte("event: " + kind + "\ndata: " + payload + "\n\n")}
}

func transformAnthropicFrames(t *testing.T, adapter sse.FrameTransformer, frames ...sse.Frame) {
	t.Helper()
	for _, frame := range frames {
		if got := adapter.Transform(context.Background(), frame); !reflect.DeepEqual(got, []sse.Frame{frame}) {
			t.Fatalf("Transform(%q) = %#v, want exact original frame %#v", frame.Type, got, frame)
		}
	}
}

func validAnthropicSSECompletion(messageID string) []sse.Frame {
	return []sse.Frame{
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"`+messageID+`","model":"reported","usage":{"input_tokens":1,"output_tokens":0}}}`),
		anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"output_tokens":2}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	}
}

func TestAnthropicUsageMeterLastNumericReportWinsAndNullPreservesEveryCount(t *testing.T) {
	sink := &memoryUsageSink{}
	adapter := enabledAnthropicUsageStream(context.Background(), sink)
	transformAnthropicFrames(t, adapter,
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg-cumulative","model":"reported","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},"output_tokens_details":{"thinking_tokens":7}}}}`),
		anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"input_tokens":12,"output_tokens":9,"cache_creation_input_tokens":2000,"cache_read_input_tokens":6000,"cache_creation":{"ephemeral_5m_input_tokens":750,"ephemeral_1h_input_tokens":1250},"output_tokens_details":{"thinking_tokens":4}}}`),
		anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"input_tokens":null,"output_tokens":null,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"cache_creation":{"ephemeral_5m_input_tokens":null,"ephemeral_1h_input_tokens":null},"output_tokens_details":{"thinking_tokens":null}}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	)
	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("recorded Turns = %+v", turns)
	}
	native := turns[0].Usage.(usage.AnthropicUsage)
	if native.InputTokens != 12 || native.OutputTokens != 9 ||
		pointerValue(native.CacheCreationInputTokens) != 2000 || pointerValue(native.CacheReadInputTokens) != 6000 ||
		pointerValue(native.Ephemeral5mInputTokens) != 750 || pointerValue(native.Ephemeral1hInputTokens) != 1250 ||
		pointerValue(native.ThinkingTokens) != 4 {
		t.Errorf("cumulative native usage = %+v", native)
	}
}

func TestAnthropicUsageMeterUsesJoinedDataAndAdvisoryFallbackWithoutDecodingIrrelevantFrames(t *testing.T) {
	stream := "event: message_start\r\n" +
		"data: {\"type\":\"message_start\",\r\n" +
		"data:\t\"message\":{\"id\":\"msg-framing\",\"model\":\"reported\",\"usage\":{\"input_tokens\":0}}}\r\n\r\n" +
		"event: content_block_delta\r\ndata: {\"type\":\r\n\r\n" +
		"event:\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\r\n\r\n" +
		"data: {\"type\":\"message_stop\"}\r\n\r\n"
	reader := sse.NewReader(strings.NewReader(stream), nil)
	sink := &memoryUsageSink{}
	adapter := enabledAnthropicUsageStream(context.Background(), sink)
	var observed []sse.Frame
	for {
		frame, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		observed = append(observed, frame)
		transformAnthropicFrames(t, adapter, frame)
	}
	if len(observed) != 4 || observed[2].Type != "message_delta" || observed[3].Type != "message_stop" {
		t.Fatalf("fallback-classified frames = %+v", observed)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestID != "" || turns[0].ResponseID != "msg-framing" || turns[0].Transport != usage.TransportSSE {
		t.Fatalf("framed stream Turns = %+v", turns)
	}
	native := turns[0].Usage.(usage.AnthropicUsage)
	if native.InputTokens != 0 || native.OutputTokens != 0 {
		t.Errorf("framed stream usage = %+v", native)
	}
}

func TestAnthropicUsageMeterPermanentlyPoisonsMalformedRelevantSequences(t *testing.T) {
	validStart := anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"bad-candidate","model":"reported","usage":{"input_tokens":1,"output_tokens":0}}}`)
	tests := []struct {
		name   string
		prefix []sse.Frame
	}{
		{name: "absent start data", prefix: []sse.Frame{{Type: "message_start", Raw: []byte("event: message_start\nid: opaque\n\n")}}},
		{name: "empty delta data", prefix: []sse.Frame{validStart, {Type: "message_delta", Raw: []byte("event: message_delta\ndata:\n\n")}}},
		{name: "malformed stop", prefix: []sse.Frame{validStart, anthropicSSEFrame("message_stop", `{"type":`)}},
		{name: "stop type conflict", prefix: []sse.Frame{validStart, anthropicSSEFrame("message_stop", `{"type":"message_delta"}`)}},
		{name: "wrong core type", prefix: []sse.Frame{anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"bad","model":"reported","usage":{"input_tokens":"1"}}}`)}},
		{name: "negative optional", prefix: []sse.Frame{validStart, anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"cache_read_input_tokens":-1}}`)}},
		{name: "overflow nested optional", prefix: []sse.Frame{validStart, anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"output_tokens_details":{"thinking_tokens":9223372036854775808}}}`)}},
		{name: "wrong nested object", prefix: []sse.Frame{validStart, anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"cache_creation":[]}}`)}},
		{name: "conflicting starts", prefix: []sse.Frame{validStart, validStart}},
		{name: "upstream error", prefix: []sse.Frame{validStart, anthropicSSEFrame("error", `{"type":"error","error":{"message":"upstream"}}`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := &memoryUsageSink{}
			adapter := enabledAnthropicUsageStream(context.Background(), sink)
			transformAnthropicFrames(t, adapter, test.prefix...)
			transformAnthropicFrames(t, adapter, validAnthropicSSECompletion("must-stay-suppressed")...)
			if turns := sink.snapshot(); len(turns) != 0 {
				t.Errorf("poisoned instance recorded Turns: %+v", turns)
			}
		})
	}
}

func TestAnthropicUsageMeterClearsCandidatesAndKeepsSubmittedSnapshotsImmutable(t *testing.T) {
	t.Run("interrupted candidate has no Turn", func(t *testing.T) {
		sink := &memoryUsageSink{}
		adapter := enabledAnthropicUsageStream(context.Background(), sink)
		transformAnthropicFrames(t, adapter,
			anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"interrupted","model":"reported"}}`),
			anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"input_tokens":1,"output_tokens":2}}`),
		)
		if turns := sink.snapshot(); len(turns) != 0 {
			t.Fatalf("interrupted stream recorded Turns: %+v", turns)
		}
	})

	sink := &memoryUsageSink{}
	constructionCtx := logging.WithRequestID(context.Background(), "captured-anthropic-request")
	adapter := enabledAnthropicUsageStream(constructionCtx, sink)
	incomplete := []sse.Frame{
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"incomplete","model":"reported"}}`),
		anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"input_tokens":1}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	}
	transformAnthropicFrames(t, adapter, incomplete...)
	first := []sse.Frame{
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"reused","model":"first-model","usage":{"input_tokens":12,"output_tokens":0,"cache_creation_input_tokens":2000,"cache_read_input_tokens":6000,"cache_creation":{"ephemeral_5m_input_tokens":750,"ephemeral_1h_input_tokens":1250},"output_tokens_details":{"thinking_tokens":0}}}}`),
		anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":{"input_tokens":null,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"cache_creation":null,"output_tokens":9,"output_tokens_details":{"thinking_tokens":4}}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	}
	before := time.Now()
	transformAnthropicFrames(t, adapter, first...)
	firstSnapshot := sink.snapshot()[0].Usage.(usage.AnthropicUsage)
	second := []sse.Frame{
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"reused","model":"second-model","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":0}}}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	}
	transformAnthropicFrames(t, adapter, second...)
	after := time.Now()

	turns := sink.snapshot()
	if len(turns) != 2 {
		t.Fatalf("recorded Turns = %+v, want only two complete candidates", turns)
	}
	for index, turn := range turns {
		if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "captured-anthropic-request" ||
			turn.ResponseID != "reused" || turn.Transport != usage.TransportSSE || turn.TurnIndex != index {
			t.Errorf("Turn %d envelope = %+v", index, turn)
		}
	}
	if turns[0].Model != "first-model" || turns[1].Model != "second-model" {
		t.Errorf("reported models = %q, %q", turns[0].Model, turns[1].Model)
	}
	if firstSnapshot.InputTokens != 12 || firstSnapshot.OutputTokens != 9 ||
		pointerValue(firstSnapshot.CacheCreationInputTokens) != 2000 || pointerValue(firstSnapshot.CacheReadInputTokens) != 6000 ||
		pointerValue(firstSnapshot.Ephemeral5mInputTokens) != 750 || pointerValue(firstSnapshot.Ephemeral1hInputTokens) != 1250 ||
		pointerValue(firstSnapshot.ThinkingTokens) != 4 {
		t.Errorf("first submitted snapshot changed after reset/reuse: %+v", firstSnapshot)
	}
	zero := turns[1].Usage.(usage.AnthropicUsage)
	if zero.InputTokens != 0 || zero.OutputTokens != 0 || zero.CacheCreationInputTokens == nil ||
		zero.CacheReadInputTokens == nil || zero.Ephemeral5mInputTokens == nil ||
		zero.Ephemeral1hInputTokens == nil || zero.ThinkingTokens == nil {
		t.Errorf("second reported zeros = %+v", zero)
	}
}

func TestAnthropicUsageMeterSubmittedPointersRemainImmutableDuringLaterSSEUpdates(t *testing.T) {
	sink := &memoryUsageSink{}
	adapter := enabledAnthropicUsageStream(context.Background(), sink)
	transformAnthropicFrames(t, adapter,
		anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"retained","model":"reported","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},"output_tokens_details":{"thinking_tokens":7}}}}`),
		anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
	)
	retained := sink.snapshot()[0].Usage.(usage.AnthropicUsage)
	stopReaders := make(chan struct{})
	mismatch := make(chan struct{}, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					if pointerValue(retained.CacheCreationInputTokens) != 3 || pointerValue(retained.CacheReadInputTokens) != 4 ||
						pointerValue(retained.Ephemeral5mInputTokens) != 5 || pointerValue(retained.Ephemeral1hInputTokens) != 6 ||
						pointerValue(retained.ThinkingTokens) != 7 {
						select {
						case mismatch <- struct{}{}:
						default:
						}
						return
					}
				}
			}
		}()
	}
	for index := range 256 {
		frames := validAnthropicSSECompletion("later-" + strings.Repeat("x", index%8))
		transformAnthropicFrames(t, adapter, frames...)
	}
	close(stopReaders)
	readers.Wait()
	select {
	case <-mismatch:
		t.Fatal("retained optional pointers changed during later accumulator updates")
	default:
	}
}

func TestAnthropicUsageMeterAdvancesSSEOrdinalWhenSinkDrops(t *testing.T) {
	sink := &dropFirstUsageSink{}
	adapter := enabledAnthropicUsageStream(context.Background(), sink)
	transformAnthropicFrames(t, adapter, validAnthropicSSECompletion("dropped")...)
	transformAnthropicFrames(t, adapter, validAnthropicSSECompletion("kept")...)
	turns := sink.kept.snapshot()
	if sink.calls != 2 || len(turns) != 1 || turns[0].ResponseID != "kept" ||
		turns[0].TurnIndex != 1 || turns[0].Transport != usage.TransportSSE {
		t.Fatalf("dropped/retained submissions = calls:%d Turns:%+v", sink.calls, turns)
	}
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
