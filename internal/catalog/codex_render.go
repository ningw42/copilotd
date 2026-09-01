package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// CodexRenderConfig contains the aliases, reviewer routing, and limits policy
// the pure Codex renderer may apply. Whether to emit the Codex catalog at all
// is a handler concern.
type CodexRenderConfig struct {
	// ModelAliases maps a live Copilot model ID to the exact official Codex
	// entry that supplies its complete metadata.
	ModelAliases    map[string]string
	AutoReviewModel string
	// AutoReviewModelOverrides must contain non-empty reviewer slugs from
	// validated configuration. A present main-model key is authoritative, so
	// RenderCodex does not fall back to AutoReviewModel for that model.
	AutoReviewModelOverrides map[string]string
	OverrideLimits           bool
}

// SkippedReviewer identifies one emitted main model whose resolved reviewer
// could not safely be injected.
type SkippedReviewer struct {
	Model    string
	Reviewer string
}

// CatalogAliasSkipReason identifies why a configured catalog alias mapping was
// not applied.
type CatalogAliasSkipReason string

const (
	CatalogAliasNotForwardable        CatalogAliasSkipReason = "alias_not_forwardable"
	CatalogAliasShadowedByOfficial    CatalogAliasSkipReason = "shadowed_by_official"
	CatalogAliasMetadataSourceMissing CatalogAliasSkipReason = "metadata_source_missing"
)

// UnappliedCatalogAlias identifies one configured alias mapping that had no
// effect on alias resolution.
type UnappliedCatalogAlias struct {
	Alias  string
	Source string
	Reason CatalogAliasSkipReason
}

// CodexRenderOutcome reports configured mappings and reviewers that could not
// safely be applied. Callers can turn each pure outcome event into one warning.
type CodexRenderOutcome struct {
	UnappliedAliases []UnappliedCatalogAlias
	SkippedReviewers []SkippedReviewer
}

// RenderCodex resolves complete official metadata for Responses-forwardable
// Copilot models, preserving Copilot's order. Codex entry fields are copied
// verbatim except for the served alias slug and the explicitly configured
// reviewer and limits mutations.
func RenderCodex(codexModels CodexModels, forwardable []Model, cfg CodexRenderConfig) ([]byte, CodexRenderOutcome, error) {
	var outcome CodexRenderOutcome
	forwardableByID := make(map[string]struct{}, len(forwardable))
	for _, model := range forwardable {
		forwardableByID[model.ID] = struct{}{}
	}
	aliases := make([]string, 0, len(cfg.ModelAliases))
	for alias := range cfg.ModelAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if _, ok := forwardableByID[alias]; !ok {
			outcome.UnappliedAliases = append(outcome.UnappliedAliases, UnappliedCatalogAlias{
				Alias: alias, Source: cfg.ModelAliases[alias], Reason: CatalogAliasNotForwardable,
			})
			continue
		}
		if _, ok := codexModels[alias]; ok {
			outcome.UnappliedAliases = append(outcome.UnappliedAliases, UnappliedCatalogAlias{
				Alias: alias, Source: cfg.ModelAliases[alias], Reason: CatalogAliasShadowedByOfficial,
			})
			continue
		}
		if _, ok := codexModels[cfg.ModelAliases[alias]]; !ok {
			outcome.UnappliedAliases = append(outcome.UnappliedAliases, UnappliedCatalogAlias{
				Alias: alias, Source: cfg.ModelAliases[alias], Reason: CatalogAliasMetadataSourceMissing,
			})
		}
	}

	type resolvedEntry struct {
		fields  map[string]json.RawMessage
		aliased bool
	}
	resolved := make(map[string]resolvedEntry, len(forwardable))
	emitted := make(map[string]struct{}, len(forwardable))
	for _, model := range forwardable {
		entry, ok := codexModels[model.ID]
		aliased := false
		if !ok {
			if source, configured := cfg.ModelAliases[model.ID]; configured {
				entry, ok = codexModels[source]
				aliased = ok
			}
		}
		if ok {
			resolved[model.ID] = resolvedEntry{fields: entry, aliased: aliased}
			emitted[model.ID] = struct{}{}
		}
	}
	sort.Slice(outcome.UnappliedAliases, func(i, j int) bool {
		return outcome.UnappliedAliases[i].Alias < outcome.UnappliedAliases[j].Alias
	})

	entries := make([]map[string]json.RawMessage, 0, len(emitted))
	for _, model := range forwardable {
		resolvedEntry, ok := resolved[model.ID]
		if !ok {
			continue
		}

		fields := copyCodexEntry(resolvedEntry.fields)
		if resolvedEntry.aliased {
			rawAlias, err := json.Marshal(model.ID)
			if err != nil {
				return nil, outcome, fmt.Errorf("encode Codex catalog alias: %w", err)
			}
			fields["slug"] = rawAlias
		}
		// The Codex entry's value is not authoritative for this deployment. Omit
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

func copyCodexEntry(entry map[string]json.RawMessage) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage, len(entry))
	for field, raw := range entry {
		fields[field] = bytes.Clone(raw)
	}
	return fields
}

// marshalCodexEnvelope writes raw field values directly so current Codex values,
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
