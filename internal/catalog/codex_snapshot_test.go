package catalog

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedCodexModelsLoadAtStartup(t *testing.T) {
	wantSlugs := []string{
		"codex-auto-review",
		"gpt-5.2",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.5",
		"gpt-5.6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
	}

	gotSlugs := make([]string, 0, len(testCodexModels))
	for slug, fields := range testCodexModels {
		gotSlugs = append(gotSlugs, slug)

		var embeddedSlug string
		if err := json.Unmarshal(fields["slug"], &embeddedSlug); err != nil {
			t.Errorf("decode slug field for %q: %v", slug, err)
		} else if embeddedSlug != slug {
			t.Errorf("entry keyed by %q carries slug %q", slug, embeddedSlug)
		}
		if len(fields) <= 1 {
			t.Errorf("entry %q did not retain its non-slug fields", slug)
		}
		for field, raw := range fields {
			if !json.Valid(raw) {
				t.Errorf("entry %q field %q is not valid raw JSON", slug, field)
			}
		}
	}
	sort.Strings(gotSlugs)
	if !reflect.DeepEqual(gotSlugs, wantSlugs) {
		t.Errorf("embedded Codex slugs = %q, want %q", gotSlugs, wantSlugs)
	}
}

func TestDecodeCodexModelsRejectsMalformedModelsBytes(t *testing.T) {
	tests := map[string]string{
		"invalid JSON":     `{`,
		"missing models":   `{}`,
		"null models":      `{"models":null}`,
		"non-array models": `{"models":{}}`,
		"null entry":       `{"models":[null]}`,
		"missing slug":     `{"models":[{"base_instructions":"prompt"}]}`,
		"non-string slug":  `{"models":[{"slug":1}]}`,
		"empty slug":       `{"models":[{"slug":""}]}`,
		"duplicate slug":   `{"models":[{"slug":"gpt"},{"slug":"gpt"}]}`,
		"incomplete entry": `{"models":[{"slug":"gpt","base_instructions":"prompt"}]}`,
	}

	for name, modelsBytes := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCodexModels([]byte(modelsBytes)); err == nil {
				t.Error("decodeCodexModels succeeded, want packaging-defect error")
			}
		})
	}
}

func TestDecodeCodexModelsRejectsIncompleteNestedRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		field  string
	}{
		{name: "truncation mode", parent: "truncation_policy", field: "mode"},
		{name: "truncation limit", parent: "truncation_policy", field: "limit"},
		{name: "model messages instructions template", parent: "model_messages", field: "instructions_template"},
		{name: "model messages instructions variables", parent: "model_messages", field: "instructions_variables"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			incomplete := codexModelsBytesWithoutNestedField(t, tc.parent, tc.field)

			if _, err := decodeCodexModels(incomplete); err == nil {
				t.Fatalf("decodeCodexModels accepted %s without required %s", tc.parent, tc.field)
			}
		})
	}
}

func TestDecodeCodexModelsAcceptsAndPreservesUnknownEnumValues(t *testing.T) {
	const (
		shellType      = "future_shell"
		visibility     = "future_visibility"
		truncationMode = "future_truncation"
	)
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
		t.Fatalf("decode complete fixture: %v", err)
	}
	entry := envelope.Models[0]
	entry["shell_type"] = shellType
	entry["visibility"] = visibility
	truncationPolicy, ok := entry["truncation_policy"].(map[string]any)
	if !ok {
		t.Fatalf("fixture truncation_policy = %#v, want object", entry["truncation_policy"])
	}
	truncationPolicy["mode"] = truncationMode
	currentBytes, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode future enum fixture: %v", err)
	}

	models, err := decodeCodexModels(currentBytes)
	if err != nil {
		t.Fatalf("decodeCodexModels rejected future enum values: %v", err)
	}
	fields := models["gpt-test"]
	for field, want := range map[string]string{
		"shell_type": shellType,
		"visibility": visibility,
	} {
		var got string
		if err := json.Unmarshal(fields[field], &got); err != nil {
			t.Fatalf("decode preserved %s: %v", field, err)
		}
		if got != want {
			t.Errorf("preserved %s = %q, want %q", field, got, want)
		}
	}
	var decodedTruncationPolicy struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(fields["truncation_policy"], &decodedTruncationPolicy); err != nil {
		t.Fatalf("decode preserved truncation_policy: %v", err)
	}
	if decodedTruncationPolicy.Mode != truncationMode {
		t.Errorf("preserved truncation mode = %q, want %q", decodedTruncationPolicy.Mode, truncationMode)
	}
}

func codexModelsBytesWithoutNestedField(t *testing.T, parentField, nestedField string) []byte {
	t.Helper()
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
		t.Fatalf("decode complete fixture: %v", err)
	}
	parent, ok := envelope.Models[0][parentField].(map[string]any)
	if !ok {
		t.Fatalf("fixture %s = %#v, want object", parentField, envelope.Models[0][parentField])
	}
	delete(parent, nestedField)
	incomplete, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode incomplete fixture: %v", err)
	}
	return incomplete
}

func validCodexModelsBytes(t *testing.T, slug, prompt string) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"models": []any{map[string]any{
			"slug":                         slug,
			"display_name":                 "Fresh model",
			"supported_reasoning_levels":   []any{map[string]any{"effort": "medium", "description": "Balanced"}},
			"shell_type":                   "shell_command",
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     1,
			"base_instructions":            prompt,
			"supports_reasoning_summaries": true,
			"support_verbosity":            true,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls": true,
			"experimental_supported_tools": []string{},
			"model_messages": map[string]any{
				"instructions_template":  "{{ instructions }}",
				"instructions_variables": map[string]string{"personality_default": ""},
				"approvals":              nil,
			},
		}},
	})
	if err != nil {
		t.Fatalf("encode valid Codex models bytes: %v", err)
	}
	return encoded
}

func TestMustDecodeCodexModelsPanicsOnDecodeFailure(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("mustDecodeCodexModels returned, want startup panic")
		}
		if message := got.(string); !strings.Contains(message, "decode embedded Codex models") {
			t.Errorf("panic = %q, want vendored-model context", message)
		}
	}()

	mustDecodeCodexModels([]byte(`{"models":null}`))
}
