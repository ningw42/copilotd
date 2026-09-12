package pricing

import "github.com/ningw42/copilotd/internal/usage"

// CalculateAnthropic selects this tariff's rate vector from complete Anthropic
// input and values one persisted Anthropic-native Turn. Complete input is the
// checked sum of uncached input, aggregate cache creation, and cache read;
// output, thinking, and cache TTL subdivisions never influence tier selection.
func (t Tariff) CalculateAnthropic(native usage.AnthropicUsage) (Contribution, error) {
	if len(t.tiers) == 0 {
		return CalculateAnthropic(native, t.rates(0))
	}
	completeInput, reason := anthropicCompleteInput(native)
	if reason != "" {
		if t.missingAnthropicRateInEveryVector(native) {
			return Contribution{Reason: ExclusionMissingRate}, nil
		}
		return Contribution{Reason: reason}, nil
	}
	return CalculateAnthropic(native, t.rates(completeInput))
}

func anthropicCompleteInput(native usage.AnthropicUsage) (uint64, ExclusionReason) {
	if native.CacheReadInputTokens == nil || native.CacheCreationInputTokens == nil {
		return 0, ExclusionMissingUsage
	}
	cacheRead := *native.CacheReadInputTokens
	cacheCreation := *native.CacheCreationInputTokens
	if native.InputTokens < 0 || cacheRead < 0 || cacheCreation < 0 {
		return 0, ExclusionInconsistentUsage
	}
	total := uint64(native.InputTokens)
	for _, count := range []int64{cacheCreation, cacheRead} {
		value := uint64(count)
		if value > ^uint64(0)-total {
			return 0, ExclusionInconsistentUsage
		}
		total += value
	}
	return total, ""
}

func (t Tariff) missingAnthropicRateInEveryVector(native usage.AnthropicUsage) bool {
	if !missingAnthropicRate(native, t.base) {
		return false
	}
	for _, tier := range t.tiers {
		if !missingAnthropicRate(native, tier.rates) {
			return false
		}
	}
	return true
}

// CalculateAnthropic values one persisted Anthropic-native Turn at the supplied
// selected original-provider rates; it does not select rates or resolve a
// model. InputTokens is the uncached remainder, so cache-read and aggregate
// cache-creation input are additive. OutputTokens is charged once; TTL
// subdivisions and ThinkingTokens are ignored, whether absent or unsupported.
//
// Input and output rates are always required. Positive known cache counts also
// require their corresponding rates, while known zero counts do not. Both
// aggregate cache counts must be present. Persisted chargeable counts are
// expected to be nonnegative; unlike OpenAI cache subsets, Anthropic cache
// counts have no inclusive relationship to InputTokens. Unsupported negative
// values are excluded rather than clamped. Exclusions follow missing-rate,
// missing-usage, then inconsistent-usage order. Exact arithmetic errors are
// returned without a zero or partial contribution.
func CalculateAnthropic(native usage.AnthropicUsage, rates Rates) (Contribution, error) {
	if missingAnthropicRate(native, rates) {
		return Contribution{Reason: ExclusionMissingRate}, nil
	}
	if native.CacheReadInputTokens == nil || native.CacheCreationInputTokens == nil {
		return Contribution{Reason: ExclusionMissingUsage}, nil
	}
	cacheRead := *native.CacheReadInputTokens
	cacheCreation := *native.CacheCreationInputTokens
	if native.InputTokens < 0 || native.OutputTokens < 0 || cacheRead < 0 || cacheCreation < 0 {
		return Contribution{Reason: ExclusionInconsistentUsage}, nil
	}

	lines := []pricedLine{
		{tokens: native.InputTokens, rate: rates.Input},
		{tokens: native.OutputTokens, rate: rates.Output},
		{tokens: cacheRead, rate: rates.CacheRead},
		{tokens: cacheCreation, rate: rates.CacheWrite},
	}
	return sumPricedLines(lines)
}

func missingAnthropicRate(native usage.AnthropicUsage, rates Rates) bool {
	return rates.Input == nil || rates.Output == nil ||
		native.CacheReadInputTokens != nil && *native.CacheReadInputTokens > 0 && rates.CacheRead == nil ||
		native.CacheCreationInputTokens != nil && *native.CacheCreationInputTokens > 0 && rates.CacheWrite == nil
}
