package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"strconv"

	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

type openAIUsageMeter struct {
	recorder turnRecorder
}

var (
	_ RequestTransformer       = (*openAIUsageMeter)(nil)
	_ BufferedTransformer      = (*openAIUsageMeter)(nil)
	_ EventTransformer         = (*openAIUsageMeter)(nil)
	_ ServerMessageTransformer = (*openAIUsageMeter)(nil)
)

func newOpenAIUsageMeter(ctx context.Context, sink usage.Sink) *openAIUsageMeter {
	return &openAIUsageMeter{recorder: newTurnRecorder(ctx, sink)}
}

// TransformRequest observes optional HTTP attribution without validating or
// changing the request. Unknown attribution never prevents completion recording.
func (m *openAIUsageMeter) TransformRequest(_ context.Context, request *Request) error {
	m.recorder.observeRequest(request.Body)
	return nil
}

// TransformBuffered observes only self-contained completed Responses objects.
// Every path leaves Body.Bytes untouched and returns nil so malformed,
// incomplete, irrelevant, or future payloads remain Copilot-authoritative.
func (m *openAIUsageMeter) TransformBuffered(_ context.Context, body *Body) error {
	m.observeResponse(body.Bytes, usage.TransportBuffered)
	return nil
}

// TransformEvent routes on the advisory frame type, then validates the decoded
// event type and its self-contained response. Every path returns the exact
// original frame; Raw is SSE framing and is never decoded as JSON or rewritten.
func (m *openAIUsageMeter) TransformEvent(_ context.Context, f sse.Frame) []sse.Frame {
	if f.Type == "response.completed" {
		if payload, present := f.Data(); present {
			m.observeResponseCompletedEvent(payload, usage.TransportSSE)
		}
	}
	return []sse.Frame{f}
}

// TransformServerMessage observes each upstream Message independently using the
// shared completed-response validator. Every path preserves its kind and data
// and emits it, including malformed, irrelevant, and noncompletion events.
func (m *openAIUsageMeter) TransformServerMessage(_ context.Context, message *Message) bool {
	m.observeResponseCompletedEvent(message.Data, usage.TransportWebSocket)
	return true
}

func (m *openAIUsageMeter) observeResponseCompletedEvent(raw []byte, transport usage.Transport) {
	object, ok := decodeJSONObject(raw)
	if !ok {
		return
	}
	eventType, ok := requiredNonemptyString(object, "type")
	if !ok || eventType != "response.completed" {
		return
	}
	response, exists := object["response"]
	if !exists || isJSONNull(response) {
		return
	}
	m.observeResponse(response, transport)
}

func (m *openAIUsageMeter) observeResponse(raw []byte, transport usage.Transport) {
	turn, ok := parseOpenAIResponse(raw)
	if !ok {
		return
	}
	turn.Transport = transport
	m.recorder.record(turn)
}

func parseOpenAIResponse(raw []byte) (usage.Turn, bool) {
	object, serviceTier, ok := decodeOpenAIResponseObject(raw)
	if !ok {
		return usage.Turn{}, false
	}
	responseID, ok := requiredNonemptyString(object, "id")
	if !ok {
		return usage.Turn{}, false
	}
	model, ok := requiredNonemptyString(object, "model")
	if !ok {
		return usage.Turn{}, false
	}
	status, ok := requiredNonemptyString(object, "status")
	if !ok || status != "completed" {
		return usage.Turn{}, false
	}
	usageObject, ok := requiredJSONObject(object, "usage")
	if !ok {
		return usage.Turn{}, false
	}
	inputTokens, ok := requiredNonnegativeInt64(usageObject, "input_tokens")
	if !ok {
		return usage.Turn{}, false
	}
	outputTokens, ok := requiredNonnegativeInt64(usageObject, "output_tokens")
	if !ok {
		return usage.Turn{}, false
	}
	totalTokens, ok := optionalNonnegativeInt64(usageObject, "total_tokens")
	if !ok {
		return usage.Turn{}, false
	}

	var cachedTokens, cacheWriteTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "input_tokens_details"); !valid {
		return usage.Turn{}, false
	} else if present {
		cachedTokens, ok = optionalNonnegativeInt64(details, "cached_tokens")
		if !ok {
			return usage.Turn{}, false
		}
		cacheWriteTokens, ok = optionalNonnegativeInt64(details, "cache_write_tokens")
		if !ok {
			return usage.Turn{}, false
		}
	}

	var reasoningTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "output_tokens_details"); !valid {
		return usage.Turn{}, false
	} else if present {
		reasoningTokens, ok = optionalNonnegativeInt64(details, "reasoning_tokens")
		if !ok {
			return usage.Turn{}, false
		}
	}

	return usage.Turn{
		ResponseID:        responseID,
		Model:             model,
		OpenAIServiceTier: serviceTier,
		Usage: usage.OpenAIUsage{
			InputTokens:      inputTokens,
			OutputTokens:     outputTokens,
			CachedTokens:     cachedTokens,
			CacheWriteTokens: cacheWriteTokens,
			ReasoningTokens:  reasoningTokens,
			TotalTokens:      totalTokens,
		},
	}, true
}

// decodeOpenAIResponseObject performs the one Response-level traversal. It
// preserves encoding/json's existing last-wins behavior for unrelated members
// while retaining enough information to reject ambiguous optional tier evidence.
func decodeOpenAIResponseObject(raw []byte) (map[string]json.RawMessage, *string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, false
	}
	object := make(map[string]json.RawMessage)
	var tierRaw json.RawMessage
	tierOccurrences := 0
	for decoder.More() {
		token, err := decoder.Token()
		key, keyOK := token.(string)
		if err != nil || !keyOK {
			return nil, nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, false
		}
		object[key] = value
		if key == "service_tier" {
			tierOccurrences++
			if tierOccurrences == 1 {
				tierRaw = value
			}
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, nil, false
	}
	if tierOccurrences != 1 {
		return object, nil, true
	}
	decoded, err := jsontext.AppendUnquote(nil, bytes.TrimSpace(tierRaw))
	if err != nil {
		return object, nil, true
	}
	serviceTier := string(decoded)
	return object, &serviceTier, true
}

func decodeJSONObject(raw []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func requiredJSONObject(object map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return nil, false
	}
	return decodeJSONObject(raw)
}

func optionalJSONObject(object map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool, bool) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return nil, false, true
	}
	decoded, ok := decodeJSONObject(raw)
	return decoded, true, ok
}

func requiredNonemptyString(object map[string]json.RawMessage, key string) (string, bool) {
	raw, exists := object[key]
	if !exists {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func requiredNonnegativeInt64(object map[string]json.RawMessage, key string) (int64, bool) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return 0, false
	}
	value, ok := parseNonnegativeInt64(raw)
	return value, ok
}

func optionalNonnegativeInt64(object map[string]json.RawMessage, key string) (*int64, bool) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return nil, true
	}
	value, ok := parseNonnegativeInt64(raw)
	if !ok {
		return nil, false
	}
	return &value, true
}

func parseNonnegativeInt64(raw json.RawMessage) (int64, bool) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil && value >= 0
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
