package pricing_test

import (
	"math"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestCalculateAnthropicValuesAdditiveInputAndCompleteOutput(t *testing.T) {
	t.Parallel()

	cacheRead, cacheCreation := int64(6000), int64(2000)
	contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
		InputTokens:              12,
		OutputTokens:             9,
		CacheReadInputTokens:     &cacheRead,
		CacheCreationInputTokens: &cacheCreation,
	}, pricing.Rates{
		Input:      mustRate(t, "1"),
		Output:     mustRate(t, "2"),
		CacheRead:  mustRate(t, "0.1"),
		CacheWrite: mustRate(t, "1.25"),
	})
	if err != nil {
		t.Fatalf("CalculateAnthropic() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0.00313" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want amount 0.00313 and no exclusion", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateAnthropicValuesKnownZeroCachesWithoutOptionalRates(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
		InputTokens:              3,
		OutputTokens:             4,
		CacheReadInputTokens:     &zero,
		CacheCreationInputTokens: &zero,
	}, pricing.Rates{
		Input:  mustRate(t, "2"),
		Output: mustRate(t, "5"),
	})
	if err != nil {
		t.Fatalf("CalculateAnthropic() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0.000026" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want amount 0.000026 and no exclusion", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateAnthropicRequiresInputAndOutputRatesForZeroCounts(t *testing.T) {
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
			contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
				CacheReadInputTokens:     &zero,
				CacheCreationInputTokens: &zero,
			}, tc.rates)
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingRate {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingRate)
			}
		})
	}
}

func TestCalculateAnthropicRequiresRatesForPositiveCacheCounts(t *testing.T) {
	t.Parallel()

	one, zero := int64(1), int64(0)
	rate := mustRate(t, "1")
	tests := []struct {
		name          string
		cacheRead     *int64
		cacheCreation *int64
		rates         pricing.Rates
	}{
		{
			name:          "cache read rate",
			cacheRead:     &one,
			cacheCreation: &zero,
			rates:         pricing.Rates{Input: rate, Output: rate},
		},
		{
			name:          "cache write rate",
			cacheRead:     &zero,
			cacheCreation: &one,
			rates:         pricing.Rates{Input: rate, Output: rate},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
				CacheReadInputTokens:     tc.cacheRead,
				CacheCreationInputTokens: tc.cacheCreation,
			}, tc.rates)
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingRate {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingRate)
			}
		})
	}
}

func TestCalculateAnthropicRequiresBothAggregateCacheCounts(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	rate := mustRate(t, "1")
	tests := []struct {
		name          string
		cacheRead     *int64
		cacheCreation *int64
	}{
		{name: "cache-read count", cacheCreation: &zero},
		{name: "cache-creation count", cacheRead: &zero},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
				CacheReadInputTokens:     tc.cacheRead,
				CacheCreationInputTokens: tc.cacheCreation,
			}, pricing.Rates{Input: rate, Output: rate})
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionMissingUsage {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionMissingUsage)
			}
		})
	}
}

func TestCalculateAnthropicRejectsNegativeChargeableCounts(t *testing.T) {
	t.Parallel()

	rate := mustRate(t, "1")
	tests := []struct {
		name          string
		input         int64
		output        int64
		cacheRead     int64
		cacheCreation int64
	}{
		{name: "input", input: -1},
		{name: "output", output: -1},
		{name: "cache read", cacheRead: -1},
		{name: "cache creation", cacheCreation: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
				InputTokens:              tc.input,
				OutputTokens:             tc.output,
				CacheReadInputTokens:     &tc.cacheRead,
				CacheCreationInputTokens: &tc.cacheCreation,
			}, pricing.Rates{Input: rate, Output: rate, CacheRead: rate, CacheWrite: rate})
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != pricing.ExclusionInconsistentUsage {
				t.Fatalf("exclusion = %q, want %q", contribution.Reason, pricing.ExclusionInconsistentUsage)
			}
		})
	}
}

func TestCalculateAnthropicAppliesExclusionPrecedence(t *testing.T) {
	t.Parallel()

	minusOne, zero, one := int64(-1), int64(0), int64(1)
	rate := mustRate(t, "1")
	tests := []struct {
		name       string
		native     usage.AnthropicUsage
		rates      pricing.Rates
		wantReason pricing.ExclusionReason
	}{
		{
			name:       "missing required rate before missing and inconsistent usage",
			native:     usage.AnthropicUsage{InputTokens: -1, OutputTokens: -1},
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name: "missing positive-cache rate before missing and inconsistent usage",
			native: usage.AnthropicUsage{
				InputTokens:          -1,
				CacheReadInputTokens: &one,
			},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name: "missing positive-cache-write rate before missing and inconsistent usage",
			native: usage.AnthropicUsage{
				OutputTokens:             -1,
				CacheCreationInputTokens: &one,
			},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionMissingRate,
		},
		{
			name: "missing usage before inconsistent usage",
			native: usage.AnthropicUsage{
				InputTokens:              -1,
				OutputTokens:             -1,
				CacheCreationInputTokens: &zero,
			},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionMissingUsage,
		},
		{
			name: "negative cache does not require absent optional rate",
			native: usage.AnthropicUsage{
				CacheReadInputTokens:     &minusOne,
				CacheCreationInputTokens: &zero,
			},
			rates:      pricing.Rates{Input: rate, Output: rate},
			wantReason: pricing.ExclusionInconsistentUsage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateAnthropic(tc.native, tc.rates)
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != tc.wantReason || contribution.Amount.String() != "0" {
				t.Fatalf("contribution = {amount:%s reason:%q}, want no amount and reason %q", contribution.Amount.String(), contribution.Reason, tc.wantReason)
			}
		})
	}
}

func TestCalculateAnthropicIgnoresTTLSubdivisionsAndThinking(t *testing.T) {
	t.Parallel()

	cacheRead, cacheCreation := int64(6000), int64(2000)
	fiveMinutes, oneHour, thinking := int64(750), int64(1250), int64(4)
	tooManyFiveMinutes, tooManyOneHour, thinkingExceedsOutput := int64(3000), int64(4000), int64(10)
	unsupported := int64(-1)
	tests := []struct {
		name       string
		fiveMinute *int64
		oneHour    *int64
		thinking   *int64
	}{
		{name: "missing"},
		{name: "reported", fiveMinute: &fiveMinutes, oneHour: &oneHour, thinking: &thinking},
		{name: "inconsistent", fiveMinute: &tooManyFiveMinutes, oneHour: &tooManyOneHour, thinking: &thinkingExceedsOutput},
		{name: "unsupported negative values", fiveMinute: &unsupported, oneHour: &unsupported, thinking: &unsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
				InputTokens:              12,
				OutputTokens:             9,
				CacheReadInputTokens:     &cacheRead,
				CacheCreationInputTokens: &cacheCreation,
				Ephemeral5mInputTokens:   tc.fiveMinute,
				Ephemeral1hInputTokens:   tc.oneHour,
				ThinkingTokens:           tc.thinking,
			}, pricing.Rates{
				Input:      mustRate(t, "1"),
				Output:     mustRate(t, "2"),
				CacheRead:  mustRate(t, "0.1"),
				CacheWrite: mustRate(t, "1.25"),
			})
			if err != nil {
				t.Fatalf("CalculateAnthropic() error = %v", err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != "0.00313" {
				t.Fatalf("contribution = {amount:%s reason:%q}, want unchanged amount 0.00313 and no exclusion", contribution.Amount.String(), contribution.Reason)
			}
		})
	}
}

func TestCalculateAnthropicTreatsExplicitZeroRatesAsFree(t *testing.T) {
	t.Parallel()

	cacheRead, cacheCreation := int64(3), int64(2)
	zeroRate := mustRate(t, "0")
	contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
		InputTokens:              10,
		OutputTokens:             4,
		CacheReadInputTokens:     &cacheRead,
		CacheCreationInputTokens: &cacheCreation,
	}, pricing.Rates{
		Input:      zeroRate,
		Output:     zeroRate,
		CacheRead:  zeroRate,
		CacheWrite: zeroRate,
	})
	if err != nil {
		t.Fatalf("CalculateAnthropic() error = %v", err)
	}
	if contribution.Reason != "" || contribution.Amount.String() != "0" {
		t.Fatalf("contribution = {amount:%s reason:%q}, want priceable exact zero", contribution.Amount.String(), contribution.Reason)
	}
}

func TestCalculateAnthropicUsesExactSupportedDecimalBounds(t *testing.T) {
	t.Parallel()

	t.Run("smallest fractional rate on four maximum separate counts", func(t *testing.T) {
		maximum := int64(math.MaxInt64)
		smallestRate := mustRate(t, "0.000000000000000001")
		contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
			InputTokens:              maximum,
			OutputTokens:             maximum,
			CacheReadInputTokens:     &maximum,
			CacheCreationInputTokens: &maximum,
		}, pricing.Rates{
			Input:      smallestRate,
			Output:     smallestRate,
			CacheRead:  smallestRate,
			CacheWrite: smallestRate,
		})
		if err != nil {
			t.Fatalf("CalculateAnthropic() error = %v", err)
		}
		const want = "0.000036893488147419103228"
		if contribution.Reason != "" || contribution.Amount.String() != want {
			t.Fatalf("contribution = {amount:%s reason:%q}, want exact minimum-rate amount %s", contribution.Amount.String(), contribution.Reason, want)
		}
	})

	t.Run("maximum rates on four maximum separate counts", func(t *testing.T) {
		maximum := int64(math.MaxInt64)
		maximumRate := mustRate(t, "999999999999999999.999999999999999999")
		contribution, err := pricing.CalculateAnthropic(usage.AnthropicUsage{
			InputTokens:              maximum,
			OutputTokens:             maximum,
			CacheReadInputTokens:     &maximum,
			CacheCreationInputTokens: &maximum,
		}, pricing.Rates{
			Input:      maximumRate,
			Output:     maximumRate,
			CacheRead:  maximumRate,
			CacheWrite: maximumRate,
		})
		if err != nil {
			t.Fatalf("CalculateAnthropic() error = %v", err)
		}
		const want = "36893488147419103227999999999999.999963106511852580896772"
		if contribution.Reason != "" || contribution.Amount.String() != want {
			t.Fatalf("contribution = {amount:%s reason:%q}, want exact maximum %s", contribution.Amount.String(), contribution.Reason, want)
		}
	})
}
