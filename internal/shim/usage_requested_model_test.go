package shim

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
)

func TestUsageMeterObservesRequestedModelWithoutChangingRequestOrReportedModel(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	// Synthetic request and response literals isolate Requested-model intent
	// from the response-authoritative tier without altering recorded fixtures.
	request := []byte(`{"model":"gpt-5.6-sol-fast","service_tier":"priority","input":"private prompt"}`)
	original := bytes.Clone(request)
	header := http.Header{"X-Test": {"unchanged"}}
	gotHeader, gotRequest, err := chain.RunRequest(ctx, "keep=query", header, request)
	if err != nil || !reflect.DeepEqual(gotHeader, header) || !bytes.Equal(gotRequest, original) || &gotRequest[0] != &request[0] {
		t.Fatalf("request changed: header=%v body=%q err=%v", gotHeader, gotRequest, err)
	}
	// The submitted metadata must not retain the mutable request bytes.
	clear(request)
	response := []byte(`{"id":"response","model":"gpt-5.6-sol","status":"completed","service_tier":"default","usage":{"input_tokens":12,"output_tokens":6}}`)
	gotResponse, err := chain.RunBuffered(ctx, response)
	if err != nil || !bytes.Equal(gotResponse, response) || &gotResponse[0] != &response[0] {
		t.Fatalf("response changed: %q, %v", gotResponse, err)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestedModel == nil || *turns[0].RequestedModel != "gpt-5.6-sol-fast" ||
		turns[0].Model != "gpt-5.6-sol" || turns[0].OpenAIServiceTier == nil || *turns[0].OpenAIServiceTier != "default" {
		t.Fatalf("Turns = %+v, want distinct request intent and authoritative response evidence", turns)
	}
}

func TestUsageMeterNeverInfersOpenAIServiceTierFromRequestIntent(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	chain, response := enabledBufferedUsageFixture(ctx, endpoint.OpenAI, sink)
	// This synthetic request asks for priority/Fast while the synthetic
	// completion deliberately omits service_tier.
	request := []byte(`{"model":"gpt-5.6-sol-fast","service_tier":"priority"}`)
	if _, got, err := chain.RunRequest(ctx, "", nil, request); err != nil || !bytes.Equal(got, request) {
		t.Fatalf("request changed or failed: got=%q err=%v", got, err)
	}
	if got, err := chain.RunBuffered(ctx, response); err != nil || !bytes.Equal(got, response) {
		t.Fatalf("response changed or failed: got=%q err=%v", got, err)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestedModel == nil || *turns[0].RequestedModel != "gpt-5.6-sol-fast" ||
		turns[0].OpenAIServiceTier != nil {
		t.Fatalf("Turns = %+v, want Requested model but unavailable service-tier evidence", turns)
	}
}

func TestUsageMeterOpenAISSEUsesResponseTierWithoutRequestFallback(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	// Synthetic SSE literals exercise a priority/Fast request followed by one
	// downgraded default response and one response with no tier evidence.
	request := []byte(`{"model":"gpt-5.6-sol-fast","service_tier":"priority"}`)
	requestWant := bytes.Clone(request)
	if _, got, err := chain.RunRequest(ctx, "", nil, request); err != nil || !bytes.Equal(got, requestWant) || &got[0] != &request[0] {
		t.Fatalf("request changed or failed: got=%q err=%v", got, err)
	}
	clear(request)
	adapter := chain.StreamAdapter(ctx, nil)
	responses := [][]byte{
		syntheticOpenAIResponse([]byte(`"service_tier":"default",`)),
		syntheticOpenAIResponse(nil),
	}
	for _, response := range responses {
		payload := syntheticOpenAICompletedEvent(response)
		frame := sse.Frame{Type: "response.completed", Raw: append(append([]byte("event: response.completed\ndata: "), payload...), '\n', '\n')}
		want := sse.Frame{Type: frame.Type, Raw: bytes.Clone(frame.Raw)}
		if got := adapter.Transform(ctx, frame); !reflect.DeepEqual(got, []sse.Frame{want}) {
			t.Fatalf("SSE frame changed: got=%#v want=%#v", got, want)
		}
		clear(frame.Raw)
	}
	turns := sink.snapshot()
	wantTiers := []*string{stringPointer("default"), nil}
	if len(turns) != len(wantTiers) {
		t.Fatalf("Turns = %+v, want two", turns)
	}
	for index, turn := range turns {
		if turn.RequestedModel == nil || *turn.RequestedModel != "gpt-5.6-sol-fast" || turn.Model != "reported" ||
			turn.Transport != usage.TransportSSE || turn.TurnIndex != index || !reflect.DeepEqual(turn.OpenAIServiceTier, wantTiers[index]) {
			t.Errorf("Turn %d = %+v, want response-authoritative SSE evidence", index, turn)
		}
	}
}

func TestUsageMeterRequestedModelNullableSemantics(t *testing.T) {
	for _, surface := range []endpoint.Surface{endpoint.OpenAI, endpoint.Anthropic} {
		t.Run(surface.String(), func(t *testing.T) {
			for _, tc := range []struct {
				name, body string
				want       *string
			}{
				{name: "empty", body: `{"model":""}`, want: stringPointer("")},
				{name: "verbatim decoded contents", body: `{"model":"  GPT.\\/Alias\t\u96ea\n  "}`, want: stringPointer("  GPT.\\/Alias\t雪\n  ")},
				{name: "escaped key", body: `{"\u006dodel":"exact"}`, want: stringPointer("exact")},
				{name: "nested and cased do not conflict", body: `{"model":"exact","Model":"wrong","input":{"model":"nested"},"large":1e999}`, want: stringPointer("exact")},
				{name: "missing", body: `{}`},
				{name: "null", body: `{"model":null}`},
				{name: "number", body: `{"model":42}`},
				{name: "boolean", body: `{"model":true}`},
				{name: "array value", body: `{"model":["x"]}`},
				{name: "object value", body: `{"model":{"model":"x"}}`},
				{name: "malformed", body: `{"model":"x",`},
				{name: "invalid UTF-8 model", body: "{\"model\":\"gpt-\xff\"}"},
				{name: "invalid UTF-8 unrelated field", body: "{\"model\":\"exact\",\"input\":\"\xff\"}"},
				{name: "trailing garbage", body: `{"model":"x"} invalid`},
				{name: "trailing object", body: `{"model":"x"}{}`},
				{name: "non object", body: `[ {"model":"x"} ]`},
				{name: "top level string", body: `"model"`},
				{name: "top level null", body: `null`},
				{name: "no body", body: ``},
				{name: "nested only", body: `{"input":{"model":"nested"}}`},
				{name: "cased only", body: `{"Model":"wrong","MODEL":"wrong"}`},
				{name: "duplicate", body: `{"model":"a","model":"b"}`},
				{name: "identical duplicate", body: `{"model":"a","model":"a"}`},
				{name: "escaped duplicate", body: `{"model":null,"\u006dodel":"b"}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					sink := &memoryUsageSink{}
					chain, response := enabledBufferedUsageFixture(ctx, surface, sink)
					body := []byte(tc.body)
					_, got, err := chain.RunRequest(ctx, "", nil, body)
					if err != nil || string(got) != tc.body {
						t.Fatalf("request changed or rejected: %q, %v", got, err)
					}
					clear(body)
					if _, err := chain.RunBuffered(ctx, response); err != nil {
						t.Fatal(err)
					}
					turns := sink.snapshot()
					if len(turns) != 1 {
						t.Fatalf("Turns = %+v, want qualifying completion even without attribution", turns)
					}
					if !reflect.DeepEqual(turns[0].RequestedModel, tc.want) || turns[0].Model != "reported" {
						t.Errorf("Turn = %+v, requested model = %v, want %v", turns[0], turns[0].RequestedModel, tc.want)
					}
				})
			}
		})
	}
}

func TestUsageMeterWebSocketIgnoresSyntheticRequestMetadataAndHasNoClientHook(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	// Explicitly invoke the HTTP request hook to populate metadata. Production
	// WebSocket forwarding does not run this hook; the synthetic setup tests
	// defensive nil attribution, not actual handshake wiring.
	if _, _, err := chain.RunRequest(ctx, "", nil, []byte(`{"model":"not-websocket-attribution"}`)); err != nil {
		t.Fatal(err)
	}
	if chain.WSClientAdapter(ctx, nil) != nil {
		t.Fatal("meter installed a WebSocket client-message hook")
	}
	server := chain.WSServerAdapter(ctx, nil)
	for i, kind := range []MessageKind{MessageText, MessageBinary} {
		data := []byte(`{"type":"response.completed","response":{"id":"reused","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
		message := &Message{Kind: kind, Data: data}
		if !server(ctx, message) || message.Kind != kind || !bytes.Equal(message.Data, data) || &message.Data[0] != &data[0] {
			t.Fatalf("WebSocket message changed: %+v", message)
		}
		turns := sink.snapshot()
		if len(turns) != i+1 || turns[i].RequestedModel != nil || turns[i].Model != "reported" || turns[i].TurnIndex != i {
			t.Fatalf("WebSocket Turns = %+v, want unchanged response-derived metadata with nil requested model", turns)
		}
	}
}

func TestUsageMeterRequestedModelNeverBackfillsCompletionRequirements(t *testing.T) {
	ctx := context.Background()
	for _, surface := range []endpoint.Surface{endpoint.OpenAI, endpoint.Anthropic} {
		t.Run(surface.String(), func(t *testing.T) {
			sink := &memoryUsageSink{}
			chain, response := enabledBufferedUsageFixture(ctx, surface, sink)
			if _, _, err := chain.RunRequest(ctx, "", nil, []byte(`{"model":"requested"}`)); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range [][]byte{
				bytes.ReplaceAll(response, []byte(`"model":"reported",`), nil),
				bytes.ReplaceAll(response, []byte(`"model":"reported"`), []byte(`"model":""`)),
				bytes.ReplaceAll(response, []byte(`"input_tokens":1`), []byte(`"input_tokens":-1`)),
			} {
				if _, err := chain.RunBuffered(ctx, invalid); err != nil {
					t.Fatal(err)
				}
			}
			if turns := sink.snapshot(); len(turns) != 0 {
				t.Fatalf("requested model made invalid completions eligible: %+v", turns)
			}
		})
	}
}

func enabledBufferedUsageFixture(ctx context.Context, surface endpoint.Surface, sink *memoryUsageSink) (*Chain, []byte) {
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	route := endpoint.RouteOpenAIResponses
	response := `{"id":"r","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`
	if surface == endpoint.Anthropic {
		route = endpoint.RouteAnthropicMessages
		response = `{"id":"m","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	}
	return registry.NewChain(ctx, surface, route), []byte(response)
}

func stringPointer(value string) *string { return &value }
