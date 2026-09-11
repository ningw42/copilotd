package pricing_test

import (
	"math"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
)

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

func mustRate(t *testing.T, raw string) *pricing.Rate {
	t.Helper()
	rate, err := pricing.ParseRate(raw)
	if err != nil {
		t.Fatalf("ParseRate(%q) error = %v", raw, err)
	}
	return &rate
}
