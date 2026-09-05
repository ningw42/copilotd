package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/ningw42/copilotd/internal/sse"
)

type responsesItemIDStabilizer struct {
	pinnedByOutputIndex map[int]string
}

type responsesItemIDRewriteOutcome uint8

const (
	responsesItemIDUnchanged responsesItemIDRewriteOutcome = iota
	responsesItemIDChanged
	responsesItemIDUncertain
)

type validatedResponsesOutputItem struct {
	object map[string]json.RawMessage
	id     string
	hasID  bool
}

func newResponsesItemIDStabilizer() *responsesItemIDStabilizer {
	return &responsesItemIDStabilizer{pinnedByOutputIndex: make(map[int]string)}
}

var (
	_ EventTransformer         = (*responsesItemIDStabilizer)(nil)
	_ ServerMessageTransformer = (*responsesItemIDStabilizer)(nil)
)

// TransformEvent adapts the shared JSON rewrite to SSE while retaining the
// upstream frame whenever no confident rewrite is available.
func (s *responsesItemIDStabilizer) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	payload, present := frame.Data()
	if !present {
		return []sse.Frame{frame}
	}

	rewritten := s.rewrite(payload)
	if bytes.Equal(rewritten, payload) {
		return []sse.Frame{frame}
	}
	reframed, ok := frame.WithData(rewritten)
	if !ok {
		return []sse.Frame{frame}
	}
	return []sse.Frame{reframed}
}

// TransformServerMessage adapts the shared JSON rewrite to an upstream-to-
// client WebSocket message. Unrecognized payloads remain byte-verbatim, and
// every message is emitted so the stabilizer cannot fault a session.
func (s *responsesItemIDStabilizer) TransformServerMessage(_ context.Context, message *Message) bool {
	message.Data = s.rewrite(message.Data)
	return true
}

// rewrite returns payload with item ids stabilized per output_index. Payloads
// it cannot confidently rewrite are returned verbatim.
func (s *responsesItemIDStabilizer) rewrite(payload []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return payload
	}

	eventType, _, validEventType := optionalRawString(top, "type")
	if !validEventType {
		return payload
	}

	outcome := responsesItemIDUnchanged
	if outputIndexRaw, hasOutputIndex := top["output_index"]; hasOutputIndex {
		outputIndex, valid := decodeRawInt(outputIndexRaw)
		if !valid {
			outcome = responsesItemIDUncertain
		} else {
			outcome = s.rewritePerItem(top, outputIndex)
		}
	} else {
		outcome = s.rewriteEnvelope(top)
	}

	if isResponsesTurnTerminal(eventType) {
		clear(s.pinnedByOutputIndex)
	}
	if outcome != responsesItemIDChanged {
		return payload
	}
	return marshalRawObject(top)
}

func (s *responsesItemIDStabilizer) rewritePerItem(top map[string]json.RawMessage, outputIndex int) responsesItemIDRewriteOutcome {
	var item map[string]json.RawMessage
	if itemRaw, present := top["item"]; present {
		var ok bool
		item, ok = decodeRawObject(itemRaw)
		if !ok {
			return responsesItemIDUncertain
		}
	}
	itemID, hasItemID, validItemID := optionalRawString(item, "id")
	topItemID, hasTopItemID, validTopItemID := optionalRawString(top, "item_id")
	if !validItemID || !validTopItemID {
		return responsesItemIDUncertain
	}
	if !hasItemID && !hasTopItemID {
		return responsesItemIDUnchanged
	}

	pinned, alreadyPinned := s.pinnedByOutputIndex[outputIndex]
	if !alreadyPinned {
		if s.pinnedByOutputIndex == nil {
			s.pinnedByOutputIndex = make(map[int]string)
		}
		if hasItemID {
			s.pinnedByOutputIndex[outputIndex] = itemID
		} else {
			s.pinnedByOutputIndex[outputIndex] = topItemID
		}
		return responsesItemIDUnchanged
	}

	changed := false
	if hasItemID && itemID != pinned {
		item["id"] = mustMarshalString(pinned)
		top["item"] = marshalRawObject(item)
		changed = true
	}
	if hasTopItemID && topItemID != pinned {
		top["item_id"] = mustMarshalString(pinned)
		changed = true
	}
	if changed {
		return responsesItemIDChanged
	}
	return responsesItemIDUnchanged
}

func (s *responsesItemIDStabilizer) rewriteEnvelope(top map[string]json.RawMessage) responsesItemIDRewriteOutcome {
	responseRaw, present := top["response"]
	if !present {
		return responsesItemIDUnchanged
	}
	response, ok := decodeRawObject(responseRaw)
	if !ok {
		return responsesItemIDUncertain
	}
	outputRaw, ok := response["output"]
	if !ok {
		return responsesItemIDUnchanged
	}
	trimmedOutput := bytes.TrimSpace(outputRaw)
	if len(trimmedOutput) == 0 || trimmedOutput[0] != '[' {
		return responsesItemIDUncertain
	}
	var output []json.RawMessage
	if err := json.Unmarshal(outputRaw, &output); err != nil {
		return responsesItemIDUncertain
	}
	if len(output) == 0 {
		return responsesItemIDUnchanged
	}

	items := make([]validatedResponsesOutputItem, len(output))
	for index, rawItem := range output {
		item, ok := decodeRawObject(rawItem)
		if !ok {
			return responsesItemIDUncertain
		}
		itemID, present, valid := optionalRawString(item, "id")
		if !valid {
			return responsesItemIDUncertain
		}
		items[index] = validatedResponsesOutputItem{object: item, id: itemID, hasID: present}
	}

	changed := false
	for index := range output {
		pinned, ok := s.pinnedByOutputIndex[index]
		if !ok {
			continue
		}
		item := items[index]
		if !item.hasID || item.id == pinned {
			continue
		}
		item.object["id"] = mustMarshalString(pinned)
		output[index] = marshalRawObject(item.object)
		changed = true
	}
	if !changed {
		return responsesItemIDUnchanged
	}

	response["output"] = marshalRawArray(output)
	top["response"] = marshalRawObject(response)
	return responsesItemIDChanged
}

func isResponsesTurnTerminal(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

func decodeRawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if raw == nil {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func decodeRawString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

func decodeRawInt(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, false
	}
	var value int
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return 0, false
	}
	return value, true
}

func optionalRawString(object map[string]json.RawMessage, key string) (value string, present, valid bool) {
	raw, present := object[key]
	if !present {
		return "", false, true
	}
	value, valid = decodeRawString(raw)
	return value, true, valid
}

func mustMarshalString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func marshalRawObject(object map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var encoded bytes.Buffer
	encoded.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			encoded.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(key)
		encoded.Write(keyJSON)
		encoded.WriteByte(':')
		encoded.Write(object[key])
	}
	encoded.WriteByte('}')
	return encoded.Bytes()
}

func marshalRawArray(values []json.RawMessage) []byte {
	var encoded bytes.Buffer
	encoded.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			encoded.WriteByte(',')
		}
		encoded.Write(value)
	}
	encoded.WriteByte(']')
	return encoded.Bytes()
}
