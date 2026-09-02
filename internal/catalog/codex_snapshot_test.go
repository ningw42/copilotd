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
		"gpt-daybreak-blue-latest",
		"gpt-daybreak-red-latest",
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

func TestValidateCodexModelsRejectsMalformedModelsBytes(t *testing.T) {
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
			if _, err := validateCodexModels([]byte(modelsBytes)); err == nil {
				t.Error("validateCodexModels succeeded, want packaging-defect error")
			}
		})
	}
}

func TestParseCodexModelsDefersContractValidation(t *testing.T) {
	modelsBytes := codexModelsBytesWithField(t, "context_window", "bad")

	models, err := parseCodexModels(modelsBytes)
	if err != nil {
		t.Fatalf("parseCodexModels rejected structurally valid cached bytes: %v", err)
	}
	if _, ok := models["gpt-test"]; !ok {
		t.Fatal("parseCodexModels lost the model entry")
	}
	if _, err := validateCodexModels(modelsBytes); err == nil {
		t.Fatal("validateCodexModels accepted the malformed known field")
	}
}

func TestValidateCodexModelsRejectsIncompleteNestedRequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		field  string
	}{
		{name: "truncation mode", parent: "truncation_policy", field: "mode"},
		{name: "truncation limit", parent: "truncation_policy", field: "limit"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			incomplete := codexModelsBytesWithoutNestedField(t, tc.parent, tc.field)

			if _, err := validateCodexModels(incomplete); err == nil {
				t.Fatalf("validateCodexModels accepted %s without required %s", tc.parent, tc.field)
			}
		})
	}
}

func TestValidateCodexModelsRejectsMissingInstructionSource(t *testing.T) {
	if _, err := validateCodexModels(codexModelsBytesWithoutInstructionSources(t)); err == nil {
		t.Fatal("validateCodexModels accepted a model without legacy or canonical instructions")
	}
}

func TestValidateCodexModelsAcceptsAndPreservesOptionalInstructionsVariables(t *testing.T) {
	tests := []struct {
		name        string
		modelsBytes []byte
		wantPresent bool
		wantJSON    string
	}{
		{
			name:        "absent",
			modelsBytes: codexModelsBytesWithoutNestedField(t, "model_messages", "instructions_variables"),
		},
		{
			name:        "null",
			modelsBytes: codexModelsBytesWithInstructionsVariables(t, nil),
			wantPresent: true,
			wantJSON:    "null",
		},
		{
			name:        "empty object",
			modelsBytes: codexModelsBytesWithInstructionsVariables(t, map[string]string{}),
			wantPresent: true,
			wantJSON:    `{}`,
		},
		{
			name:        "populated object",
			modelsBytes: validCodexModelsBytes(t, "gpt-test", "prompt"),
			wantPresent: true,
			wantJSON:    `{"personality_default":""}`,
		},
		{
			name:        "nullable known values",
			modelsBytes: codexModelsBytesWithInstructionsVariables(t, map[string]any{"personality_default": nil}),
			wantPresent: true,
			wantJSON:    `{"personality_default":null}`,
		},
		{
			name:        "unknown values are ignored by Serde and preserved raw",
			modelsBytes: codexModelsBytesWithInstructionsVariables(t, map[string]any{"future_variable": 1}),
			wantPresent: true,
			wantJSON:    `{"future_variable":1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, err := validateCodexModels(tc.modelsBytes)
			if err != nil {
				t.Fatalf("validateCodexModels rejected optional instructions_variables: %v", err)
			}
			var messages map[string]json.RawMessage
			if err := json.Unmarshal(models["gpt-test"]["model_messages"], &messages); err != nil {
				t.Fatalf("decode preserved model_messages: %v", err)
			}
			got, present := messages["instructions_variables"]
			if present != tc.wantPresent {
				t.Fatalf("instructions_variables presence = %t, want %t", present, tc.wantPresent)
			}
			if present && string(got) != tc.wantJSON {
				t.Errorf("instructions_variables = %s, want %s", got, tc.wantJSON)
			}
		})
	}
}

func TestValidateCodexModelsAcceptsCanonicalInstructionSourceWithoutLegacyBase(t *testing.T) {
	modelsBytes := codexModelsBytesWithoutField(t, "base_instructions")

	models, err := validateCodexModels(modelsBytes)
	if err != nil {
		t.Fatalf("validateCodexModels rejected canonical instruction source: %v", err)
	}
	if _, present := models["gpt-test"]["base_instructions"]; present {
		t.Fatal("validateCodexModels fabricated legacy base_instructions")
	}
	if got := models["gpt-test"]["model_messages"]; len(got) == 0 {
		t.Fatal("validateCodexModels lost canonical model_messages")
	}
}

func TestValidateCodexModelsAcceptsLegacyInstructionSourceWithoutModelMessages(t *testing.T) {
	modelsBytes := codexModelsBytesWithoutField(t, "model_messages")

	models, err := validateCodexModels(modelsBytes)
	if err != nil {
		t.Fatalf("validateCodexModels rejected legacy instruction source: %v", err)
	}
	if _, present := models["gpt-test"]["model_messages"]; present {
		t.Fatal("validateCodexModels fabricated canonical model_messages")
	}
	if got := decodeStringField(t, models["gpt-test"], "base_instructions"); got != "prompt" {
		t.Fatalf("preserved base_instructions = %q, want prompt", got)
	}
}

func TestValidateCodexModelsDoesNotRequireRemovedLegacyFields(t *testing.T) {
	for _, field := range []string{"supports_reasoning_summaries", "supports_parallel_tool_calls"} {
		t.Run(field, func(t *testing.T) {
			modelsBytes := codexModelsBytesWithoutField(t, field)

			models, err := validateCodexModels(modelsBytes)
			if err != nil {
				t.Fatalf("validateCodexModels required removed ModelInfo field %q: %v", field, err)
			}
			if _, present := models["gpt-test"][field]; present {
				t.Fatalf("validateCodexModels fabricated removed field %q", field)
			}
		})
	}
}

func TestValidateCodexModelsRequiresCurrentSerdeFields(t *testing.T) {
	// This literal list is independent of codexRequiredFields. It mirrors the
	// non-Option ModelInfo members without Serde defaults at rust-v0.152.1.
	required := []string{
		"slug",
		"display_name",
		"supported_reasoning_levels",
		"shell_type",
		"visibility",
		"supported_in_api",
		"priority",
		"support_verbosity",
		"truncation_policy",
		"experimental_supported_tools",
	}
	wantRequired := append([]string(nil), required...)
	gotRequired := append([]string(nil), requiredCodexModelFields...)
	sort.Strings(wantRequired)
	sort.Strings(gotRequired)
	if !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Fatalf("requiredCodexModelFields = %q, want exact current Serde set %q", gotRequired, wantRequired)
	}
	for _, field := range required {
		t.Run(field, func(t *testing.T) {
			if _, err := validateCodexModels(codexModelsBytesWithoutField(t, field)); err == nil {
				t.Fatalf("validateCodexModels accepted a model missing current required field %q", field)
			}
		})
	}
}

func TestValidateCodexModelsRejectsPriorityOutsideI32(t *testing.T) {
	modelsBytes := codexModelsBytesWithField(t, "priority", int64(1)<<31)

	if _, err := validateCodexModels(modelsBytes); err == nil {
		t.Fatal("validateCodexModels accepted a priority outside Codex's i32 range")
	}
}

func TestValidateCodexModelsRequiresReasoningPresetFields(t *testing.T) {
	for _, field := range []string{"effort", "description"} {
		t.Run(field, func(t *testing.T) {
			var envelope struct {
				Models []map[string]any `json:"models"`
			}
			if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
				t.Fatalf("decode complete fixture: %v", err)
			}
			levels := envelope.Models[0]["supported_reasoning_levels"].([]any)
			delete(levels[0].(map[string]any), field)
			currentBytes, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("encode incomplete reasoning preset: %v", err)
			}
			if _, err := validateCodexModels(currentBytes); err == nil {
				t.Fatalf("validateCodexModels accepted reasoning preset missing %q", field)
			}
		})
	}
}

func TestValidateCodexModelsAcceptsEmptyReasoningLevels(t *testing.T) {
	modelsBytes := codexModelsBytesWithField(t, "supported_reasoning_levels", []any{})

	models, err := validateCodexModels(modelsBytes)
	if err != nil {
		t.Fatalf("validateCodexModels rejected Serde-valid empty reasoning levels: %v", err)
	}
	if got := string(models["gpt-test"]["supported_reasoning_levels"]); got != "[]" {
		t.Fatalf("preserved supported_reasoning_levels = %s, want []", got)
	}
}

func TestValidateCodexModelsAcceptsKnownOptionalFieldsAbsentFromVendoredSnapshot(t *testing.T) {
	modelsBytes := mutateCodexModelsBytes(t, func(entry map[string]any) {
		entry["effective_context_window_percent"] = 95
		entry["multi_agent_reasoning_effort"] = "future_effort"
		entry["availability_nux"] = map[string]any{"message": "Available now"}
		messages := entry["model_messages"].(map[string]any)
		messages["persistent_instructions"] = "Persistent instructions"
		messages["tools"] = map[string]any{
			"send_user_message_async": map[string]any{"description": "Send a message"},
		}
		messages["auto_review"] = map[string]any{"node_repl_policy": "Review policy"}
		messages["multi_agent"] = map[string]any{
			"mode": map[string]any{"proactive": "Proactive instructions"},
		}
		budget := completeCodexTokenBudget()
		budget["enabled"] = true
		budget["use_history_notes_extension"] = true
		messages["token_budget"] = budget
		messages["confirmation_policies"] = map[string]any{
			"browser_use":  "Browser policy",
			"computer_use": nil,
		}
	})

	models, err := validateCodexModels(modelsBytes)
	if err != nil {
		t.Fatalf("validateCodexModels rejected valid optional fields: %v", err)
	}
	got := models["gpt-test"]
	if got["availability_nux"] == nil || got["model_messages"] == nil {
		t.Fatal("validateCodexModels did not preserve optional fields")
	}
	var messages map[string]json.RawMessage
	if err := json.Unmarshal(got["model_messages"], &messages); err != nil {
		t.Fatalf("decode preserved model_messages: %v", err)
	}
	for _, field := range []string{"tools", "auto_review", "multi_agent", "token_budget", "confirmation_policies"} {
		if messages[field] == nil {
			t.Errorf("validateCodexModels did not preserve model_messages.%s", field)
		}
	}
}

func TestValidateCodexModelsAcceptsNullableMessageLeavesAndDefaultedTokenBudgetFlags(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "tool description null", mutate: func(messages map[string]any) {
			messages["tools"] = map[string]any{
				"send_user_message_async": map[string]any{"description": nil},
			}
		}},
		{name: "auto-review node repl policy null", mutate: func(messages map[string]any) {
			messages["auto_review"] = map[string]any{"node_repl_policy": nil}
		}},
		{name: "proactive mode message null", mutate: func(messages map[string]any) {
			messages["multi_agent"] = map[string]any{
				"mode": map[string]any{"proactive": nil},
			}
		}},
		{name: "token budget flags absent", mutate: func(messages map[string]any) {
			messages["token_budget"] = completeCodexTokenBudget()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modelsBytes := mutateCodexModelsBytes(t, func(entry map[string]any) {
				tc.mutate(entry["model_messages"].(map[string]any))
			})
			if _, err := validateCodexModels(modelsBytes); err != nil {
				t.Fatalf("validateCodexModels rejected current optional field form: %v", err)
			}
		})
	}
}

func TestValidateCodexModelsRejectsMalformedInstructionsVariables(t *testing.T) {
	tests := []struct {
		name      string
		variables any
	}{
		{name: "string", variables: "not an object"},
		{name: "array", variables: []string{}},
		{name: "non-string object value", variables: map[string]any{"personality_default": 1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modelsBytes := codexModelsBytesWithInstructionsVariables(t, tc.variables)
			if _, err := validateCodexModels(modelsBytes); err == nil {
				t.Fatal("validateCodexModels accepted malformed instructions_variables")
			}
		})
	}
}

func TestValidateCodexModelsRejectsMalformedInstructionSources(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "non-string legacy base", mutate: func(entry map[string]any) { entry["base_instructions"] = 1 }},
		{name: "non-object model messages", mutate: func(entry map[string]any) { entry["model_messages"] = []any{} }},
		{name: "non-string canonical template", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["instructions_template"] = 1
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var envelope struct {
				Models []map[string]any `json:"models"`
			}
			if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
				t.Fatalf("decode complete fixture: %v", err)
			}
			tc.mutate(envelope.Models[0])
			currentBytes, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("encode malformed instruction fixture: %v", err)
			}
			if _, err := validateCodexModels(currentBytes); err == nil {
				t.Fatal("validateCodexModels accepted a malformed instruction source")
			}
		})
	}
}

func TestValidateCodexModelsRejectsInstructionSourceThatRendersEmpty(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "canonical wins with empty text", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["instructions_template"] = ""
		}},
		{name: "legacy-only empty text", mutate: func(entry map[string]any) {
			entry["base_instructions"] = ""
			delete(entry, "model_messages")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var envelope struct {
				Models []map[string]any `json:"models"`
			}
			if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
				t.Fatalf("decode complete fixture: %v", err)
			}
			tc.mutate(envelope.Models[0])
			currentBytes, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("encode empty instruction fixture: %v", err)
			}
			if _, err := validateCodexModels(currentBytes); err == nil {
				t.Fatal("validateCodexModels accepted an instruction source that degrades the active model")
			}
		})
	}
}

func TestValidateCodexModelsRejectsInvalidTokenBudgetDefaultFlags(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]any
		wantErr string
	}{
		{
			name:    "null",
			values:  map[string]any{"enabled": nil},
			wantErr: `has null non-optional field "model_messages.token_budget.enabled"`,
		},
		{
			name:    "non-boolean",
			values:  map[string]any{"use_history_notes_extension": "bad"},
			wantErr: "model_messages.token_budget.use_history_notes_extension is not a boolean",
		},
		{
			name: "both invalid report enabled first",
			values: map[string]any{
				"enabled":                     "bad",
				"use_history_notes_extension": "bad",
			},
			wantErr: "model_messages.token_budget.enabled is not a boolean",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modelsBytes := mutateCodexModelsBytes(t, func(entry map[string]any) {
				budget := completeCodexTokenBudget()
				for field, value := range tc.values {
					budget[field] = value
				}
				entry["model_messages"].(map[string]any)["token_budget"] = budget
			})
			_, err := validateCodexModels(modelsBytes)
			if err == nil {
				t.Fatal("validateCodexModels accepted an invalid token-budget default flag")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateCodexModels error = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateCodexModelsRejectsMalformedPresentOptionalAndDefaultFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "context window type", mutate: func(entry map[string]any) { entry["context_window"] = "bad" }},
		{name: "null default reasoning summary", mutate: func(entry map[string]any) { entry["default_reasoning_summary"] = nil }},
		{name: "unknown default verbosity", mutate: func(entry map[string]any) { entry["default_verbosity"] = "future" }},
		{name: "unknown input modality", mutate: func(entry map[string]any) { entry["input_modalities"] = []any{"future"} }},
		{name: "empty optional reasoning effort", mutate: func(entry map[string]any) { entry["default_reasoning_level"] = "" }},
		{name: "incomplete availability nux", mutate: func(entry map[string]any) { entry["availability_nux"] = map[string]any{} }},
		{name: "incomplete service tier", mutate: func(entry map[string]any) { entry["service_tiers"] = []any{map[string]any{}} }},
		{name: "non-string tool mode", mutate: func(entry map[string]any) { entry["tool_mode"] = 1 }},
		{name: "incomplete token budget", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["token_budget"] = map[string]any{}
		}},
		{name: "malformed approval message", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["approvals"] = map[string]any{"on_request": 1}
		}},
		{name: "malformed tool message", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["tools"] = map[string]any{"send_user_message_async": "bad"}
		}},
		{name: "malformed auto-review node repl policy", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["auto_review"] = map[string]any{"node_repl_policy": 1}
		}},
		{name: "malformed proactive mode message", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["multi_agent"] = map[string]any{
				"mode": map[string]any{"proactive": 1},
			}
		}},
		{name: "guardian threshold outside u16", mutate: func(entry map[string]any) {
			entry["model_messages"].(map[string]any)["guardian_v2"] = map[string]any{"review_threshold_basis_points": 65536}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			modelsBytes := mutateCodexModelsBytes(t, tc.mutate)
			if _, err := validateCodexModels(modelsBytes); err == nil {
				t.Fatal("validateCodexModels accepted a present field current Codex rejects")
			}
		})
	}
}

func TestValidateCodexModelsRejectsUnknownClosedEnumValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "shell type", mutate: func(entry map[string]any) { entry["shell_type"] = "future_shell" }},
		{name: "visibility", mutate: func(entry map[string]any) { entry["visibility"] = "future_visibility" }},
		{name: "truncation mode", mutate: func(entry map[string]any) {
			entry["truncation_policy"].(map[string]any)["mode"] = "future_truncation"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var envelope struct {
				Models []map[string]any `json:"models"`
			}
			if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
				t.Fatalf("decode complete fixture: %v", err)
			}
			tc.mutate(envelope.Models[0])
			currentBytes, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("encode unknown enum fixture: %v", err)
			}
			if _, err := validateCodexModels(currentBytes); err == nil {
				t.Fatal("validateCodexModels accepted a closed enum value current Codex rejects")
			}
		})
	}
}

func completeCodexTokenBudget() map[string]any {
	return map[string]any{
		"reminder_threshold_tokens":           100,
		"reminder_message_template":           "Reminder",
		"guidance_message":                    "Guidance",
		"auto_compact_fallback_prompt":        "Compact",
		"auto_compact_fallback_buffer_tokens": 10,
	}
}

func codexModelsBytesWithoutNestedField(t *testing.T, parentField, nestedField string) []byte {
	t.Helper()
	return mutateCodexModelsBytes(t, func(entry map[string]any) {
		parent, ok := entry[parentField].(map[string]any)
		if !ok {
			t.Fatalf("fixture %s = %#v, want object", parentField, entry[parentField])
		}
		delete(parent, nestedField)
	})
}

func codexModelsBytesWithoutInstructionSources(t *testing.T) []byte {
	t.Helper()
	return mutateCodexModelsBytes(t, func(entry map[string]any) {
		delete(entry, "base_instructions")
		messages := entry["model_messages"].(map[string]any)
		delete(messages, "instructions_template")
	})
}

func codexModelsBytesWithoutField(t *testing.T, field string) []byte {
	t.Helper()
	return mutateCodexModelsBytes(t, func(entry map[string]any) {
		delete(entry, field)
	})
}

func codexModelsBytesWithInstructionsVariables(t *testing.T, variables any) []byte {
	t.Helper()
	return mutateCodexModelsBytes(t, func(entry map[string]any) {
		messages, ok := entry["model_messages"].(map[string]any)
		if !ok {
			t.Fatalf("fixture model_messages = %#v, want object", entry["model_messages"])
		}
		messages["instructions_variables"] = variables
	})
}

func codexModelsBytesWithField(t *testing.T, field string, value any) []byte {
	t.Helper()
	return mutateCodexModelsBytes(t, func(entry map[string]any) {
		entry[field] = value
	})
}

func mutateCodexModelsBytes(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(validCodexModelsBytes(t, "gpt-test", "prompt"), &envelope); err != nil {
		t.Fatalf("decode complete fixture: %v", err)
	}
	mutate(envelope.Models[0])
	current, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode mutated fixture: %v", err)
	}
	return current
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
