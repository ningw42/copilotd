package pricing

// ExclusionReason explains why native counts and selected rates produce no
// monetary contribution. Model-resolution exclusions belong to reporting, not
// native calculators. An empty reason means the Turn is fully priceable,
// including a zero amount.
type ExclusionReason string

const (
	// ExclusionMissingRate means one rate required by known chargeable counts
	// was absent from the selected vector.
	ExclusionMissingRate ExclusionReason = "missing_rate"
	// ExclusionMissingUsage means a native count required by the formula was
	// not reported.
	ExclusionMissingUsage ExclusionReason = "missing_usage"
	// ExclusionInconsistentUsage means reported chargeable counts violate the
	// native formula's nonnegative or inclusive-subset relationships.
	ExclusionInconsistentUsage ExclusionReason = "inconsistent_usage"
)

// Contribution is the exact valuation outcome for one native Turn. Amount is
// meaningful when Reason is empty; otherwise the whole Turn is excluded.
type Contribution struct {
	Amount Amount
	Reason ExclusionReason
}

type pricedLine struct {
	tokens int64
	rate   *Rate
}

func sumPricedLines(lines []pricedLine) (Contribution, error) {
	var total Amount
	for _, line := range lines {
		if line.tokens == 0 {
			continue
		}
		part, err := line.rate.ForTokens(line.tokens)
		if err != nil {
			return Contribution{}, err
		}
		total, err = total.Add(part)
		if err != nil {
			return Contribution{}, err
		}
	}
	return Contribution{Amount: total}, nil
}
