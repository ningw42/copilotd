package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// CodexRenderConfig contains the reviewer and limits mutations the pure Codex
// renderer may apply. Whether to emit the Codex catalog at all is a handler
// concern.
type CodexRenderConfig struct {
	AutoReviewModel          string
	AutoReviewModelOverrides map[string]string
	OverrideLimits           bool
}

// SkippedReviewer identifies one emitted main model whose resolved reviewer
// could not safely be injected.
type SkippedReviewer struct {
	Model    string
	Reviewer string
}

// CodexRenderOutcome reports configured reviewers that could not safely be
// injected. Callers can turn each pure outcome event into one warning.
type CodexRenderOutcome struct {
	SkippedReviewers []SkippedReviewer
}

// RenderCodex intersects Responses-forwardable Copilot models with the
// embedded Codex snapshot, preserving Copilot's order. Snapshot fields are
// copied verbatim except for the explicitly configured reviewer and limits
// mutations.
func RenderCodex(forwardable []Model, cfg CodexRenderConfig) ([]byte, CodexRenderOutcome, error) {
	emitted := make(map[string]struct{}, len(forwardable))
	for _, model := range forwardable {
		if _, ok := codexModels[model.ID]; ok {
			emitted[model.ID] = struct{}{}
		}
	}

	var outcome CodexRenderOutcome

	entries := make([]map[string]json.RawMessage, 0, len(emitted))
	for _, model := range forwardable {
		snapshot, ok := codexModels[model.ID]
		if !ok {
			continue
		}

		fields := copyCodexFields(snapshot)
		// The snapshot's value is not authoritative for this deployment. Omit
		// it unless the configured reviewer is itself safe to advertise.
		delete(fields, "auto_review_model_override")
		reviewer, overridden := cfg.AutoReviewModelOverrides[model.ID]
		if !overridden {
			reviewer = cfg.AutoReviewModel
		}
		_, injectReviewer := emitted[reviewer]
		if reviewer != "" && injectReviewer {
			rawReviewer, err := json.Marshal(reviewer)
			if err != nil {
				return nil, outcome, fmt.Errorf("encode Codex reviewer: %w", err)
			}
			fields["auto_review_model_override"] = rawReviewer
		} else if reviewer != "" {
			outcome.SkippedReviewers = append(outcome.SkippedReviewers, SkippedReviewer{
				Model:    model.ID,
				Reviewer: reviewer,
			})
		}
		if cfg.OverrideLimits {
			if limit := model.Capabilities.Limits.MaxPromptTokens; limit != nil {
				fields["context_window"] = json.RawMessage(fmt.Sprintf("%d", *limit))
			}
			if limit := model.Capabilities.Limits.MaxContextWindowTokens; limit != nil {
				fields["max_context_window"] = json.RawMessage(fmt.Sprintf("%d", *limit))
			}
		}
		entries = append(entries, fields)
	}

	body, err := marshalCodexEnvelope(entries)
	if err != nil {
		return nil, outcome, err
	}
	return body, outcome, nil
}

func copyCodexFields(snapshot map[string]json.RawMessage) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage, len(snapshot))
	for field, raw := range snapshot {
		fields[field] = bytes.Clone(raw)
	}
	return fields
}

// marshalCodexEnvelope writes raw field values directly so snapshot values,
// including whitespace inside arrays and objects, remain byte-identical. Map
// keys are sorted to keep the output deterministic.
func marshalCodexEnvelope(entries []map[string]json.RawMessage) ([]byte, error) {
	var body bytes.Buffer
	body.WriteString(`{"models":[`)
	for i, entry := range entries {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteByte('{')
		fields := make([]string, 0, len(entry))
		for field := range entry {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for j, field := range fields {
			if j > 0 {
				body.WriteByte(',')
			}
			encodedField, err := json.Marshal(field)
			if err != nil {
				return nil, fmt.Errorf("encode Codex field name %q: %w", field, err)
			}
			raw := entry[field]
			if !json.Valid(raw) {
				return nil, fmt.Errorf("Codex field %q contains invalid JSON", field)
			}
			body.Write(encodedField)
			body.WriteByte(':')
			body.Write(raw)
		}
		body.WriteByte('}')
	}
	body.WriteString(`]}`)
	return body.Bytes(), nil
}
