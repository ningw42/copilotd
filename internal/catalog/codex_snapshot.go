package catalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// embeddedCodexModels is Codex's bundled model catalog at rust-v0.152.1.
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

type codexRequiredFields struct {
	Slug                       string                `json:"slug"`
	DisplayName                string                `json:"display_name"`
	SupportedReasoningLevels   []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                  string                `json:"shell_type"`
	Visibility                 string                `json:"visibility"`
	SupportedInAPI             bool                  `json:"supported_in_api"`
	Priority                   int32                 `json:"priority"`
	SupportVerbosity           bool                  `json:"support_verbosity"`
	TruncationPolicy           codexTruncationPolicy `json:"truncation_policy"`
	ExperimentalSupportedTools []*string             `json:"experimental_supported_tools"`
}

var requiredCodexModelFields = requiredCodexJSONFields()

var nonNullableCodexDefaultFields = []string{
	"additional_speed_tiers",
	"service_tiers",
	"include_skills_usage_instructions",
	"include_plugin_usage_instructions",
	"include_apps_usage_instructions",
	"supports_reasoning_summary_parameter",
	"default_reasoning_summary",
	"web_search_tool_type",
	"supports_image_detail_original",
	"effective_context_window_percent",
	"input_modalities",
	"supports_search_tool",
	"use_responses_lite",
	"node_repl_auto_review_required",
	"node_repl_disabled",
}

func requiredCodexJSONFields() []string {
	modelType := reflect.TypeFor[codexRequiredFields]()
	fields := make([]string, 0, modelType.NumField())
	for i := range modelType.NumField() {
		field := modelType.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			panic(fmt.Sprintf("codexRequiredFields.%s has no required JSON field name", field.Name))
		}
		fields = append(fields, name)
	}
	return fields
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
	PersistentInstructions *string            `json:"persistent_instructions"`
	Tools                  *codexToolMessages `json:"tools"`
	InstructionsTemplate   *string            `json:"instructions_template"`
	// Codex treats absent or null variables as literal-template mode.
	InstructionsVariables *codexModelInstructionVariables `json:"instructions_variables"`
	Approvals             *codexApprovalMessages          `json:"approvals"`
	CollaborationModes    *codexCollaborationModeMessages `json:"collaboration_modes"`
	AutoReview            *codexAutoReviewMessages        `json:"auto_review"`
	Permissions           *codexPermissionMessages        `json:"permissions"`
	MultiAgent            *codexMultiAgentMessages        `json:"multi_agent"`
	TokenBudget           *codexModelTokenBudgetConfig    `json:"token_budget"`
	GuardianV2            *codexGuardianV2ModelConfig     `json:"guardian_v2"`
	ConfirmationPolicies  *codexConfirmationPolicies      `json:"confirmation_policies"`
}

type codexToolMessages struct {
	SendUserMessageAsync *codexToolMessage `json:"send_user_message_async"`
}

type codexToolMessage struct {
	Description *string `json:"description"`
}

type codexModelInstructionVariables struct {
	PersonalityDefault   *string `json:"personality_default"`
	PersonalityFriendly  *string `json:"personality_friendly"`
	PersonalityPragmatic *string `json:"personality_pragmatic"`
}

type codexKnownFields struct {
	Description                       *string                    `json:"description"`
	DefaultReasoningLevel             *string                    `json:"default_reasoning_level"`
	AdditionalSpeedTiers              []*string                  `json:"additional_speed_tiers"`
	ServiceTiers                      []codexModelServiceTier    `json:"service_tiers"`
	DefaultServiceTier                *string                    `json:"default_service_tier"`
	AvailabilityNUX                   *codexModelAvailabilityNUX `json:"availability_nux"`
	Upgrade                           *codexModelInfoUpgrade     `json:"upgrade"`
	ModelMessages                     *codexModelMessages        `json:"model_messages"`
	IncludeSkillsUsageInstructions    bool                       `json:"include_skills_usage_instructions"`
	IncludePluginUsageInstructions    bool                       `json:"include_plugin_usage_instructions"`
	IncludeAppsUsageInstructions      bool                       `json:"include_apps_usage_instructions"`
	SupportsReasoningSummaryParameter bool                       `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary           string                     `json:"default_reasoning_summary"`
	DefaultVerbosity                  *string                    `json:"default_verbosity"`
	ApplyPatchToolType                *string                    `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                     `json:"web_search_tool_type"`
	SupportsImageDetailOriginal       bool                       `json:"supports_image_detail_original"`
	ContextWindow                     *int64                     `json:"context_window"`
	MaxContextWindow                  *int64                     `json:"max_context_window"`
	AutoCompactTokenLimit             *int64                     `json:"auto_compact_token_limit"`
	CompHash                          *string                    `json:"comp_hash"`
	EffectiveContextWindowPercent     int64                      `json:"effective_context_window_percent"`
	InputModalities                   []*string                  `json:"input_modalities"`
	SupportsSearchTool                bool                       `json:"supports_search_tool"`
	UseResponsesLite                  bool                       `json:"use_responses_lite"`
	NodeREPLAutoReviewRequired        bool                       `json:"node_repl_auto_review_required"`
	NodeREPLDisabled                  bool                       `json:"node_repl_disabled"`
	AutoReviewModelOverride           *string                    `json:"auto_review_model_override"`
	ModelSpecialty                    *string                    `json:"model_specialty"`
	ToolMode                          *string                    `json:"tool_mode"`
	MultiAgentVersion                 *string                    `json:"multi_agent_version"`
	MultiAgentReasoningEffort         *string                    `json:"multi_agent_reasoning_effort"`
}

type codexModelServiceTier struct {
	ID          *string `json:"id"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

type codexModelAvailabilityNUX struct {
	Message *string `json:"message"`
}

type codexModelInfoUpgrade struct {
	Model             *string         `json:"model"`
	MigrationMarkdown *string         `json:"migration_markdown"`
	RetirementAt      json.RawMessage `json:"retirement_at"`
}

type codexApprovalMessages struct {
	OnRequest           *string `json:"on_request"`
	OnRequestAutoReview *string `json:"on_request_auto_review"`
	Never               *string `json:"never"`
	UnlessTrusted       *string `json:"unless_trusted"`
}

type codexCollaborationModeMessages struct {
	Default *string `json:"default"`
	Plan    *string `json:"plan"`
}

type codexAutoReviewMessages struct {
	Policy                *string `json:"policy"`
	PolicyTemplate        *string `json:"policy_template"`
	NodeREPLPolicy        *string `json:"node_repl_policy"`
	RejectionInstructions *string `json:"rejection_instructions"`
	TimeoutInstructions   *string `json:"timeout_instructions"`
}

type codexPermissionMessages struct {
	DangerFullAccess *string `json:"danger_full_access"`
	WorkspaceWrite   *string `json:"workspace_write"`
	ReadOnly         *string `json:"read_only"`
}

type codexMultiAgentMessages struct {
	Role *codexMultiAgentRoleMessages `json:"role"`
	Mode *codexMultiAgentModeMessages `json:"mode"`
}

type codexMultiAgentRoleMessages struct {
	Root     *string `json:"root"`
	Subagent *string `json:"subagent"`
}

type codexMultiAgentModeMessages struct {
	Explicit  *string `json:"explicit"`
	Proactive *string `json:"proactive"`
	HintText  *string `json:"hint_text"`
}

type codexModelTokenBudgetConfig struct {
	Enabled                         json.RawMessage `json:"enabled"`
	UseHistoryNotesExtension        json.RawMessage `json:"use_history_notes_extension"`
	ReminderThresholdTokens         *int64          `json:"reminder_threshold_tokens"`
	ReminderMessageTemplate         *string         `json:"reminder_message_template"`
	GuidanceMessage                 *string         `json:"guidance_message"`
	AutoCompactFallbackPrompt       *string         `json:"auto_compact_fallback_prompt"`
	AutoCompactFallbackBufferTokens *int64          `json:"auto_compact_fallback_buffer_tokens"`
}

type codexGuardianV2ModelConfig struct {
	ClassifierInstructions         *string                               `json:"classifier_instructions"`
	ReviewThresholdBasisPoints     *uint16                               `json:"review_threshold_basis_points"`
	MaxToolCallLag                 *uint                                 `json:"max_tool_call_lag"`
	ReasoningEffort                *string                               `json:"reasoning_effort"`
	Transcript                     *codexGuardianV2TranscriptModelConfig `json:"transcript"`
	MaxActionTokens                *uint                                 `json:"max_action_tokens"`
	MaxClassifierInstructionTokens *uint                                 `json:"max_classifier_instruction_tokens"`
	ReuseParentCompaction          *bool                                 `json:"reuse_parent_compaction"`
	MaxParentCompactionTokens      *uint                                 `json:"max_parent_compaction_tokens"`
}

type codexGuardianV2TranscriptModelConfig struct {
	Sources                    []*string `json:"sources"`
	IncludeImages              *bool     `json:"include_images"`
	MaxMessageEntryTokens      *uint     `json:"max_message_entry_tokens"`
	MaxToolEntryTokens         *uint     `json:"max_tool_entry_tokens"`
	MaxMessageTranscriptTokens *uint     `json:"max_message_transcript_tokens"`
	MaxToolTranscriptTokens    *uint     `json:"max_tool_transcript_tokens"`
	MaxRecentNonUserEntries    *uint     `json:"max_recent_non_user_entries"`
}

type codexConfirmationPolicies struct {
	BrowserUse  *string `json:"browser_use"`
	ComputerUse *string `json:"computer_use"`
}

func mustDecodeCodexModels(vendoredBytes []byte) CodexModels {
	models, err := validateCodexModels(vendoredBytes)
	if err != nil {
		panic(fmt.Sprintf("decode embedded Codex models: %v", err))
	}
	return models
}

func parseCodexModels(currentBytes []byte) (CodexModels, error) {
	return decodeCodexModelEntries(currentBytes, false)
}

func validateCodexModels(currentBytes []byte) (CodexModels, error) {
	return decodeCodexModelEntries(currentBytes, true)
}

func decodeCodexModelEntries(currentBytes []byte, validateEntries bool) (CodexModels, error) {
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
		if validateEntries {
			if err := validateCodexModel(i, rawEntry, fields); err != nil {
				return nil, err
			}
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
	for _, field := range nonNullableCodexDefaultFields {
		if raw, present := fields[field]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("models[%d] has null non-optional field %q", index, field)
		}
	}

	var model codexRequiredFields
	if err := json.Unmarshal(rawEntry, &model); err != nil {
		return fmt.Errorf("decode models[%d] required fields: %w", index, err)
	}
	if model.Slug == "" || model.DisplayName == "" {
		return fmt.Errorf("models[%d] has an empty required string", index)
	}
	for levelIndex, level := range model.SupportedReasoningLevels {
		if level.Effort == "" || level.Description == nil {
			return fmt.Errorf("models[%d].supported_reasoning_levels[%d] is incomplete", index, levelIndex)
		}
	}
	switch model.ShellType {
	case "default", "local", "shell_command", "unified_exec", "disabled":
	default:
		return fmt.Errorf("models[%d] has unknown shell_type %q", index, model.ShellType)
	}
	switch model.Visibility {
	case "list", "hide", "none":
	default:
		return fmt.Errorf("models[%d] has unknown visibility %q", index, model.Visibility)
	}
	switch model.TruncationPolicy.Mode {
	case "bytes", "tokens":
	default:
		return fmt.Errorf("models[%d] has unknown truncation_policy mode %q", index, model.TruncationPolicy.Mode)
	}
	if model.TruncationPolicy.Limit == nil {
		return fmt.Errorf("models[%d] truncation_policy is missing limit", index)
	}
	if model.ExperimentalSupportedTools == nil {
		return fmt.Errorf("models[%d] has null experimental_supported_tools", index)
	}
	for toolIndex, tool := range model.ExperimentalSupportedTools {
		if tool == nil {
			return fmt.Errorf("models[%d].experimental_supported_tools[%d] is null", index, toolIndex)
		}
	}

	var known codexKnownFields
	if err := json.Unmarshal(rawEntry, &known); err != nil {
		return fmt.Errorf("decode models[%d] known fields: %w", index, err)
	}
	if err := validateKnownCodexFields(index, fields, known); err != nil {
		return err
	}
	return validateCodexInstructionSource(index, fields, known.ModelMessages)
}

func validateKnownCodexFields(index int, fields map[string]json.RawMessage, model codexKnownFields) error {
	for tierIndex, tier := range model.ServiceTiers {
		if tier.ID == nil || tier.Name == nil || tier.Description == nil {
			return fmt.Errorf("models[%d].service_tiers[%d] is incomplete", index, tierIndex)
		}
	}
	for tierIndex, tier := range model.AdditionalSpeedTiers {
		if tier == nil {
			return fmt.Errorf("models[%d].additional_speed_tiers[%d] is null", index, tierIndex)
		}
	}
	if model.AvailabilityNUX != nil && model.AvailabilityNUX.Message == nil {
		return fmt.Errorf("models[%d].availability_nux is missing message", index)
	}
	if model.Upgrade != nil && (model.Upgrade.Model == nil || model.Upgrade.MigrationMarkdown == nil) {
		return fmt.Errorf("models[%d].upgrade is incomplete", index)
	}
	if model.DefaultReasoningLevel != nil && *model.DefaultReasoningLevel == "" {
		return fmt.Errorf("models[%d] has empty default_reasoning_level", index)
	}
	if model.MultiAgentReasoningEffort != nil && *model.MultiAgentReasoningEffort == "" {
		return fmt.Errorf("models[%d] has empty multi_agent_reasoning_effort", index)
	}
	if _, present := fields["default_reasoning_summary"]; present {
		switch model.DefaultReasoningSummary {
		case "auto", "concise", "detailed", "none":
		default:
			return fmt.Errorf("models[%d] has unknown default_reasoning_summary %q", index, model.DefaultReasoningSummary)
		}
	}
	if model.DefaultVerbosity != nil {
		switch *model.DefaultVerbosity {
		case "low", "medium", "high":
		default:
			return fmt.Errorf("models[%d] has unknown default_verbosity %q", index, *model.DefaultVerbosity)
		}
	}
	if model.ApplyPatchToolType != nil && *model.ApplyPatchToolType != "freeform" {
		return fmt.Errorf("models[%d] has unknown apply_patch_tool_type %q", index, *model.ApplyPatchToolType)
	}
	if _, present := fields["web_search_tool_type"]; present {
		switch model.WebSearchToolType {
		case "text", "text_and_image":
		default:
			return fmt.Errorf("models[%d] has unknown web_search_tool_type %q", index, model.WebSearchToolType)
		}
	}
	for modalityIndex, modality := range model.InputModalities {
		if modality == nil {
			return fmt.Errorf("models[%d].input_modalities[%d] is null", index, modalityIndex)
		}
		switch *modality {
		case "text", "image", "audio":
		default:
			return fmt.Errorf("models[%d].input_modalities[%d] is unknown", index, modalityIndex)
		}
	}
	return validateCodexModelMessages(index, model.ModelMessages)
}

func validateCodexModelMessages(index int, messages *codexModelMessages) error {
	if messages == nil {
		return nil
	}
	if budget := messages.TokenBudget; budget != nil {
		if budget.ReminderThresholdTokens == nil ||
			budget.ReminderMessageTemplate == nil ||
			budget.GuidanceMessage == nil ||
			budget.AutoCompactFallbackPrompt == nil ||
			budget.AutoCompactFallbackBufferTokens == nil {
			return fmt.Errorf("models[%d].model_messages.token_budget is incomplete", index)
		}
		for field, raw := range map[string]json.RawMessage{
			"enabled":                     budget.Enabled,
			"use_history_notes_extension": budget.UseHistoryNotesExtension,
		} {
			if len(raw) == 0 {
				continue
			}
			var value bool
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return fmt.Errorf("models[%d].model_messages.token_budget.%s is not a boolean", index, field)
			}
		}
	}
	if guardian := messages.GuardianV2; guardian != nil {
		if guardian.ReasoningEffort != nil && *guardian.ReasoningEffort == "" {
			return fmt.Errorf("models[%d].model_messages.guardian_v2 has empty reasoning_effort", index)
		}
		if transcript := guardian.Transcript; transcript != nil {
			for sourceIndex, source := range transcript.Sources {
				if source == nil {
					return fmt.Errorf("models[%d].model_messages.guardian_v2.transcript.sources[%d] is null", index, sourceIndex)
				}
			}
		}
	}
	return nil
}

func validateCodexInstructionSource(index int, fields map[string]json.RawMessage, messages *codexModelMessages) error {
	var legacyBase *string
	if raw, present := fields["base_instructions"]; present {
		if err := json.Unmarshal(raw, &legacyBase); err != nil {
			return fmt.Errorf("decode models[%d] base_instructions: %w", index, err)
		}
	}

	var instructionsTemplate *string
	if messages != nil {
		instructionsTemplate = messages.InstructionsTemplate
	}
	if instructionsTemplate != nil {
		// Serde accepts Some(""), but get_model_instructions then sends an
		// empty instruction string. Keep the complete-entry semantic gate.
		if *instructionsTemplate == "" {
			return fmt.Errorf("models[%d] has empty model_messages instructions_template", index)
		}
		return nil
	}
	if legacyBase == nil {
		return fmt.Errorf("models[%d] is missing both base_instructions and model_messages instructions_template", index)
	}
	// The legacy promotion path likewise accepts Some(""), which promotes to
	// an empty canonical template and degrades the active model.
	if *legacyBase == "" {
		return fmt.Errorf("models[%d] has empty base_instructions", index)
	}
	return nil
}
