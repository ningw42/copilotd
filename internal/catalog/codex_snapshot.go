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

// codexModels is initialized once when the package loads. A panic here is
// intentional: malformed embedded data is a build/packaging defect and must
// fail startup rather than surface on a request path.
var codexModels = mustDecodeCodexModels(embeddedCodexModels)

func mustDecodeCodexModels(snapshot []byte) map[string]map[string]json.RawMessage {
	models, err := decodeCodexModels(snapshot)
	if err != nil {
		panic(fmt.Sprintf("decode embedded Codex models: %v", err))
	}
	return models
}

func decodeCodexModels(snapshot []byte) (map[string]map[string]json.RawMessage, error) {
	var envelope struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(snapshot, &envelope); err != nil {
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

	models := make(map[string]map[string]json.RawMessage, len(entries))
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
		models[slug] = fields
	}
	return models, nil
}
