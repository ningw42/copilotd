package pricing_test

import (
	"math"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestTariffCalculatesOpenAIWithCompleteInputContext(t *testing.T) {
	t.Parallel()

	tariff := tariffFromCost(t, `{
		"input":1,"output":2,
		"tiers":[{"input":3,"output":5,"tier":{"type":"context","size":200}}]
	}`)
	zero, ignored := int64(0), int64(1_000_000)
	tests := []struct {
		name       string
		input      int64
		wantAmount string
	}{
		{name: "equal threshold uses base", input: 200, wantAmount: "0.0002"},
		{name: "above threshold uses tier", input: 201, wantAmount: "0.000603"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      tc.input,
				CachedTokens:     &zero,
				CacheWriteTokens: &zero,
				ReasoningTokens:  &ignored,
				TotalTokens:      &ignored,
			}, nil)
			if err != nil {
				t.Fatalf("Tariff.CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != tc.wantAmount {
				t.Fatalf("contribution = {amount:%s reason:%q}, want amount %s and no exclusion", contribution.Amount.String(), contribution.Reason, tc.wantAmount)
			}
		})
	}
}

func TestTariffCalculatesOpenAIFromReportedFastTier(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"openai":{"id":"openai","models":{"model":{"id":"model",
			"cost":{"input":1,"output":2,"tiers":[{"input":2,"output":3,"tier":{"type":"context","size":100}}]},
			"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4},"provider":{"body":{"service_tier":"priority"}}}}}
		}}},
		"anthropic":{"id":"anthropic","models":{}},
		"google":{"id":"google","models":{}},
		"xai":{"id":"xai","models":{}}
	}`)
	snapshot, err := pricing.ParseSnapshot(t.Context(), raw)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
	if !ok {
		t.Fatal("fixture model has no tariff")
	}
	zero := int64(0)
	tests := []struct {
		name       string
		tier       *string
		input      int64
		wantAmount string
	}{
		{name: "unavailable uses normal base", input: 50, wantAmount: "0.00007"},
		{name: "default uses normal base", tier: stringPointer("default"), input: 50, wantAmount: "0.00007"},
		{name: "unknown uses normal context", tier: stringPointer("flex"), input: 150, wantAmount: "0.00033"},
		{name: "priority uses explicit Fast base", tier: stringPointer("priority"), input: 50, wantAmount: "0.00014"},
		{name: "Fast ASCII case uses derived context", tier: stringPointer("FAST"), input: 150, wantAmount: "0.00066"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      tc.input,
				OutputTokens:     10,
				CachedTokens:     &zero,
				CacheWriteTokens: &zero,
			}, tc.tier)
			if err != nil {
				t.Fatalf("Tariff.CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != tc.wantAmount {
				t.Fatalf("contribution = {amount:%s reason:%q}, want amount %s and no exclusion", contribution.Amount.String(), contribution.Reason, tc.wantAmount)
			}
		})
	}
}

func TestTariffDistinguishesAbsentAndUnpricedFastDeclarations(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	native := usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: &zero, CacheWriteTokens: &zero}
	priority := "priority"
	tests := []struct {
		name       string
		model      string
		tier       *string
		wantAmount string
		wantReason pricing.ExclusionReason
	}{
		{
			name:       "Fast cost works without normal cost",
			model:      `{"id":"model","experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}`,
			tier:       &priority,
			wantAmount: "0.000006",
		},
		{
			name:       "normal fallback is unavailable without normal cost",
			model:      `{"id":"model","experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}`,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "Fast alias falls back when declaration is absent",
			model:      `{"id":"model","cost":{"input":1,"output":2}}`,
			tier:       &priority,
			wantAmount: "0.000003",
		},
		{
			name:       "declared but unpriced Fast does not borrow normal rates",
			model:      `{"id":"model","cost":{"input":1,"output":2},"experimental":{"modes":{"fast":{}}}}`,
			tier:       &priority,
			wantReason: pricing.ExclusionMissingRate,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tariff := tariffFromModel(t, tc.model)
			contribution, err := tariff.CalculateOpenAI(native, tc.tier)
			if err != nil {
				t.Fatalf("Tariff.CalculateOpenAI() error = %v", err)
			}
			assertContribution(t, contribution, tc.wantAmount, tc.wantReason)
		})
	}
}

func TestTariffRecognizesOnlyFixedASCIIServiceTierAliases(t *testing.T) {
	t.Parallel()

	if pricing.MaxServiceTierLookupBytes != len("priority") {
		t.Fatalf("MaxServiceTierLookupBytes = %d, want fixed longest alias length %d", pricing.MaxServiceTierLookupBytes, len("priority"))
	}
	tariff := tariffFromModel(t, `{"id":"model","cost":{"input":1,"output":2},"experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":"priority"}},"cost":{"input":2,"output":4}}}}}`)
	zero := int64(0)
	native := usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: &zero, CacheWriteTokens: &zero}
	tests := []struct {
		name       string
		tier       *string
		wantAmount string
	}{
		{name: "null", wantAmount: "0.000003"},
		{name: "empty", tier: stringPointer(""), wantAmount: "0.000003"},
		{name: "default", tier: stringPointer("DeFaUlT"), wantAmount: "0.000003"},
		{name: "fast", tier: stringPointer("fast"), wantAmount: "0.000006"},
		{name: "priority ASCII case", tier: stringPointer("PrIoRiTy"), wantAmount: "0.000006"},
		{name: "source mode name is not an alias", tier: stringPointer("accelerated"), wantAmount: "0.000003"},
		{name: "flex", tier: stringPointer("flex"), wantAmount: "0.000003"},
		{name: "scale", tier: stringPointer("scale"), wantAmount: "0.000003"},
		{name: "leading space", tier: stringPointer(" fast"), wantAmount: "0.000003"},
		{name: "trailing space", tier: stringPointer("priority "), wantAmount: "0.000003"},
		{name: "embedded NUL", tier: stringPointer("fast\x00"), wantAmount: "0.000003"},
		{name: "Unicode lookalike", tier: stringPointer("faſt"), wantAmount: "0.000003"},
		{name: "overlong", tier: stringPointer("priority-future"), wantAmount: "0.000003"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := tariff.CalculateOpenAI(native, tc.tier)
			if err != nil {
				t.Fatalf("Tariff.CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != tc.wantAmount {
				t.Fatalf("contribution = {amount:%s reason:%q}, want amount %s", contribution.Amount.String(), contribution.Reason, tc.wantAmount)
			}
		})
	}
}

func TestTariffDerivesExactFastContextRatesPerCategory(t *testing.T) {
	t.Parallel()

	priority := "priority"
	zero := int64(0)
	tests := []struct {
		name       string
		model      string
		input      int64
		output     int64
		cached     int64
		cacheWrite int64
		wantAmount string
		wantReason pricing.ExclusionReason
	}{
		{
			name:       "repeating intermediate has terminating final rate",
			model:      `{"id":"model","cost":{"input":3,"output":1,"tiers":[{"input":6,"output":1,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":1,"output":0}}}}}`,
			input:      1_000_000,
			wantAmount: "2",
		},
		{
			name:       "final repeating decimal is unavailable",
			model:      `{"id":"model","cost":{"input":3,"output":1,"tiers":[{"input":1,"output":1,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":1,"output":0}}}}}`,
			input:      101,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "unequal category factors remain independent",
			model:      `{"id":"model","cost":{"input":2,"output":4,"tiers":[{"input":4,"output":12,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":6,"output":8}}}}}`,
			input:      1_000_000,
			output:     1_000_000,
			wantAmount: "36",
		},
		{
			name:       "zero Fast numerator yields explicit zero",
			model:      `{"id":"model","cost":{"input":2,"output":2,"tiers":[{"input":4,"output":4,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":0,"output":0}}}}}`,
			input:      101,
			wantAmount: "0",
		},
		{
			name:       "zero context rate yields explicit zero",
			model:      `{"id":"model","cost":{"input":2,"output":2,"tiers":[{"input":0,"output":0,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":4,"output":4}}}}}`,
			input:      101,
			wantAmount: "0",
		},
		{
			name:       "zero base denominator is unavailable even with zero numerator",
			model:      `{"id":"model","cost":{"input":0,"output":2,"tiers":[{"input":0,"output":4,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":0,"output":4}}}}}`,
			input:      101,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "missing operand is unavailable without cross-category borrowing",
			model:      `{"id":"model","cost":{"input":2,"output":2,"tiers":[{"input":4,"output":4,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"output":4}}}}}`,
			input:      101,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "expanded fractional bound is enforced on final rate",
			model:      `{"id":"model","cost":{"input":1,"output":1,"tiers":[{"input":0.0000000001,"output":1,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":0.000000001,"output":0}}}}}`,
			input:      101,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "expanded integer bound is enforced on final rate",
			model:      `{"id":"model","cost":{"input":1,"output":1,"tiers":[{"input":2,"output":1,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":999999999999999999,"output":0}}}}}`,
			input:      101,
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "missing derived cache rate is optional for zero cache",
			model:      `{"id":"model","cost":{"input":1,"output":1,"cache_read":1,"tiers":[{"input":2,"output":2,"cache_read":2,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":2}}}}}`,
			input:      101,
			wantAmount: "0.000404",
		},
		{
			name:       "missing derived cache rate excludes positive cache",
			model:      `{"id":"model","cost":{"input":1,"output":1,"cache_read":1,"tiers":[{"input":2,"output":2,"cache_read":2,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":2}}}}}`,
			input:      101,
			cached:     1,
			wantReason: pricing.ExclusionMissingRate,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tariff := tariffFromModel(t, tc.model)
			cached, cacheWrite := tc.cached, tc.cacheWrite
			if tc.cached == 0 {
				cached = zero
			}
			if tc.cacheWrite == 0 {
				cacheWrite = zero
			}
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      tc.input,
				OutputTokens:     tc.output,
				CachedTokens:     &cached,
				CacheWriteTokens: &cacheWrite,
			}, &priority)
			if err != nil {
				t.Fatalf("Tariff.CalculateOpenAI() error = %v", err)
			}
			assertContribution(t, contribution, tc.wantAmount, tc.wantReason)
		})
	}
}

func TestTariffSelectsDerivedFastRatesAtStrictGreatestContextThreshold(t *testing.T) {
	t.Parallel()

	priority := "priority"
	zero := int64(0)
	for _, tc := range []struct {
		name       string
		model      string
		input      int64
		wantAmount string
	}{
		{
			name: "equal greatest threshold retains lower tier",
			model: `{"id":"model","cost":{"input":1,"output":1,"tiers":[
				{"input":3,"output":1,"tier":{"type":"context","size":200}},
				{"input":2,"output":1,"tier":{"type":"context","size":100}}
			]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":0}}}}}`,
			input: 200, wantAmount: "0.0008",
		},
		{
			name: "above greatest threshold selects greatest tier",
			model: `{"id":"model","cost":{"input":1,"output":1,"tiers":[
				{"input":3,"output":1,"tier":{"type":"context","size":200}},
				{"input":2,"output":1,"tier":{"type":"context","size":100}}
			]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":0}}}}}`,
			input: 201, wantAmount: "0.001206",
		},
		{
			name:  "legacy context tier composes only without structured tiers",
			model: `{"id":"model","cost":{"input":1,"output":1,"context_over_200k":{"input":3,"output":1}},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":0}}}}}`,
			input: 200001, wantAmount: "1.200006",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tariff := tariffFromModel(t, tc.model)
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{InputTokens: tc.input, CachedTokens: &zero, CacheWriteTokens: &zero}, &priority)
			if err != nil {
				t.Fatal(err)
			}
			assertContribution(t, contribution, tc.wantAmount, "")
		})
	}
}

func TestTariffMatchesAuditedFastContextMatrices(t *testing.T) {
	t.Parallel()

	priority := "priority"
	zero, million := int64(0), int64(1_000_000)
	for _, tc := range []struct {
		model, base, fast, context        string
		input, read, write, readAndOutput string
	}{
		{model: "gpt-5.4", base: `"input":2.5,"cache_read":0.25,"output":15`, fast: `"input":5,"cache_read":0.5,"output":30`, context: `"input":5,"cache_read":0.5,"output":22.5`, input: "10", read: "1", readAndOutput: "46"},
		{model: "gpt-5.5", base: `"input":5,"cache_read":0.5,"output":30`, fast: `"input":12.5,"cache_read":1.25,"output":75`, context: `"input":10,"cache_read":1,"output":45`, input: "25", read: "2.5", readAndOutput: "115"},
		{model: "gpt-5.6", base: `"input":4,"cache_read":0.4,"cache_write":5,"output":20`, fast: `"input":8,"cache_read":0.8,"cache_write":10,"output":40`, context: `"input":8,"cache_read":0.8,"cache_write":10,"output":30`, input: "16", read: "1.6", write: "20", readAndOutput: "61.6"},
		{model: "gpt-5.6-luna", base: `"input":0.2,"cache_read":0.02,"cache_write":0.25,"output":1.2`, fast: `"input":0.4,"cache_read":0.04,"cache_write":0.5,"output":2.4`, context: `"input":0.4,"cache_read":0.04,"cache_write":0.5,"output":1.8`, input: "0.8", read: "0.08", write: "1", readAndOutput: "3.68"},
		{model: "gpt-5.6-sol", base: `"input":4,"cache_read":0.4,"cache_write":5,"output":20`, fast: `"input":8,"cache_read":0.8,"cache_write":10,"output":40`, context: `"input":8,"cache_read":0.8,"cache_write":10,"output":30`, input: "16", read: "1.6", write: "20", readAndOutput: "61.6"},
		{model: "gpt-5.6-terra", base: `"input":2,"cache_read":0.2,"cache_write":2.5,"output":12`, fast: `"input":4,"cache_read":0.4,"cache_write":5,"output":24`, context: `"input":4,"cache_read":0.4,"cache_write":5,"output":18`, input: "8", read: "0.8", write: "10", readAndOutput: "36.8"},
		{model: "gpt-6-astra", base: `"input":10,"cache_read":1,"cache_write":12.5,"output":50`, fast: `"input":20,"cache_read":2,"cache_write":25,"output":100`, context: `"input":20,"cache_read":2,"cache_write":25,"output":75`, input: "40", read: "4", write: "50", readAndOutput: "154"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			model := `{"id":"model","cost":{` + tc.base + `,"tiers":[{` + tc.context + `,"tier":{"type":"context","size":272000}}]},"experimental":{"modes":{"fast":{"cost":{` + tc.fast + `}}}}}`
			tariff := tariffFromModel(t, model)
			calculate := func(native usage.OpenAIUsage) pricing.Contribution {
				t.Helper()
				contribution, err := tariff.CalculateOpenAI(native, &priority)
				if err != nil {
					t.Fatal(err)
				}
				return contribution
			}
			assertContribution(t, calculate(usage.OpenAIUsage{InputTokens: million, CachedTokens: &zero, CacheWriteTokens: &zero}), tc.input, "")
			assertContribution(t, calculate(usage.OpenAIUsage{InputTokens: million, CachedTokens: &million, CacheWriteTokens: &zero}), tc.read, "")
			assertContribution(t, calculate(usage.OpenAIUsage{InputTokens: million, OutputTokens: million, CachedTokens: &million, CacheWriteTokens: &zero}), tc.readAndOutput, "")
			if tc.write != "" {
				assertContribution(t, calculate(usage.OpenAIUsage{InputTokens: million, CachedTokens: &zero, CacheWriteTokens: &million}), tc.write, "")
			}
		})
	}
}

func TestTariffUsesFastBaseWhenContextDerivationIsUnavailable(t *testing.T) {
	t.Parallel()

	tariff := tariffFromModel(t, `{"id":"model","cost":{"input":3,"output":1,"tiers":[{"input":1,"output":1,"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{"cost":{"input":1,"output":0}}}}}`)
	priority := "priority"
	zero := int64(0)
	for _, tc := range []struct {
		name       string
		input      int64
		wantAmount string
		wantReason pricing.ExclusionReason
	}{
		{name: "equal threshold uses explicit Fast base", input: 100, wantAmount: "0.0001"},
		{name: "above threshold uses unavailable derivation", input: 101, wantReason: pricing.ExclusionMissingRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{InputTokens: tc.input, CachedTokens: &zero, CacheWriteTokens: &zero}, &priority)
			if err != nil {
				t.Fatal(err)
			}
			assertContribution(t, contribution, tc.wantAmount, tc.wantReason)
		})
	}
}

func TestTariffAppliesContextSelectionPrecedenceToFastRates(t *testing.T) {
	t.Parallel()

	priority := "priority"
	for _, tc := range []struct {
		name       string
		model      string
		wantReason pricing.ExclusionReason
	}{
		{
			name:       "retained context rejects negative complete input before missing rate",
			model:      `{"id":"model","cost":{"tiers":[{"tier":{"type":"context","size":100}}]},"experimental":{"modes":{"fast":{}}}}`,
			wantReason: pricing.ExclusionInconsistentUsage,
		},
		{
			name:       "no context preserves explicit-vector missing-rate precedence",
			model:      `{"id":"model","cost":{},"experimental":{"modes":{"fast":{}}}}`,
			wantReason: pricing.ExclusionMissingRate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tariff := tariffFromModel(t, tc.model)
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{InputTokens: -1}, &priority)
			if err != nil {
				t.Fatal(err)
			}
			assertContribution(t, contribution, "", tc.wantReason)
		})
	}
}

func TestCalculateOpenAIValuesInclusiveInputAndCompleteOutput(t *testing.T) {
	t.Parallel()

	cached, cacheWrite := int64(30), int64(10)
	contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
		InputTokens:      100,
		OutputTokens:     20,
		CachedTokens:     &cached,
		CacheWriteTokens: &cacheWrite,
	}, pricing.Rates{
		Input:      mustRate(t, "2"),
		Output:     mustRate(t, "8"),
		CacheRead:  mustRate(t, "0.5"),
		CacheWrite: mustRate(t, "3"),
	})
	if err != nil {
		t.Fatalf("CalculateOpenAI() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0.000325" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want amount 0.000325 and no exclusion", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateOpenAIValuesKnownZeroCacheWithoutOptionalRates(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
		InputTokens:      3,
		OutputTokens:     4,
		CachedTokens:     &zero,
		CacheWriteTokens: &zero,
	}, pricing.Rates{
		Input:  mustRate(t, "2"),
		Output: mustRate(t, "5"),
	})
	if err != nil {
		t.Fatalf("CalculateOpenAI() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0.000026" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want amount 0.000026 and no exclusion", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateOpenAIRequiresInputAndOutputRatesForZeroCounts(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	one := mustRate(t, "1")
	tests := []struct {
		name  string
		rates pricing.Rates
	}{
		{name: "input rate", rates: pricing.Rates{Output: one}},
		{name: "output rate", rates: pricing.Rates{Input: one}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
				CachedTokens:     &zero,
				CacheWriteTokens: &zero,
			}, tc.rates)
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingRate {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingRate)
			}
		})
	}
}

func TestCalculateOpenAIRequiresRatesForPositiveCacheCounts(t *testing.T) {
	t.Parallel()

	one := int64(1)
	zero := int64(0)
	rate := mustRate(t, "1")
	tests := []struct {
		name       string
		cached     *int64
		cacheWrite *int64
		rates      pricing.Rates
	}{
		{
			name:       "cache read rate",
			cached:     &one,
			cacheWrite: &zero,
			rates:      pricing.Rates{Input: rate, Output: rate},
		},
		{
			name:       "cache write rate",
			cached:     &zero,
			cacheWrite: &one,
			rates:      pricing.Rates{Input: rate, Output: rate},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      1,
				CachedTokens:     tc.cached,
				CacheWriteTokens: tc.cacheWrite,
			}, tc.rates)
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingRate {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingRate)
			}
		})
	}
}

func TestCalculateOpenAIRequiresBothCacheCounts(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	rate := mustRate(t, "1")
	tests := []struct {
		name       string
		cached     *int64
		cacheWrite *int64
	}{
		{name: "cached count", cacheWrite: &zero},
		{name: "cache-write count", cached: &zero},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
				CachedTokens:     tc.cached,
				CacheWriteTokens: tc.cacheWrite,
			}, pricing.Rates{Input: rate, Output: rate})
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingUsage {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingUsage)
			}
		})
	}
}

func TestCalculateOpenAIRejectsInvalidChargeableNativeCounts(t *testing.T) {
	t.Parallel()

	rate := mustRate(t, "1")
	tests := []struct {
		name       string
		input      int64
		output     int64
		cached     int64
		cacheWrite int64
	}{
		{name: "negative input", input: -1},
		{name: "negative output", output: -1},
		{name: "negative cached", cached: -1},
		{name: "negative cache write", cacheWrite: -1},
		{name: "cached exceeds input", input: 1, cached: 2},
		{name: "cache write exceeds input", input: 1, cacheWrite: 2},
		{name: "cache subsets together exceed input", input: 10, cached: 6, cacheWrite: 5},
		{name: "cache subset sum would overflow", input: math.MaxInt64, cached: math.MaxInt64, cacheWrite: math.MaxInt64},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      tc.input,
				OutputTokens:     tc.output,
				CachedTokens:     &tc.cached,
				CacheWriteTokens: &tc.cacheWrite,
			}, pricing.Rates{Input: rate, Output: rate, CacheRead: rate, CacheWrite: rate})
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionInconsistentUsage {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionInconsistentUsage)
			}
		})
	}
}

func TestCalculateOpenAIAppliesExclusionPrecedence(t *testing.T) {
	t.Parallel()

	minusOne, zero, one := int64(-1), int64(0), int64(1)
	rate := mustRate(t, "1")
	tests := []struct {
		name       string
		native     usage.OpenAIUsage
		rates      pricing.Rates
		wantReason pricing.ExclusionReason
	}{
		{
			name:       "missing required rate before missing and inconsistent usage",
			native:     usage.OpenAIUsage{InputTokens: -1, OutputTokens: -1},
			rates:      pricing.Rates{},
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "missing positive-cache rate before missing and inconsistent usage",
			native:     usage.OpenAIUsage{InputTokens: 0, CachedTokens: &one},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name:       "missing usage before inconsistent usage",
			native:     usage.OpenAIUsage{InputTokens: -1, OutputTokens: -1, CacheWriteTokens: &zero},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionMissingUsage,
		},
		{
			name:       "negative cache does not require absent optional rate",
			native:     usage.OpenAIUsage{CachedTokens: &minusOne, CacheWriteTokens: &zero},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionInconsistentUsage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(tc.native, tc.rates)
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != tc.wantReason {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, tc.wantReason)
			}
		})
	}
}

func TestCalculateOpenAIIgnoresReasoningAndReportedTotal(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	positive, negative := int64(1_000_000), int64(-1)
	tests := []struct {
		name      string
		reasoning *int64
		total     *int64
	}{
		{name: "missing"},
		{name: "reported", reasoning: &positive, total: &positive},
		{name: "unsupported negative values", reasoning: &negative, total: &negative},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
				InputTokens:      5,
				OutputTokens:     7,
				CachedTokens:     &zero,
				CacheWriteTokens: &zero,
				ReasoningTokens:  tc.reasoning,
				TotalTokens:      tc.total,
			}, pricing.Rates{Input: mustRate(t, "1"), Output: mustRate(t, "2")})
			if err != nil {
				t.Fatalf("CalculateOpenAI() error = %v", err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != "0.000019" {
				t.Fatalf("contribution = {amount:%s reason:%q}, want amount 0.000019 and no exclusion", contribution.Amount.String(), contribution.Reason)
			}
		})
	}
}

func TestCalculateOpenAITreatsExplicitZeroRatesAsFree(t *testing.T) {
	t.Parallel()

	cached, cacheWrite := int64(3), int64(2)
	zeroRate := mustRate(t, "0")
	contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
		InputTokens:      10,
		OutputTokens:     4,
		CachedTokens:     &cached,
		CacheWriteTokens: &cacheWrite,
	}, pricing.Rates{
		Input:      zeroRate,
		Output:     zeroRate,
		CacheRead:  zeroRate,
		CacheWrite: zeroRate,
	})
	if err != nil {
		t.Fatalf("CalculateOpenAI() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want priceable exact zero", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateOpenAIUsesExactSupportedDecimalBounds(t *testing.T) {
	t.Parallel()

	t.Run("smallest fractional rate and maximum count", func(t *testing.T) {
		zero := int64(0)
		contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
			InputTokens:      math.MaxInt64,
			CachedTokens:     &zero,
			CacheWriteTokens: &zero,
		}, pricing.Rates{
			Input:  mustRate(t, "0.000000000000000001"),
			Output: mustRate(t, "0"),
		})
		if err != nil {
			t.Fatalf("CalculateOpenAI() error = %v", err)
		}
		if contribution.Reason != "" || contribution.Amount.String() != "0.000009223372036854775807" {
			t.Fatalf("contribution = {amount:%s reason:%q}, want exact lower-bound-rate amount", contribution.Amount.String(), contribution.Reason)
		}
	})

	t.Run("maximum rates and counts", func(t *testing.T) {
		cached, cacheWrite := int64(math.MaxInt64-1), int64(1)
		maximumRate := mustRate(t, "999999999999999999.999999999999999999")
		contribution, err := pricing.CalculateOpenAI(usage.OpenAIUsage{
			InputTokens:      math.MaxInt64,
			OutputTokens:     math.MaxInt64,
			CachedTokens:     &cached,
			CacheWriteTokens: &cacheWrite,
		}, pricing.Rates{
			Input:      maximumRate,
			Output:     maximumRate,
			CacheRead:  maximumRate,
			CacheWrite: maximumRate,
		})
		if err != nil {
			t.Fatalf("CalculateOpenAI() error = %v", err)
		}
		const want = "18446744073709551613999999999999.999981553255926290448386"
		if contribution.Reason != "" || contribution.Amount.String() != want {
			t.Fatalf("contribution = {amount:%s reason:%q}, want exact maximum %s", contribution.Amount.String(), contribution.Reason, want)
		}
	})
}

func tariffFromCost(t *testing.T, cost string) pricing.Tariff {
	t.Helper()
	return tariffFromModel(t, `{"id":"model","cost":`+cost+`}`)
}

func tariffFromModel(t *testing.T, model string) pricing.Tariff {
	t.Helper()
	snapshot, err := pricing.ParseSnapshot(t.Context(), snapshotModelFixture(model))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
	if !ok {
		t.Fatal("fixture model has no tariff")
	}
	return tariff
}

func stringPointer(value string) *string { return &value }

func assertContribution(t *testing.T, contribution pricing.Contribution, wantAmount string, wantReason pricing.ExclusionReason) {
	t.Helper()
	if wantReason != "" {
		wantAmount = "0"
	}
	if contribution.Reason != wantReason || contribution.Amount.String() != wantAmount {
		t.Fatalf("contribution = {amount:%q reason:%q}, want {%q %q}", contribution.Amount.String(), contribution.Reason, wantAmount, wantReason)
	}
}

func mustRate(t *testing.T, raw string) *pricing.Rate {
	t.Helper()
	rate, err := pricing.ParseRate(raw)
	if err != nil {
		t.Fatalf("ParseRate(%q) error = %v", raw, err)
	}
	return &rate
}
