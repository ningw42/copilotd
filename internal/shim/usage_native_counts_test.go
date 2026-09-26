package shim

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

// The tests in this file characterize native count parsing with hand-written
// literal payloads and explicitly typed expectations. Neither side is derived
// from any production declaration of the projection.

func int64Pointer(value int64) *int64 { return &value }

type anthropicCountCase struct {
	name   string
	object string                // literal Messages usage object
	want   *usage.AnthropicUsage // nil means the candidate is declined
}

func anthropicCountCases() []anthropicCountCase {
	required := &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2}
	return []anthropicCountCase{
		{name: "missing detail containers", object: `{"input_tokens":1,"output_tokens":2}`, want: required},
		{name: "null detail containers", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":null,"output_tokens_details":null}`, want: required},
		{name: "empty detail containers", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":{},"output_tokens_details":{}}`, want: required},
		{name: "array cache detail container", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":[]}`},
		{name: "string cache detail container", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":"{}"}`},
		{name: "number output detail container", object: `{"input_tokens":1,"output_tokens":2,"output_tokens_details":0}`},
		{name: "boolean output detail container", object: `{"input_tokens":1,"output_tokens":2,"output_tokens_details":false}`},
		{
			name:   "negative zero",
			object: `{"input_tokens":-0,"output_tokens":2,"cache_read_input_tokens":-0}`,
			want:   &usage.AnthropicUsage{InputTokens: 0, OutputTokens: 2, CacheReadInputTokens: int64Pointer(0)},
		},
		{
			name:   "maximum int64",
			object: `{"input_tokens":9223372036854775807,"output_tokens":9223372036854775807,"cache_creation":{"ephemeral_1h_input_tokens":9223372036854775807}}`,
			want:   &usage.AnthropicUsage{InputTokens: 9223372036854775807, OutputTokens: 9223372036854775807, Ephemeral1hInputTokens: int64Pointer(9223372036854775807)},
		},
		{
			name:   "above float64 integer precision",
			object: `{"input_tokens":9007199254740993,"output_tokens":2,"output_tokens_details":{"thinking_tokens":9007199254740995}}`,
			want:   &usage.AnthropicUsage{InputTokens: 9007199254740993, OutputTokens: 2, ThinkingTokens: int64Pointer(9007199254740995)},
		},
		{name: "required overflow", object: `{"input_tokens":9223372036854775808,"output_tokens":2}`},
		{name: "optional overflow", object: `{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":9223372036854775808}`},
		{name: "required decimal", object: `{"input_tokens":1,"output_tokens":2.0}`},
		{name: "optional decimal", object: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":1.0}`},
		{name: "required exponent", object: `{"input_tokens":1e0,"output_tokens":2}`},
		{name: "optional exponent", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":1e0}}`},
		{name: "case-mismatched required key", object: `{"Input_Tokens":1,"output_tokens":2}`},
		{
			name:   "case-mismatched optional keys are unknown fields",
			object: `{"input_tokens":1,"output_tokens":2,"Cache_Read_Input_Tokens":"bad","cache_creation":{"EPHEMERAL_5M_INPUT_TOKENS":5},"Output_Tokens_Details":[]}`,
			want:   required,
		},
		{
			name:   "escaped keys",
			object: `{"\u0069nput_tokens":1,"output_tokens":2,"cache_\u0072ead_input_tokens":4,"\u0063ache_creation":{"ephemeral_5m_input_token\u0073":5}}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: int64Pointer(4), Ephemeral5mInputTokens: int64Pointer(5)},
		},
		{
			name:   "duplicate count invalid then valid",
			object: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":"bad","cache_read_input_tokens":4}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: int64Pointer(4)},
		},
		{name: "duplicate count valid then invalid", object: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":4,"cache_read_input_tokens":"bad"}`},
		{name: "duplicate count valid then null", object: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":4,"cache_read_input_tokens":null}`, want: required},
		{
			name:   "duplicate escaped count",
			object: `{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":4,"cache_read_input_token\u0073":8}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: int64Pointer(8)},
		},
		{name: "duplicate required count", object: `{"input_tokens":1,"output_tokens":2,"input_tokens":3}`, want: &usage.AnthropicUsage{InputTokens: 3, OutputTokens: 2}},
		{
			name:   "duplicate container invalid then valid",
			object: `{"input_tokens":1,"output_tokens":2,"cache_creation":[],"cache_creation":{"ephemeral_5m_input_tokens":5}}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, Ephemeral5mInputTokens: int64Pointer(5)},
		},
		{name: "duplicate container valid then invalid", object: `{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":5},"cache_creation":[]}`},
		{
			name:   "duplicate containers are not merged",
			object: `{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":5},"cache_creation":{"ephemeral_1h_input_tokens":6}}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, Ephemeral1hInputTokens: int64Pointer(6)},
		},
		{
			name:   "unknown fields and enormous unknown numbers",
			object: `{"input_tokens":1,"output_tokens":2,"future_usage":1e999999,"future_count":123456789012345678901234567890,"future_object":{"nested":[-1.5,"x",null]},"cache_creation":{"ephemeral_5m_input_tokens":5,"future_ttl":-1e400},"output_tokens_details":{"thinking_tokens":7,"future_output":99999999999999999999999}}`,
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, Ephemeral5mInputTokens: int64Pointer(5), ThinkingTokens: int64Pointer(7)},
		},
		{
			name:   "insignificant whitespace",
			object: "{ \"input_tokens\" : 1 ,\t\"output_tokens\":2 , \"cache_creation\" : { \"ephemeral_5m_input_tokens\" : 5 } , \"cache_read_input_tokens\" : null }",
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, Ephemeral5mInputTokens: int64Pointer(5)},
		},
	}
}

func TestAnthropicUsageMeterCountParsingCharacterization(t *testing.T) {
	for _, tc := range anthropicCountCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("buffered", func(t *testing.T) {
				message := `{"id":"msg-count","type":"message","model":"reported","stop_reason":"end_turn","usage":` + tc.object + `}`
				assertAnthropicCountTurns(t, observeAnthropicBufferedUsage(t, message), usage.TransportBuffered, tc.want)
			})
			t.Run("SSE start", func(t *testing.T) {
				turns := observeAnthropicSSEUsage(t,
					anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg-count","model":"reported","usage":`+tc.object+`}}`),
					anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
				)
				assertAnthropicCountTurns(t, turns, usage.TransportSSE, tc.want)
			})
			t.Run("SSE delta", func(t *testing.T) {
				turns := observeAnthropicSSEUsage(t,
					anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg-count","model":"reported"}}`),
					anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":`+tc.object+`}`),
					anthropicSSEFrame("message_stop", `{"type":"message_stop"}`),
				)
				assertAnthropicCountTurns(t, turns, usage.TransportSSE, tc.want)
			})
		})
	}
}

func TestAnthropicUsageMeterSSECountSequenceCharacterization(t *testing.T) {
	start := func(object string) sse.Frame {
		return anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg-sequence","model":"reported","usage":`+object+`}}`)
	}
	delta := func(object string) sse.Frame {
		return anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":`+object+`}`)
	}
	stop := anthropicSSEFrame("message_stop", `{"type":"message_stop"}`)
	complete := `{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":6},"output_tokens_details":{"thinking_tokens":7}}`
	completeUsage := &usage.AnthropicUsage{
		InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: int64Pointer(3), CacheReadInputTokens: int64Pointer(4),
		Ephemeral5mInputTokens: int64Pointer(5), Ephemeral1hInputTokens: int64Pointer(6), ThinkingTokens: int64Pointer(7),
	}
	tests := []struct {
		name   string
		frames []sse.Frame
		want   *usage.AnthropicUsage
	}{
		{
			name:   "partial counts qualify only at stop",
			frames: []sse.Frame{start(`{"input_tokens":1}`), delta(`{"cache_read_input_tokens":4}`), delta(`{"output_tokens":2}`), stop},
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: int64Pointer(4)},
		},
		{
			name:   "partial counts still missing a required count at stop",
			frames: []sse.Frame{start(`{"output_tokens":2}`), delta(`{"cache_read_input_tokens":4}`), stop},
		},
		{
			name:   "valid pre-start delta is decoded and discarded",
			frames: []sse.Frame{delta(`{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":7}`), start(`{"input_tokens":1,"output_tokens":2}`), stop},
			want:   &usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2},
		},
		{
			name:   "valid pre-start delta does not supply required counts",
			frames: []sse.Frame{delta(`{"input_tokens":5,"output_tokens":6}`), start(`{}`), stop},
		},
		{
			name:   "invalid pre-start delta poisons the instance",
			frames: []sse.Frame{delta(`{"input_tokens":"5"}`), start(`{"input_tokens":1,"output_tokens":2}`), stop},
		},
		{
			name:   "invalid pre-start detail container poisons the instance",
			frames: []sse.Frame{delta(`{"cache_creation":[]}`), start(`{"input_tokens":1,"output_tokens":2}`), stop},
		},
		{
			name:   "null containers keep earlier reported counts",
			frames: []sse.Frame{start(complete), delta(`{"cache_creation":null,"output_tokens_details":null}`), stop},
			want:   completeUsage,
		},
		{
			name:   "empty containers keep earlier reported counts",
			frames: []sse.Frame{start(complete), delta(`{"cache_creation":{},"output_tokens_details":{}}`), stop},
			want:   completeUsage,
		},
		{
			name: "missing and null counts and usage keep earlier reported counts",
			frames: []sse.Frame{
				start(complete),
				delta(`{"input_tokens":null,"cache_read_input_tokens":null,"cache_creation":{"ephemeral_5m_input_tokens":null}}`),
				delta(`{}`),
				delta(`null`),
				anthropicSSEFrame("message_delta", `{"type":"message_delta"}`),
				stop,
			},
			want: completeUsage,
		},
		{
			name:   "later smaller and zero reports replace earlier ones",
			frames: []sse.Frame{start(complete), delta(`{"input_tokens":0,"cache_creation_input_tokens":1,"cache_creation":{"ephemeral_1h_input_tokens":0}}`), stop},
			want: &usage.AnthropicUsage{
				InputTokens: 0, OutputTokens: 2, CacheCreationInputTokens: int64Pointer(1), CacheReadInputTokens: int64Pointer(4),
				Ephemeral5mInputTokens: int64Pointer(5), Ephemeral1hInputTokens: int64Pointer(0), ThinkingTokens: int64Pointer(7),
			},
		},
		{
			name:   "invalid container after start poisons the candidate",
			frames: []sse.Frame{start(complete), delta(`{"output_tokens_details":"x"}`), stop},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertAnthropicCountTurns(t, observeAnthropicSSEUsage(t, tc.frames...), usage.TransportSSE, tc.want)
		})
	}
}

type openAICountCase struct {
	name   string
	object string             // literal Responses usage object
	want   *usage.OpenAIUsage // nil means the candidate is declined
}

func openAICountCases() []openAICountCase {
	required := &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}
	return []openAICountCase{
		{name: "missing detail containers", object: `{"input_tokens":1,"output_tokens":2}`, want: required},
		{name: "null detail containers", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":null,"output_tokens_details":null}`, want: required},
		{name: "empty detail containers", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":{},"output_tokens_details":{}}`, want: required},
		{name: "array input detail container", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":[]}`},
		{name: "string input detail container", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":"{}"}`},
		{name: "number output detail container", object: `{"input_tokens":1,"output_tokens":2,"output_tokens_details":0}`},
		{name: "boolean output detail container", object: `{"input_tokens":1,"output_tokens":2,"output_tokens_details":false}`},
		{
			name:   "negative zero",
			object: `{"input_tokens":-0,"output_tokens":2,"total_tokens":-0}`,
			want:   &usage.OpenAIUsage{InputTokens: 0, OutputTokens: 2, TotalTokens: int64Pointer(0)},
		},
		{
			name:   "maximum int64",
			object: `{"input_tokens":9223372036854775807,"output_tokens":9223372036854775807,"output_tokens_details":{"reasoning_tokens":9223372036854775807}}`,
			want:   &usage.OpenAIUsage{InputTokens: 9223372036854775807, OutputTokens: 9223372036854775807, ReasoningTokens: int64Pointer(9223372036854775807)},
		},
		{
			name:   "above float64 integer precision",
			object: `{"input_tokens":9007199254740993,"output_tokens":2,"input_tokens_details":{"cache_write_tokens":9007199254740995}}`,
			want:   &usage.OpenAIUsage{InputTokens: 9007199254740993, OutputTokens: 2, CacheWriteTokens: int64Pointer(9007199254740995)},
		},
		{name: "required overflow", object: `{"input_tokens":1,"output_tokens":9223372036854775808}`},
		{name: "optional overflow", object: `{"input_tokens":1,"output_tokens":2,"total_tokens":9223372036854775808}`},
		{name: "required decimal", object: `{"input_tokens":1.0,"output_tokens":2}`},
		{name: "optional decimal", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":1.0}}`},
		{name: "required exponent", object: `{"input_tokens":1,"output_tokens":2e0}`},
		{name: "optional exponent", object: `{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1e0}}`},
		{name: "case-mismatched required key", object: `{"input_tokens":1,"Output_Tokens":2}`},
		{
			name:   "case-mismatched optional keys are unknown fields",
			object: `{"input_tokens":1,"output_tokens":2,"Total_Tokens":"bad","input_tokens_details":{"CACHED_TOKENS":5},"Output_Tokens_Details":[]}`,
			want:   required,
		},
		{
			name:   "escaped keys",
			object: `{"\u0069nput_tokens":1,"output_tokens":2,"total_token\u0073":3,"\u0069nput_tokens_details":{"cached_\u0074okens":4}}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, CachedTokens: int64Pointer(4), TotalTokens: int64Pointer(3)},
		},
		{
			name:   "duplicate count invalid then valid",
			object: `{"input_tokens":1,"output_tokens":2,"total_tokens":"bad","total_tokens":3}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: int64Pointer(3)},
		},
		{name: "duplicate count valid then invalid", object: `{"input_tokens":1,"output_tokens":2,"total_tokens":3,"total_tokens":"bad"}`},
		{name: "duplicate count valid then null", object: `{"input_tokens":1,"output_tokens":2,"total_tokens":3,"total_tokens":null}`, want: required},
		{
			name:   "duplicate escaped count",
			object: `{"input_tokens":1,"output_tokens":2,"total_tokens":3,"total_token\u0073":8}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: int64Pointer(8)},
		},
		{name: "duplicate required count", object: `{"input_tokens":1,"output_tokens":2,"output_tokens":3}`, want: &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 3}},
		{
			name:   "duplicate container invalid then valid",
			object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":[],"input_tokens_details":{"cached_tokens":4}}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, CachedTokens: int64Pointer(4)},
		},
		{name: "duplicate container valid then invalid", object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":4},"input_tokens_details":[]}`},
		{
			name:   "duplicate containers are not merged",
			object: `{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":4},"input_tokens_details":{"cache_write_tokens":5}}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, CacheWriteTokens: int64Pointer(5)},
		},
		{
			name:   "unknown fields and enormous unknown numbers",
			object: `{"input_tokens":1,"output_tokens":2,"future_usage":1e999999,"future_count":123456789012345678901234567890,"future_object":{"nested":[-1.5,"x",null]},"input_tokens_details":{"cached_tokens":4,"future_input":-1e400},"output_tokens_details":{"reasoning_tokens":5,"future_output":99999999999999999999999}}`,
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, CachedTokens: int64Pointer(4), ReasoningTokens: int64Pointer(5)},
		},
		{
			name:   "insignificant whitespace",
			object: "{ \"input_tokens\" : 1 ,\t\"output_tokens\":2 , \"input_tokens_details\" : { \"cached_tokens\" : 4 } , \"total_tokens\" : null }",
			want:   &usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2, CachedTokens: int64Pointer(4)},
		},
	}
}

func TestOpenAIUsageMeterCountParsingCharacterization(t *testing.T) {
	for _, tc := range openAICountCases() {
		t.Run(tc.name, func(t *testing.T) {
			response := `{"id":"resp-count","model":"reported","status":"completed","usage":` + tc.object + `}`
			for _, transport := range syntheticOpenAITransportCases() {
				t.Run(transport.name, func(t *testing.T) {
					turns := observeOpenAIUsage(t, transport, response)
					if tc.want == nil {
						if len(turns) != 0 {
							t.Fatalf("declined candidate recorded %+v", turns)
						}
						return
					}
					if len(turns) != 1 || turns[0].Transport != transport.transport {
						t.Fatalf("Turns = %+v, want one %s Turn", turns, transport.transport)
					}
					if got := turns[0].Usage; !reflect.DeepEqual(got, *tc.want) {
						t.Errorf("native usage = %s, want %s", formatOpenAIUsage(got), formatOpenAIUsage(*tc.want))
					}
				})
			}
		})
	}
}

// TestUsageMeterBindsEveryNativeCountToItsOwnField gives every count a value
// no other count uses, on every transport of each Surface, so a count read from
// the wrong key or stored in the wrong field cannot produce the expected value.
func TestUsageMeterBindsEveryNativeCountToItsOwnField(t *testing.T) {
	t.Run("Anthropic buffered", func(t *testing.T) {
		turns := observeAnthropicBufferedUsage(t, `{"id":"msg-distinct","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1001,"output_tokens":1002,"cache_creation_input_tokens":1003,"cache_read_input_tokens":1004,"cache_creation":{"ephemeral_5m_input_tokens":1005,"ephemeral_1h_input_tokens":1006},"output_tokens_details":{"thinking_tokens":1007}}}`)
		assertAnthropicCountTurns(t, turns, usage.TransportBuffered, &usage.AnthropicUsage{
			InputTokens: 1001, OutputTokens: 1002, CacheCreationInputTokens: int64Pointer(1003), CacheReadInputTokens: int64Pointer(1004),
			Ephemeral5mInputTokens: int64Pointer(1005), Ephemeral1hInputTokens: int64Pointer(1006), ThinkingTokens: int64Pointer(1007),
		})
	})

	start := func(object string) sse.Frame {
		return anthropicSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg-distinct","model":"reported","usage":`+object+`}}`)
	}
	delta := func(object string) sse.Frame {
		return anthropicSSEFrame("message_delta", `{"type":"message_delta","usage":`+object+`}`)
	}
	stop := anthropicSSEFrame("message_stop", `{"type":"message_stop"}`)
	sseTests := []struct {
		name   string
		frames []sse.Frame
		want   usage.AnthropicUsage
	}{
		{
			name:   "start reports every count",
			frames: []sse.Frame{start(`{"input_tokens":2001,"output_tokens":2002,"cache_creation_input_tokens":2003,"cache_read_input_tokens":2004,"cache_creation":{"ephemeral_5m_input_tokens":2005,"ephemeral_1h_input_tokens":2006},"output_tokens_details":{"thinking_tokens":2007}}`), stop},
			want: usage.AnthropicUsage{
				InputTokens: 2001, OutputTokens: 2002, CacheCreationInputTokens: int64Pointer(2003), CacheReadInputTokens: int64Pointer(2004),
				Ephemeral5mInputTokens: int64Pointer(2005), Ephemeral1hInputTokens: int64Pointer(2006), ThinkingTokens: int64Pointer(2007),
			},
		},
		{
			name: "delta overwrites every count",
			frames: []sse.Frame{
				start(`{"input_tokens":2001,"output_tokens":2002,"cache_creation_input_tokens":2003,"cache_read_input_tokens":2004,"cache_creation":{"ephemeral_5m_input_tokens":2005,"ephemeral_1h_input_tokens":2006},"output_tokens_details":{"thinking_tokens":2007}}`),
				delta(`{"input_tokens":3001,"output_tokens":3002,"cache_creation_input_tokens":3003,"cache_read_input_tokens":3004,"cache_creation":{"ephemeral_5m_input_tokens":3005,"ephemeral_1h_input_tokens":3006},"output_tokens_details":{"thinking_tokens":3007}}`),
				stop,
			},
			want: usage.AnthropicUsage{
				InputTokens: 3001, OutputTokens: 3002, CacheCreationInputTokens: int64Pointer(3003), CacheReadInputTokens: int64Pointer(3004),
				Ephemeral5mInputTokens: int64Pointer(3005), Ephemeral1hInputTokens: int64Pointer(3006), ThinkingTokens: int64Pointer(3007),
			},
		},
		{
			name: "delta overwrites every count with explicit zero",
			frames: []sse.Frame{
				start(`{"input_tokens":2001,"output_tokens":2002,"cache_creation_input_tokens":2003,"cache_read_input_tokens":2004,"cache_creation":{"ephemeral_5m_input_tokens":2005,"ephemeral_1h_input_tokens":2006},"output_tokens_details":{"thinking_tokens":2007}}`),
				delta(`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":0}}`),
				stop,
			},
			want: usage.AnthropicUsage{
				InputTokens: 0, OutputTokens: 0, CacheCreationInputTokens: int64Pointer(0), CacheReadInputTokens: int64Pointer(0),
				Ephemeral5mInputTokens: int64Pointer(0), Ephemeral1hInputTokens: int64Pointer(0), ThinkingTokens: int64Pointer(0),
			},
		},
		{
			name: "one delta per count overwrites explicit zero",
			frames: []sse.Frame{
				start(`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":0}}`),
				delta(`{"input_tokens":4001}`),
				delta(`{"output_tokens":4002}`),
				delta(`{"cache_creation_input_tokens":4003}`),
				delta(`{"cache_read_input_tokens":4004}`),
				delta(`{"cache_creation":{"ephemeral_5m_input_tokens":4005}}`),
				delta(`{"cache_creation":{"ephemeral_1h_input_tokens":4006}}`),
				delta(`{"output_tokens_details":{"thinking_tokens":4007}}`),
				stop,
			},
			want: usage.AnthropicUsage{
				InputTokens: 4001, OutputTokens: 4002, CacheCreationInputTokens: int64Pointer(4003), CacheReadInputTokens: int64Pointer(4004),
				Ephemeral5mInputTokens: int64Pointer(4005), Ephemeral1hInputTokens: int64Pointer(4006), ThinkingTokens: int64Pointer(4007),
			},
		},
	}
	for _, tc := range sseTests {
		t.Run("Anthropic SSE "+tc.name, func(t *testing.T) {
			assertAnthropicCountTurns(t, observeAnthropicSSEUsage(t, tc.frames...), usage.TransportSSE, &tc.want)
		})
	}

	response := `{"id":"resp-distinct","model":"reported","status":"completed","usage":{"input_tokens":5001,"input_tokens_details":{"cached_tokens":5002,"cache_write_tokens":5003},"output_tokens":5004,"output_tokens_details":{"reasoning_tokens":5005},"total_tokens":5006}}`
	want := usage.OpenAIUsage{
		InputTokens: 5001, CachedTokens: int64Pointer(5002), CacheWriteTokens: int64Pointer(5003),
		OutputTokens: 5004, ReasoningTokens: int64Pointer(5005), TotalTokens: int64Pointer(5006),
	}
	for _, transport := range syntheticOpenAITransportCases() {
		t.Run("OpenAI "+transport.name, func(t *testing.T) {
			turns := observeOpenAIUsage(t, transport, response)
			if len(turns) != 1 || turns[0].Transport != transport.transport {
				t.Fatalf("Turns = %+v, want one %s Turn", turns, transport.transport)
			}
			if got := turns[0].Usage; !reflect.DeepEqual(got, want) {
				t.Errorf("native usage = %s, want %s", formatOpenAIUsage(got), formatOpenAIUsage(want))
			}
		})
	}
}

func observeAnthropicBufferedUsage(t *testing.T, message string) []usage.Turn {
	t.Helper()
	sink := &memoryUsageSink{}
	chain := enabledAnthropicUsageChain(context.Background(), sink)
	raw := []byte(message)
	got, err := chain.RunBuffered(context.Background(), raw)
	if err != nil || !reflect.DeepEqual(got, raw) || &got[0] != &raw[0] {
		t.Fatalf("RunBuffered = %v body %q, want nil and the same unchanged slice %q", err, got, raw)
	}
	return sink.snapshot()
}

func observeAnthropicSSEUsage(t *testing.T, frames ...sse.Frame) []usage.Turn {
	t.Helper()
	sink := &memoryUsageSink{}
	transformAnthropicFrames(t, enabledAnthropicUsageStream(context.Background(), sink), frames...)
	return sink.snapshot()
}

// observeOpenAIUsage submits one literal Response through transport and returns
// every recorded Turn; a declined Response records none.
func observeOpenAIUsage(t *testing.T, transport syntheticOpenAITransportCase, response string) []usage.Turn {
	t.Helper()
	sink := &memoryUsageSink{}
	ctx := context.Background()
	switch transport.transport {
	case usage.TransportBuffered:
		registry := CanonicalRegistry(sink)
		registry[len(registry)-1].Enabled = true
		chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
		raw := []byte(response)
		if got, err := chain.RunBuffered(ctx, raw); err != nil || !reflect.DeepEqual(got, raw) {
			t.Fatalf("RunBuffered = %v body %q, want nil and unchanged %q", err, got, raw)
		}
	case usage.TransportSSE:
		payload := syntheticOpenAICompletedEvent([]byte(response))
		frame := sse.Frame{Type: "response.completed", Raw: append(append([]byte("event: response.completed\ndata: "), payload...), '\n', '\n')}
		if got := enabledOpenAIUsageStream(ctx, sink).Transform(ctx, frame); !reflect.DeepEqual(got, []sse.Frame{frame}) {
			t.Fatalf("SSE Transform = %#v, want exact original frame %#v", got, frame)
		}
	case usage.TransportWebSocket:
		data := syntheticOpenAICompletedEvent([]byte(response))
		message := &Message{Kind: transport.kind, Data: data}
		if emit := enabledOpenAIUsageWSServer(ctx, sink)(ctx, message); !emit || message.Kind != transport.kind || !reflect.DeepEqual(message.Data, data) {
			t.Fatalf("WebSocket Message changed: emit=%t message=%+v", emit, message)
		}
	default:
		t.Fatalf("unsupported transport %q", transport.transport)
	}
	return sink.snapshot()
}

func assertAnthropicCountTurns(t *testing.T, turns []usage.Turn, transport usage.Transport, want *usage.AnthropicUsage) {
	t.Helper()
	if want == nil {
		if len(turns) != 0 {
			t.Fatalf("declined candidate recorded %+v", turns)
		}
		return
	}
	if len(turns) != 1 || turns[0].Transport != transport {
		t.Fatalf("Turns = %+v, want one %s Turn", turns, transport)
	}
	if got := turns[0].Usage; !reflect.DeepEqual(got, *want) {
		t.Errorf("native usage = %s, want %s", formatAnthropicUsage(got), formatAnthropicUsage(*want))
	}
}

func formatAnthropicUsage(value usage.Usage) string {
	native, ok := value.(usage.AnthropicUsage)
	if !ok {
		return reflect.TypeOf(value).String()
	}
	return formatCounts(
		labeledCount{"input", &native.InputTokens}, labeledCount{"output", &native.OutputTokens},
		labeledCount{"cache_creation", native.CacheCreationInputTokens}, labeledCount{"cache_read", native.CacheReadInputTokens},
		labeledCount{"5m", native.Ephemeral5mInputTokens}, labeledCount{"1h", native.Ephemeral1hInputTokens},
		labeledCount{"thinking", native.ThinkingTokens},
	)
}

func formatOpenAIUsage(value usage.Usage) string {
	native, ok := value.(usage.OpenAIUsage)
	if !ok {
		return reflect.TypeOf(value).String()
	}
	return formatCounts(
		labeledCount{"input", &native.InputTokens}, labeledCount{"output", &native.OutputTokens},
		labeledCount{"cached", native.CachedTokens}, labeledCount{"cache_write", native.CacheWriteTokens},
		labeledCount{"reasoning", native.ReasoningTokens}, labeledCount{"total", native.TotalTokens},
	)
}

// labeledCount names one count in a failure message; nil means unreported.
type labeledCount struct {
	label string
	value *int64
}

// formatCounts shows nil as unreported rather than as a pointer address.
func formatCounts(counts ...labeledCount) string {
	var text strings.Builder
	text.WriteString("{")
	for index, count := range counts {
		if index > 0 {
			text.WriteString(" ")
		}
		text.WriteString(count.label + ":")
		if count.value == nil {
			text.WriteString("nil")
		} else {
			text.WriteString(strconv.FormatInt(*count.value, 10))
		}
	}
	text.WriteString("}")
	return text.String()
}
