package sse

import "bytes"

// Data returns the joined data-field values from the current Raw bytes. It
// removes at most one space after each "data:" prefix and joins repeated values
// with a newline. The bool distinguishes an absent data field from an empty one.
// The returned bytes do not alias Raw.
func (f Frame) Data() ([]byte, bool) {
	payload, fields := parseData(f.Raw)
	return payload, len(fields) > 0
}

// WithData replaces data-field values while preserving Type and all other Raw
// bytes, including field prefixes and line endings. Surplus fields remain empty,
// so Data may return additional trailing joining newlines. Extra payload lines
// use the last field's prefix and line ending. WithData returns the original
// frame and false if there are no data fields or expansion needs a missing line
// ending. A byte-identical payload returns the original frame and true, retaining
// its Raw allocation. It does not mutate the frame or payload.
func (f Frame) WithData(payload []byte) (Frame, bool) {
	current, fields := parseData(f.Raw)
	if len(fields) == 0 {
		return f, false
	}
	if bytes.Equal(current, payload) {
		return f, true
	}
	raw, ok := replaceData(f.Raw, fields, payload)
	if !ok {
		return f, false
	}
	f.Raw = raw
	return f, true
}

type dataField struct {
	lineStart  int
	valueStart int
	valueEnd   int
	lineEnd    int
}

func parseData(raw []byte) ([]byte, []dataField) {
	var payload []byte
	var fields []dataField
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
			fields = append(fields, dataField{
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

func replaceData(raw []byte, fields []dataField, payload []byte) ([]byte, bool) {
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
