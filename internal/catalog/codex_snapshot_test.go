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

	gotSlugs := make([]string, 0, len(codexModels))
	for slug, fields := range codexModels {
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

func TestDecodeCodexModelsRejectsMalformedSnapshots(t *testing.T) {
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
	}

	for name, snapshot := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCodexModels([]byte(snapshot)); err == nil {
				t.Error("decodeCodexModels succeeded, want packaging-defect error")
			}
		})
	}
}

func TestMustDecodeCodexModelsPanicsOnDecodeFailure(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("mustDecodeCodexModels returned, want startup panic")
		}
		if message := got.(string); !strings.Contains(message, "decode embedded Codex models") {
			t.Errorf("panic = %q, want embedded-snapshot context", message)
		}
	}()

	mustDecodeCodexModels([]byte(`{"models":null}`))
}
