package catalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

// embeddedCodexModels is Codex's bundled model catalog at rust-v0.144.5.
// Its exact origin and license are recorded alongside the vendored file.
//
//go:embed codexdata/models.json
var embeddedCodexModels []byte

func init() {
	// A panic here is intentional: malformed embedded data is a build/packaging
	// defect. The decoded map is discarded so the process retains only bytes.
	_ = mustDecodeCodexModels(embeddedCodexModels)
}

// CodexModels is the decoded, slug-keyed representation consumed by the pure
// Codex renderer. It is request-scoped; the cached value retains raw bytes.
type CodexModels map[string]map[string]json.RawMessage

var requiredCodexModelFields = []string{
	"slug", "display_name", "supported_reasoning_levels", "shell_type",
	"visibility", "supported_in_api", "priority", "base_instructions",
	"supports_reasoning_summaries", "support_verbosity", "truncation_policy",
	"supports_parallel_tool_calls", "experimental_supported_tools", "model_messages",
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
	TruncationPolicy           codexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls  bool                  `json:"supports_parallel_tool_calls"`
	ExperimentalSupportedTools []string              `json:"experimental_supported_tools"`
	ModelMessages              codexModelMessages    `json:"model_messages"`
}

type codexReasoningLevel struct {
	Effort      string  `json:"effort"`
	Description *string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit *int64 `json:"limit"`
}

type codexModelMessages struct {
	InstructionsTemplate  string            `json:"instructions_template"`
	InstructionsVariables map[string]string `json:"instructions_variables"`
}

func mustDecodeCodexModels(vendoredBytes []byte) CodexModels {
	models, err := decodeCodexModels(vendoredBytes)
	if err != nil {
		panic(fmt.Sprintf("decode embedded Codex models: %v", err))
	}
	return models
}

func decodeCodexModels(currentBytes []byte) (CodexModels, error) {
	var envelope struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(currentBytes, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}

	rawModels := bytes.TrimSpace(envelope.Models)
	if len(rawModels) == 0 || bytes.Equal(rawModels, []byte("null")) {
		return nil, fmt.Errorf("models array is missing or null")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rawModels, &entries); err != nil {
		return nil, fmt.Errorf("decode models array: %w", err)
	}

	models := make(CodexModels, len(entries))
	for i, rawEntry := range entries {
		if bytes.Equal(bytes.TrimSpace(rawEntry), []byte("null")) {
			return nil, fmt.Errorf("models[%d] is null", i)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawEntry, &fields); err != nil {
			return nil, fmt.Errorf("decode models[%d]: %w", i, err)
		}

		rawSlug, ok := fields["slug"]
		if !ok {
			return nil, fmt.Errorf("models[%d] is missing slug", i)
		}
		var slug string
		if err := json.Unmarshal(rawSlug, &slug); err != nil {
			return nil, fmt.Errorf("decode models[%d] slug: %w", i, err)
		}
		if slug == "" {
			return nil, fmt.Errorf("models[%d] has empty slug", i)
		}
		if _, duplicate := models[slug]; duplicate {
			return nil, fmt.Errorf("models[%d] duplicates slug %q", i, slug)
		}
		if err := validateCodexModel(i, rawEntry, fields); err != nil {
			return nil, err
		}
		models[slug] = fields
	}
	return models, nil
}

func validateCodexModel(index int, rawEntry json.RawMessage, fields map[string]json.RawMessage) error {
	for _, field := range requiredCodexModelFields {
		raw, ok := fields[field]
		if !ok || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("models[%d] is missing required field %q", index, field)
		}
	}

	var model codexRequiredFields
	if err := json.Unmarshal(rawEntry, &model); err != nil {
		return fmt.Errorf("decode models[%d] required fields: %w", index, err)
	}
	if model.Slug == "" || model.DisplayName == "" || model.BaseInstructions == "" {
		return fmt.Errorf("models[%d] has an empty required string", index)
	}
	if len(model.SupportedReasoningLevels) == 0 {
		return fmt.Errorf("models[%d] has no supported reasoning levels", index)
	}
	for levelIndex, level := range model.SupportedReasoningLevels {
		if level.Effort == "" || level.Description == nil {
			return fmt.Errorf("models[%d].supported_reasoning_levels[%d] is incomplete", index, levelIndex)
		}
	}
	switch model.ShellType {
	case "default", "local", "unified_exec", "disabled", "shell_command":
	default:
		return fmt.Errorf("models[%d] has invalid shell_type %q", index, model.ShellType)
	}
	switch model.Visibility {
	case "list", "hide", "none":
	default:
		return fmt.Errorf("models[%d] has invalid visibility %q", index, model.Visibility)
	}
	switch model.TruncationPolicy.Mode {
	case "bytes", "tokens":
	default:
		return fmt.Errorf("models[%d] has invalid truncation policy mode %q", index, model.TruncationPolicy.Mode)
	}
	if model.TruncationPolicy.Limit == nil {
		return fmt.Errorf("models[%d] truncation_policy is missing limit", index)
	}
	if model.ExperimentalSupportedTools == nil {
		return fmt.Errorf("models[%d] has null experimental_supported_tools", index)
	}
	if model.ModelMessages.InstructionsTemplate == "" {
		return fmt.Errorf("models[%d] model_messages is missing instructions_template", index)
	}
	if model.ModelMessages.InstructionsVariables == nil {
		return fmt.Errorf("models[%d] model_messages is missing instructions_variables", index)
	}
	return nil
}
