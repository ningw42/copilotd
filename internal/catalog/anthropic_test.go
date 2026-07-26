package catalog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestRenderAnthropicEmptyCatalogHasNullBoundaryIDs(t *testing.T) {
	body, err := RenderAnthropic(nil)
	if err != nil {
		t.Fatalf("RenderAnthropic() error = %v", err)
	}
	var got struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
		FirstID *string           `json:"first_id"`
		LastID  *string           `json:"last_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode empty Anthropic catalog: %v", err)
	}
	if len(got.Data) != 0 || got.HasMore || got.FirstID != nil || got.LastID != nil {
		t.Errorf("empty envelope = %+v, want empty data, false, null, null", got)
	}
}

func TestRenderAnthropicNormalizesOnlyAnthropicClaudeModelIDs(t *testing.T) {
	body, err := RenderAnthropicWithConfig([]Model{
		{ID: "claude-opus-4.8", Name: "Claude Opus 4.8", Vendor: "Anthropic"},
		{ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro Preview", Vendor: "Google"},
		{ID: "future.model", Name: "Future Model", Vendor: "Anthropic"},
		{ID: "claude-future-4.9", Name: "Claude Future 4.9", Vendor: "Anthropic"},
		{ID: "claude-sonnet-4.5", Name: "Claude Sonnet 4.5", Vendor: "Anthropic"},
	}, AnthropicRenderConfig{ModelIDNormalizationEnabled: true})
	if err != nil {
		t.Fatalf("RenderAnthropicWithConfig() error = %v", err)
	}
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode Anthropic catalog: %v", err)
	}
	want := []string{
		"claude-opus-4-8", "gemini-3.1-pro-preview", "future.model",
		"claude-future-4-9", "claude-sonnet-4-5",
	}
	if len(got.Data) != len(want) {
		t.Fatalf("data length = %d, want %d", len(got.Data), len(want))
	}
	for i, model := range got.Data {
		if model.ID != want[i] {
			t.Errorf("data[%d].id = %q, want %q", i, model.ID, want[i])
		}
	}
}

func TestRenderAnthropicRejectsNormalizedModelIDCollisions(t *testing.T) {
	_, err := RenderAnthropicWithConfig([]Model{
		{ID: "claude-opus-4.8", Name: "Claude Opus 4.8", Vendor: "Anthropic"},
		{ID: "claude-opus-4-8", Name: "Claude Opus 4.8 Alias", Vendor: "Anthropic"},
	}, AnthropicRenderConfig{ModelIDNormalizationEnabled: true})
	if err == nil {
		t.Fatal("RenderAnthropicWithConfig() error = nil, want normalized ID collision rejected")
	}
	for _, want := range []string{"claude-opus-4.8", "claude-opus-4-8", "collide"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q context", err, want)
		}
	}
}

func TestRenderAnthropicMapsOnlyEvidenceBackedOptionalCapabilities(t *testing.T) {
	models, err := Decode([]byte(`{
		"unknown_top_level":"ignored",
		"data":[{
			"id":"evidence-model","name":"Evidence Model","vendor":"Anthropic",
			"model_picker_enabled":true,"supported_endpoints":["/v1/messages"],
			"warning_message":"must not leak","policy":{"terms":"must not leak"},
			"capabilities":{
				"family":"must-not-leak","tokenizer":"must-not-leak",
				"limits":{"vision":{"supported_media_types":["image/png"]}},
				"supports":{
					"structured_outputs":false,"vision":false,
					"reasoning_effort":["none","minimal","low"],
					"adaptive_thinking":true
				}
			}
		}]
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	body, err := RenderAnthropic(Filter(models, endpoint.RouteAnthropicMessages))
	if err != nil {
		t.Fatalf("RenderAnthropic() error = %v", err)
	}

	var got struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode Anthropic result: %v", err)
	}
	wantJSON := `{
		"id":"evidence-model","type":"model","display_name":"Evidence Model",
		"created_at":"1970-01-01T00:00:00Z",
		"capabilities":{
			"structured_outputs":{"supported":false},
			"image_input":{"supported":false},
			"pdf_input":{"supported":false},
			"effort":{
				"supported":true,"low":{"supported":true},"medium":{"supported":false},
				"high":{"supported":false},"xhigh":{"supported":false},"max":{"supported":false}
			},
			"thinking":{
				"supported":true,
				"types":{"adaptive":{"supported":true},"enabled":{"supported":false}}
			}
		}
	}`
	var want map[string]any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("decode expected Anthropic object: %v", err)
	}
	if len(got.Data) != 1 || !reflect.DeepEqual(got.Data[0], want) {
		t.Errorf("model = %#v\nwant %#v", got.Data, want)
	}
	for _, forbidden := range []string{
		"warning_message", "policy", "family", "tokenizer", "none", "minimal",
		"batch", "citations", "code_execution", "context_management",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("provider-shaped output leaked unsupported field/value %q: %s", forbidden, body)
		}
	}
}

func TestRenderAnthropicOmitsAbsentSignals(t *testing.T) {
	body, err := RenderAnthropic([]Model{{ID: "defensive", Name: "Defensive"}})
	if err != nil {
		t.Fatalf("RenderAnthropic() error = %v", err)
	}
	var got struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode Anthropic result: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(got.Data))
	}
	for _, absent := range []string{"max_input_tokens", "max_tokens"} {
		if _, ok := got.Data[0][absent]; ok {
			t.Errorf("absent source fabricated %q: %s", absent, body)
		}
	}
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(got.Data[0]["capabilities"], &capabilities); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	for _, absent := range []string{"structured_outputs", "image_input", "pdf_input", "effort", "thinking", "batch", "citations", "code_execution", "context_management"} {
		if _, ok := capabilities[absent]; ok {
			t.Errorf("absent source fabricated capability %q: %s", absent, body)
		}
	}
}
