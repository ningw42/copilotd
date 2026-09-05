package shim

import (
	"context"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
)

type anthropicUsageMeter struct {
	sink      usage.Sink
	requestID string
	turnIndex int
	transport usage.Transport
}

var _ BufferedTransformer = (*anthropicUsageMeter)(nil)

func newAnthropicUsageMeter(ctx context.Context, sink usage.Sink, transport usage.Transport) *anthropicUsageMeter {
	requestID, _ := logging.RequestIDFrom(ctx)
	return &anthropicUsageMeter{sink: sink, requestID: requestID, transport: transport}
}

// TransformBuffered observes only self-contained completed Messages objects.
// Every path leaves Body.Bytes untouched and returns nil so malformed,
// incomplete, irrelevant, or future payloads remain Copilot-authoritative.
func (m *anthropicUsageMeter) TransformBuffered(_ context.Context, body *Body) error {
	messageID, model, native, ok := parseAnthropicMessage(body.Bytes)
	if !ok {
		return nil
	}
	turnIndex := m.turnIndex
	m.turnIndex++
	m.sink.Record(usage.Turn{
		At:         time.Now(),
		RequestID:  m.requestID,
		ResponseID: messageID,
		Model:      model,
		Transport:  m.transport,
		TurnIndex:  turnIndex,
		Usage:      native,
	})
	return nil
}

func parseAnthropicMessage(raw []byte) (string, string, usage.AnthropicUsage, bool) {
	object, ok := decodeJSONObject(raw)
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	messageType, ok := requiredNonemptyString(object, "type")
	if !ok || messageType != "message" {
		return "", "", usage.AnthropicUsage{}, false
	}
	messageID, ok := requiredNonemptyString(object, "id")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	model, ok := requiredNonemptyString(object, "model")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	if _, ok := requiredNonemptyString(object, "stop_reason"); !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	usageObject, ok := requiredJSONObject(object, "usage")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	inputTokens, ok := requiredNonnegativeInt64(usageObject, "input_tokens")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	outputTokens, ok := requiredNonnegativeInt64(usageObject, "output_tokens")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	cacheCreationInputTokens, ok := optionalNonnegativeInt64(usageObject, "cache_creation_input_tokens")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	cacheReadInputTokens, ok := optionalNonnegativeInt64(usageObject, "cache_read_input_tokens")
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}

	var ephemeral5mInputTokens, ephemeral1hInputTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "cache_creation"); !valid {
		return "", "", usage.AnthropicUsage{}, false
	} else if present {
		ephemeral5mInputTokens, ok = optionalNonnegativeInt64(details, "ephemeral_5m_input_tokens")
		if !ok {
			return "", "", usage.AnthropicUsage{}, false
		}
		ephemeral1hInputTokens, ok = optionalNonnegativeInt64(details, "ephemeral_1h_input_tokens")
		if !ok {
			return "", "", usage.AnthropicUsage{}, false
		}
	}

	var thinkingTokens *int64
	if details, present, valid := optionalJSONObject(usageObject, "output_tokens_details"); !valid {
		return "", "", usage.AnthropicUsage{}, false
	} else if present {
		thinkingTokens, ok = optionalNonnegativeInt64(details, "thinking_tokens")
		if !ok {
			return "", "", usage.AnthropicUsage{}, false
		}
	}

	return messageID, model, usage.AnthropicUsage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: cacheCreationInputTokens,
		CacheReadInputTokens:     cacheReadInputTokens,
		Ephemeral5mInputTokens:   ephemeral5mInputTokens,
		Ephemeral1hInputTokens:   ephemeral1hInputTokens,
		ThinkingTokens:           thinkingTokens,
	}, true
}
