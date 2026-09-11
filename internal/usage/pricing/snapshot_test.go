package pricing_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestSnapshotProjectsAllowedProviderIdentitiesAndPreservesUnpricedModels(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"openai":{"id":"openai","models":{
			"gpt-priced":{"id":"gpt-priced","cost":{"input":1.25,"output":10,"cache_read":0}},
			"gpt-unpriced":{"id":"gpt-unpriced","future":{"ignored":true}}
		}},
		"anthropic":{"id":"anthropic","models":{}},
		"google":{"id":"google","models":{}},
		"xai":{"id":"xai","models":{}},
		"reseller":{"models":"irrelevant additive data"},
		"future":{"anything":[1,2,3]}
	}`)

	snapshot, err := pricing.ParseSnapshot(context.Background(), raw)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	wantIdentities := []pricing.Identity{
		{Provider: "openai", Model: "gpt-priced"},
		{Provider: "openai", Model: "gpt-unpriced"},
	}
	if got := snapshot.Identities(); !reflect.DeepEqual(got, wantIdentities) {
		t.Fatalf("identities = %#v, want %#v", got, wantIdentities)
	}
	priced, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-priced"})
	if !ok {
		t.Fatal("priced identity has no selected rates")
	}
	if priced.Input.String() != "1.25" || priced.Output.String() != "10" || priced.CacheRead == nil || priced.CacheRead.String() != "0" || priced.CacheWrite != nil {
		t.Fatalf("selected rates = %#v, want input=1.25 output=10 cache_read=0 and absent cache_write", priced)
	}
	if _, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-unpriced"}); ok {
		t.Fatal("unpriced identity reported selected rates")
	}
}

func TestSnapshotSelectsHighestStructuredTierThenLegacyThenBase(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"openai":{"id":"openai","models":{
			"structured":{"id":"structured","cost":{
				"input":1,"output":2,"cache_write":3,
				"context_over_200k":{"input":8,"output":9,"cache_write":10},
				"tiers":[
					{"input":3,"output":4,"cache_write":9,"tier":{"type":"context","size":100000}},
					{"input":4,"output":5,"cache_read":0.4,"tier":{"type":"context","size":200000}}
				]
			},"experimental":{"modes":{"fast":{"cost":{"input":99,"output":99}}}}},
			"legacy":{"id":"legacy","cost":{"input":1,"output":2,"context_over_200k":{"input":6,"output":7,"cache_write":8}}},
			"base":{"id":"base","cost":{"input":1.5,"output":2.5,"cache_read":0.15}}
		}},
		"anthropic":{"id":"anthropic","models":{}},
		"google":{"id":"google","models":{}},
		"xai":{"id":"xai","models":{}}
	}`)

	snapshot, err := pricing.ParseSnapshot(context.Background(), raw)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	assertRates := func(model, input, output string, cacheRead, cacheWrite *string) {
		t.Helper()
		got, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: model})
		if !ok {
			t.Fatalf("%s has no selected rates", model)
		}
		if got.Input.String() != input || got.Output.String() != output || optionalRateString(got.CacheRead) != optionalString(cacheRead) || optionalRateString(got.CacheWrite) != optionalString(cacheWrite) {
			t.Fatalf("%s rates = input %s output %s read %q write %q; want %s/%s/%q/%q", model, got.Input.String(), got.Output.String(), optionalRateString(got.CacheRead), optionalRateString(got.CacheWrite), input, output, optionalString(cacheRead), optionalString(cacheWrite))
		}
	}
	read04, write8, read015 := "0.4", "8", "0.15"
	assertRates("structured", "4", "5", &read04, nil)
	assertRates("legacy", "6", "7", nil, &write8)
	assertRates("base", "1.5", "2.5", &read015, nil)
}

func TestSnapshotRejectsAmbiguousJSONAndInvalidSelectedIdentities(t *testing.T) {
	t.Parallel()

	valid := `{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}},"future":{"accepted":true}}`
	longID := strings.Repeat("m", 1025)
	tests := []struct {
		name string
		raw  []byte
		ok   bool
	}{
		{name: "valid Unicode identity", raw: []byte(strings.Replace(valid, `"models":{}`, `"models":{"caf\u00e9-\ud83d\ude00":{"id":"caf\u00e9-\ud83d\ude00"}}`, 1)), ok: true},
		{name: "duplicate selected provider", raw: []byte(strings.Replace(valid, `"openai":`, `"openai":{"id":"openai","models":{}},"op\u0065nai":`, 1))},
		{name: "duplicate additive member", raw: []byte(strings.Replace(valid, `"accepted":true`, `"accepted":true,"acc\u0065pted":false`, 1))},
		{name: "invalid UTF-8", raw: append([]byte(valid[:len(valid)-1]+`,"bad":"`), append([]byte{0xff}, []byte(`"}`)...)...)},
		{name: "unpaired surrogate", raw: []byte(strings.Replace(valid, `"accepted":true`, `"accepted":"\ud800"`, 1))},
		{name: "missing required provider", raw: []byte(strings.Replace(valid, `,"google":{"id":"google","models":{}}`, "", 1))},
		{name: "mismatched model id", raw: []byte(strings.Replace(valid, `"models":{}`, `"models":{"key":{"id":"other"}}`, 1))},
		{name: "oversized model identity", raw: []byte(strings.Replace(valid, `"models":{}`, `"models":{"`+longID+`":{"id":"`+longID+`"}}`, 1))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pricing.ParseSnapshot(context.Background(), tc.raw)
			if tc.ok && err != nil {
				t.Fatalf("ParseSnapshot() error = %v, want accepted", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ParseSnapshot() error = nil, want rejection")
			}
		})
	}
}

func TestSnapshotValidatesEveryRecognizedStandardCostFieldAndTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cost string
		ok   bool
	}{
		{name: "all recognized rates", cost: `{"input":1,"output":2,"reasoning":3,"cache_read":4,"cache_write":5,"input_audio":6,"output_audio":7}`, ok: true},
		{name: "unknown price field is additive", cost: `{"input":1,"output":2,"future_price":{"anything":true}}`, ok: true},
		{name: "negative rate", cost: `{"input":-1,"output":2}`},
		{name: "wrong rate type", cost: `{"input":1,"output":"2"}`},
		{name: "null optional rate", cost: `{"input":1,"output":2,"cache_read":null}`},
		{name: "malformed recognized unselected rate", cost: `{"input":1,"output":2,"reasoning":{},"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":100}}]}`},
		{name: "missing required output", cost: `{"input":1}`},
		{name: "tiers wrong type", cost: `{"input":1,"output":2,"tiers":{}}`},
		{name: "tier descriptor missing", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4}]}`},
		{name: "tier type unknown", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"other","size":100}}]}`},
		{name: "tier threshold fractional", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":1.5}}]}`},
		{name: "tier threshold negative", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":-1}}]}`},
		{name: "duplicate tier threshold", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":100}},{"input":5,"output":6,"tier":{"type":"context","size":100}}]}`},
		{name: "malformed lower tier rate", cost: `{"input":1,"output":2,"tiers":[{"input":3,"output":4,"cache_write":null,"tier":{"type":"context","size":100}},{"input":5,"output":6,"tier":{"type":"context","size":200}}]}`},
		{name: "malformed base rate below tier", cost: `{"input":1,"output":2,"input_audio":false,"tiers":[{"input":5,"output":6,"tier":{"type":"context","size":200}}]}`},
		{name: "malformed legacy rate below tier", cost: `{"input":1,"output":2,"context_over_200k":{"input":3,"output":4,"output_audio":null},"tiers":[{"input":5,"output":6,"tier":{"type":"context","size":200}}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(tc.cost))
			if tc.ok && err != nil {
				t.Fatalf("ParseSnapshot() error = %v, want accepted", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ParseSnapshot() error = nil, want rejection")
			}
		})
	}
}

func TestSnapshotComparesBoundedExactExponentThresholds(t *testing.T) {
	t.Parallel()

	valid := snapshotFixture(`{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":1e2}},{"input":5,"output":6,"tier":{"type":"context","size":2e2}}]}`)
	snapshot, err := pricing.ParseSnapshot(context.Background(), valid)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	rates, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "model"})
	if !ok || rates.Input.String() != "5" || rates.Output.String() != "6" {
		t.Fatalf("selected rates = %#v, %t; want threshold 2e2 row", rates, ok)
	}

	for _, cost := range []string{
		`{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":1e999}}]}`,
		`{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":1e2}},{"input":5,"output":6,"tier":{"type":"context","size":100}}]}`,
	} {
		if _, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(cost)); err == nil {
			t.Fatalf("ParseSnapshot() accepted invalid thresholds in %s", cost)
		}
	}
}

func snapshotFixture(cost string) []byte {
	return []byte(`{"openai":{"id":"openai","models":{"model":{"id":"model","cost":` + cost + `}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`)
}

func optionalRateString(rate *pricing.Rate) string {
	if rate == nil {
		return "<absent>"
	}
	return rate.String()
}

func optionalString(value *string) string {
	if value == nil {
		return "<absent>"
	}
	return *value
}
