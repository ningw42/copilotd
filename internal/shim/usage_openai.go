package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

type openAIUsageMeter struct {
	sink      usage.Sink
	requestID string
	turnIndex int
}

var (
	_ BufferedTransformer = (*openAIUsageMeter)(nil)
	_ EventTransformer    = (*openAIUsageMeter)(nil)
)

func newOpenAIUsageMeter(ctx context.Context, sink usage.Sink) *openAIUsageMeter {
	requestID, _ := logging.RequestIDFrom(ctx)
	return &openAIUsageMeter{sink: sink, requestID: requestID}
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
	responseID, model, native, ok := parseOpenAIResponse(raw)
	if !ok {
		return
	}
	turnIndex := m.turnIndex
	m.turnIndex++
	m.sink.Record(usage.Turn{
		At:         time.Now(),
		RequestID:  m.requestID,
		ResponseID: responseID,
		Model:      model,
		Transport:  transport,
		TurnIndex:  turnIndex,
		Usage:      native,
	})
}

func parseOpenAIResponse(raw []byte) (string, string, usage.OpenAIUsage, bool) {
	object, ok := decodeJSONObject(raw)
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	responseID, ok := requiredNonemptyString(object, "id")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	model, ok := requiredNonemptyString(object, "model")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	status, ok := requiredNonemptyString(object, "status")
	if !ok || status != "completed" {
		return "", "", usage.OpenAIUsage{}, false
	}
	usageObject, ok := requiredJSONObject(object, "usage")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	inputTokens, ok := requiredNonnegativeInt64(usageObject, "input_tokens")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	outputTokens, ok := requiredNonnegativeInt64(usageObject, "output_tokens")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}
	totalTokens, ok := optionalNonnegativeInt64(usageObject, "total_tokens")
	if !ok {
		return "", "", usage.OpenAIUsage{}, false
	}

	var cachedTokens, cacheWriteTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "input_tokens_details"); !valid {
		return "", "", usage.OpenAIUsage{}, false
	} else if present {
		cachedTokens, ok = optionalNonnegativeInt64(details, "cached_tokens")
		if !ok {
			return "", "", usage.OpenAIUsage{}, false
		}
		cacheWriteTokens, ok = optionalNonnegativeInt64(details, "cache_write_tokens")
		if !ok {
			return "", "", usage.OpenAIUsage{}, false
		}
	}

	var reasoningTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "output_tokens_details"); !valid {
		return "", "", usage.OpenAIUsage{}, false
	} else if present {
		reasoningTokens, ok = optionalNonnegativeInt64(details, "reasoning_tokens")
		if !ok {
			return "", "", usage.OpenAIUsage{}, false
		}
	}

	return responseID, model, usage.OpenAIUsage{
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		CachedTokens:     cachedTokens,
		CacheWriteTokens: cacheWriteTokens,
		ReasoningTokens:  reasoningTokens,
		TotalTokens:      totalTokens,
	}, true
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
