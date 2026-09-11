package pricing

import (
	"errors"
	"math/big"
	"strings"
)

// Rate is an immutable exact USD-per-million-token decimal.
type Rate struct {
	coefficient *big.Int
	scale       int
}

// ParseRate parses an exact nonnegative JSON number. Its canonical expansion
// is bounded to 18 integer and 18 fractional digits.
func ParseRate(raw string) (Rate, error) {
	const (
		maxSourceBytes = 128
		maxDigits      = 18
		maxExponent    = 36
	)
	if raw == "" || len(raw) > maxSourceBytes {
		return Rate{}, errors.New("invalid rate length")
	}

	index := 0
	if raw[index] == '-' || raw[index] == '+' {
		return Rate{}, errors.New("rate must be nonnegative")
	}
	integerStart := index
	switch {
	case raw[index] == '0':
		index++
		if index < len(raw) && raw[index] >= '0' && raw[index] <= '9' {
			return Rate{}, errors.New("rate has a leading zero")
		}
	case raw[index] >= '1' && raw[index] <= '9':
		for index < len(raw) && raw[index] >= '0' && raw[index] <= '9' {
			index++
		}
	default:
		return Rate{}, errors.New("rate has no integer part")
	}
	integer := raw[integerStart:index]

	fraction := ""
	if index < len(raw) && raw[index] == '.' {
		index++
		start := index
		for index < len(raw) && raw[index] >= '0' && raw[index] <= '9' {
			index++
		}
		if start == index {
			return Rate{}, errors.New("rate has an empty fraction")
		}
		fraction = raw[start:index]
	}
	exponent := 0
	if index < len(raw) && (raw[index] == 'e' || raw[index] == 'E') {
		index++
		sign := 1
		if index < len(raw) && (raw[index] == '+' || raw[index] == '-') {
			if raw[index] == '-' {
				sign = -1
			}
			index++
		}
		start := index
		for index < len(raw) && raw[index] >= '0' && raw[index] <= '9' {
			if index-start >= 2 {
				return Rate{}, errors.New("rate exponent is excessive")
			}
			exponent = exponent*10 + int(raw[index]-'0')
			index++
		}
		if start == index || exponent > maxExponent {
			return Rate{}, errors.New("rate exponent is invalid")
		}
		exponent *= sign
	}
	if index != len(raw) {
		return Rate{}, errors.New("rate is not a JSON number")
	}

	coefficient, ok := new(big.Int).SetString(integer+fraction, 10)
	if !ok {
		return Rate{}, errors.New("invalid rate coefficient")
	}
	scale := len(fraction) - exponent
	if scale < 0 {
		coefficient.Mul(coefficient, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-scale)), nil))
		scale = 0
	}
	for scale > 0 && coefficient.Sign() != 0 && new(big.Int).Rem(coefficient, big.NewInt(10)).Sign() == 0 {
		coefficient.Quo(coefficient, big.NewInt(10))
		scale--
	}
	if coefficient.Sign() == 0 {
		scale = 0
	}
	integerDigits := len(coefficient.String()) - scale
	if integerDigits < 1 {
		integerDigits = 1
	}
	if integerDigits > maxDigits || scale > maxDigits {
		return Rate{}, errors.New("rate exceeds expanded decimal bounds")
	}
	return Rate{coefficient: coefficient, scale: scale}, nil
}

// String returns the canonical, non-exponent decimal spelling of the rate.
func (r Rate) String() string {
	if r.coefficient == nil || r.coefficient.Sign() == 0 {
		return "0"
	}
	digits := r.coefficient.String()
	if r.scale == 0 {
		return digits
	}
	if len(digits) <= r.scale {
		return "0." + strings.Repeat("0", r.scale-len(digits)) + digits
	}
	cut := len(digits) - r.scale
	return digits[:cut] + "." + digits[cut:]
}
