package pricing

import "math/big"

// deriveFastContextRates preserves each category's same-model Fast premium in
// the selected normal context band. Missing or non-representable categories
// remain absent; no category supplies an operand for another.
func deriveFastContextRates(base, fast, selectedContext Rates) Rates {
	return Rates{
		Input:      deriveFastContextRate(base.Input, fast.Input, selectedContext.Input),
		Output:     deriveFastContextRate(base.Output, fast.Output, selectedContext.Output),
		CacheRead:  deriveFastContextRate(base.CacheRead, fast.CacheRead, selectedContext.CacheRead),
		CacheWrite: deriveFastContextRate(base.CacheWrite, fast.CacheWrite, selectedContext.CacheWrite),
	}
}

func deriveFastContextRate(base, fast, selectedContext *Rate) *Rate {
	if base == nil || fast == nil || selectedContext == nil || base.coefficient.Sign() == 0 {
		return nil
	}
	if fast.coefficient.Sign() == 0 || selectedContext.coefficient.Sign() == 0 {
		return &Rate{coefficient: new(big.Int)}
	}

	numerator := new(big.Int).Mul(new(big.Int).Set(fast.coefficient), selectedContext.coefficient)
	if base.scale > 0 {
		numerator.Mul(numerator, ratePowerOfTen(base.scale))
	}
	denominator := new(big.Int).Set(base.coefficient)
	if scale := fast.scale + selectedContext.scale; scale > 0 {
		denominator.Mul(denominator, ratePowerOfTen(scale))
	}
	common := new(big.Int).GCD(nil, nil, numerator, denominator)
	numerator.Quo(numerator, common)
	denominator.Quo(denominator, common)

	twos := removeFactor(denominator, 2)
	fives := removeFactor(denominator, 5)
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return nil
	}
	scale := max(twos, fives)
	if twos < scale {
		numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(scale-twos)), nil))
	}
	if fives < scale {
		numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(scale-fives)), nil))
	}
	for scale > 0 {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(numerator, big.NewInt(10), remainder)
		if remainder.Sign() != 0 {
			break
		}
		numerator = quotient
		scale--
	}

	integerDigits := len(numerator.String()) - scale
	if integerDigits < 1 {
		integerDigits = 1
	}
	if integerDigits > maxRateExpandedDigits || scale > maxRateExpandedDigits {
		return nil
	}
	return &Rate{coefficient: numerator, scale: scale}
}

func ratePowerOfTen(exponent int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

func removeFactor(value *big.Int, factor int64) int {
	count := 0
	divisor := big.NewInt(factor)
	for {
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(value, divisor, remainder)
		if remainder.Sign() != 0 {
			return count
		}
		value.Set(quotient)
		count++
	}
}
