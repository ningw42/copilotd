// Package catalog decodes GitHub Copilot's model catalog and renders the
// provider-shaped catalogs exposed by copilotd's inference Surfaces.
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// Model contains only the Copilot model fields that provider-shaped catalogs
// can honestly map. Pointer fields preserve the distinction between an absent
// capability and an explicitly advertised false or zero value.
type Model struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	Vendor             string           `json:"vendor"`
	ModelPickerEnabled bool             `json:"model_picker_enabled"`
	SupportedRoutes    []endpoint.Route `json:"supported_endpoints"`
	Capabilities       Capabilities     `json:"capabilities"`
}

// Capabilities is the relevant subset of Copilot's model capabilities.
type Capabilities struct {
	Limits   Limits   `json:"limits"`
	Supports Supports `json:"supports"`
}

// Limits is the relevant subset of Copilot's model limits.
type Limits struct {
	MaxPromptTokens        *int          `json:"max_prompt_tokens"`
	MaxOutputTokens        *int          `json:"max_output_tokens"`
	MaxContextWindowTokens *int          `json:"max_context_window_tokens"`
	Vision                 *VisionLimits `json:"vision"`
}

// VisionLimits contains the source signal used for Anthropic PDF input.
type VisionLimits struct {
	SupportedMediaTypes []string `json:"supported_media_types"`
}

// Supports is the relevant subset of Copilot's advertised features.
type Supports struct {
	StructuredOutputs *bool    `json:"structured_outputs"`
	Vision            *bool    `json:"vision"`
	ReasoningEffort   []string `json:"reasoning_effort"`
	AdaptiveThinking  *bool    `json:"adaptive_thinking"`
	MinThinkingBudget *int     `json:"min_thinking_budget"`
	MaxThinkingBudget *int     `json:"max_thinking_budget"`
}

// Decode reads Copilot's {"data":[...]} model envelope. Unknown fields are
// intentionally ignored so catalog shaping is insulated from unrelated
// Copilot metadata.
func Decode(body []byte) ([]Model, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	data := bytes.TrimSpace(envelope.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, fmt.Errorf("catalog data array is missing or null")
	}
	var encodedModels []json.RawMessage
	if err := json.Unmarshal(data, &encodedModels); err != nil {
		return nil, fmt.Errorf("decode catalog data array: %w", err)
	}
	models := make([]Model, len(encodedModels))
	for i, encodedModel := range encodedModels {
		if bytes.Equal(bytes.TrimSpace(encodedModel), []byte("null")) {
			return nil, fmt.Errorf("catalog data[%d] is null", i)
		}
		if err := json.Unmarshal(encodedModel, &models[i]); err != nil {
			return nil, fmt.Errorf("decode catalog data[%d]: %w", i, err)
		}
	}
	return models, nil
}

// Filter selects picker-visible models that advertise the exact upstream
// Route required by a Surface. Input order is preserved.
func Filter(models []Model, requiredRoute endpoint.Route) []Model {
	filtered := make([]Model, 0, len(models))
	for _, model := range models {
		if !model.ModelPickerEnabled || !contains(model.SupportedRoutes, requiredRoute) {
			continue
		}
		filtered = append(filtered, model)
	}
	return filtered
}

// RenderOpenAI renders Copilot models in OpenAI's unpaginated model-list
// schema. Copilot supplies no creation time, so created uses the documented
// Phase 6a epoch stub.
func RenderOpenAI(models []Model) ([]byte, error) {
	type openAIModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	envelope := struct {
		Object string        `json:"object"`
		Data   []openAIModel `json:"data"`
	}{
		Object: "list",
		Data:   make([]openAIModel, 0, len(models)),
	}
	for _, model := range models {
		if model.ID == "" || model.Vendor == "" {
			return nil, fmt.Errorf("OpenAI catalog model is missing id or vendor")
		}
		envelope.Data = append(envelope.Data, openAIModel{
			ID: model.ID, Object: "model", Created: 0, OwnedBy: model.Vendor,
		})
	}
	return json.Marshal(envelope)
}

type supportedCapability struct {
	Supported bool `json:"supported"`
}

type effortCapability struct {
	Supported bool                `json:"supported"`
	Low       supportedCapability `json:"low"`
	Medium    supportedCapability `json:"medium"`
	High      supportedCapability `json:"high"`
	XHigh     supportedCapability `json:"xhigh"`
	Max       supportedCapability `json:"max"`
}

type thinkingCapability struct {
	Supported bool `json:"supported"`
	Types     struct {
		Adaptive supportedCapability `json:"adaptive"`
		Enabled  supportedCapability `json:"enabled"`
	} `json:"types"`
}

type anthropicCapabilities struct {
	StructuredOutputs *supportedCapability `json:"structured_outputs,omitempty"`
	ImageInput        *supportedCapability `json:"image_input,omitempty"`
	PDFInput          *supportedCapability `json:"pdf_input,omitempty"`
	Effort            *effortCapability    `json:"effort,omitempty"`
	Thinking          *thinkingCapability  `json:"thinking,omitempty"`
}

// RenderAnthropic renders Copilot models in Anthropic's model-list schema,
// enriching only the capability claims for which Copilot supplies a signal.
func RenderAnthropic(models []Model) ([]byte, error) {
	type anthropicModel struct {
		ID             string                `json:"id"`
		Type           string                `json:"type"`
		DisplayName    string                `json:"display_name"`
		CreatedAt      string                `json:"created_at"`
		MaxInputTokens *int                  `json:"max_input_tokens,omitempty"`
		MaxTokens      *int                  `json:"max_tokens,omitempty"`
		Capabilities   anthropicCapabilities `json:"capabilities"`
	}
	envelope := struct {
		Data    []anthropicModel `json:"data"`
		HasMore bool             `json:"has_more"`
		FirstID *string          `json:"first_id"`
		LastID  *string          `json:"last_id"`
	}{Data: make([]anthropicModel, 0, len(models))}

	for _, model := range models {
		if model.ID == "" || model.Name == "" {
			return nil, fmt.Errorf("Anthropic catalog model is missing id or name")
		}
		envelope.Data = append(envelope.Data, anthropicModel{
			ID:             model.ID,
			Type:           "model",
			DisplayName:    model.Name,
			CreatedAt:      "1970-01-01T00:00:00Z",
			MaxInputTokens: model.Capabilities.Limits.MaxPromptTokens,
			MaxTokens:      model.Capabilities.Limits.MaxOutputTokens,
			Capabilities:   renderAnthropicCapabilities(model.Capabilities),
		})
	}
	if len(models) > 0 {
		envelope.FirstID = &models[0].ID
		envelope.LastID = &models[len(models)-1].ID
	}
	return json.Marshal(envelope)
}

func renderAnthropicCapabilities(capabilities Capabilities) anthropicCapabilities {
	var rendered anthropicCapabilities
	supports := capabilities.Supports
	if supports.StructuredOutputs != nil {
		rendered.StructuredOutputs = &supportedCapability{Supported: *supports.StructuredOutputs}
	}
	if supports.Vision != nil {
		rendered.ImageInput = &supportedCapability{Supported: *supports.Vision}
	}
	if capabilities.Limits.Vision != nil {
		rendered.PDFInput = &supportedCapability{
			Supported: contains(capabilities.Limits.Vision.SupportedMediaTypes, "application/pdf"),
		}
	}
	if len(supports.ReasoningEffort) > 0 {
		rendered.Effort = &effortCapability{
			Supported: true,
			Low:       supportedCapability{Supported: contains(supports.ReasoningEffort, "low")},
			Medium:    supportedCapability{Supported: contains(supports.ReasoningEffort, "medium")},
			High:      supportedCapability{Supported: contains(supports.ReasoningEffort, "high")},
			XHigh:     supportedCapability{Supported: contains(supports.ReasoningEffort, "xhigh")},
			Max:       supportedCapability{Supported: contains(supports.ReasoningEffort, "max")},
		}
	}
	if supports.AdaptiveThinking != nil || supports.MinThinkingBudget != nil || supports.MaxThinkingBudget != nil {
		adaptive := supports.AdaptiveThinking != nil && *supports.AdaptiveThinking
		enabled := supports.MinThinkingBudget != nil || supports.MaxThinkingBudget != nil
		thinking := &thinkingCapability{Supported: adaptive || enabled}
		thinking.Types.Adaptive.Supported = adaptive
		thinking.Types.Enabled.Supported = enabled
		rendered.Thinking = thinking
	}
	return rendered
}

func contains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
