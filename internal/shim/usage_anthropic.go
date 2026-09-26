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
	// counts holds one nullable value per declared Anthropic count while a
	// candidate is active. Required counts are checked only at message_stop.
	counts []*int64
}

var (
	_ RequestTransformer  = (*anthropicUsageMeter)(nil)
	_ BufferedTransformer = (*anthropicUsageMeter)(nil)
	_ EventTransformer    = (*anthropicUsageMeter)(nil)
)

func newAnthropicUsageMeter(ctx context.Context, sink usage.Sink) *anthropicUsageMeter {
	return &anthropicUsageMeter{recorder: newTurnRecorder(ctx, sink)}
}

// TransformRequest observes optional HTTP attribution independently of the
// response accumulator. Invalid attribution leaves the request unchanged.
func (m *anthropicUsageMeter) TransformRequest(_ context.Context, request *Request) error {
	m.recorder.observeRequest(request.Body)
	return nil
}

// TransformBuffered observes only self-contained completed Messages objects.
// Every path leaves Body.Bytes untouched and returns nil so malformed,
// incomplete, irrelevant, or future payloads remain Copilot-authoritative.
func (m *anthropicUsageMeter) TransformBuffered(_ context.Context, body *Body) error {
	messageID, model, native, ok := parseAnthropicMessage(body.Bytes)
	if ok {
		m.recorder.record(usage.Turn{
			ResponseID: messageID,
			Model:      model,
			Transport:  usage.TransportBuffered,
			Usage:      native,
		})
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
	counts, ok := decodeOptionalAnthropicCounts(message, "usage")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	m.accumulator.active = true
	m.accumulator.messageID = messageID
	m.accumulator.model = model
	m.accumulator.counts = make([]*int64, usage.AnthropicProjection().Len())
	m.accumulator.apply(counts)
}

func (m *anthropicUsageMeter) observeDelta(event map[string]json.RawMessage) {
	counts, ok := decodeOptionalAnthropicCounts(event, "usage")
	if !ok {
		m.accumulator.poisoned = true
		return
	}
	if m.accumulator.active {
		m.accumulator.apply(counts)
	}
}

func (m *anthropicUsageMeter) observeStop() {
	if m.accumulator.active {
		if native, err := usage.AnthropicProjection().Usage(m.accumulator.counts); err == nil {
			m.recorder.record(usage.Turn{
				ResponseID: m.accumulator.messageID,
				Model:      m.accumulator.model,
				Transport:  usage.TransportSSE,
				Usage:      native,
			})
		}
	}
	m.accumulator.clearCandidate()
}

func (a *anthropicUsageAccumulator) clearCandidate() {
	poisoned := a.poisoned
	*a = anthropicUsageAccumulator{poisoned: poisoned}
}

// apply replaces each accumulated count with a later reported value, even a
// smaller or zero one. An unreported count keeps the earlier value.
func (a *anthropicUsageAccumulator) apply(update []*int64) {
	for index, value := range update {
		if value != nil {
			a.counts[index] = value
		}
	}
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
	native, ok := decodeNativeUsage(usage.AnthropicProjection(), usageObject)
	if !ok {
		return "", "", usage.AnthropicUsage{}, false
	}
	return messageID, model, native, true
}

// decodeOptionalAnthropicCounts treats a missing or null usage object as an
// update that reports nothing; a present non-object usage is invalid.
func decodeOptionalAnthropicCounts(object map[string]json.RawMessage, key string) ([]*int64, bool) {
	usageObject, present, valid := optionalJSONObject(object, key)
	if !valid {
		return nil, false
	}
	if !present {
		return nil, true
	}
	return decodeNativeCounts(usage.AnthropicProjection(), usageObject)
}
