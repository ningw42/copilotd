package pricing

import (
	"math/big"
	"strings"
)

func formatNonnegativeScaledDecimal(coefficient *big.Int, scale int) string {
	if coefficient == nil || coefficient.Sign() == 0 {
		return "0"
	}
	digits := coefficient.String()
	if scale == 0 {
		return digits
	}
	if len(digits) <= scale {
		return "0." + strings.Repeat("0", scale-len(digits)) + digits
	}
	cut := len(digits) - scale
	return digits[:cut] + "." + digits[cut:]
}
