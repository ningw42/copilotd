package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func capturedModels(t *testing.T) []Model {
	t.Helper()
	body, err := os.ReadFile("testdata/copilot-models-2026-07-18.json")
	if err != nil {
		t.Fatalf("read captured models: %v", err)
	}
	models, err := Decode(body)
	if err != nil {
		t.Fatalf("decode captured models: %v", err)
	}
	return models
}

func modelIDs(models []Model) []string {
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

func TestDecodeRejectsStructurallyMalformedCatalogs(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{}`,
		`{"data":null}`,
		`{"data":{}}`,
		`{"data":[null]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := Decode([]byte(body)); err == nil {
				t.Errorf("Decode(%s) succeeded, want malformed Catalog error", body)
			}
		})
	}
	if models, err := Decode([]byte(`{"data":[]}`)); err != nil || len(models) != 0 {
		t.Errorf("empty data array = (%v, %v), want valid empty Catalog", models, err)
	}
}

func TestFilterSelectsModelsForwardableOnEachSurfaceInCaptureOrder(t *testing.T) {
	models := capturedModels(t)
	// The current capture has no picker-hidden model that still advertises a
	// forwardable Route and no websocket-only model, so keep those two defensive
	// cases explicit without presenting them as captured data.
	models = append(models,
		Model{ID: "future-hidden-response", SupportedRoutes: []endpoint.Route{endpoint.RouteOpenAIResponses}},
		Model{ID: "websocket-only", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{"ws:/responses"}},
	)

	wantAnthropic := []string{
		"claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8",
		"claude-sonnet-4.6", "claude-sonnet-5", "claude-sonnet-4.5", "claude-haiku-4.5",
	}
	if got := modelIDs(Filter(models, endpoint.RouteAnthropicMessages)); !reflect.DeepEqual(got, wantAnthropic) {
		t.Errorf("Anthropic catalog IDs = %q, want %q", got, wantAnthropic)
	}

	wantOpenAI := []string{
		"gpt-5.3-codex", "gpt-5.4-mini", "gpt-5.4", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
		"mai-code-1-flash-picker", "gpt-5-mini",
	}
	if got := modelIDs(Filter(models, endpoint.RouteOpenAIResponses)); !reflect.DeepEqual(got, wantOpenAI) {
		t.Errorf("OpenAI catalog IDs = %q, want %q", got, wantOpenAI)
	}
}

func TestRenderOpenAIUsesProviderSchemaAndCopilotValues(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	body, err := RenderOpenAI(models)
	if err != nil {
		t.Fatalf("render OpenAI catalog: %v", err)
	}

	var got struct {
		Object string                   `json:"object"`
		Data   []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode rendered OpenAI catalog: %v", err)
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	if len(got.Data) != len(models) {
		t.Fatalf("data length = %d, want %d", len(got.Data), len(models))
	}
	wantOwners := []string{
		"OpenAI", "OpenAI", "OpenAI", "OpenAI", "OpenAI", "OpenAI", "OpenAI",
		"Microsoft", "Azure OpenAI",
	}
	for i, object := range got.Data {
		want := map[string]interface{}{
			"id":       models[i].ID,
			"object":   "model",
			"created":  float64(0),
			"owned_by": wantOwners[i],
		}
		if !reflect.DeepEqual(object, want) {
			t.Errorf("data[%d] = %#v, want exactly %#v", i, object, want)
		}
	}
}

func TestRenderOpenAIEmptyCatalogUsesAnEmptyList(t *testing.T) {
	body, err := RenderOpenAI(nil)
	if err != nil {
		t.Fatalf("render empty OpenAI catalog: %v", err)
	}
	if got, want := string(body), `{"object":"list","data":[]}`; got != want {
		t.Errorf("empty catalog = %s, want %s", got, want)
	}
}

func TestRenderersRejectModelsWithoutRequiredIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		render func([]Model) ([]byte, error)
		model  Model
	}{
		{name: "OpenAI missing ID", render: RenderOpenAI, model: Model{Vendor: "OpenAI"}},
		{name: "OpenAI missing vendor", render: RenderOpenAI, model: Model{ID: "gpt"}},
		{name: "Anthropic missing ID", render: RenderAnthropic, model: Model{Name: "Claude"}},
		{name: "Anthropic missing display name", render: RenderAnthropic, model: Model{ID: "claude"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.render([]Model{tc.model}); err == nil {
				t.Error("renderer fabricated an empty identity field, want error")
			}
		})
	}
}

func TestRenderAnthropicEnrichesFromCopilotSignals(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteAnthropicMessages)
	body, err := RenderAnthropic(models)
	if err != nil {
		t.Fatalf("render Anthropic catalog: %v", err)
	}

	var envelope struct {
		Data    []map[string]interface{} `json:"data"`
		HasMore bool                     `json:"has_more"`
		FirstID *string                  `json:"first_id"`
		LastID  *string                  `json:"last_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode rendered Anthropic catalog: %v", err)
	}
	if envelope.HasMore || envelope.FirstID == nil || *envelope.FirstID != "claude-opus-4.6" || envelope.LastID == nil || *envelope.LastID != "claude-haiku-4.5" {
		t.Errorf("pagination = has_more:%v first_id:%v last_id:%v", envelope.HasMore, envelope.FirstID, envelope.LastID)
	}

	objects := make(map[string]map[string]interface{}, len(envelope.Data))
	for _, object := range envelope.Data {
		if object["type"] != "model" || object["created_at"] != "1970-01-01T00:00:00Z" {
			t.Errorf("model identity constants = type:%v created_at:%v, want model/epoch", object["type"], object["created_at"])
		}
		objects[object["id"].(string)] = object
	}
	assertJSONObject(t, objects["claude-opus-4.6"], `{
		"id":"claude-opus-4.6","type":"model","display_name":"Claude Opus 4.6",
		"created_at":"1970-01-01T00:00:00Z","max_input_tokens":936000,"max_tokens":64000,
		"capabilities":{
			"structured_outputs":{"supported":true},"image_input":{"supported":true},"pdf_input":{"supported":true},
			"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":false},"max":{"supported":true}},
			"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":true}}}
		}
	}`)
	assertJSONObject(t, objects["claude-opus-4.8"], `{
		"id":"claude-opus-4.8","type":"model","display_name":"Claude Opus 4.8",
		"created_at":"1970-01-01T00:00:00Z","max_input_tokens":936000,"max_tokens":64000,
		"capabilities":{
			"structured_outputs":{"supported":true},"image_input":{"supported":true},"pdf_input":{"supported":true},
			"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}},
			"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":true}}}
		}
	}`)
	for _, id := range []string{"claude-sonnet-4.5", "claude-haiku-4.5"} {
		capabilities := objects[id]["capabilities"].(map[string]interface{})
		if _, ok := capabilities["effort"]; ok {
			t.Errorf("%s unexpectedly advertised effort: %#v", id, capabilities["effort"])
		}
		wantThinking := map[string]interface{}{
			"supported": true,
			"types": map[string]interface{}{
				"adaptive": map[string]interface{}{"supported": false},
				"enabled":  map[string]interface{}{"supported": true},
			},
		}
		if got := capabilities["thinking"]; !reflect.DeepEqual(got, wantThinking) {
			t.Errorf("%s thinking = %#v, want %#v", id, got, wantThinking)
		}
	}
}

func assertJSONObject(t *testing.T, got map[string]interface{}, wantJSON string) {
	t.Helper()
	var want map[string]interface{}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("object = %#v\nwant %#v", got, want)
	}
}
