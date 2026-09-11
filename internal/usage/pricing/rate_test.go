package pricing_test

import (
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestRateParsesExactExpandedDecimalsWithinBounds(t *testing.T) {
	t.Parallel()

	valid := map[string]string{
		"0":       "0",
		"1.2300":  "1.23",
		"1.25e-2": "0.0125",
		"1e+17":   "100000000000000000",
		"100e-20": "0.000000000000000001",
		"0.000000000000000000123456789012345678e18": "0.123456789012345678",
		"1.000000000000000000000000000000000000":    "1",
		"999999999999999999.999999999999999999":     "999999999999999999.999999999999999999",
		"999999999999999999000000000000000000e-18":  "999999999999999999",
	}
	for raw, want := range valid {
		rate, err := pricing.ParseRate(raw)
		if err != nil {
			t.Errorf("ParseRate(%q) error = %v", raw, err)
			continue
		}
		if got := rate.String(); got != want {
			t.Errorf("ParseRate(%q).String() = %q, want %q", raw, got, want)
		}
	}

	invalid := []string{
		"-1", "+1", "01", ".1", "1.", "1e", "1e999", "0e999",
		"1000000000000000000", "0.0000000000000000001", "999999999999999999e1", "1e-19",
		strings.Repeat("1", 129),
	}
	for _, raw := range invalid {
		if rate, err := pricing.ParseRate(raw); err == nil {
			t.Errorf("ParseRate(%q) = %s, want rejection", raw, rate.String())
		}
	}
}
