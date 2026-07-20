package catalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestRenderCodexIntersectsInLiveOrderAndEmitsCompleteEntries(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	body, outcome, err := RenderCodex(models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if outcome.SkippedReviewer != "" {
		t.Errorf("skipped reviewer = %q, want empty", outcome.SkippedReviewer)
	}

	entries := decodeRenderedCodex(t, body)
	wantSlugs := []string{
		"gpt-5.4-mini", "gpt-5.4", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Errorf("rendered slugs = %q, want %q", got, wantSlugs)
	}

	required := []string{
		"slug", "display_name", "supported_reasoning_levels", "shell_type",
		"visibility", "supported_in_api", "priority", "base_instructions",
		"supports_reasoning_summaries", "support_verbosity", "truncation_policy",
		"supports_parallel_tool_calls", "experimental_supported_tools",
	}
	for i, entry := range entries {
		for _, field := range required {
			if _, ok := entry[field]; !ok {
				t.Errorf("models[%d] is missing Codex required field %q", i, field)
			}
		}
		var mirror codexRequiredFields
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal models[%d]: %v", i, err)
		}
		if err := json.Unmarshal(encoded, &mirror); err != nil {
			t.Errorf("models[%d] does not match Codex required-field types: %v", i, err)
		}
		assertCodexRequiredFieldValues(t, i, mirror)
		if mirror.Slug == "" || mirror.BaseInstructions == "" {
			t.Errorf("models[%d] has empty slug or base_instructions", i)
		}
		if raw := bytes.TrimSpace(entry["model_messages"]); len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("{}")) {
			t.Errorf("models[%d] has empty model_messages", i)
		}
	}
}

func TestRenderCodexCopiesSnapshotFieldsVerbatimAndDoesNotAliasThem(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	body, _, err := RenderCodex(models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, body) {
		var slug string
		if err := json.Unmarshal(entry["slug"], &slug); err != nil {
			t.Fatalf("decode rendered slug: %v", err)
		}
		for field, want := range codexModels[slug] {
			if field == "auto_review_model_override" {
				continue
			}
			if got := entry[field]; !bytes.Equal(got, want) {
				t.Errorf("%s.%s changed:\n got: %s\nwant: %s", slug, field, got, want)
			}
		}
		if _, ok := entry["auto_review_model_override"]; ok {
			t.Errorf("%s retained auto_review_model_override without a reviewer", slug)
		}
	}

	copy := copyCodexFields(codexModels["gpt-5.4"])
	copy["slug"][0] = 'x'
	if bytes.Equal(copy["slug"], codexModels["gpt-5.4"]["slug"]) {
		t.Error("copyCodexFields retained a RawMessage alias into the embedded snapshot")
	}
}

func TestRenderCodexInjectsOnlyAnEmittedReviewer(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	tests := []struct {
		name        string
		reviewer    string
		wantValue   string
		wantSkipped string
	}{
		{name: "empty reviewer"},
		{name: "emitted reviewer overwrites snapshot value", reviewer: "gpt-5.4-mini", wantValue: "gpt-5.4-mini"},
		{name: "snapshot-only reviewer is skipped", reviewer: "codex-auto-review", wantSkipped: "codex-auto-review"},
		{name: "Copilot-only reviewer is skipped", reviewer: "gpt-5.3-codex", wantSkipped: "gpt-5.3-codex"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, outcome, err := RenderCodex(models, CodexRenderConfig{AutoReviewModel: tc.reviewer})
			if err != nil {
				t.Fatalf("RenderCodex: %v", err)
			}
			if outcome.SkippedReviewer != tc.wantSkipped {
				t.Errorf("skipped reviewer = %q, want %q", outcome.SkippedReviewer, tc.wantSkipped)
			}
			for i, entry := range decodeRenderedCodex(t, body) {
				raw, ok := entry["auto_review_model_override"]
				if tc.wantValue == "" {
					if ok {
						t.Errorf("models[%d] has unexpected override %s", i, raw)
					}
					continue
				}
				var got string
				if !ok || json.Unmarshal(raw, &got) != nil || got != tc.wantValue {
					t.Errorf("models[%d] override = %s, want %q", i, raw, tc.wantValue)
				}
			}
		})
	}
}

func TestRenderCodexDropsAReviewerCopilotStopsForwarding(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	withoutReviewer := make([]Model, 0, len(models)-1)
	for _, model := range models {
		if model.ID != "gpt-5.4" {
			withoutReviewer = append(withoutReviewer, model)
		}
	}

	body, outcome, err := RenderCodex(withoutReviewer, CodexRenderConfig{AutoReviewModel: "gpt-5.4"})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if outcome.SkippedReviewer != "gpt-5.4" {
		t.Errorf("skipped reviewer = %q, want gpt-5.4", outcome.SkippedReviewer)
	}
	if slugs := renderedSlugs(t, decodeRenderedCodex(t, body)); contains(slugs, "gpt-5.4") {
		t.Errorf("rendered slugs %q retained model Copilot stopped forwarding", slugs)
	}
}

func TestRenderCodexOverlaysLimitsWithIndependentSnapshotFallbacks(t *testing.T) {
	promptOnly, contextOnly, both := 111, 222, 333
	models := []Model{
		{ID: "gpt-5.4-mini", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &promptOnly}}},
		{ID: "gpt-5.4", Capabilities: Capabilities{Limits: Limits{MaxContextWindowTokens: &contextOnly}}},
		{ID: "gpt-5.5", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &both, MaxContextWindowTokens: &both}}},
	}

	offBody, _, err := RenderCodex(models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex with overlay off: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, offBody) {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "context_window", entry["context_window"], codexModels[slug]["context_window"])
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], codexModels[slug]["max_context_window"])
	}

	onBody, _, err := RenderCodex(models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex with overlay on: %v", err)
	}
	entries := decodeRenderedCodex(t, onBody)
	assertJSONInt(t, entries[0], "context_window", promptOnly)
	assertRawFieldEqual(t, "gpt-5.4-mini", "max_context_window", entries[0]["max_context_window"], codexModels["gpt-5.4-mini"]["max_context_window"])
	assertRawFieldEqual(t, "gpt-5.4", "context_window", entries[1]["context_window"], codexModels["gpt-5.4"]["context_window"])
	assertJSONInt(t, entries[1], "max_context_window", contextOnly)
	assertJSONInt(t, entries[2], "context_window", both)
	assertJSONInt(t, entries[2], "max_context_window", both)
}

func TestRenderCodexFallsBackWhenCapturedCopilotModelsOmitLimits(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	for _, model := range models {
		if model.Capabilities.Limits.MaxContextWindowTokens != nil {
			t.Fatalf("captured %s unexpectedly has max_context_window_tokens", model.ID)
		}
	}

	body, _, err := RenderCodex(models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, body) {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], codexModels[slug]["max_context_window"])
	}
}

func TestDecodePreservesOptionalMaxContextWindowTokens(t *testing.T) {
	models, err := Decode([]byte(`{"data":[{"capabilities":{"limits":{"max_context_window_tokens":456}}}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	limit := models[0].Capabilities.Limits.MaxContextWindowTokens
	if limit == nil || *limit != 456 {
		t.Errorf("max_context_window_tokens = %v, want 456", limit)
	}
}

type codexRequiredFields struct {
	Slug                       string                `json:"slug"`
	DisplayName                string                `json:"display_name"`
	SupportedReasoningLevels   []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                  string                `json:"shell_type"`
	Visibility                 string                `json:"visibility"`
	SupportedInAPI             bool                  `json:"supported_in_api"`
	Priority                   int                   `json:"priority"`
	BaseInstructions           string                `json:"base_instructions"`
	SupportsReasoningSummaries bool                  `json:"supports_reasoning_summaries"`
	SupportVerbosity           bool                  `json:"support_verbosity"`
	TruncationPolicy           struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	} `json:"truncation_policy"`
	SupportsParallelToolCalls  bool     `json:"supports_parallel_tool_calls"`
	ExperimentalSupportedTools []string `json:"experimental_supported_tools"`
}

type codexReasoningLevel struct {
	Effort      string  `json:"effort"`
	Description *string `json:"description"`
}

func assertCodexRequiredFieldValues(t *testing.T, index int, model codexRequiredFields) {
	t.Helper()
	for levelIndex, level := range model.SupportedReasoningLevels {
		if level.Effort == "" || level.Description == nil {
			t.Errorf("models[%d].supported_reasoning_levels[%d] is not a complete Codex reasoning preset", index, levelIndex)
		}
	}
	switch model.ShellType {
	case "default", "local", "unified_exec", "disabled", "shell_command":
	default:
		t.Errorf("models[%d].shell_type = %q, want a Codex ConfigShellToolType", index, model.ShellType)
	}
	switch model.Visibility {
	case "list", "hide", "none":
	default:
		t.Errorf("models[%d].visibility = %q, want a Codex ModelVisibility", index, model.Visibility)
	}
	switch model.TruncationPolicy.Mode {
	case "bytes", "tokens":
	default:
		t.Errorf("models[%d].truncation_policy.mode = %q, want a Codex TruncationMode", index, model.TruncationPolicy.Mode)
	}
}

func decodeRenderedCodex(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode Codex envelope: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("Codex envelope keys = %v, want only models", reflect.ValueOf(top).MapKeys())
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(top["models"], &entries); err != nil {
		t.Fatalf("decode Codex models: %v", err)
	}
	if entries == nil {
		t.Fatal("Codex models is null, want array")
	}
	return entries
}

func renderedSlugs(t *testing.T, entries []map[string]json.RawMessage) []string {
	t.Helper()
	slugs := make([]string, len(entries))
	for i, entry := range entries {
		slugs[i] = decodeStringField(t, entry, "slug")
	}
	return slugs
}

func decodeStringField(t *testing.T, entry map[string]json.RawMessage, field string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(entry[field], &value); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	return value
}

func assertRawFieldEqual(t *testing.T, slug, field string, got, want json.RawMessage) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s.%s = %s, want snapshot %s", slug, field, got, want)
	}
}

func assertJSONInt(t *testing.T, entry map[string]json.RawMessage, field string, want int) {
	t.Helper()
	var got int
	if err := json.Unmarshal(entry[field], &got); err != nil || got != want {
		t.Errorf("%s = %s (%v), want %d", field, entry[field], err, want)
	}
}
