package pricing

import "github.com/ningw42/copilotd/internal/usage"

// CalculateOpenAI selects this tariff's rate vector from complete OpenAI input
// and completed-response service-tier evidence, then values one persisted
// OpenAI-native Turn. Output, reasoning, reported total, and cache subsets never
// influence context-tier selection.
func (t Tariff) CalculateOpenAI(native usage.OpenAIUsage, reportedTier *string) (Contribution, error) {
	base, tiers := t.base, t.tiers
	if t.fast != nil && useFastRates(reportedTier) {
		base, tiers = t.fast.base, t.fast.tiers
	}
	if len(tiers) == 0 {
		return CalculateOpenAI(native, base)
	}
	if native.InputTokens < 0 {
		return Contribution{Reason: ExclusionInconsistentUsage}, nil
	}
	return CalculateOpenAI(native, selectContextRates(base, tiers, uint64(native.InputTokens)))
}

// CalculateOpenAI values one persisted OpenAI-native Turn at the supplied
// selected original-provider rates; it does not select rates or resolve a
// model. InputTokens is complete input, so its cached and cache-write subsets
// are deducted before applying the input rate. OutputTokens is charged once;
// ReasoningTokens and TotalTokens are ignored, whether absent or unsupported.
//
// Input and output rates are always required. Positive known cache counts also
// require their corresponding rates, while known zero counts do not. Both
// cache counts must be present. Persisted chargeable counts are expected to be
// nonnegative, with cache counts inclusive in complete input; unsupported
// negative values and invalid relationships are excluded rather than clamped.
// Exclusions follow missing-rate, missing-usage, then inconsistent-usage order.
// Exact arithmetic errors are returned without a zero or partial contribution.
func CalculateOpenAI(native usage.OpenAIUsage, rates Rates) (Contribution, error) {
	if rates.Input == nil || rates.Output == nil {
		return Contribution{Reason: ExclusionMissingRate}, nil
	}
	if native.CachedTokens != nil && *native.CachedTokens > 0 && rates.CacheRead == nil {
		return Contribution{Reason: ExclusionMissingRate}, nil
	}
	if native.CacheWriteTokens != nil && *native.CacheWriteTokens > 0 && rates.CacheWrite == nil {
		return Contribution{Reason: ExclusionMissingRate}, nil
	}
	if native.CachedTokens == nil || native.CacheWriteTokens == nil {
		return Contribution{Reason: ExclusionMissingUsage}, nil
	}
	cached := *native.CachedTokens
	cacheWrite := *native.CacheWriteTokens
	if native.InputTokens < 0 || native.OutputTokens < 0 || cached < 0 || cacheWrite < 0 ||
		cached > native.InputTokens || cacheWrite > native.InputTokens-cached {
		return Contribution{Reason: ExclusionInconsistentUsage}, nil
	}

	ordinaryInput := native.InputTokens - cached - cacheWrite
	lines := []pricedLine{
		{tokens: ordinaryInput, rate: rates.Input},
		{tokens: native.OutputTokens, rate: rates.Output},
		{tokens: cached, rate: rates.CacheRead},
		{tokens: cacheWrite, rate: rates.CacheWrite},
	}
	return sumPricedLines(lines)
}
