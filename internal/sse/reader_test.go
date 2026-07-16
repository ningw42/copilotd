package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderReadsLFFrameByEventLine(t *testing.T) {
	const raw = "event: message_start\ndata: {\"type\":\"ignored\"}\n\n"
	r := NewReader(strings.NewReader(raw), nil)

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if frame.Type != "message_start" {
		t.Errorf("Type = %q, want message_start", frame.Type)
	}
	if string(frame.Raw) != raw {
		t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, raw)
	}

	_, err = r.Read()
	if err != io.EOF {
		t.Errorf("second Read() error = %v, want io.EOF", err)
	}
}

func TestReaderPreservesMultilineCRLFFrameAndUnknownType(t *testing.T) {
	const raw = ": vendor metadata\r\nevent: vendor.future_event\r\ndata: {\"unknown\":true,\r\ndata: \"kept\":\"verbatim\"}\r\n\r\n"
	r := NewReader(strings.NewReader(raw), nil)

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if frame.Type != "vendor.future_event" {
		t.Errorf("Type = %q, want vendor.future_event", frame.Type)
	}
	if string(frame.Raw) != raw {
		t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, raw)
	}
}

func TestReaderPrefersEventLineAndSignalsDataTypeFallback(t *testing.T) {
	const eventRaw = "event: line_wins\ndata: {\"type\":\"data_loses\"}\n\n"
	const fallbackRaw = "data: {\"type\":\"response.output_text.delta\",\"unknown\":true}\n\n"
	fallbacks := 0
	r := NewReader(strings.NewReader(eventRaw+fallbackRaw), func() { fallbacks++ })

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("first Read() error = %v, want nil", err)
	}
	if frame.Type != "line_wins" {
		t.Errorf("first Type = %q, want line_wins", frame.Type)
	}
	if string(frame.Raw) != eventRaw {
		t.Errorf("first Raw = %q, want %q", frame.Raw, eventRaw)
	}
	if fallbacks != 0 {
		t.Errorf("fallback count after event-line frame = %d, want 0", fallbacks)
	}

	frame, err = r.Read()
	if err != nil {
		t.Fatalf("second Read() error = %v, want nil", err)
	}
	if frame.Type != "response.output_text.delta" {
		t.Errorf("second Type = %q, want response.output_text.delta", frame.Type)
	}
	if string(frame.Raw) != fallbackRaw {
		t.Errorf("second Raw = %q, want %q", frame.Raw, fallbackRaw)
	}
	if fallbacks != 1 {
		t.Errorf("fallback count after data.type frame = %d, want 1", fallbacks)
	}
}

func TestReaderUsesLastRepeatedEventField(t *testing.T) {
	const raw = "event: response.output_text.delta\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	r := NewReader(strings.NewReader(raw), nil)

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if frame.Type != "response.completed" {
		t.Errorf("Type = %q, want last event field response.completed", frame.Type)
	}
	if string(frame.Raw) != raw {
		t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, raw)
	}
}

func TestReaderRepeatedEventFallbackFollowsLastField(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantType      string
		wantFallbacks int
	}{
		{
			name:          "last event is empty",
			raw:           "event: error\nevent:\ndata: {\"type\":\"response.output_text.delta\"}\n\n",
			wantType:      "response.output_text.delta",
			wantFallbacks: 1,
		},
		{
			name:          "last event is non-empty",
			raw:           "event:\nevent: response.completed\ndata: {\"type\":\"response.output_text.delta\"}\n\n",
			wantType:      "response.completed",
			wantFallbacks: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallbacks := 0
			r := NewReader(strings.NewReader(tt.raw), func() { fallbacks++ })

			frame, err := r.Read()
			if err != nil {
				t.Fatalf("Read() error = %v, want nil", err)
			}
			if frame.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", frame.Type, tt.wantType)
			}
			if string(frame.Raw) != tt.raw {
				t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, tt.raw)
			}
			if fallbacks != tt.wantFallbacks {
				t.Errorf("fallback count = %d, want %d", fallbacks, tt.wantFallbacks)
			}
		})
	}
}

func TestReaderEmptyEventAndNeitherCasesUseFallback(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty event uses data type", raw: "event:\ndata: {\"type\":\"response.created\"}\n\n", want: "response.created"},
		{name: "comment frame", raw: ": upstream keepalive\n\n", want: "message"},
		{name: "non JSON data", raw: "data: [DONE]\n\n", want: "message"},
		{name: "absent data", raw: "id: opaque\n\n", want: "message"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallbacks := 0
			r := NewReader(strings.NewReader(tt.raw), func() { fallbacks++ })
			frame, err := r.Read()
			if err != nil {
				t.Fatalf("Read() error = %v, want nil", err)
			}
			if frame.Type != tt.want {
				t.Errorf("Type = %q, want %q", frame.Type, tt.want)
			}
			if string(frame.Raw) != tt.raw {
				t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, tt.raw)
			}
			if fallbacks != 1 {
				t.Errorf("fallback count = %d, want 1", fallbacks)
			}
		})
	}
}

func TestReaderFallbackJoinsMultilineDataPayload(t *testing.T) {
	const raw = "data: {\r\ndata: \"type\": \"response.completed\",\r\ndata: \"unknown\": true\r\ndata: }\r\n\r\n"
	fallbacks := 0
	r := NewReader(strings.NewReader(raw), func() { fallbacks++ })

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if frame.Type != "response.completed" {
		t.Errorf("Type = %q, want response.completed", frame.Type)
	}
	if string(frame.Raw) != raw {
		t.Errorf("Raw = %q, want exact upstream bytes %q", frame.Raw, raw)
	}
	if fallbacks != 1 {
		t.Errorf("fallback count = %d, want 1", fallbacks)
	}
}

func TestReaderReturnsEOFForIncompleteFrameWithoutClassifyingIt(t *testing.T) {
	fallbacks := 0
	r := NewReader(strings.NewReader("data: {\"type\":\"not-dispatched\"}\n"), func() { fallbacks++ })

	frame, err := r.Read()
	if err != io.EOF {
		t.Fatalf("Read() error = %v, want io.EOF", err)
	}
	if frame.Type != "" || frame.Raw != nil {
		t.Errorf("Frame = %#v, want zero value for incomplete input", frame)
	}
	if fallbacks != 0 {
		t.Errorf("fallback count = %d, want 0 for an incomplete frame", fallbacks)
	}
}

func TestReaderReturnsUpstreamReadError(t *testing.T) {
	boom := errors.New("upstream read failed")
	r := NewReader(&readerEndingWithError{
		data: []byte("event: incomplete\ndata: partial"),
		err:  boom,
	}, nil)

	frame, err := r.Read()
	if !errors.Is(err, boom) {
		t.Fatalf("Read() error = %v, want %v", err, boom)
	}
	if frame.Type != "" || frame.Raw != nil {
		t.Errorf("Frame = %#v, want zero value for incomplete input", frame)
	}
}

func TestReaderAllowsNilFallbackSignal(t *testing.T) {
	const raw = ": keepalive\n\n"
	r := NewReader(strings.NewReader(raw), nil)

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if frame.Type != "message" || string(frame.Raw) != raw {
		t.Errorf("Frame = %#v, want message with exact raw bytes %q", frame, raw)
	}
}

type readerEndingWithError struct {
	data []byte
	err  error
}

func (r *readerEndingWithError) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
