package report_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
)

type fixedPricingSource struct {
	snapshot *pricing.Snapshot
	status   pricing.SnapshotStatus
}

type pricingSourceFunc func(context.Context, pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error)

func (fn pricingSourceFunc) Current(ctx context.Context, limit pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
	return fn(ctx, limit)
}

type mutablePricingSource struct {
	mu      sync.Mutex
	current *fixedPricingSource
	calls   int
}

func (s *mutablePricingSource) Current(ctx context.Context, limit pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.current.Current(ctx, limit)
}

func (s *mutablePricingSource) set(current *fixedPricingSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = current
}

func (s *mutablePricingSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fixedPricingSource) Current(ctx context.Context, limit pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if s.snapshot.IdentityBytes() > limit.MaxIdentityBytes {
		return nil, pricing.SnapshotStatus{}, pricing.ErrProjectionLimit
	}
	return s.snapshot, s.status, nil
}

func pricingSource(t *testing.T, models string, status pricing.SnapshotStatus) *fixedPricingSource {
	t.Helper()
	return pricingSourceRaw(t, `{"openai":{"id":"openai","models":`+models+`},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`, status)
}

func pricingSourceRaw(t *testing.T, raw string, status pricing.SnapshotStatus) *fixedPricingSource {
	t.Helper()
	snapshot, err := pricing.ParseSnapshot(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("parse pricing fixture: %v", err)
	}
	return &fixedPricingSource{snapshot: snapshot, status: status}
}

func newReporter(t *testing.T, path string) *report.Reporter {
	t.Helper()
	return report.New(path, pricingSource(t, `{}`, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:empty-test-prices"}))
}

func amount(t *testing.T, value string) *pricing.Amount {
	t.Helper()
	if value == "0" {
		return &pricing.Amount{}
	}
	parsed, err := pricing.ParseAmount(value)
	if err != nil {
		t.Fatalf("parse expected amount %q: %v", value, err)
	}
	return &parsed
}

func TestQueryValuesCompleteOpenAIReportFromOnePricingSnapshot(t *testing.T) {
	lastSuccess := time.Date(2026, 8, 31, 23, 0, 0, 0, time.UTC)
	source := pricingSource(t, `{"gpt-5.6-sol":{"id":"gpt-5.6-sol","cost":{"input":2,"output":8,"cache_read":0.5,"cache_write":3}}}`, pricing.SnapshotStatus{
		Source: "fetched", Version: "sha256:openai-fixture", LastSuccess: &lastSuccess,
	})
	zero := int64(0)
	first := turn("2026-09-01T12:00:00Z", "gpt-5.6-sol-fast", usage.OpenAIUsage{
		InputTokens: 100, OutputTokens: 20, CachedTokens: ptr(30), CacheWriteTokens: ptr(10),
	})
	requested := "a-distinct-requested-model"
	first.RequestedModel = &requested
	second := turn("2026-09-02T12:00:00Z", "gpt-5.6-sol-fast", usage.OpenAIUsage{
		InputTokens: 50, OutputTokens: 5, CachedTokens: &zero, CacheWriteTokens: &zero,
	})
	path := stored(t, first, second)
	reader := report.New(path, source)
	report.SetNowForTest(reader, time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC))

	got, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}

	match := report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "gpt-5.6-sol", Method: report.PricingMatchBySuffix}
	metrics := func(input, output, cached, cacheWrite int64, reported int64) map[string]report.Metric {
		return map[string]report.Metric{
			"input_tokens":       {Sum: ptr(input), ReportedTurns: reported},
			"output_tokens":      {Sum: ptr(output), ReportedTurns: reported},
			"cached_tokens":      {Sum: ptr(cached), ReportedTurns: reported},
			"cache_write_tokens": {Sum: ptr(cacheWrite), ReportedTurns: reported},
			"reasoning_tokens":   {},
			"total_tokens":       {},
		}
	}
	firstTotal := report.Total{Turns: 1, Usage: metrics(100, 20, 30, 10, 1), Cost: report.Cost{Amount: amount(t, "0.000325"), PricedTurns: 1}}
	secondTotal := report.Total{Turns: 1, Usage: metrics(50, 5, 0, 0, 1), Cost: report.Cost{Amount: amount(t, "0.00014"), PricedTurns: 1}}
	rangeTotal := report.Total{Turns: 2, Usage: metrics(150, 25, 30, 10, 2), Cost: report.Cost{Amount: amount(t, "0.000465"), PricedTurns: 2}}
	model := "gpt-5.6-sol-fast"
	want := report.Report{
		SchemaVersion: 1,
		GeneratedAt:   time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC),
		Timezone:      "UTC",
		Period:        "day",
		Since:         "2026-09-01",
		Until:         "2026-09-03",
		WindowStart:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:     time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		Scope:         "configured_database",
		Collection:    "best_effort",
		Surface:       "openai",
		Model:         nil,
		Buckets: []report.Bucket{
			{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), RangeEnd: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
			{StartDate: "2026-09-02", UntilDate: "2026-09-03", RangeStart: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), RangeEnd: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), InProgress: true},
		},
		Pricing: report.PricingProvenance{
			Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider", ContextPolicy: "highest_tier", CacheWritePolicy: "single_rate",
			Version: "sha256:openai-fixture", Source: "fetched", LastSuccess: &lastSuccess,
		},
		OpenAI: &report.Section{
			Rows: []report.Row{
				{BucketStart: "2026-09-01", ModelTotal: report.ModelTotal{Model: model, Total: firstTotal, PricingMatch: match}},
				{BucketStart: "2026-09-02", ModelTotal: report.ModelTotal{Model: model, Total: secondTotal, PricingMatch: match}},
			},
			Models: []report.ModelTotal{{Model: model, Total: rangeTotal, PricingMatch: match}},
			Total:  rangeTotal,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("complete report mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if got.OpenAI.Rows[0].Cost.Amount == got.OpenAI.Rows[1].Cost.Amount || got.OpenAI.Rows[0].Cost.Amount == got.OpenAI.Models[0].Cost.Amount || got.OpenAI.Models[0].Cost.Amount == got.OpenAI.Total.Cost.Amount {
		t.Fatal("aggregate amounts share mutable result pointers")
	}
}

func TestQueryReportsEveryExclusivePricingOutcomeWithRequiredPrecedence(t *testing.T) {
	source := pricingSourceRaw(t, `{
		"openai":{"id":"openai","models":{
			"ambiguous":{"id":"ambiguous","cost":{"input":1,"output":1}},
			"free":{"id":"free","cost":{"input":0,"output":0}},
			"inconsistent":{"id":"inconsistent","cost":{"input":1,"output":1,"cache_read":1,"cache_write":1}},
			"missing-usage":{"id":"missing-usage","cost":{"input":1,"output":1}},
			"unpriced":{"id":"unpriced","cost":{"input":9,"output":9}},
			"unpriced-fast":{"id":"unpriced-fast"}
		}},
		"anthropic":{"id":"anthropic","models":{"ambiguous":{"id":"ambiguous","cost":{"input":9,"output":9}}}},
		"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}
	}`, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:outcomes"})
	zero := int64(0)
	path := stored(t,
		turn("2026-09-01T01:00:00Z", "unknown", usage.OpenAIUsage{InputTokens: 6, OutputTokens: 7}),
		turn("2026-09-01T02:00:00Z", "ambiguous", usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}),
		turn("2026-09-01T03:00:00Z", "unpriced-fast", usage.OpenAIUsage{InputTokens: 8, OutputTokens: 9}),
		turn("2026-09-01T04:00:00Z", "missing-usage", usage.OpenAIUsage{InputTokens: 4, OutputTokens: 5}),
		turn("2026-09-01T05:00:00Z", "inconsistent", usage.OpenAIUsage{InputTokens: 10, OutputTokens: 1, CachedTokens: ptr(8), CacheWriteTokens: ptr(5)}),
		turn("2026-09-01T06:00:00Z", "free", usage.OpenAIUsage{InputTokens: 2, OutputTokens: 3, CachedTokens: &zero, CacheWriteTokens: &zero}),
	)

	got, err := report.New(path, source).Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenAI == nil || got.Anthropic != nil || len(got.OpenAI.Rows) != 6 || len(got.OpenAI.Models) != 6 {
		t.Fatalf("selected pricing section = %+v", got)
	}
	wantModels := []struct {
		name  string
		match report.PricingMatch
		cost  report.Cost
	}{
		{"ambiguous", report.PricingMatch{Status: report.PricingMatchAmbiguous}, report.Cost{Unpriced: report.UnpricedCoverage{AmbiguousModel: 1}}},
		{"free", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "free", Method: report.PricingMatchByExact}, report.Cost{Amount: amount(t, "0"), PricedTurns: 1}},
		{"inconsistent", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "inconsistent", Method: report.PricingMatchByExact}, report.Cost{Unpriced: report.UnpricedCoverage{InconsistentUsage: 1}}},
		{"missing-usage", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "missing-usage", Method: report.PricingMatchByExact}, report.Cost{Unpriced: report.UnpricedCoverage{MissingUsage: 1}}},
		{"unknown", report.PricingMatch{Status: report.PricingMatchUnknown}, report.Cost{Unpriced: report.UnpricedCoverage{UnknownModel: 1}}},
		{"unpriced-fast", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "unpriced-fast", Method: report.PricingMatchByExact}, report.Cost{Unpriced: report.UnpricedCoverage{MissingRate: 1}}},
	}
	for index, want := range wantModels {
		model := got.OpenAI.Models[index]
		row := got.OpenAI.Rows[index]
		if model.Model != want.name || row.Model != want.name || !reflect.DeepEqual(model.PricingMatch, want.match) || !reflect.DeepEqual(row.PricingMatch, want.match) || !reflect.DeepEqual(model.Cost, want.cost) || !reflect.DeepEqual(row.Cost, want.cost) {
			t.Fatalf("pricing outcome %d = row %+v model %+v; want %s %+v %+v", index, row, model, want.name, want.match, want.cost)
		}
	}
	wantTotalCost := report.Cost{
		Amount: amount(t, "0"), PricedTurns: 1,
		Unpriced: report.UnpricedCoverage{UnknownModel: 1, AmbiguousModel: 1, MissingRate: 1, MissingUsage: 1, InconsistentUsage: 1},
	}
	if !reflect.DeepEqual(got.OpenAI.Total.Cost, wantTotalCost) || got.OpenAI.Total.Turns != 6 || *got.OpenAI.Total.Usage["input_tokens"].Sum != 31 || *got.OpenAI.Total.Usage["output_tokens"].Sum != 26 || *got.OpenAI.Total.Usage["cached_tokens"].Sum != 8 || got.OpenAI.Total.Usage["cached_tokens"].ReportedTurns != 2 || *got.OpenAI.Total.Usage["cache_write_tokens"].Sum != 5 || got.OpenAI.Total.Usage["cache_write_tokens"].ReportedTurns != 2 {
		t.Fatalf("mixed pricing/native total = %+v", got.OpenAI.Total)
	}
}

func TestQueryValuesBothNativeSurfacesByReportedModelOnly(t *testing.T) {
	source := pricingSource(t, `{"shared":{"id":"shared","cost":{"input":1,"output":2,"cache_read":0.1,"cache_write":1.25}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:combined"})
	cacheRead, cacheWrite := int64(6000), int64(2000)
	anthropic := anthropicTurn("2026-09-01T01:00:00Z", "shared", usage.AnthropicUsage{
		InputTokens: 12, OutputTokens: 9, CacheReadInputTokens: &cacheRead, CacheCreationInputTokens: &cacheWrite,
		Ephemeral5mInputTokens: ptr(750), Ephemeral1hInputTokens: ptr(1250), ThinkingTokens: ptr(4),
	})
	openAI := turn("2026-09-01T02:00:00Z", "shared", usage.OpenAIUsage{
		InputTokens: 8012, OutputTokens: 9, CachedTokens: &cacheRead, CacheWriteTokens: &cacheWrite, ReasoningTokens: ptr(4), TotalTokens: ptr(8021),
	})
	requestedA, requestedO := "anthropic-requested-alias", "openai-requested-alias"
	anthropic.RequestedModel, openAI.RequestedModel = &requestedA, &requestedO
	path := stored(t, anthropic, openAI,
		anthropicTurn("2026-09-01T03:00:00Z", "excluded", usage.AnthropicUsage{InputTokens: 999, OutputTokens: 999}),
		turn("2026-09-01T04:00:00Z", "excluded", usage.OpenAIUsage{InputTokens: 999, OutputTokens: 999}),
	)
	selected := "shared"
	q := selection()
	q.Surface, q.Model = "all", &selected
	reader := report.New(path, source)
	report.SetNowForTest(reader, time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC))
	got, err := reader.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}

	wantMatch := report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "shared", Method: report.PricingMatchByExact}
	anthropicTotal := report.Total{Turns: 1, Cost: report.Cost{Amount: amount(t, "0.00313"), PricedTurns: 1}, Usage: map[string]report.Metric{
		"input_tokens": {Sum: ptr(12), ReportedTurns: 1}, "output_tokens": {Sum: ptr(9), ReportedTurns: 1},
		"cache_creation_input_tokens": {Sum: ptr(2000), ReportedTurns: 1}, "cache_read_input_tokens": {Sum: ptr(6000), ReportedTurns: 1},
		"ephemeral_5m_input_tokens": {Sum: ptr(750), ReportedTurns: 1}, "ephemeral_1h_input_tokens": {Sum: ptr(1250), ReportedTurns: 1},
		"thinking_tokens": {Sum: ptr(4), ReportedTurns: 1},
	}}
	openAITotal := report.Total{Turns: 1, Cost: report.Cost{Amount: amount(t, "0.00313"), PricedTurns: 1}, Usage: map[string]report.Metric{
		"input_tokens": {Sum: ptr(8012), ReportedTurns: 1}, "output_tokens": {Sum: ptr(9), ReportedTurns: 1},
		"cached_tokens": {Sum: ptr(6000), ReportedTurns: 1}, "cache_write_tokens": {Sum: ptr(2000), ReportedTurns: 1},
		"reasoning_tokens": {Sum: ptr(4), ReportedTurns: 1}, "total_tokens": {Sum: ptr(8021), ReportedTurns: 1},
	}}
	section := func(total report.Total) *report.Section {
		model := report.ModelTotal{Model: "shared", PricingMatch: wantMatch, Total: total}
		return &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: model}}, Models: []report.ModelTotal{model}, Total: total}
	}
	want := report.Report{
		SchemaVersion: 1, GeneratedAt: time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC), Timezone: "UTC", Period: "day",
		Since: "2026-09-01", Until: "2026-09-03", WindowStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		Scope: "configured_database", Collection: "best_effort", Surface: "all", Model: &selected,
		Buckets: []report.Bucket{
			{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), RangeEnd: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
			{StartDate: "2026-09-02", UntilDate: "2026-09-03", RangeStart: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), RangeEnd: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)},
		},
		Pricing:   report.PricingProvenance{Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider", ContextPolicy: "highest_tier", CacheWritePolicy: "single_rate", Version: "sha256:combined", Source: "fetched"},
		Anthropic: section(anthropicTotal), OpenAI: section(openAITotal),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("complete combined report mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestQueryRepricesAndRematchesEachCapturedSourceRevision(t *testing.T) {
	firstSource := pricingSource(t, `{"switch":{"id":"switch","cost":{"input":1,"output":1}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:first"})
	secondSource := pricingSource(t, `{"switch-fast":{"id":"switch-fast","cost":{"input":2,"output":3}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:second"})
	removedSource := pricingSource(t, `{}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:removed"})
	source := &mutablePricingSource{current: firstSource}
	zero := int64(0)
	path := stored(t, turn("2026-09-01T01:00:00Z", "switch-fast", usage.OpenAIUsage{InputTokens: 10, OutputTokens: 2, CachedTokens: &zero, CacheWriteTokens: &zero}))
	reader := report.New(path, source)

	first, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	source.set(secondSource)
	second, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	source.set(removedSource)
	removed, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}

	if first.Pricing.Version != "sha256:first" || first.OpenAI.Total.Cost.Amount == nil || first.OpenAI.Total.Cost.Amount.String() != "0.000012" || first.OpenAI.Models[0].PricingMatch.Method != report.PricingMatchBySuffix || first.OpenAI.Models[0].PricingMatch.Model != "switch" {
		t.Fatalf("first captured revision = %+v", first)
	}
	if second.Pricing.Version != "sha256:second" || second.OpenAI.Total.Cost.Amount == nil || second.OpenAI.Total.Cost.Amount.String() != "0.000026" || second.OpenAI.Models[0].PricingMatch.Method != report.PricingMatchByExact || second.OpenAI.Models[0].PricingMatch.Model != "switch-fast" {
		t.Fatalf("second captured revision = %+v", second)
	}
	if removed.Pricing.Version != "sha256:removed" || removed.OpenAI.Total.Cost.Amount != nil || removed.OpenAI.Total.Cost.Unpriced.UnknownModel != 1 || removed.OpenAI.Models[0].PricingMatch.Status != report.PricingMatchUnknown {
		t.Fatalf("removed candidate revision = %+v", removed)
	}
	if first.OpenAI.Total.Cost.Amount.String() != "0.000012" || first.OpenAI.Models[0].PricingMatch.Model != "switch" || source.callCount() != 3 {
		t.Fatalf("later source revisions mutated a prior report or Current calls = %d", source.callCount())
	}
}

func TestQueryUsesOneCapturedPricingRevisionAcrossBothSurfaces(t *testing.T) {
	oldSource := pricingSource(t, `{"shared":{"id":"shared","cost":{"input":1,"output":1}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:old"})
	newSource := pricingSource(t, `{"shared-new":{"id":"shared-new","cost":{"input":10,"output":10}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:new"})
	source := &mutablePricingSource{current: oldSource}
	zero := int64(0)
	path := stored(t,
		anthropicTurn("2026-09-01T01:00:00Z", "shared", usage.AnthropicUsage{InputTokens: 2, OutputTokens: 3, CacheReadInputTokens: &zero, CacheCreationInputTokens: &zero}),
		turn("2026-09-01T02:00:00Z", "shared", usage.OpenAIUsage{InputTokens: 5, OutputTokens: 7, CachedTokens: &zero, CacheWriteTokens: &zero}),
	)
	reader := report.New(path, source)
	report.NotifyAfterExaminedTurnForTest(reader, func(examined int) {
		if examined == 1 {
			source.set(newSource)
		}
	})
	q := selection()
	q.Surface = "all"
	captured, err := reader.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Pricing.Version != "sha256:old" || captured.Anthropic.Total.Cost.Amount == nil || captured.Anthropic.Total.Cost.Amount.String() != "0.000005" || captured.OpenAI.Total.Cost.Amount == nil || captured.OpenAI.Total.Cost.Amount.String() != "0.000012" || captured.Anthropic.Models[0].PricingMatch.Model != "shared" || captured.OpenAI.Models[0].PricingMatch.Model != "shared" {
		t.Fatalf("mid-query source change mixed revisions: %+v", captured)
	}

	report.NotifyAfterExaminedTurnForTest(reader, nil)
	next, err := reader.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if next.Pricing.Version != "sha256:new" || next.Anthropic.Total.Cost.Unpriced.UnknownModel != 1 || next.OpenAI.Total.Cost.Unpriced.UnknownModel != 1 || next.Anthropic.Total.Cost.Amount != nil || next.OpenAI.Total.Cost.Amount != nil || source.callCount() != 2 {
		t.Fatalf("next Query did not capture the replacement revision: %+v calls=%d", next, source.callCount())
	}
}

func TestQueryDistinguishesEmptyAndAllUnpriceableCostAmounts(t *testing.T) {
	source := pricingSource(t, `{"known-unpriced":{"id":"known-unpriced"}}`, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:unpriced"})
	path := stored(t, turn("2026-09-01T01:00:00Z", "known-unpriced", usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}))
	reader := report.New(path, source)

	nonempty, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if nonempty.OpenAI.Total.Turns != 1 || nonempty.OpenAI.Total.Cost.Amount != nil || nonempty.OpenAI.Total.Cost.PricedTurns != 0 || nonempty.OpenAI.Total.Cost.Unpriced != (report.UnpricedCoverage{MissingRate: 1}) {
		t.Fatalf("all-unpriceable total = %+v", nonempty.OpenAI.Total)
	}
	missing := "not-present"
	q := selection()
	q.Model = &missing
	empty, err := reader.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if empty.OpenAI.Total.Turns != 0 || empty.OpenAI.Total.Cost.Amount == nil || empty.OpenAI.Total.Cost.Amount.String() != "0" || empty.OpenAI.Total.Cost.PricedTurns != 0 || empty.OpenAI.Total.Cost.Unpriced != (report.UnpricedCoverage{}) || len(empty.OpenAI.Rows) != 0 || len(empty.OpenAI.Models) != 0 {
		t.Fatalf("empty cost total = %+v", empty.OpenAI)
	}
}

func TestQuerySharesIdentityRetentionAcrossSourceIndexMemoAndNativeSections(t *testing.T) {
	source := pricingSource(t, `{"model":{"id":"model","cost":{"input":1,"output":1}}}`, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:budget"})
	zero := int64(0)
	path := stored(t,
		anthropicTurn("2026-09-01T01:00:00Z", "model", usage.AnthropicUsage{InputTokens: 1, OutputTokens: 1, CacheReadInputTokens: &zero, CacheCreationInputTokens: &zero}),
		turn("2026-09-01T02:00:00Z", "model", usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: &zero, CacheWriteTokens: &zero}),
	)
	q := selection()
	q.Surface = "all"

	for _, tc := range []struct {
		name  string
		limit int
	}{{"source projection", 10}, {"matcher index", 64}, {"memoized Reported model", 69}} {
		t.Run(tc.name, func(t *testing.T) {
			tooSmall := report.NewReadLimitsForTest(path, source, report.MaxRows, report.MaxGroups, tc.limit)
			failed, err := tooSmall.Query(context.Background(), q)
			var failure *report.Error
			if !errors.As(err, &failure) || failure.Code != report.TooLarge || !reflect.DeepEqual(failed, report.Report{}) {
				t.Fatalf("%d-byte shared retention result = %+v, %v", tc.limit, failed, err)
			}
		})
	}

	exact := report.NewReadLimitsForTest(path, source, report.MaxRows, report.MaxGroups, 70)
	got, err := exact.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Anthropic.Total.Turns != 1 || got.OpenAI.Total.Turns != 1 || got.Anthropic.Models[0].Model != "model" || got.OpenAI.Models[0].Model != "model" || got.Anthropic.Total.Cost.PricedTurns != 1 || got.OpenAI.Total.Cost.PricedTurns != 1 {
		t.Fatalf("exact shared retention report = %+v", got)
	}
}

func TestQuerySafelyTranslatesSourceFailureAndProjectionCancellation(t *testing.T) {
	path := stored(t)
	private := errors.New(`interpret /private/prices.json: {"api_key":"secret"}`)
	failed, err := report.New(path, pricingSourceFunc(func(context.Context, pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
		return nil, pricing.SnapshotStatus{}, private
	})).Query(context.Background(), selection())
	var failure *report.Error
	if !errors.As(err, &failure) || failure.Code != report.Unavailable || !reflect.DeepEqual(failed, report.Report{}) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "api_key") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe source failure translation: report=%+v error=%v", failed, err)
	}

	fixed := pricingSource(t, `{"model":{"id":"model","cost":{"input":1,"output":1}}}`, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:cancel"})
	ctx, cancel := context.WithCancel(context.Background())
	cancelDuringProjection := pricingSourceFunc(func(context.Context, pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
		cancel()
		return fixed.snapshot, fixed.status, nil
	})
	canceled, err := report.New(path, cancelDuringProjection).Query(ctx, selection())
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Code != report.Unavailable || !reflect.DeepEqual(canceled, report.Report{}) {
		t.Fatalf("projection cancellation = %+v, %v", canceled, err)
	}
}
