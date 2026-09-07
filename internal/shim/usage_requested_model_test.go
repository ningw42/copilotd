package shim

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestUsageMeterObservesRequestedModelWithoutChangingRequestOrReportedModel(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	request := []byte(`{"model":"gpt-5.6-sol-fast","input":"private prompt"}`)
	original := bytes.Clone(request)
	header := http.Header{"X-Test": {"unchanged"}}
	gotHeader, gotRequest, err := chain.RunRequest(ctx, "keep=query", header, request)
	if err != nil || !reflect.DeepEqual(gotHeader, header) || !bytes.Equal(gotRequest, original) || &gotRequest[0] != &request[0] {
		t.Fatalf("request changed: header=%v body=%q err=%v", gotHeader, gotRequest, err)
	}
	// The submitted metadata must not retain the mutable request bytes.
	clear(request)
	response := []byte(`{"id":"response","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":12,"output_tokens":6}}`)
	gotResponse, err := chain.RunBuffered(ctx, response)
	if err != nil || !bytes.Equal(gotResponse, response) || &gotResponse[0] != &response[0] {
		t.Fatalf("response changed: %q, %v", gotResponse, err)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestedModel == nil || *turns[0].RequestedModel != "gpt-5.6-sol-fast" || turns[0].Model != "gpt-5.6-sol" {
		t.Fatalf("Turns = %+v, want distinct requested and reported model", turns)
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
					registry := CanonicalRegistry(sink)
					registry[len(registry)-1].Enabled = true
					route := endpoint.RouteOpenAIResponses
					response := `{"id":"r","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`
					if surface == endpoint.Anthropic {
						route = endpoint.RouteAnthropicMessages
						response = `{"id":"m","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
					}
					chain := registry.NewChain(ctx, surface, route)
					body := []byte(tc.body)
					_, got, err := chain.RunRequest(ctx, "", nil, body)
					if err != nil || string(got) != tc.body {
						t.Fatalf("request changed or rejected: %q, %v", got, err)
					}
					clear(body)
					if _, err := chain.RunBuffered(ctx, []byte(response)); err != nil {
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
			registry := CanonicalRegistry(sink)
			registry[len(registry)-1].Enabled = true
			route := endpoint.RouteOpenAIResponses
			response := `{"id":"r","model":"reported","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}`
			if surface == endpoint.Anthropic {
				route = endpoint.RouteAnthropicMessages
				response = `{"id":"m","type":"message","model":"reported","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
			}
			chain := registry.NewChain(ctx, surface, route)
			if _, _, err := chain.RunRequest(ctx, "", nil, []byte(`{"model":"requested"}`)); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range [][]byte{
				bytes.ReplaceAll([]byte(response), []byte(`"model":"reported",`), nil),
				bytes.ReplaceAll([]byte(response), []byte(`"model":"reported"`), []byte(`"model":""`)),
				bytes.ReplaceAll([]byte(response), []byte(`"input_tokens":1`), []byte(`"input_tokens":-1`)),
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

func stringPointer(value string) *string { return &value }
