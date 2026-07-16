// Package sse contains the frame-aware, payload-opaque streaming engine.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Frame is one SSE event: its identified type and the exact bytes to re-emit
// downstream, including the terminating blank line. Type is the non-empty event
// name, the data.type fallback, or the SSE-default "message". Raw is
// authoritative; Type is advisory for routing and terminal detection only.
type Frame struct {
	Type string
	Raw  []byte
}

// Reader splits an upstream byte stream into SSE frames.
type Reader struct {
	source     *bufio.Reader
	onFallback func()
}

// NewReader returns a frame reader over source. onFallback is called exactly
// once whenever an absent or empty event line sends classification down the
// data.type fallback path, even when data is absent or invalid; nil is a no-op.
func NewReader(source io.Reader, onFallback func()) *Reader {
	return &Reader{source: bufio.NewReader(source), onFallback: onFallback}
}

// Read returns the next complete SSE frame. A frame is complete only once its
// terminating blank line has been read.
func (r *Reader) Read() (Frame, error) {
	var raw []byte
	for {
		line, err := r.source.ReadBytes('\n')
		raw = append(raw, line...)
		if err != nil {
			return Frame{}, err
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			typeName := eventType(raw)
			if typeName == "" {
				if r.onFallback != nil {
					r.onFallback()
				}
				typeName = dataType(raw)
				if typeName == "" {
					typeName = "message"
				}
			}
			return Frame{Type: typeName, Raw: raw}, nil
		}
	}
}

func eventType(raw []byte) string {
	var eventType string
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			end = len(raw)
		}
		line := bytes.TrimSuffix(raw[:end], []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			value = bytes.TrimPrefix(value, []byte(" "))
			eventType = string(value)
		}
		if end == len(raw) {
			break
		}
		raw = raw[end+1:]
	}
	return eventType
}

func dataType(raw []byte) string {
	var payload []byte
	found := false
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			end = len(raw)
		}
		line := bytes.TrimSuffix(raw[:end], []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			value = bytes.TrimPrefix(value, []byte(" "))
			if found {
				payload = append(payload, '\n')
			}
			payload = append(payload, value...)
			found = true
		}
		if end == len(raw) {
			break
		}
		raw = raw[end+1:]
	}
	if !found {
		return ""
	}
	var data struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &data) != nil {
		return ""
	}
	return data.Type
}
