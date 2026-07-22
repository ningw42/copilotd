package catalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

var testCodexModels = mustDecodeCodexModels(embeddedCodexModels)

func TestRenderCodexIntersectsInLiveOrderAndEmitsCompleteEntries(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %v, want none", outcome.SkippedReviewers)
	}

	entries := decodeRenderedCodex(t, body)
	wantSlugs := []string{
		"gpt-5.4-mini", "gpt-5.4", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Errorf("rendered slugs = %q, want %q", got, wantSlugs)
	}

	for i, entry := range entries {
		for _, field := range requiredCodexModelFields {
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

func TestRenderCodexCopiesCurrentFieldsVerbatimAndDoesNotAliasThem(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	body, _, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{"gpt-5.4": "gpt-5.4-mini"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, body) {
		var slug string
		if err := json.Unmarshal(entry["slug"], &slug); err != nil {
			t.Fatalf("decode rendered slug: %v", err)
		}
		for field, want := range testCodexModels[slug] {
			if field == "auto_review_model_override" {
				continue
			}
			if got := entry[field]; !bytes.Equal(got, want) {
				t.Errorf("%s.%s changed:\n got: %s\nwant: %s", slug, field, got, want)
			}
		}
		rawReviewer, hasReviewer := entry["auto_review_model_override"]
		if slug == "gpt-5.4" {
			if got := decodeStringField(t, entry, "auto_review_model_override"); got != "gpt-5.4-mini" {
				t.Errorf("%s reviewer = %q, want gpt-5.4-mini", slug, got)
			}
		} else if hasReviewer {
			t.Errorf("%s retained auto_review_model_override without a reviewer: %s", slug, rawReviewer)
		}
	}

	copy := copyCodexEntry(testCodexModels["gpt-5.4"])
	copy["slug"][0] = 'x'
	if bytes.Equal(copy["slug"], testCodexModels["gpt-5.4"]["slug"]) {
		t.Error("copyCodexEntry retained a RawMessage alias into the vendored snapshot")
	}
}

func TestRenderCodexInjectsOnlyAnEmittedReviewer(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	tests := []struct {
		name      string
		reviewer  string
		wantValue string
		wantSkips bool
	}{
		{name: "empty reviewer"},
		{name: "emitted reviewer overwrites Codex value", reviewer: "gpt-5.4-mini", wantValue: "gpt-5.4-mini"},
		{name: "Codex-only reviewer is skipped", reviewer: "codex-auto-review", wantSkips: true},
		{name: "Copilot-only reviewer is skipped", reviewer: "gpt-5.3-codex", wantSkips: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{AutoReviewModel: tc.reviewer})
			if err != nil {
				t.Fatalf("RenderCodex: %v", err)
			}
			entries := decodeRenderedCodex(t, body)
			wantSkipCount := 0
			if tc.wantSkips {
				wantSkipCount = len(entries)
			}
			if len(outcome.SkippedReviewers) != wantSkipCount {
				t.Errorf("skipped reviewers = %#v, want %d", outcome.SkippedReviewers, wantSkipCount)
			}
			for _, skipped := range outcome.SkippedReviewers {
				if skipped.Reviewer != tc.reviewer {
					t.Errorf("skipped reviewer = %q, want %q", skipped.Reviewer, tc.reviewer)
				}
			}
			for i, entry := range entries {
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

func TestRenderCodexResolvesPerModelReviewerBeforeGlobalFallback(t *testing.T) {
	models := []Model{{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModel: "gpt-5.5",
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4-mini": "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %v, want none", outcome.SkippedReviewers)
	}

	entries := decodeRenderedCodex(t, body)
	if got := decodeStringField(t, entries[0], "auto_review_model_override"); got != "gpt-5.4" {
		t.Errorf("gpt-5.4-mini reviewer = %q, want per-model gpt-5.4", got)
	}
	for _, entry := range entries[1:] {
		if got := decodeStringField(t, entry, "auto_review_model_override"); got != "gpt-5.5" {
			t.Errorf("%s reviewer = %q, want global gpt-5.5", decodeStringField(t, entry, "slug"), got)
		}
	}
}

func TestRenderCodexResolvesReviewerOverridesSingleHop(t *testing.T) {
	models := []Model{{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, _, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4-mini": "gpt-5.4",
			"gpt-5.4":      "gpt-5.5",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}

	entries := decodeRenderedCodex(t, body)
	if got := decodeStringField(t, entries[0], "auto_review_model_override"); got != "gpt-5.4" {
		t.Errorf("gpt-5.4-mini reviewer = %q, want single-hop gpt-5.4", got)
	}
	if got := decodeStringField(t, entries[1], "auto_review_model_override"); got != "gpt-5.5" {
		t.Errorf("gpt-5.4 reviewer = %q, want gpt-5.5", got)
	}
	if _, ok := entries[2]["auto_review_model_override"]; ok {
		t.Error("gpt-5.5 has an override without an explicit or global reviewer")
	}
}

func TestRenderCodexSkipsBadExplicitReviewerWithoutGlobalFallback(t *testing.T) {
	const missingReviewer = "missing-reviewer"
	limit := 123
	models := []Model{
		{ID: "gpt-5.4-mini", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &limit}}},
		{ID: "gpt-5.4"},
		{ID: "gpt-5.5"},
	}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModel: "gpt-5.5",
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4-mini": missingReviewer,
		},
		OverrideLimits: true,
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSkipped := []SkippedReviewer{{Model: "gpt-5.4-mini", Reviewer: missingReviewer}}
	if !reflect.DeepEqual(outcome.SkippedReviewers, wantSkipped) {
		t.Errorf("skipped reviewers = %#v, want %#v", outcome.SkippedReviewers, wantSkipped)
	}

	entries := decodeRenderedCodex(t, body)
	if got := renderedSlugs(t, entries); !contains(got, "gpt-5.4-mini") {
		t.Fatalf("rendered slugs %q dropped the main model", got)
	}
	if _, ok := entries[0]["auto_review_model_override"]; ok {
		t.Error("main model fell back to the valid global reviewer")
	}
	assertJSONInt(t, entries[0], "context_window", limit)
	for _, entry := range entries[1:] {
		if got := decodeStringField(t, entry, "auto_review_model_override"); got != "gpt-5.5" {
			t.Errorf("%s reviewer = %q, want global gpt-5.5", decodeStringField(t, entry, "slug"), got)
		}
	}
}

func TestRenderCodexReportsBadGlobalPerAffectedModelInEmissionOrder(t *testing.T) {
	const missingReviewer = "missing-global-reviewer"
	models := []Model{{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModel: missingReviewer,
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4": "gpt-5.4-mini",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSkipped := []SkippedReviewer{
		{Model: "gpt-5.4-mini", Reviewer: missingReviewer},
		{Model: "gpt-5.5", Reviewer: missingReviewer},
	}
	if !reflect.DeepEqual(outcome.SkippedReviewers, wantSkipped) {
		t.Errorf("skipped reviewers = %#v, want emission-ordered %#v", outcome.SkippedReviewers, wantSkipped)
	}

	entries := decodeRenderedCodex(t, body)
	if _, ok := entries[0]["auto_review_model_override"]; ok {
		t.Error("gpt-5.4-mini injected an unforwardable global reviewer")
	}
	if got := decodeStringField(t, entries[1], "auto_review_model_override"); got != "gpt-5.4-mini" {
		t.Errorf("gpt-5.4 reviewer = %q, want valid explicit reviewer", got)
	}
	if _, ok := entries[2]["auto_review_model_override"]; ok {
		t.Error("gpt-5.5 injected an unforwardable global reviewer")
	}
}

func TestRenderCodexIgnoresNonAdvertisedAndMiscasedOverrideKeys(t *testing.T) {
	models := []Model{{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.5":      "missing-reviewer",
			"GPT-5.4-MINI": "missing-reviewer",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %#v, want none for inert keys", outcome.SkippedReviewers)
	}
	for _, entry := range decodeRenderedCodex(t, body) {
		if _, ok := entry["auto_review_model_override"]; ok {
			t.Errorf("%s gained a reviewer from an inert key", decodeStringField(t, entry, "slug"))
		}
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

	body, outcome, err := RenderCodex(testCodexModels, withoutReviewer, CodexRenderConfig{AutoReviewModel: "gpt-5.4"})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	slugs := renderedSlugs(t, entries)
	if len(outcome.SkippedReviewers) != len(slugs) {
		t.Errorf("skipped reviewers = %#v, want one per emitted model", outcome.SkippedReviewers)
	}
	for i, skipped := range outcome.SkippedReviewers {
		if skipped.Model != slugs[i] || skipped.Reviewer != "gpt-5.4" {
			t.Errorf("skipped reviewers[%d] = %#v, want model %q reviewer gpt-5.4", i, skipped, slugs[i])
		}
	}
	if contains(slugs, "gpt-5.4") {
		t.Errorf("rendered slugs %q retained model Copilot stopped forwarding", slugs)
	}
}

func TestRenderCodexOverlaysLimitsWithIndependentVendoredFallbacks(t *testing.T) {
	promptOnly, contextOnly, both := 111, 222, 333
	models := []Model{
		{ID: "gpt-5.4-mini", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &promptOnly}}},
		{ID: "gpt-5.4", Capabilities: Capabilities{Limits: Limits{MaxContextWindowTokens: &contextOnly}}},
		{ID: "gpt-5.5", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &both, MaxContextWindowTokens: &both}}},
	}

	offBody, _, err := RenderCodex(testCodexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex with overlay off: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, offBody) {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "context_window", entry["context_window"], testCodexModels[slug]["context_window"])
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], testCodexModels[slug]["max_context_window"])
	}

	onBody, _, err := RenderCodex(testCodexModels, models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex with overlay on: %v", err)
	}
	entries := decodeRenderedCodex(t, onBody)
	assertJSONInt(t, entries[0], "context_window", promptOnly)
	assertRawFieldEqual(t, "gpt-5.4-mini", "max_context_window", entries[0]["max_context_window"], testCodexModels["gpt-5.4-mini"]["max_context_window"])
	assertRawFieldEqual(t, "gpt-5.4", "context_window", entries[1]["context_window"], testCodexModels["gpt-5.4"]["context_window"])
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

	body, _, err := RenderCodex(testCodexModels, models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, body) {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], testCodexModels[slug]["max_context_window"])
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
		t.Errorf("%s.%s = %s, want vendored value %s", slug, field, got, want)
	}
}

func assertJSONInt(t *testing.T, entry map[string]json.RawMessage, field string, want int) {
	t.Helper()
	var got int
	if err := json.Unmarshal(entry[field], &got); err != nil || got != want {
		t.Errorf("%s = %s (%v), want %d", field, entry[field], err, want)
	}
}
