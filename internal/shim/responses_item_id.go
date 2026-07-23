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
func (s *responsesItemIDStabilizer) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	payload, fields := sseDataPayload(frame.Raw)
	if len(fields) == 0 {
		return []sse.Frame{frame}, nil
	}

	rewritten := s.rewrite(payload)
	if bytes.Equal(rewritten, payload) {
		return []sse.Frame{frame}, nil
	}
	reframed, ok := replaceSSEDataPayload(frame.Raw, fields, rewritten)
	if !ok {
		return []sse.Frame{frame}, nil
	}
	frame.Raw = reframed
	return []sse.Frame{frame}, nil
}

// TransformServerMessage adapts the shared JSON rewrite to an upstream-to-
// client WebSocket message. Unrecognized payloads remain byte-verbatim, and
// every message is emitted so the stabilizer cannot fault a session.
func (s *responsesItemIDStabilizer) TransformServerMessage(_ context.Context, message *Message) (bool, error) {
	message.Data = s.rewrite(message.Data)
	return true, nil
}

type sseDataField struct {
	lineStart  int
	valueStart int
	valueEnd   int
	lineEnd    int
}

// sseDataPayload mirrors sse.Reader's data-field semantics: one optional space
// after "data:" is ignored and repeated fields are joined with a newline.
func sseDataPayload(raw []byte) ([]byte, []sseDataField) {
	var payload []byte
	var fields []sseDataField
	for lineStart := 0; lineStart < len(raw); {
		lineEnd := bytes.IndexByte(raw[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd += lineStart + 1
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && raw[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > lineStart && raw[contentEnd-1] == '\r' {
			contentEnd--
		}

		line := raw[lineStart:contentEnd]
		if bytes.HasPrefix(line, []byte("data:")) {
			valueStart := lineStart + len("data:")
			if valueStart < contentEnd && raw[valueStart] == ' ' {
				valueStart++
			}
			if len(fields) > 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, raw[valueStart:contentEnd]...)
			fields = append(fields, sseDataField{
				lineStart:  lineStart,
				valueStart: valueStart,
				valueEnd:   contentEnd,
				lineEnd:    lineEnd,
			})
		}
		if lineEnd == len(raw) {
			break
		}
		lineStart = lineEnd
	}
	return payload, fields
}

func replaceSSEDataPayload(raw []byte, fields []sseDataField, payload []byte) ([]byte, bool) {
	logicalLines := bytes.Split(payload, []byte("\n"))
	replacements := make([][]byte, len(fields))
	for i := 0; i < len(logicalLines) && i < len(fields); i++ {
		replacements[i] = logicalLines[i]
	}
	if len(logicalLines) > len(fields) {
		last := fields[len(fields)-1]
		lineEnding := raw[last.valueEnd:last.lineEnd]
		if len(lineEnding) == 0 {
			return nil, false
		}
		prefix := raw[last.lineStart:last.valueStart]
		var expanded bytes.Buffer
		expanded.Write(replacements[len(replacements)-1])
		for _, logicalLine := range logicalLines[len(fields):] {
			expanded.Write(lineEnding)
			expanded.Write(prefix)
			expanded.Write(logicalLine)
		}
		replacements[len(replacements)-1] = expanded.Bytes()
	}

	var reframed bytes.Buffer
	previous := 0
	for i, field := range fields {
		reframed.Write(raw[previous:field.valueStart])
		reframed.Write(replacements[i])
		previous = field.valueEnd
	}
	reframed.Write(raw[previous:])
	return reframed.Bytes(), true
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
