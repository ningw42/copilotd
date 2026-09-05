package shim

import (
	"context"
	"encoding/json"

	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

type anthropicUsageMeter struct {
	recorder    turnRecorder
	accumulator anthropicUsageAccumulator
}

type anthropicUsageAccumulator struct {
	active    bool
	poisoned  bool
	messageID string
	model     string
	usage     anthropicUsageReport
}

type anthropicUsageReport struct {
	inputTokens              anthropicReportedCount
	outputTokens             anthropicReportedCount
	cacheCreationInputTokens anthropicReportedCount
	cacheReadInputTokens     anthropicReportedCount
	ephemeral5mInputTokens   anthropicReportedCount
	ephemeral1hInputTokens   anthropicReportedCount
	thinkingTokens           anthropicReportedCount
}

type anthropicReportedCount struct {
	value    int64
	reported bool
}

var (
	_ BufferedTransformer = (*anthropicUsageMeter)(nil)
	_ EventTransformer    = (*anthropicUsageMeter)(nil)
)

func newAnthropicUsageMeter(ctx context.Context, sink usage.Sink) *anthropicUsageMeter {
	return &anthropicUsageMeter{recorder: newTurnRecorder(ctx, sink)}
}

// TransformBuffered observes only self-contained completed Messages objects.
// Every path leaves Body.Bytes untouched and returns nil so malformed,
// incomplete, irrelevant, or future payloads remain Copilot-authoritative.
func (m *anthropicUsageMeter) TransformBuffered(_ context.Context, body *Body) error {
	messageID, model, native, ok := parseAnthropicMessage(body.Bytes)
	if ok {
		m.recorder.record(messageID, model, usage.TransportBuffered, native)
	}
	return nil
}

// TransformEvent accumulates only Messages lifecycle events routed by the
// advisory frame type. Every path returns the exact original frame; JSON is
// decoded only from Frame.Data, never from Raw SSE framing.
func (m *anthropicUsageMeter) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	switch frame.Type {
	case "message_start", "message_delta", "message_stop", "error":
		payload, present := frame.Data()
		m.observeEvent(frame.Type, payload, present)
	}
	return []sse.Frame{frame}
}

func (m *anthropicUsageMeter) observeEvent(advisoryType string, payload []byte, present bool) {
	if m.accumulator.poisoned {
		return
	}
	if !present {
		m.accumulator.poisoned = true
		return
	}
	object, ok := decodeJSONObject(payload)
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	decodedType, ok := requiredNonemptyString(object, "type")
	if !ok || decodedType != advisoryType {
		m.accumulator.poisoned = true
		return
	}

	switch advisoryType {
	case "message_start":
		m.observeStart(object)
	case "message_delta":
		m.observeDelta(object)
	case "message_stop":
		m.observeStop()
	case "error":
		m.accumulator.poisoned = true
	}
}

func (m *anthropicUsageMeter) observeStart(event map[string]json.RawMessage) {
	if m.accumulator.active {
		m.accumulator.poisoned = true
		return
	}
	message, ok := requiredJSONObject(event, "message")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	messageID, ok := requiredNonemptyString(message, "id")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	model, ok := requiredNonemptyString(message, "model")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	report, ok := decodeOptionalAnthropicUsage(message, "usage")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	m.accumulator.active = true
	m.accumulator.messageID = messageID
	m.accumulator.model = model
	m.accumulator.usage.apply(report)
}

func (m *anthropicUsageMeter) observeDelta(event map[string]json.RawMessage) {
	report, ok := decodeOptionalAnthropicUsage(event, "usage")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	if m.accumulator.active {
		m.accumulator.usage.apply(report)
	}
}

func (m *anthropicUsageMeter) observeStop() {
	if m.accumulator.active && m.accumulator.usage.inputTokens.reported && m.accumulator.usage.outputTokens.reported {
		m.recorder.record(m.accumulator.messageID, m.accumulator.model, usage.TransportSSE, m.accumulator.usage.native())
	}
	m.accumulator.clearCandidate()
}

func (a *anthropicUsageAccumulator) clearCandidate() {
	poisoned := a.poisoned
	*a = anthropicUsageAccumulator{poisoned: poisoned}
}

func (r *anthropicUsageReport) apply(update anthropicUsageReport) {
	applyAnthropicCount(&r.inputTokens, update.inputTokens)
	applyAnthropicCount(&r.outputTokens, update.outputTokens)
	applyAnthropicCount(&r.cacheCreationInputTokens, update.cacheCreationInputTokens)
	applyAnthropicCount(&r.cacheReadInputTokens, update.cacheReadInputTokens)
	applyAnthropicCount(&r.ephemeral5mInputTokens, update.ephemeral5mInputTokens)
	applyAnthropicCount(&r.ephemeral1hInputTokens, update.ephemeral1hInputTokens)
	applyAnthropicCount(&r.thinkingTokens, update.thinkingTokens)
}

func applyAnthropicCount(current *anthropicReportedCount, update anthropicReportedCount) {
	if update.reported {
		*current = update
	}
}

func (r anthropicUsageReport) native() usage.AnthropicUsage {
	return usage.AnthropicUsage{
		InputTokens:              r.inputTokens.value,
		OutputTokens:             r.outputTokens.value,
		CacheCreationInputTokens: r.cacheCreationInputTokens.pointer(),
		CacheReadInputTokens:     r.cacheReadInputTokens.pointer(),
		Ephemeral5mInputTokens:   r.ephemeral5mInputTokens.pointer(),
		Ephemeral1hInputTokens:   r.ephemeral1hInputTokens.pointer(),
		ThinkingTokens:           r.thinkingTokens.pointer(),
	}
}

func (c anthropicReportedCount) pointer() *int64 {
	if !c.reported {
		return nil
	}
	value := c.value
	return &value
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
	report, ok := decodeAnthropicUsage(usageObject)
	if !ok || !report.inputTokens.reported || !report.outputTokens.reported {
		return "", "", usage.AnthropicUsage{}, false
	}
	return messageID, model, report.native(), true
}

func decodeOptionalAnthropicUsage(object map[string]json.RawMessage, key string) (anthropicUsageReport, bool) {
	usageObject, present, valid := optionalJSONObject(object, key)
	if !valid {
		return anthropicUsageReport{}, false
	}
	if !present {
		return anthropicUsageReport{}, true
	}
	return decodeAnthropicUsage(usageObject)
}

func decodeAnthropicUsage(object map[string]json.RawMessage) (anthropicUsageReport, bool) {
	var report anthropicUsageReport
	var ok bool
	if report.inputTokens, ok = reportedAnthropicCount(object, "input_tokens"); !ok {
		return anthropicUsageReport{}, false
	}
	if report.outputTokens, ok = reportedAnthropicCount(object, "output_tokens"); !ok {
		return anthropicUsageReport{}, false
	}
	if report.cacheCreationInputTokens, ok = reportedAnthropicCount(object, "cache_creation_input_tokens"); !ok {
		return anthropicUsageReport{}, false
	}
	if report.cacheReadInputTokens, ok = reportedAnthropicCount(object, "cache_read_input_tokens"); !ok {
		return anthropicUsageReport{}, false
	}
	if details, present, valid := optionalJSONObject(object, "cache_creation"); !valid {
		return anthropicUsageReport{}, false
	} else if present {
		if report.ephemeral5mInputTokens, ok = reportedAnthropicCount(details, "ephemeral_5m_input_tokens"); !ok {
			return anthropicUsageReport{}, false
		}
		if report.ephemeral1hInputTokens, ok = reportedAnthropicCount(details, "ephemeral_1h_input_tokens"); !ok {
			return anthropicUsageReport{}, false
		}
	}
	if details, present, valid := optionalJSONObject(object, "output_tokens_details"); !valid {
		return anthropicUsageReport{}, false
	} else if present {
		if report.thinkingTokens, ok = reportedAnthropicCount(details, "thinking_tokens"); !ok {
			return anthropicUsageReport{}, false
		}
	}
	return report, true
}

func reportedAnthropicCount(object map[string]json.RawMessage, key string) (anthropicReportedCount, bool) {
	raw, exists := object[key]
	if !exists || isJSONNull(raw) {
		return anthropicReportedCount{}, true
	}
	value, ok := parseNonnegativeInt64(raw)
	if !ok {
		return anthropicReportedCount{}, false
	}
	return anthropicReportedCount{value: value, reported: true}, true
}
