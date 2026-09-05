package sse_test

import (
	"bytes"
	"testing"

	"github.com/ningw42/copilotd/internal/sse"
)

func TestFrameDataPresenceAndExistingFieldGrammar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    string
		present bool
	}{
		{name: "zero frame"},
		{name: "metadata only", raw: ": keepalive\r\nretry: 1000\r\n\r\n"},
		{name: "empty field", raw: "data:\n\n", present: true},
		{name: "empty spaced field", raw: "data: \r\n\r\n", present: true},
		{name: "repeated empty fields", raw: "data:\ndata: \n\n", want: "\n", present: true},
		{name: "spaces and tabs", raw: "data:  first\ndata:\tsecond\ndata:third\n\n", want: " first\n\tsecond\nthird", present: true},
		{name: "unrecognized field spellings", raw: " data: ignored\nData: ignored\ndata\ndata : ignored\n\n"},
		{name: "unterminated field", raw: "data: last", want: "last", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, present := (sse.Frame{Raw: []byte(tc.raw)}).Data()
			if string(payload) != tc.want || present != tc.present {
				t.Fatalf("Data() = (%q, %t), want (%q, %t)", payload, present, tc.want, tc.present)
			}
		})
	}
}

func TestFrameDataReturnsIndependentBytes(t *testing.T) {
	frame := sse.Frame{Raw: []byte("data: original\n\n")}
	payload, present := frame.Data()
	if !present || string(payload) != "original" {
		t.Fatalf("Data() = (%q, %t), want original", payload, present)
	}
	payload[0] = 'O'
	if string(frame.Raw) != "data: original\n\n" {
		t.Fatal("changing extracted data mutated Raw")
	}
	frame.Raw[6] = 'X'
	if string(payload) != "Original" {
		t.Fatal("changing Raw mutated extracted data")
	}
}

func TestFrameWithDataReplacesUsingExistingLayout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		payload string
		want    string
	}{
		{
			name:    "LF",
			raw:     "data: first\nid: between\ndata: second\nretry: 1000\n\n",
			payload: "one\ntwo\nthree",
			want:    "data: one\nid: between\ndata: two\ndata: three\nretry: 1000\n\n",
		},
		{
			name:    "mixed prefixes and line endings",
			raw:     "data: first\n: between\ndata:second\r\nretry: 1000\r\n\r\n",
			payload: "one\ntwo\nthree",
			want:    "data: one\n: between\ndata:two\r\ndata:three\r\nretry: 1000\r\n\r\n",
		},
		{
			name:    "trailing payload newline",
			raw:     "data: old\n\n",
			payload: "one\n",
			want:    "data: one\ndata: \n\n",
		},
		{
			name: "empty replacement retains fields",
			raw:  "data: first\n: between\ndata: second\n\n",
			want: "data: \n: between\ndata: \n\n",
		},
		{
			name:    "unterminated field without expansion",
			raw:     "data: old",
			payload: "new",
			want:    "data: new",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := sse.Frame{Type: "advisory", Raw: []byte(tc.raw)}
			payload := []byte(tc.payload)
			got, ok := frame.WithData(payload)
			if !ok || string(got.Raw) != tc.want || got.Type != frame.Type {
				t.Fatalf("WithData() = (%#v, %t), want type preserved and %q", got, ok, tc.want)
			}
			if string(frame.Raw) != tc.raw || string(payload) != tc.payload {
				t.Fatal("WithData mutated its input frame or payload")
			}
		})
	}
}

func TestFrameWithDataDeclinesWithoutChangingInput(t *testing.T) {
	for _, raw := range []string{"", ": metadata\r\nid: opaque\r\n\r\n", "data: old", "data:"} {
		frame := sse.Frame{Type: "advisory", Raw: []byte(raw)}
		got, ok := frame.WithData([]byte("one\ntwo"))
		if ok || !bytes.Equal(got.Raw, frame.Raw) || got.Type != frame.Type {
			t.Fatalf("WithData(%q) = (%#v, %t), want original frame and false", raw, got, ok)
		}
		if len(raw) > 0 && &got.Raw[0] != &frame.Raw[0] {
			t.Fatal("declined WithData allocated replacement Raw")
		}
	}
}

func TestFrameWithDataUsesLatestRawAcrossRewrites(t *testing.T) {
	const raw = "event: advisory\ndata: short\n\n"
	original := sse.Frame{Type: "advisory", Raw: []byte(raw)}
	first, ok := original.WithData([]byte("longer\nsecond"))
	if !ok {
		t.Fatal("first replacement declined")
	}
	last, ok := first.WithData([]byte("last"))
	const want = "event: advisory\ndata: last\ndata: \n\n"
	if !ok || string(last.Raw) != want || last.Type != original.Type {
		t.Fatalf("second replacement = (%#v, %t), want current field layout %q", last, ok, want)
	}
	if string(original.Raw) != raw || string(first.Raw) != "event: advisory\ndata: longer\ndata: second\n\n" {
		t.Fatal("chained replacements mutated earlier frames")
	}
}

func TestFrameWithDataUnchangedRetainsOriginalRaw(t *testing.T) {
	const raw = "data:first\r\n: between\r\ndata: second\r\n\r\n"
	frame := sse.Frame{Type: "advisory", Raw: []byte(raw)}

	got, ok := frame.WithData([]byte("first\nsecond"))

	if !ok || string(got.Raw) != raw || got.Type != frame.Type {
		t.Fatalf("WithData() = (%#v, %t), want original frame", got, ok)
	}
	if &got.Raw[0] != &frame.Raw[0] {
		t.Fatal("unchanged WithData allocated replacement Raw")
	}
}

func TestFrameWithDataPreservesFramingAndSurplusFields(t *testing.T) {
	const raw = ": metadata\r\nevent: response.output_item.done\r\nid: opaque\r\ndata: {\r\ndata: \"item\":{\"id\":\"old\"}}\r\nretry: 1000\r\n\r\n"
	frame := sse.Frame{Type: "advisory-type-must-stay", Raw: []byte(raw)}

	got, ok := frame.WithData([]byte(`{"item":{"id":"pinned"}}`))

	const want = ": metadata\r\nevent: response.output_item.done\r\nid: opaque\r\ndata: {\"item\":{\"id\":\"pinned\"}}\r\ndata: \r\nretry: 1000\r\n\r\n"
	if !ok || string(got.Raw) != want || got.Type != frame.Type {
		t.Fatalf("WithData() = (%#v, %t), want preserved type and framing %q", got, ok, want)
	}
	if string(frame.Raw) != raw {
		t.Fatal("WithData mutated the input frame")
	}
	payload, present := got.Data()
	if !present || string(payload) != "{\"item\":{\"id\":\"pinned\"}}\n" {
		t.Fatalf("replacement Data() = (%q, %t), want retained empty field's joining newline", payload, present)
	}
}

func TestFrameDataJoinsFieldsAndRemovesOnlyOneSpace(t *testing.T) {
	const raw = ": metadata\r\nevent: response.completed\r\ndata: {\r\ndata:  \"type\":\"response.completed\"}\r\nid: opaque\r\n\r\n"
	frame := sse.Frame{Type: "advisory", Raw: []byte(raw)}

	payload, present := frame.Data()

	if !present || string(payload) != "{\n \"type\":\"response.completed\"}" {
		t.Fatalf("Data() = (%q, %t), want joined payload with one remaining space", payload, present)
	}
}

func FuzzFrameDataPreservesInput(f *testing.F) {
	f.Add("data: one\n\n", "two\nthree")
	f.Add(": metadata\r\ndata:first\r\nid: between\r\ndata: second\r\n\r\n", "replacement")
	f.Add("data: unterminated", "one\ntwo")
	f.Add(": no data\n\n", "ignored")
	f.Fuzz(func(t *testing.T, raw, replacement string) {
		if len(raw)+len(replacement) > 64<<10 {
			t.Skip("bound fuzz input sizes, not the frame contract")
		}
		frame := sse.Frame{Type: "advisory", Raw: []byte(raw)}
		payload, present := frame.Data()
		unchanged, ok := frame.WithData(payload)
		if ok != present || string(unchanged.Raw) != raw || unchanged.Type != frame.Type {
			t.Fatal("replacing extracted data did not preserve the frame")
		}
		if len(raw) > 0 && &unchanged.Raw[0] != &frame.Raw[0] {
			t.Fatal("no-op replacement did not retain Raw")
		}
		if len(payload) > 0 {
			payload[0] ^= 0xff
		}
		if string(frame.Raw) != raw {
			t.Fatal("extracted payload aliases Raw")
		}

		input := []byte(replacement)
		got, ok := frame.WithData(input)
		if string(frame.Raw) != raw || string(input) != replacement || got.Type != frame.Type {
			t.Fatal("replacement mutated an input or changed Type")
		}
		if !ok && string(got.Raw) != raw {
			t.Fatal("declined replacement did not preserve Raw")
		}
	})
}
