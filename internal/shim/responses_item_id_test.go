package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/ningw42/copilotd/internal/sse"
)

func TestResponsesItemIDStabilizerTransformsSSEFrameWithoutDisturbingFraming(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	first := sse.Frame{
		Type: "response.output_item.added",
		Raw:  []byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"pinned-id\"}}\n\n"),
	}
	frames, err := stabilizer.TransformEvent(context.Background(), first)
	if err != nil {
		t.Fatalf("first TransformEvent() error = %v, want nil", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0].Raw, first.Raw) {
		t.Fatalf("first TransformEvent() = %#v, want original frame untouched", frames)
	}

	const changedRaw = ": vendor metadata\r\nevent: response.output_item.done\r\nid: opaque\r\ndata: {\"type\":\"response.output_item.done\",\r\ndata: \"output_index\":0,\"item\":{\"id\":\"churned-id\",\"content\":[ 1, 2 ]}}\r\nretry: 1000\r\n\r\n"
	changed := sse.Frame{Type: "advisory-type-must-stay", Raw: []byte(changedRaw)}
	frames, err = stabilizer.TransformEvent(context.Background(), changed)
	if err != nil {
		t.Fatalf("changed TransformEvent() error = %v, want nil", err)
	}
	if len(frames) != 1 {
		t.Fatalf("changed TransformEvent() frames = %d, want 1", len(frames))
	}
	got := frames[0]
	if got.Type != changed.Type {
		t.Errorf("changed frame Type = %q, want unchanged %q", got.Type, changed.Type)
	}
	const wantRaw = ": vendor metadata\r\nevent: response.output_item.done\r\nid: opaque\r\ndata: {\"item\":{\"content\":[ 1, 2 ],\"id\":\"pinned-id\"},\"output_index\":0,\"type\":\"response.output_item.done\"}\r\ndata: \r\nretry: 1000\r\n\r\n"
	if string(got.Raw) != wantRaw {
		t.Errorf("changed frame Raw = %q, want framing-preserving %q", got.Raw, wantRaw)
	}
	payload, _ := sseDataPayload(got.Raw)
	item := rawObject(t, rawObject(t, payload)["item"])
	if id := rawString(t, item["id"]); id != "pinned-id" {
		t.Errorf("rewritten item.id = %q, want pinned-id", id)
	}
}

func TestResponsesItemIDStabilizerSSEAdapterAlwaysFailsSafe(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "data-less", raw: ": upstream keepalive\r\nretry: 1000\r\n\r\n"},
		{name: "malformed JSON", raw: "event: vendor.malformed\ndata: {\"type\":\n\n"},
		{name: "structurally inert", raw: "event: vendor.metadata\ndata:  { \"type\" : \"vendor.metadata\", \"opaque\" : [ 1 ] } \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stabilizer := newResponsesItemIDStabilizer()
			frame := sse.Frame{Type: "advisory", Raw: []byte(tt.raw)}
			frames, err := stabilizer.TransformEvent(context.Background(), frame)
			if err != nil {
				t.Fatalf("TransformEvent() error = %v, want nil", err)
			}
			if len(frames) != 1 || frames[0].Type != frame.Type || !bytes.Equal(frames[0].Raw, frame.Raw) {
				t.Fatalf("TransformEvent() = %#v, want original frame untouched", frames)
			}
			if len(frame.Raw) > 0 && &frames[0].Raw[0] != &frame.Raw[0] {
				t.Error("byte-identical TransformEvent() allocated replacement Raw, want original slice")
			}
		})
	}
}

func TestResponsesItemIDStabilizerTransformsServerMessagesAndAlwaysEmits(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"pinned-id"}}`)
	message := &Message{Kind: MessageText, Data: added}
	emit, err := stabilizer.TransformServerMessage(context.Background(), message)
	if err != nil || !emit {
		t.Fatalf("first TransformServerMessage() = (emit %t, error %v), want (true, nil)", emit, err)
	}
	if !bytes.Equal(message.Data, added) {
		t.Fatalf("first message Data = %s, want verbatim %s", message.Data, added)
	}

	churned := []byte(`{"type":"response.output_text.delta","output_index":0,"item_id":"churned-id","delta":"hello"}`)
	message = &Message{Kind: MessageBinary, Data: churned}
	emit, err = stabilizer.TransformServerMessage(context.Background(), message)
	if err != nil || !emit {
		t.Fatalf("second TransformServerMessage() = (emit %t, error %v), want (true, nil)", emit, err)
	}
	if message.Kind != MessageBinary {
		t.Errorf("message Kind = %v, want unchanged binary kind", message.Kind)
	}
	if got := rawString(t, rawObject(t, message.Data)["item_id"]); got != "pinned-id" {
		t.Errorf("rewritten item_id = %q, want pinned-id", got)
	}

	for _, payload := range [][]byte{
		[]byte(`{"type":`),
		[]byte(` { "type" : "vendor.metadata", "opaque" : [ 1 ] } `),
	} {
		message = &Message{Kind: MessageText, Data: payload}
		emit, err = stabilizer.TransformServerMessage(context.Background(), message)
		if err != nil || !emit {
			t.Errorf("fail-safe TransformServerMessage(%s) = (emit %t, error %v), want (true, nil)", payload, emit, err)
		}
		if !bytes.Equal(message.Data, payload) {
			t.Errorf("fail-safe message Data = %s, want verbatim %s", message.Data, payload)
		}
	}
}

func TestResponsesItemIDStabilizerRewritesCapturedCopilotChurn(t *testing.T) {
	frames := capturedResponsesFrames(t)
	wantTypes := []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantTypes) {
		t.Fatalf("captured frames = %d, want %d", len(frames), len(wantTypes))
	}

	stabilizer := newResponsesItemIDStabilizer()
	seenUpstreamIDs := map[string]bool{}
	var pinned string
	for i, frame := range frames {
		t.Run(wantTypes[i], func(t *testing.T) {
			if frame.Type != wantTypes[i] {
				t.Fatalf("captured type = %q, want %q", frame.Type, wantTypes[i])
			}
			payload := frameData(t, frame.Raw)
			if frame.Type == "response.completed" {
				upstreamOutput := responseOutput(t, payload)
				if upstreamID := rawString(t, rawObject(t, upstreamOutput[1])["id"]); upstreamID == pinned {
					t.Fatal("captured terminal id equals the added id; fixture no longer demonstrates terminal churn")
				}
				got := stabilizer.rewrite(payload)
				if bytes.Equal(got, payload) {
					t.Fatal("captured terminal was not rewritten by output position")
				}
				gotOutput := responseOutput(t, got)
				if gotID := rawString(t, rawObject(t, gotOutput[1])["id"]); gotID != pinned {
					t.Errorf("captured terminal output[1].id = %q, want first added id %q", gotID, pinned)
				}
				if !bytes.Equal(gotOutput[0], upstreamOutput[0]) {
					t.Errorf("captured unpinned terminal output[0] changed = %s, want exact %s", gotOutput[0], upstreamOutput[0])
				}
				if len(stabilizer.pinnedByOutputIndex) != 0 {
					t.Errorf("pins after captured terminal = %v, want reset", stabilizer.pinnedByOutputIndex)
				}
				return
			}

			index, upstreamID := payloadIndexAndID(t, payload)
			if index != 1 {
				t.Fatalf("captured output_index = %d, want stable index 1", index)
			}
			if seenUpstreamIDs[upstreamID] {
				t.Fatalf("captured upstream id repeated; fixture no longer demonstrates churn")
			}
			seenUpstreamIDs[upstreamID] = true

			got := stabilizer.rewrite(payload)
			if i == 0 {
				pinned = upstreamID
				if !bytes.Equal(got, payload) {
					t.Fatal("first captured item sighting was re-marshaled, want verbatim pin")
				}
				return
			}
			_, gotID := payloadIndexAndID(t, got)
			if gotID != pinned {
				t.Errorf("stabilized id = %q, want first added id %q", gotID, pinned)
			}
		})
	}
}

func TestResponsesItemIDStabilizerNestedDecodeFailuresAreTransactional(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "non-object item", payload: []byte(`{"output_index":7,"item":"not-an-object","item_id":"must-not-pin"}`)},
		{name: "invalid item id", payload: []byte(`{"output_index":7,"item":{"id":17},"item_id":"must-not-pin"}`)},
		{name: "invalid top-level item id", payload: []byte(`{"output_index":7,"item":{"id":"must-not-pin"},"item_id":17}`)},
		{name: "invalid output index", payload: []byte(`{"output_index":"seven","item_id":"must-not-pin"}`)},
		{name: "invalid event type", payload: []byte(`{"type":17,"output_index":7,"item_id":"must-not-pin"}`)},
		{name: "null item id", payload: []byte(`{"output_index":7,"item":{"id":null},"item_id":"must-not-pin"}`)},
		{name: "null top-level item id", payload: []byte(`{"output_index":7,"item":{"id":"must-not-pin"},"item_id":null}`)},
		{name: "null output index", payload: []byte(`{"output_index":null,"item_id":"must-not-pin"}`)},
		{name: "null event type", payload: []byte(`{"type":null,"output_index":7,"item_id":"must-not-pin"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stabilizer := newResponsesItemIDStabilizer()
			if got := stabilizer.rewrite(tt.payload); !bytes.Equal(got, tt.payload) {
				t.Fatalf("malformed event = %s, want exact pass-through %s", got, tt.payload)
			}
			if len(stabilizer.pinnedByOutputIndex) != 0 {
				t.Fatalf("pins after malformed event = %v, want empty", stabilizer.pinnedByOutputIndex)
			}
			if _, accidentallyPinnedZero := stabilizer.pinnedByOutputIndex[0]; accidentallyPinnedZero {
				t.Fatal("null token established an accidental output_index 0 pin")
			}

			firstValid := []byte(`{"output_index":7,"item_id":"fresh-id"}`)
			if got := stabilizer.rewrite(firstValid); !bytes.Equal(got, firstValid) {
				t.Fatalf("first valid event after malformed input = %s, want verbatim new pin", got)
			}
			churned := []byte(`{"output_index":7,"item_id":"churned-id"}`)
			if gotID := rawString(t, rawObject(t, stabilizer.rewrite(churned))["item_id"]); gotID != "fresh-id" {
				t.Errorf("id after valid pin = %q, want fresh-id", gotID)
			}
		})
	}
}

func TestResponsesItemIDStabilizerMalformedPerItemDoesNotPartiallyRewriteExistingPin(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"pinned-id"}`))
	malformed := []byte(`{"output_index":0,"item":{"id":"would-be-rewritten","content":[ 1, 2 ]},"item_id":17}`)
	if got := stabilizer.rewrite(malformed); !bytes.Equal(got, malformed) {
		t.Fatalf("malformed event = %s, want no partial item.id rewrite in %s", got, malformed)
	}
	valid := []byte(`{"output_index":0,"item_id":"later-churn"}`)
	if gotID := rawString(t, rawObject(t, stabilizer.rewrite(valid))["item_id"]); gotID != "pinned-id" {
		t.Errorf("pin after malformed sibling = %q, want pinned-id", gotID)
	}
}

func TestResponsesItemIDStabilizerMalformedEnvelopeIsTransactionalAcrossSiblings(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "non-object response", payload: []byte(`{"response":"not-an-object"}`)},
		{name: "non-array output", payload: []byte(`{"response":{"output":"not-an-array"}}`)},
		{name: "non-object later output", payload: []byte(`{"response":{"output":[{"id":"would-be-rewritten","content":[ 1, 2 ]},"not-an-object"],"sibling": { "exact" : true }},"top": [ 2 ]}`)},
		{name: "invalid later output id", payload: []byte(`{"response":{"output":[{"id":"would-be-rewritten","content":[ 1, 2 ]},{"id":17}],"sibling": { "exact" : true }},"top": [ 2 ]}`)},
		{name: "null output item id", payload: []byte(`{"response":{"output":[{"id":null,"content":[ 1, 2 ]}]}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stabilizer := newResponsesItemIDStabilizer()
			stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"pinned-id"}`))
			if got := stabilizer.rewrite(tt.payload); !bytes.Equal(got, tt.payload) {
				t.Fatalf("malformed envelope = %s, want exact pass-through %s", got, tt.payload)
			}

			valid := []byte(`{"response":{"output":[{"id":"later-churn"}]}}`)
			gotOutput := responseOutput(t, stabilizer.rewrite(valid))
			if gotID := rawString(t, rawObject(t, gotOutput[0])["id"]); gotID != "pinned-id" {
				t.Errorf("pin after malformed envelope = %q, want pinned-id", gotID)
			}
		})
	}
}

func TestResponsesItemIDStabilizerNullTypeDoesNotResetExistingPin(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"old-id"}`))
	nullType := []byte(`{"type":null,"output_index":0,"item_id":"null-type-churn"}`)
	if got := stabilizer.rewrite(nullType); !bytes.Equal(got, nullType) {
		t.Fatalf("null type event = %s, want exact pass-through %s", got, nullType)
	}
	churned := []byte(`{"output_index":0,"item_id":"later-churn"}`)
	if gotID := rawString(t, rawObject(t, stabilizer.rewrite(churned))["item_id"]); gotID != "old-id" {
		t.Errorf("pin after null type = %q, want old-id because no valid terminal string was known", gotID)
	}
}

func TestResponsesItemIDStabilizerValidTerminalWithNullSurfaceStillClearsPins(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"old-id"}`))
	terminal := []byte(`{"type":"response.failed","output_index":0,"item_id":null}`)
	if got := stabilizer.rewrite(terminal); !bytes.Equal(got, terminal) {
		t.Fatalf("terminal with null id = %s, want exact pass-through %s", got, terminal)
	}
	newTurn := []byte(`{"output_index":0,"item_id":"new-id"}`)
	if got := stabilizer.rewrite(newTurn); !bytes.Equal(got, newTurn) {
		t.Fatalf("first event after valid terminal = %s, want reset and verbatim new pin", got)
	}
}

func TestResponsesItemIDStabilizerAcceptsGenuineEmptyStringID(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	first := []byte(`{"output_index":2,"item_id":""}`)
	if got := stabilizer.rewrite(first); !bytes.Equal(got, first) {
		t.Fatalf("genuine empty string id = %s, want verbatim first pin", got)
	}
	churned := []byte(`{"output_index":2,"item_id":"churned"}`)
	if gotID := rawString(t, rawObject(t, stabilizer.rewrite(churned))["item_id"]); gotID != "" {
		t.Errorf("rewritten id = %q, want genuine pinned empty string", gotID)
	}
}

func TestResponsesItemIDStabilizerMalformedTerminalStillClearsPins(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"old-id"}`))
	terminal := []byte(`{"type":"response.failed","output_index":0,"item":{"id":17},"item_id":"must-not-rewrite"}`)
	if got := stabilizer.rewrite(terminal); !bytes.Equal(got, terminal) {
		t.Fatalf("malformed terminal = %s, want exact pass-through %s", got, terminal)
	}
	newTurn := []byte(`{"output_index":0,"item_id":"new-id"}`)
	if got := stabilizer.rewrite(newTurn); !bytes.Equal(got, newTurn) {
		t.Fatalf("first event after malformed terminal = %s, want reset and verbatim new pin", got)
	}
}

func TestResponsesItemIDStabilizerRewritesEverySurfaceAndPreservesSiblingRawMessages(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	first := []byte(`{"type":"future.item","output_index":3,"item_id":"top-first","item":{"id":"item-first","encrypted_content":"A\u003cB","content":[ 1, 2 ],"summary": [ { "x" : 1 } ],"call_id":"call\u0026id"},"summary_index": 7}`)
	if got := stabilizer.rewrite(first); !bytes.Equal(got, first) {
		t.Fatalf("first sighting = %s, want input unchanged", got)
	}

	next := []byte(`{"type":"novel.item.event","output_index":3,"item_id":"top-next","item":{"id":"item-next","encrypted_content":"A\u003cB","content":[ 1, 2 ],"summary": [ { "x" : 1 } ],"call_id":"call\u0026id"},"summary_index": 7}`)
	got := stabilizer.rewrite(next)
	gotTop := rawObject(t, got)
	gotItem := rawObject(t, gotTop["item"])
	if rawString(t, gotTop["item_id"]) != "item-first" || rawString(t, gotItem["id"]) != "item-first" {
		t.Fatalf("rewritten surfaces = item_id %s, item.id %s; want item-first", gotTop["item_id"], gotItem["id"])
	}

	wantTop := rawObject(t, next)
	wantItem := rawObject(t, wantTop["item"])
	for _, key := range []string{"encrypted_content", "content", "summary", "call_id"} {
		if !bytes.Equal(gotItem[key], wantItem[key]) {
			t.Errorf("item.%s raw bytes = %s, want exact %s", key, gotItem[key], wantItem[key])
		}
	}
	if !bytes.Equal(gotTop["summary_index"], wantTop["summary_index"]) {
		t.Errorf("summary_index raw bytes = %s, want exact %s", gotTop["summary_index"], wantTop["summary_index"])
	}
}

func TestResponsesItemIDStabilizerEnvelopeAppliesPinsWithoutCreatingThem(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	created := []byte(`{"type":"response.created","response":{"output":[{"id":"envelope-first","content":[ 1, 2 ]}]}}`)
	if got := stabilizer.rewrite(created); !bytes.Equal(got, created) {
		t.Fatalf("pre-item envelope = %s, want unchanged and unpinned", got)
	}

	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"added-id"}}`)
	if got := stabilizer.rewrite(added); !bytes.Equal(got, added) {
		t.Fatalf("added = %s, want unchanged first per-item pin", got)
	}
	novelEnvelope := []byte(`{"type":"vendor.future.snapshot","response":{"output":[{"id":"novel-envelope-id","content":[ 1, 2 ]}]}}`)
	if gotID := rawString(t, rawObject(t, responseOutput(t, stabilizer.rewrite(novelEnvelope))[0])["id"]); gotID != "added-id" {
		t.Fatalf("structurally id-bearing novel envelope id = %q, want added-id", gotID)
	}

	completed := []byte(`{"type":"response.completed","response":{"output":[{"id":"terminal-id","content":[ 1, 2 ]},{"id":"unseen-id"}]}}`)
	got := stabilizer.rewrite(completed)
	output := responseOutput(t, got)
	if rawString(t, rawObject(t, output[0])["id"]) != "added-id" {
		t.Errorf("terminal output[0].id = %s, want added-id", rawObject(t, output[0])["id"])
	}
	if rawString(t, rawObject(t, output[1])["id"]) != "unseen-id" {
		t.Errorf("unseen terminal output[1].id changed: %s", rawObject(t, output[1])["id"])
	}
	if content := rawObject(t, output[0])["content"]; !bytes.Equal(content, []byte(`[ 1, 2 ]`)) {
		t.Errorf("terminal content raw bytes = %s, want exact spacing", content)
	}

	nextTurn := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"next-turn-id"}}`)
	if got := stabilizer.rewrite(nextTurn); !bytes.Equal(got, nextTurn) {
		t.Fatalf("first item after terminal = %s, want reset and verbatim new pin", got)
	}
}

func TestResponsesItemIDStabilizerNeverMintsAnID(t *testing.T) {
	stabilizer := newResponsesItemIDStabilizer()
	withoutID := []byte(`{"type":"vendor.future.item","output_index":4,"content":"no id surface"}`)
	if got := stabilizer.rewrite(withoutID); !bytes.Equal(got, withoutID) {
		t.Fatalf("id-less per-item event = %s, want unchanged", got)
	}
	if len(stabilizer.pinnedByOutputIndex) != 0 {
		t.Fatalf("pins after id-less event = %v, want empty", stabilizer.pinnedByOutputIndex)
	}

	envelope := []byte(`{"type":"vendor.future.snapshot","response":{"output":[{"id":"only-upstream-id"}]}}`)
	if got := stabilizer.rewrite(envelope); !bytes.Equal(got, envelope) {
		t.Fatalf("envelope without a genuine per-item pin = %s, want unchanged", got)
	}
	if len(stabilizer.pinnedByOutputIndex) != 0 {
		t.Fatalf("pins after envelope = %v, want empty", stabilizer.pinnedByOutputIndex)
	}
}

func TestResponsesItemIDStabilizerClearsPinsForEveryTurnTerminal(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete", "error"} {
		t.Run(terminal, func(t *testing.T) {
			stabilizer := newResponsesItemIDStabilizer()
			stabilizer.rewrite([]byte(`{"output_index":0,"item_id":"old-id"}`))
			terminalPayload, err := json.Marshal(map[string]string{"type": terminal})
			if err != nil {
				t.Fatal(err)
			}
			if got := stabilizer.rewrite(terminalPayload); !bytes.Equal(got, terminalPayload) {
				t.Fatalf("terminal without rewrite surface = %s, want unchanged", got)
			}
			newTurn := []byte(`{"output_index":0,"item_id":"new-id"}`)
			if got := stabilizer.rewrite(newTurn); !bytes.Equal(got, newTurn) {
				t.Fatalf("new turn first id = %s, want unchanged after reset", got)
			}
		})
	}
}

func TestResponsesItemIDStabilizerPassesThroughUncertainPayloads(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "undecodable", payload: []byte(`{"type":`)},
		{name: "no structural key", payload: []byte(` { "type" : "unknown", "content" : [ 1 ] } `)},
		{name: "invalid output index", payload: []byte(`{"output_index":"zero","item_id":"id"}`)},
		{name: "index without id", payload: []byte(`{"output_index":0,"content":"kept"}`)},
		{name: "undecodable envelope output", payload: []byte(`{"response":{"output":"not-an-array"}}`)},
		{name: "empty envelope", payload: []byte(`{"response":{"output":[]}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stabilizer := newResponsesItemIDStabilizer()
			if got := stabilizer.rewrite(tt.payload); !bytes.Equal(got, tt.payload) {
				t.Errorf("rewrite() = %s, want exact pass-through %s", got, tt.payload)
			}
		})
	}
}

func capturedResponsesFrames(t *testing.T) []sse.Frame {
	t.Helper()
	file, err := os.Open("testdata/responses_item_id_churn.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	reader := sse.NewReader(file, nil)
	var frames []sse.Frame
	for {
		frame, err := reader.Read()
		if err == io.EOF {
			return frames
		}
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
}

func frameData(t *testing.T, raw []byte) []byte {
	t.Helper()
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			return data
		}
	}
	t.Fatalf("frame has no data line: %s", raw)
	return nil
}

func payloadIndexAndID(t *testing.T, payload []byte) (int, string) {
	t.Helper()
	top := rawObject(t, payload)
	var index int
	if err := json.Unmarshal(top["output_index"], &index); err != nil {
		t.Fatal(err)
	}
	if item, ok := top["item"]; ok {
		return index, rawString(t, rawObject(t, item)["id"])
	}
	return index, rawString(t, top["item_id"])
}

func responseOutput(t *testing.T, payload []byte) []json.RawMessage {
	t.Helper()
	top := rawObject(t, payload)
	response := rawObject(t, top["response"])
	var output []json.RawMessage
	if err := json.Unmarshal(response["output"], &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func rawObject(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("decode object %s: %v", payload, err)
	}
	return object
}

func rawString(t *testing.T, payload []byte) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode string %s: %v", payload, err)
	}
	return value
}
