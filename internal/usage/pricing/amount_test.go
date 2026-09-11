package pricing_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestAmountsParseMultiplyAndAddExactlyWithinTheirContract(t *testing.T) {
	t.Parallel()

	rate, err := pricing.ParseRate("3.75")
	if err != nil {
		t.Fatal(err)
	}
	amount, err := rate.ForTokens(123)
	if err != nil {
		t.Fatal(err)
	}
	if got := amount.String(); got != "0.00046125" {
		t.Fatalf("3.75 per million * 123 tokens = %q, want 0.00046125", got)
	}
	tinyRate, err := pricing.ParseRate("0.000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	tiny, err := tinyRate.ForTokens(1)
	if err != nil || tiny.String() != "0.000000000000000000000001" {
		t.Fatalf("smallest rate contribution = %q, %v; want exact 24-place amount", tiny.String(), err)
	}

	left, _ := pricing.ParseAmount("0.003157")
	right, _ := pricing.ParseAmount("0.000343")
	total, err := left.Add(right)
	if err != nil || total.String() != "0.0035" {
		t.Fatalf("exact sum = %q, %v; want 0.0035", total.String(), err)
	}

	valid := []string{"0", "1", "1000000000000000000000000000000", "0.000000000000000000000001"}
	for _, raw := range valid {
		amount, err := pricing.ParseAmount(raw)
		if err != nil || amount.String() != raw {
			t.Errorf("ParseAmount(%q) = %q, %v", raw, amount.String(), err)
		}
	}
	invalid := []string{"", "-1", "+1", "01", ".1", "1.", "1.0", "1e3", "0.0000000000000000000000001", strings.Repeat("1", 129)}
	for _, raw := range invalid {
		if amount, err := pricing.ParseAmount(raw); err == nil {
			t.Errorf("ParseAmount(%q) = %q, want rejection", raw, amount.String())
		}
	}

	largest, err := pricing.ParseAmount(strings.Repeat("9", 128))
	if err != nil {
		t.Fatal(err)
	}
	one, _ := pricing.ParseAmount("1")
	if _, err := largest.Add(one); !errors.Is(err, pricing.ErrOverflow) {
		t.Fatalf("128-digit carry error = %v, want ErrOverflow", err)
	}
	if _, err := rate.ForTokens(-1); err == nil {
		t.Fatal("ForTokens(-1) accepted a negative token count")
	}
}
