package catalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestRenderCodexIntersectsInLiveOrderAndEmitsCompleteEntries(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	// The catalog omits most live models and adds a Codex-only entry, so only
	// the intersection may be emitted, in Copilot's order.
	codexModels := codexModelsWithSlugs(t, "gpt-5.6-terra", "codex-auto-review", "gpt-5.4", "gpt-5.5")
	body, outcome, err := RenderCodex(codexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %v, want none", outcome.SkippedReviewers)
	}

	entries := decodeRenderedCodex(t, body)
	wantSlugs := []string{"gpt-5.4", "gpt-5.5", "gpt-5.6-terra"}
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
		var messages map[string]json.RawMessage
		if err := json.Unmarshal(entry["model_messages"], &messages); err != nil {
			t.Fatalf("decode models[%d].model_messages: %v", i, err)
		}
		if mirror.Slug == "" || decodeStringField(t, messages, "instructions_template") == "" {
			t.Errorf("models[%d] has empty slug or canonical instructions_template", i)
		}
	}
}

func TestRenderCodexClonesOfficialMetadataForLiveAlias(t *testing.T) {
	const alias = "gpt-example-alias"
	codexModels := syntheticCodexModels(t,
		completeCodexEntry("gpt-5.4", map[string]any{"auto_review_model_override": "source-reviewer"}),
		completeCodexEntry("gpt-5.5", nil),
		completeCodexEntry("gpt-5.6-luna", nil),
	)
	models := []Model{{ID: "gpt-5.5"}, {ID: alias}, {ID: "gpt-5.6-luna"}}
	body, outcome, err := RenderCodex(codexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.4"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.SkippedReviewers) != 0 {
		t.Errorf("skipped reviewers = %v, want none", outcome.SkippedReviewers)
	}

	entries := decodeRenderedCodex(t, body)
	wantSlugs := []string{"gpt-5.5", alias, "gpt-5.6-luna"}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Fatalf("rendered slugs = %q, want live order %q", got, wantSlugs)
	}
	baselineBody, _, err := RenderCodex(codexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex baseline: %v", err)
	}
	baselineEntries := decodeRenderedCodex(t, baselineBody)
	if len(baselineEntries) != 2 || !reflect.DeepEqual(entries[0], baselineEntries[0]) || !reflect.DeepEqual(entries[2], baselineEntries[1]) {
		t.Errorf("alias configuration changed unrelated exact entries:\nwith alias: %#v\nbaseline: %#v", entries, baselineEntries)
	}
	aliased := entries[1]
	for field, want := range codexModels["gpt-5.4"] {
		if field == "slug" || field == "auto_review_model_override" {
			continue
		}
		assertRawFieldEqual(t, alias, field, aliased[field], want)
	}
	if _, ok := aliased["auto_review_model_override"]; ok {
		t.Error("alias retained the metadata source reviewer")
	}
}

func TestRenderCodexRejectsInvalidRawMetadataSourceField(t *testing.T) {
	const alias = "gpt-invalid-raw-alias"
	codexModels := CodexModels{
		"gpt-source": {
			"slug":   json.RawMessage(`"gpt-source"`),
			"future": json.RawMessage(`{`),
		},
	}
	_, _, err := RenderCodex(codexModels, []Model{{ID: alias}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-source"},
	})
	if err == nil || !strings.Contains(err.Error(), `Codex field "future" contains invalid JSON`) {
		t.Fatalf("RenderCodex error = %v, want invalid raw source field rejected", err)
	}
}

func TestRenderCodexReportsAliasThatIsNotForwardable(t *testing.T) {
	const alias = "gpt-missing-alias"
	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.6-luna")
	body, outcome, err := RenderCodex(codexModels, []Model{{ID: "gpt-5.4"}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-5.6-luna"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, []string{"gpt-5.4"}) {
		t.Errorf("rendered slugs = %q, want unaffected exact entry", got)
	}
	want := []UnappliedCodexAlias{{
		Alias: alias, MetadataSource: "gpt-5.6-luna", Reason: CodexAliasNotForwardable,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexOfficialEntryShadowsConfiguredAlias(t *testing.T) {
	const alias = "gpt-5.4"
	promptLimit, contextLimit := 111, 222
	models := []Model{{
		ID: alias,
		Capabilities: Capabilities{Limits: Limits{
			MaxPromptTokens:        &promptLimit,
			MaxContextWindowTokens: &contextLimit,
		}},
	}}
	codexModels := syntheticCodexModels(t,
		completeCodexEntry(alias, map[string]any{
			"auto_review_model_override": "codex-reviewer",
			"context_window":             272000,
			"max_context_window":         1000000,
		}),
		completeCodexEntry("gpt-5.5", nil),
	)
	body, outcome, err := RenderCodex(codexModels, models, CodexRenderConfig{
		ModelAliases:    map[string]string{alias: "gpt-5.5"},
		AutoReviewModel: alias,
		OverrideLimits:  true,
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	if len(entries) != 1 {
		t.Fatalf("rendered entries = %d, want official entry", len(entries))
	}
	for field, want := range codexModels[alias] {
		switch field {
		case "auto_review_model_override", "context_window", "max_context_window":
			continue
		}
		assertRawFieldEqual(t, alias, field, entries[0][field], want)
	}
	if got := decodeStringField(t, entries[0], "auto_review_model_override"); got != alias {
		t.Errorf("shadowed official reviewer = %q, want ordinary reviewer mutation %q", got, alias)
	}
	assertJSONInt(t, entries[0], "context_window", promptLimit)
	assertJSONInt(t, entries[0], "max_context_window", contextLimit)
	want := []UnappliedCodexAlias{{
		Alias: alias, MetadataSource: "gpt-5.5", Reason: CodexAliasShadowedByOfficial,
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
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5"), models, CodexRenderConfig{
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
	want := []UnappliedCodexAlias{{
		Alias: missingAlias, MetadataSource: "gpt-no-such-source", Reason: CodexAliasMetadataSourceMissing,
	}}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexReportsEachConfiguredAliasAtMostOnce(t *testing.T) {
	const alias = "gpt-duplicate-live-alias"
	_, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4"), []Model{{ID: alias}, {ID: alias}}, CodexRenderConfig{
		ModelAliases: map[string]string{alias: "gpt-no-such-source"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	want := []UnappliedCodexAlias{{
		Alias: alias, MetadataSource: "gpt-no-such-source", Reason: CodexAliasMetadataSourceMissing,
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
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4"), []Model{{ID: firstAlias}, {ID: secondAlias}}, CodexRenderConfig{
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
	want := []UnappliedCodexAlias{{
		Alias: firstAlias, MetadataSource: secondAlias, Reason: CodexAliasMetadataSourceMissing,
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
	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol")
	body, outcome, err := RenderCodex(codexModels, []Model{{ID: shadowed}, {ID: missingSource}}, CodexRenderConfig{
		ModelAliases: map[string]string{
			notForwarded:  "gpt-5.6-sol",
			shadowed:      "gpt-5.6-luna",
			missingSource: "gpt-no-such-source",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if got := renderedSlugs(t, decodeRenderedCodex(t, body)); !reflect.DeepEqual(got, []string{shadowed}) {
		t.Errorf("rendered slugs = %q, want shadowed official entry only", got)
	}
	want := []UnappliedCodexAlias{
		{Alias: missingSource, MetadataSource: "gpt-no-such-source", Reason: CodexAliasMetadataSourceMissing},
		{Alias: notForwarded, MetadataSource: "gpt-5.6-sol", Reason: CodexAliasNotForwardable},
		{Alias: shadowed, MetadataSource: "gpt-5.6-luna", Reason: CodexAliasShadowedByOfficial},
	}
	if !reflect.DeepEqual(outcome.UnappliedAliases, want) {
		t.Errorf("unapplied aliases = %#v, want alias-sorted exclusive outcomes %#v", outcome.UnappliedAliases, want)
	}
}

func TestRenderCodexUnconfiguredCopilotOnlyModelRemainsSilent(t *testing.T) {
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4"), []Model{{ID: "gpt-copilot-only"}}, CodexRenderConfig{})
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
		"gpt-a-alias": "gpt-5.6-luna",
	}
	secondAliases := map[string]string{
		"gpt-a-alias": "gpt-5.6-luna",
		"gpt-z-alias": "gpt-5.4",
	}
	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna")
	firstBody, firstOutcome, err := RenderCodex(codexModels, models, CodexRenderConfig{ModelAliases: firstAliases})
	if err != nil {
		t.Fatalf("RenderCodex first map: %v", err)
	}
	secondBody, secondOutcome, err := RenderCodex(codexModels, models, CodexRenderConfig{ModelAliases: secondAliases})
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
	models := []Model{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.4"}}
	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.6-luna")
	baselineBody, baselineOutcome, err := RenderCodex(codexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex baseline: %v", err)
	}
	emptyBody, emptyOutcome, err := RenderCodex(codexModels, models, CodexRenderConfig{ModelAliases: map[string]string{}})
	if err != nil {
		t.Fatalf("RenderCodex empty aliases: %v", err)
	}
	if !bytes.Equal(emptyBody, baselineBody) || !reflect.DeepEqual(emptyOutcome, baselineOutcome) {
		t.Errorf("empty aliases changed rendering:\nbaseline %s %#v\nempty %s %#v", baselineBody, baselineOutcome, emptyBody, emptyOutcome)
	}
}

func TestRenderCodexAliasOverrideBeatsGlobalAndOthersUseGlobal(t *testing.T) {
	const (
		alias    = "gpt-example-alias"
		source   = "gpt-5.4"
		reviewer = "gpt-5.5"
	)
	models := []Model{{ID: alias}, {ID: source}, {ID: reviewer}}
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, source, reviewer), models, CodexRenderConfig{
		ModelAliases:    map[string]string{alias: source},
		AutoReviewModel: reviewer,
		AutoReviewModelOverrides: map[string]string{
			alias: alias,
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	if len(outcome.UnappliedAliases) != 0 || len(outcome.SkippedReviewers) != 0 {
		t.Errorf("outcome = %#v, want all aliases and reviewers applied", outcome)
	}
	entries := decodeRenderedCodex(t, body)
	wantReviewers := []string{alias, reviewer, reviewer}
	for i, want := range wantReviewers {
		if got := decodeStringField(t, entries[i], "auto_review_model_override"); got != want {
			t.Errorf("%s reviewer = %q, want %q", decodeStringField(t, entries[i], "slug"), got, want)
		}
	}
}

func TestRenderCodexAliasesParticipateInCompleteReviewerMembership(t *testing.T) {
	const (
		firstAlias  = "gpt-first-review-alias"
		secondAlias = "gpt-second-review-alias"
	)
	models := []Model{{ID: firstAlias}, {ID: secondAlias}, {ID: "gpt-5.5"}}
	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna")
	body, outcome, err := RenderCodex(codexModels, models, CodexRenderConfig{
		ModelAliases: map[string]string{
			firstAlias:  "gpt-5.4",
			secondAlias: "gpt-5.6-luna",
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
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5"), []Model{{ID: "gpt-5.4"}}, CodexRenderConfig{
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
	codexModels := syntheticCodexModels(t, completeCodexEntry(source, map[string]any{
		"auto_review_model_override": "source-reviewer",
		"context_window":             272000,
		"max_context_window":         1000000,
	}))
	sourceEntry := codexModels[source]

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
	codexModels := syntheticCodexModels(t,
		completeCodexEntry("gpt-5.4", map[string]any{"auto_review_model_override": "codex-reviewer"}),
		completeCodexEntry("gpt-5.5", map[string]any{"auto_review_model_override": "codex-reviewer"}),
		completeCodexEntry("gpt-5.6-luna", nil),
		completeCodexEntry("codex-auto-review", nil),
	)
	body, _, err := RenderCodex(codexModels, models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{"gpt-5.4": "gpt-5.6-luna"},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	if got, want := renderedSlugs(t, entries), []string{"gpt-5.4", "gpt-5.5", "gpt-5.6-luna"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered slugs = %q, want %q", got, want)
	}
	for _, entry := range entries {
		var slug string
		if err := json.Unmarshal(entry["slug"], &slug); err != nil {
			t.Fatalf("decode rendered slug: %v", err)
		}
		for field, want := range codexModels[slug] {
			if field == "auto_review_model_override" {
				continue
			}
			if got := entry[field]; !bytes.Equal(got, want) {
				t.Errorf("%s.%s changed:\n got: %s\nwant: %s", slug, field, got, want)
			}
		}
		rawReviewer, hasReviewer := entry["auto_review_model_override"]
		if slug == "gpt-5.4" {
			if got := decodeStringField(t, entry, "auto_review_model_override"); got != "gpt-5.6-luna" {
				t.Errorf("%s reviewer = %q, want gpt-5.6-luna", slug, got)
			}
		} else if hasReviewer {
			t.Errorf("%s retained auto_review_model_override without a reviewer: %s", slug, rawReviewer)
		}
	}

	copy := copyCodexEntry(codexModels["gpt-5.4"])
	copy["slug"][0] = 'x'
	if bytes.Equal(copy["slug"], codexModels["gpt-5.4"]["slug"]) {
		t.Error("copyCodexEntry retained a RawMessage alias into the decoded Codex catalog")
	}
}

func TestRenderCodexInjectsOnlyAnEmittedReviewer(t *testing.T) {
	models := Filter(capturedModels(t), endpoint.RouteOpenAIResponses)
	codexModels := syntheticCodexModels(t,
		completeCodexEntry("gpt-5.4", map[string]any{"auto_review_model_override": "codex-auto-review"}),
		completeCodexEntry("gpt-5.6-luna", nil),
		completeCodexEntry("codex-auto-review", nil),
	)
	tests := []struct {
		name      string
		reviewer  string
		wantValue string
		wantSkips bool
	}{
		{name: "empty reviewer"},
		{name: "emitted reviewer overwrites Codex value", reviewer: "gpt-5.6-luna", wantValue: "gpt-5.6-luna"},
		{name: "Codex-only reviewer is skipped", reviewer: "codex-auto-review", wantSkips: true},
		{name: "Copilot-only reviewer is skipped", reviewer: "gpt-5.3-codex", wantSkips: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, outcome, err := RenderCodex(codexModels, models, CodexRenderConfig{AutoReviewModel: tc.reviewer})
			if err != nil {
				t.Fatalf("RenderCodex: %v", err)
			}
			entries := decodeRenderedCodex(t, body)
			if got, want := renderedSlugs(t, entries), []string{"gpt-5.4", "gpt-5.6-luna"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("rendered slugs = %q, want %q", got, want)
			}
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
	models := []Model{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna"), models, CodexRenderConfig{
		AutoReviewModel: "gpt-5.5",
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.6-luna": "gpt-5.4",
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
		t.Errorf("gpt-5.6-luna reviewer = %q, want per-model gpt-5.4", got)
	}
	for _, entry := range entries[1:] {
		if got := decodeStringField(t, entry, "auto_review_model_override"); got != "gpt-5.5" {
			t.Errorf("%s reviewer = %q, want global gpt-5.5", decodeStringField(t, entry, "slug"), got)
		}
	}
}

func TestRenderCodexResolvesReviewerOverridesSingleHop(t *testing.T) {
	models := []Model{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, _, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna"), models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.6-luna": "gpt-5.4",
			"gpt-5.4":      "gpt-5.5",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}

	entries := decodeRenderedCodex(t, body)
	if got := decodeStringField(t, entries[0], "auto_review_model_override"); got != "gpt-5.4" {
		t.Errorf("gpt-5.6-luna reviewer = %q, want single-hop gpt-5.4", got)
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
		{ID: "gpt-5.6-luna", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &limit}}},
		{ID: "gpt-5.4"},
		{ID: "gpt-5.5"},
	}
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna"), models, CodexRenderConfig{
		AutoReviewModel: "gpt-5.5",
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.6-luna": missingReviewer,
		},
		OverrideLimits: true,
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSkipped := []SkippedReviewer{{Model: "gpt-5.6-luna", Reviewer: missingReviewer}}
	if !reflect.DeepEqual(outcome.SkippedReviewers, wantSkipped) {
		t.Errorf("skipped reviewers = %#v, want %#v", outcome.SkippedReviewers, wantSkipped)
	}

	entries := decodeRenderedCodex(t, body)
	if got := renderedSlugs(t, entries); !contains(got, "gpt-5.6-luna") {
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
	models := []Model{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.4"}, {ID: "gpt-5.5"}}
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna"), models, CodexRenderConfig{
		AutoReviewModel: missingReviewer,
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.4": "gpt-5.6-luna",
		},
	})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	wantSkipped := []SkippedReviewer{
		{Model: "gpt-5.6-luna", Reviewer: missingReviewer},
		{Model: "gpt-5.5", Reviewer: missingReviewer},
	}
	if !reflect.DeepEqual(outcome.SkippedReviewers, wantSkipped) {
		t.Errorf("skipped reviewers = %#v, want emission-ordered %#v", outcome.SkippedReviewers, wantSkipped)
	}

	entries := decodeRenderedCodex(t, body)
	if _, ok := entries[0]["auto_review_model_override"]; ok {
		t.Error("gpt-5.6-luna injected an unforwardable global reviewer")
	}
	if got := decodeStringField(t, entries[1], "auto_review_model_override"); got != "gpt-5.6-luna" {
		t.Errorf("gpt-5.4 reviewer = %q, want valid explicit reviewer", got)
	}
	if _, ok := entries[2]["auto_review_model_override"]; ok {
		t.Error("gpt-5.5 injected an unforwardable global reviewer")
	}
}

func TestRenderCodexIgnoresNonAdvertisedAndMiscasedOverrideKeys(t *testing.T) {
	models := []Model{{ID: "gpt-5.6-luna"}, {ID: "gpt-5.4"}}
	body, outcome, err := RenderCodex(codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna"), models, CodexRenderConfig{
		AutoReviewModelOverrides: map[string]string{
			"gpt-5.5":      "missing-reviewer",
			"GPT-5.6-LUNA": "missing-reviewer",
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

	codexModels := codexModelsWithSlugs(t, "gpt-5.4", "gpt-5.5", "gpt-5.6-luna")
	body, outcome, err := RenderCodex(codexModels, withoutReviewer, CodexRenderConfig{AutoReviewModel: "gpt-5.4"})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	slugs := renderedSlugs(t, entries)
	if want := []string{"gpt-5.5", "gpt-5.6-luna"}; !reflect.DeepEqual(slugs, want) {
		t.Fatalf("rendered slugs = %q, want %q", slugs, want)
	}
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
		{ID: "gpt-5.6-luna", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &promptOnly}}},
		{ID: "gpt-5.4", Capabilities: Capabilities{Limits: Limits{MaxContextWindowTokens: &contextOnly}}},
		{ID: "gpt-5.5", Capabilities: Capabilities{Limits: Limits{MaxPromptTokens: &both, MaxContextWindowTokens: &both}}},
	}
	codexModels := syntheticCodexModels(t,
		completeCodexEntry("gpt-5.6-luna", map[string]any{"context_window": 100000, "max_context_window": 200000}),
		completeCodexEntry("gpt-5.4", map[string]any{"context_window": 110000, "max_context_window": 210000}),
		completeCodexEntry("gpt-5.5", map[string]any{"context_window": 120000, "max_context_window": 220000}),
	)

	offBody, _, err := RenderCodex(codexModels, models, CodexRenderConfig{})
	if err != nil {
		t.Fatalf("RenderCodex with overlay off: %v", err)
	}
	for _, entry := range decodeRenderedCodex(t, offBody) {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "context_window", entry["context_window"], codexModels[slug]["context_window"])
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], codexModels[slug]["max_context_window"])
	}

	onBody, _, err := RenderCodex(codexModels, models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex with overlay on: %v", err)
	}
	entries := decodeRenderedCodex(t, onBody)
	assertJSONInt(t, entries[0], "context_window", promptOnly)
	assertRawFieldEqual(t, "gpt-5.6-luna", "max_context_window", entries[0]["max_context_window"], codexModels["gpt-5.6-luna"]["max_context_window"])
	assertRawFieldEqual(t, "gpt-5.4", "context_window", entries[1]["context_window"], codexModels["gpt-5.4"]["context_window"])
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

	codexModels := syntheticCodexModels(t,
		completeCodexEntry("gpt-5.4", map[string]any{"context_window": 110000, "max_context_window": 210000}),
		completeCodexEntry("gpt-5.6-sol", map[string]any{"context_window": 130000, "max_context_window": 230000}),
	)
	body, _, err := RenderCodex(codexModels, models, CodexRenderConfig{OverrideLimits: true})
	if err != nil {
		t.Fatalf("RenderCodex: %v", err)
	}
	entries := decodeRenderedCodex(t, body)
	if got, want := renderedSlugs(t, entries), []string{"gpt-5.4", "gpt-5.6-sol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered slugs = %q, want %q", got, want)
	}
	for _, entry := range entries {
		slug := decodeStringField(t, entry, "slug")
		assertRawFieldEqual(t, slug, "max_context_window", entry["max_context_window"], codexModels[slug]["max_context_window"])
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

// syntheticCodexModels decodes complete synthetic entries through the accept contract
// the vendored snapshot must pass. The bytes are indented like upstream's
// models.json, so raw values carry interior whitespace the renderer must keep.
func syntheticCodexModels(t *testing.T, entries ...map[string]any) CodexModels {
	t.Helper()
	encoded, err := json.MarshalIndent(map[string]any{"models": entries}, "", "  ")
	if err != nil {
		t.Fatalf("encode synthetic Codex models: %v", err)
	}
	codexModels, err := validateCodexModels(encoded)
	if err != nil {
		t.Fatalf("synthetic Codex models are not complete: %v", err)
	}
	return codexModels
}

// codexModelsWithSlugs is syntheticCodexModels for tests that need only entry
// membership.
func codexModelsWithSlugs(t *testing.T, slugs ...string) CodexModels {
	t.Helper()
	return syntheticCodexModels(t, completeCodexEntries(slugs...)...)
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
		t.Errorf("%s.%s = %s, want Codex entry value %s", slug, field, got, want)
	}
}

func assertJSONInt(t *testing.T, entry map[string]json.RawMessage, field string, want int) {
	t.Helper()
	var got int
	if err := json.Unmarshal(entry[field], &got); err != nil || got != want {
		t.Errorf("%s = %s (%v), want %d", field, entry[field], err, want)
	}
}
