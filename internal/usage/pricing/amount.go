package pricing

import (
	"errors"
	"math/big"
	"strings"
)

// ErrOverflow reports an exact result outside the supported canonical amount
// representation. Results are never rounded or saturated.
var ErrOverflow = errors.New("pricing amount overflow")

// Amount is an immutable exact nonnegative USD amount with at most 24
// fractional digits.
type Amount struct {
	coefficient *big.Int
	scale       int
}

// ParseAmount parses a canonical non-exponent decimal string of at most 128
// bytes. Exact zero is spelled "0".
func ParseAmount(raw string) (Amount, error) {
	if raw == "" || len(raw) > 128 {
		return Amount{}, errors.New("invalid amount length")
	}
	integer, fraction, dotted := strings.Cut(raw, ".")
	if integer == "" || integer[0] < '0' || integer[0] > '9' || len(integer) > 1 && integer[0] == '0' {
		return Amount{}, errors.New("amount has invalid integer part")
	}
	for _, digit := range integer {
		if digit < '0' || digit > '9' {
			return Amount{}, errors.New("amount is not a decimal")
		}
	}
	if dotted {
		if fraction == "" || len(fraction) > 24 || fraction[len(fraction)-1] == '0' {
			return Amount{}, errors.New("amount has invalid fractional part")
		}
		for _, digit := range fraction {
			if digit < '0' || digit > '9' {
				return Amount{}, errors.New("amount is not a decimal")
			}
		}
	}
	coefficient, ok := new(big.Int).SetString(integer+fraction, 10)
	if !ok || coefficient.Sign() == 0 && raw != "0" {
		return Amount{}, errors.New("amount is not canonical")
	}
	return Amount{coefficient: coefficient, scale: len(fraction)}, nil
}

// ForTokens values a nonnegative token count at this USD-per-million rate.
func (r Rate) ForTokens(tokens int64) (Amount, error) {
	if tokens < 0 {
		return Amount{}, errors.New("token count must be nonnegative")
	}
	if tokens == 0 || r.coefficient == nil || r.coefficient.Sign() == 0 {
		return Amount{}, nil
	}
	coefficient := new(big.Int).Mul(new(big.Int).Set(r.coefficient), big.NewInt(tokens))
	return newAmount(coefficient, r.scale+6)
}

// Add returns the exact sum without modifying either operand.
func (a Amount) Add(other Amount) (Amount, error) {
	scale := a.scale
	if other.scale > scale {
		scale = other.scale
	}
	left := amountCoefficient(a)
	right := amountCoefficient(other)
	if a.scale < scale {
		left.Mul(left, powerOfTen(scale-a.scale))
	}
	if other.scale < scale {
		right.Mul(right, powerOfTen(scale-other.scale))
	}
	return newAmount(left.Add(left, right), scale)
}

func amountCoefficient(amount Amount) *big.Int {
	if amount.coefficient == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(amount.coefficient)
}

func newAmount(coefficient *big.Int, scale int) (Amount, error) {
	if coefficient == nil || coefficient.Sign() == 0 {
		return Amount{}, nil
	}
	for scale > 0 && new(big.Int).Rem(coefficient, big.NewInt(10)).Sign() == 0 {
		coefficient.Quo(coefficient, big.NewInt(10))
		scale--
	}
	amount := Amount{coefficient: coefficient, scale: scale}
	if scale > 24 || len(amount.String()) > 128 {
		return Amount{}, ErrOverflow
	}
	return amount, nil
}

func powerOfTen(exponent int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
}

// String returns the canonical non-exponent decimal spelling of the amount.
func (a Amount) String() string {
	return formatNonnegativeScaledDecimal(a.coefficient, a.scale)
}
