package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ningw42/copilotd/internal/config"
)

func TestRunBoundServeUsageMeterExcludesCountTokensAndCatalogs(t *testing.T) {
	// Valid catalog data plus fields that would qualify as either native
	// completion if the upstream body were mistakenly sent through a meter.
	const models = `{"id":"not-an-inference","type":"message","model":"not-an-inference-model","status":"completed","stop_reason":"end_turn","usage":{"input_tokens":91,"output_tokens":17},"data":[
		{"id":"gpt-5.4","name":"GPT test","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]},
		{"id":"claude-test","name":"Claude test","vendor":"Anthropic","model_picker_enabled":true,"supported_endpoints":["/v1/messages"]}
	]}`
	const countTokens = `{"input_tokens":91,"id":"not-a-message","type":"message","model":"count-model","stop_reason":"end_turn","usage":{"input_tokens":91,"output_tokens":17}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /models":
			_, _ = io.WriteString(w, models)
		case "POST /v1/messages/count_tokens":
			_, _ = io.WriteString(w, countTokens)
		case "POST /responses":
			_, _ = io.WriteString(w, bufferedUsageSurfaceCases[0].completion)
		case "POST /v1/messages":
			_, _ = io.WriteString(w, bufferedUsageSurfaceCases[1].completion)
		default:
			t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) {
		cfg.CodexCatalogEnabled = true
		cfg.CodexOverrideLimits = true // opens the existing Codex shape gate
	}, nil)

	// Positive controls keep an accidentally disabled meter from satisfying
	// every exclusion. The only rows after all requests must be these two.
	for _, surface := range bufferedUsageSurfaceCases {
		resp, body := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "included-"+surface.table, `{}`)
		if resp.StatusCode != http.StatusOK || string(body) != surface.completion {
			t.Fatalf("inference control = %d %q, want unchanged completion", resp.StatusCode, body)
		}
	}
	resp, body := doUsagePOST(t, context.Background(), harness.baseURL, "/anthropic/v1/messages/count_tokens", "excluded-count-tokens", `{}`)
	if resp.StatusCode != http.StatusOK || string(body) != countTokens {
		t.Errorf("count_tokens = %d %q, want unchanged estimate payload", resp.StatusCode, body)
	}

	for _, path := range []string{"/models", "/anthropic/v1/models", "/openai/v1/models", "/openai/v1/models?client_version=fixture"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, harness.baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("X-Request-Id", "excluded-catalog")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %q, %v; want successful catalog", path, resp.StatusCode, body, err)
		}
		if path == "/models" {
			if string(body) != models {
				t.Errorf("raw models changed: %s", body)
			}
			continue
		}
		var catalog struct {
			Object string `json:"object"`
			Data   []struct {
				ID     string `json:"id"`
				Object string `json:"object"`
				Type   string `json:"type"`
			} `json:"data"`
			Models []struct {
				Slug string `json:"slug"`
			} `json:"models"`
			HasMore *bool  `json:"has_more"`
			FirstID string `json:"first_id"`
		}
		if err := json.Unmarshal(body, &catalog); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		switch path {
		case "/anthropic/v1/models":
			if len(catalog.Data) != 1 || catalog.Data[0].ID != "claude-test" || catalog.Data[0].Type != "model" || catalog.HasMore == nil || *catalog.HasMore || catalog.FirstID != "claude-test" {
				t.Errorf("Anthropic catalog shape/membership = %s", body)
			}
		case "/openai/v1/models":
			if catalog.Object != "list" || len(catalog.Data) != 1 || catalog.Data[0].ID != "gpt-5.4" || catalog.Data[0].Object != "model" {
				t.Errorf("OpenAI catalog shape/membership = %s", body)
			}
		default:
			if len(catalog.Models) != 1 || catalog.Models[0].Slug != "gpt-5.4" || len(catalog.Data) != 0 {
				t.Errorf("Codex catalog shape/membership = %s", body)
			}
		}
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	for _, table := range []string{"openai_turn", "anthropic_turn"} {
		if got := queryUsageCount(t, db, table, ""); got != 1 {
			t.Errorf("%s rows = %d, want only the inference control and no excluded-route rows", table, got)
		}
		if got := queryUsageCount(t, db, table, "request_id = ?", "included-"+table); got != 1 {
			t.Errorf("%s inference control rows = %d, want 1", table, got)
		}
	}
}
