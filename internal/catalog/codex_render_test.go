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
		if mirror.Slug == "" || decodeStringField(t, entry, "base_instructions") == "" {
			t.Errorf("models[%d] has empty slug or base_instructions", i)
		}
		if raw := bytes.TrimSpace(entry["model_messages"]); len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("{}")) {
			t.Errorf("models[%d] has empty model_messages", i)
		}
	}
}

func TestRenderCodexClonesOfficialMetadataForLiveAlias(t *testing.T) {
	const alias = "gpt-example-alias"
	models := []Model{{ID: "gpt-5.5"}, {ID: alias}, {ID: "gpt-5.4-mini"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.4"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %v, want none", outcome.SkippedReviewers)
	}

	entries := decodeRenderedCodex(t, body)
	wantSlugs := []string{"gpt-5.5", alias, "gpt-5.4-mini"}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Fatalf("rendered slugs = %q, want live order %q", got, wantSlugs)
	}
	aliased := entries[1]
	for field, want := range testCodexModels["gpt-5.4"] {
		if field == "slug" || field == "auto_review_model_override" {
			continue
		}
		assertRawFieldEqual(t, alias, field, aliased[field], want)
	}
	if _, ok := aliased["auto_review_model_override"]; ok {
		t.Error("alias retained the metadata source reviewer")
	}
}

func TestRenderCodexReportsAliasThatIsNotForwardable(t *testing.T) {
	const alias = "gpt-missing-alias"
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: "gpt-5.4"}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.4-mini"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, []string{"gpt-5.4"}) {
		t.Errorf("rendered slugs = %q, want unaffected exact entry", got)
	}
	want := []UnappliedCatalogAlias{{
		Alias: alias, MetadataSource: "gpt-5.4-mini", Reason: CatalogAliasNotForwardable,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexOfficialEntryShadowsConfiguredAlias(t *testing.T) {
	const alias = "gpt-5.4"
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: alias}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.5"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	if len(entries) != 1 {
		t.Fatalf("rendered entries = %d, want official entry", len(entries))
	}
	for field, want := range testCodexModels[alias] {
		if field == "auto_review_model_override" {
			continue
		}
		assertRawFieldEqual(t, alias, field, entries[0][field], want)
	}
	want := []UnappliedCatalogAlias{{
		Alias: alias, MetadataSource: "gpt-5.5", Reason: CatalogAliasShadowedByOfficial,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexReportsMissingMetadataSourceAndContinues(t *testing.T) {
	const (
		missingAlias = "gpt-missing-source-alias"
		validAlias   = "gpt-valid-alias"
	)
	models := []Model{{ID: missingAlias}, {ID: validAlias}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{
			missingAlias: "gpt-no-such-source",
			validAlias:   "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSlugs := []string{validAlias, "gpt-5.5"}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, wantSlugs) {
		t.Errorf("rendered slugs = %q, want unaffected models %q", got, wantSlugs)
	}
	want := []UnappliedCatalogAlias{{
		Alias: missingAlias, MetadataSource: "gpt-no-such-source", Reason: CatalogAliasMetadataSourceMissing,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexReportsEachConfiguredAliasAtMostOnce(t *testing.T) {
	const alias = "gpt-duplicate-live-alias"
	_, outcome, err := RenderCodex(testCodexModels, []Model{{ID: alias}, {ID: alias}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-no-such-source"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	want := []UnappliedCatalogAlias{{
		Alias: alias, MetadataSource: "gpt-no-such-source", Reason: CatalogAliasMetadataSourceMissing,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want one outcome %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexAliasResolutionIsSingleHop(t *testing.T) {
	const (
		firstAlias  = "gpt-first-alias"
		secondAlias = "gpt-second-alias"
	)
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: firstAlias}, {ID: secondAlias}}, CodexRenderConfig{
		ModelAliases: map[string]string{
			firstAlias:  secondAlias,
			secondAlias: "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, []string{secondAlias}) {
		t.Errorf("rendered slugs = %q, want only directly resolvable alias", got)
	}
	want := []UnappliedCatalogAlias{{
		Alias: firstAlias, MetadataSource: secondAlias, Reason: CatalogAliasMetadataSourceMissing,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexUnappliedAliasReasonsAreExclusiveAndAliasSorted(t *testing.T) {
	const (
		missingSource = "a-missing-source"
		shadowed      = "gpt-5.5"
		notForwarded  = "gpt-5.4"
	)
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: shadowed}, {ID: missingSource}}, CodexRenderConfig{
		ModelAliases: map[string]string{
			notForwarded:  "gpt-5.6-sol",
			shadowed:      "gpt-5.4-mini",
			missingSource: "gpt-no-such-source",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, []string{shadowed}) {
		t.Errorf("rendered slugs = %q, want shadowed official entry only", got)
	}
	want := []UnappliedCatalogAlias{
		{Alias: missingSource, MetadataSource: "gpt-no-such-source", Reason: CatalogAliasMetadataSourceMissing},
		{Alias: notForwarded, MetadataSource: "gpt-5.6-sol", Reason: CatalogAliasNotForwardable},
		{Alias: shadowed, MetadataSource: "gpt-5.4-mini", Reason: CatalogAliasShadowedByOfficial},
	}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want alias-sorted exclusive outcomes %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexUnconfiguredCopilotOnlyModelRemainsSilent(t *testing.T) {
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: "gpt-copilot-only"}}, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); len(got) != 0 {
		t.Errorf("rendered slugs = %q, want none", got)
	}
	if len(outcome.UnappliedAliases) != 0 {
		t.Errorf("unapplied aliases = %#v, want none for unconfigured model", outcome.UnappliedAliases)
	}
}

func TestRenderCodexAliasOrderFollowsLiveModelsAndIsMapOrderIndependent(t *testing.T) {
	models := []Model{{ID: "gpt-z-alias"}, {ID: "gpt-5.5"}, {ID: "gpt-a-alias"}}
	firstAliases := map[string]string{
		"gpt-z-alias": "gpt-5.4",
		"gpt-a-alias": "gpt-5.4-mini",
	}
	secondAliases := map[string]string{
		"gpt-a-alias": "gpt-5.4-mini",
		"gpt-z-alias": "gpt-5.4",
	}
	firstBody, firstOutcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{ModelAliases: firstAliases})
	if err != nil {
		t.Fatalf("RenderCodex first map: %v", err)
	}
	secondBody, secondOutcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{ModelAliases: secondAliases})
	if err != nil {
		t.Fatalf("RenderCodex second map: %v", err)
	}
	if !bytes.Equal(firstBody, secondBody) || !reflect.DeepEqual(firstOutcome, secondOutcome) {
		t.Errorf("map insertion order changed rendering:\nfirst %s %#v\nsecond %s %#v", firstBody, firstOutcome, secondBody, secondOutcome)
	}
	want := []string{"gpt-z-alias", "gpt-5.5", "gpt-a-alias"}
	if got := renderedSlugs(t, decodeRenderedCodex(t, firstBody)); !reflect.DeepEqual(got, want) {
		t.Errorf("rendered slugs = %q, want live order %q", got, want)
	}
}

func TestRenderCodexEmptyAliasMapPreservesExistingOutput(t *testing.T) {
	models := []Model{{ID: "gpt-5.4-mini"}, {ID: "gpt-5.4"}}
	baselineBody, baselineOutcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex baseline: %v", err)
	}
	emptyBody, emptyOutcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{ModelAliases: map[string]string{}})
	if err != nil {
		t.Fatalf("RenderCodex empty aliases: %v", err)
	}
	if !bytes.Equal(emptyBody, baselineBody) || !reflect.DeepEqual(emptyOutcome, baselineOutcome) {
		t.Errorf("empty aliases changed rendering:\nbaseline %s %#v\nempty %s %#v", baselineBody, baselineOutcome, emptyBody, emptyOutcome)
	}
}

func TestRenderCodexAliasesParticipateInCompleteReviewerMembership(t *testing.T) {
	const (
		firstAlias  = "gpt-first-review-alias"
		secondAlias = "gpt-second-review-alias"
	)
	models := []Model{{ID: firstAlias}, {ID: secondAlias}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(testCodexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{
			firstAlias:  "gpt-5.4",
			secondAlias: "gpt-5.4-mini",
		},
		AutoReviewModelOverrides: map[string]string{
			firstAlias:  firstAlias,
			secondAlias: firstAlias,
			"gpt-5.5":   secondAlias,
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.UnappliedAliases) != 0 || len(outcome.SkippedReviewers) != 0 {
		t.Errorf("outcome = %#v, want every alias and reviewer applied", outcome)
	}
	entries := decodeRenderedCodex(t, body)
	wantReviewers := []string{firstAlias, firstAlias, secondAlias}
	for i, want := range wantReviewers {
		if got := decodeStringField(t, entries[i], "auto_review_model_override"); got != want {
			t.Errorf("%s reviewer = %q, want %q", decodeStringField(t, entries[i], "slug"), got, want)
		}
	}
}

func TestRenderCodexDoesNotAdvertiseUnappliedAliasAsReviewer(t *testing.T) {
	const alias = "gpt-unapplied-review-alias"
	body, outcome, err := RenderCodex(testCodexModels, []Model{{ID: "gpt-5.4"}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.5"},
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4": alias,
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSkipped := []SkippedReviewer{{Model: "gpt-5.4", Reviewer: alias}}
	if !reflect.DeepEqual(outcome.SkippedReviewers, wantSkipped) {
		t.Errorf("skipped reviewers = %#v, want %#v", outcome.SkippedReviewers, wantSkipped)
	}
	entries := decodeRenderedCodex(t, body)
	if _, ok := entries[0]["auto_review_model_override"]; ok {
		t.Error("main model advertised an unapplied alias as its reviewer")
	}
}

func TestRenderCodexAliasRemovesSourceReviewerAndUsesAliasLiveLimits(t *testing.T) {
	const (
		alias  = "gpt-limit-alias"
		source = "gpt-5.4"
	)
	codexModels := make(CodexModels, len(testCodexModels))
	for slug, entry := range testCodexModels {
		codexModels[slug] = entry
	}
	sourceEntry := copyCodexEntry(testCodexModels[source])
	sourceEntry["auto_review_model_override"] = json.RawMessage(`"source-reviewer"`)
	codexModels[source] = sourceEntry

	aliasPromptLimit := 111
	sourcePromptLimit, sourceContextLimit := 777, 888
	models := []Model{
		{ID: source, Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &sourcePromptLimit, MaxContextWindowTokens: &sourceContextLimit}}},
		{ID: alias, Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &aliasPromptLimit}}},
	}
	offBody, _, err := RenderCodex(codexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{alias: source},
	})
	if err != nil {
		t.Fatalf("RenderCodex overlay off: %v", err)
	}
	offAlias := decodeRenderedCodex(t, offBody)[1]
	assertRawFieldEqual(t, alias, "context_window", offAlias["context_window"], sourceEntry["context_window"])
	assertRawFieldEqual(t, alias, "max_context_window", offAlias["max_context_window"], sourceEntry["max_context_window"])

	onBody, _, err := RenderCodex(codexModels, models, CodexRenderConfig{
		ModelAliases:   map[string]string{alias: source},
		OverrideLimits: true,
	})
	if err != nil {
		t.Fatalf("RenderCodex overlay on: %v", err)
	}
	onAlias := decodeRenderedCodex(t, onBody)[1]
	if _, ok := onAlias["auto_review_model_override"]; ok {
		t.Error("alias retained its metadata source reviewer")
	}
	assertJSONInt(t, onAlias, "context_window", aliasPromptLimit)
	assertRawFieldEqual(t, alias, "max_context_window", onAlias["max_context_window"], sourceEntry["max_context_window"])
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
