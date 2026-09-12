package pricing_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "gpt-priced"})
	if !ok {
		t.Fatal("priced identity has no tariff")
	}
	priced := tariff.Rates(0)
	if optionalRateString(priced.Input) != "1.25" || optionalRateString(priced.Output) != "10" || optionalRateString(priced.CacheRead) != "0" || priced.CacheWrite != nil {
		t.Fatalf("base rates = %#v, want input=1.25 output=10 cache_read=0 and absent cache_write", priced)
	}
	if _, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "gpt-unpriced"}); ok {
		t.Fatal("unpriced identity reported a tariff")
	}
}

func TestSnapshotTariffsAreDetachedForConcurrentCallers(t *testing.T) {
	t.Parallel()

	snapshot, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(`{
		"input":1,"output":2,"cache_read":0.5,"cache_write":3,
		"tiers":[{"input":4,"output":5,"cache_read":0.25,"cache_write":6,"tier":{"type":"context","size":100}}]
	}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	identity := pricing.Identity{Provider: "openai", Model: "model"}
	replacement, err := pricing.ParseRate("99")
	if err != nil {
		t.Fatalf("ParseRate() error = %v", err)
	}

	firstTariff, ok := snapshot.Tariff(identity)
	if !ok {
		t.Fatal("Tariff() did not return the priced identity")
	}
	firstBase, firstTier := firstTariff.Rates(0), firstTariff.Rates(101)
	for _, rates := range []*pricing.Rates{&firstBase, &firstTier} {
		*rates.Input = replacement
		*rates.Output = replacement
		*rates.CacheRead = replacement
		*rates.CacheWrite = replacement
	}
	laterTariff, ok := snapshot.Tariff(identity)
	if !ok {
		t.Fatal("Tariff() stopped returning the priced identity")
	}
	laterBase, laterTier := laterTariff.Rates(0), laterTariff.Rates(101)
	if optionalRateString(laterBase.Input) != "1" || optionalRateString(laterBase.Output) != "2" || optionalRateString(laterBase.CacheRead) != "0.5" || optionalRateString(laterBase.CacheWrite) != "3" || optionalRateString(laterTier.Input) != "4" || optionalRateString(laterTier.Output) != "5" || optionalRateString(laterTier.CacheRead) != "0.25" || optionalRateString(laterTier.CacheWrite) != "6" {
		t.Fatalf("Tariff() after returned-vector mutation = base %#v tier %#v; want original vectors", laterBase, laterTier)
	}

	const callers = 32
	var wait sync.WaitGroup
	failures := make(chan struct{}, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tariff, ok := snapshot.Tariff(identity)
			if !ok {
				failures <- struct{}{}
				return
			}
			got := tariff.Rates(101)
			if optionalRateString(got.Input) != "4" || optionalRateString(got.Output) != "5" || optionalRateString(got.CacheRead) != "0.25" || optionalRateString(got.CacheWrite) != "6" {
				failures <- struct{}{}
				return
			}
			*got.Input = replacement
			*got.Output = replacement
			*got.CacheRead = replacement
			*got.CacheWrite = replacement
		}()
	}
	wait.Wait()
	if len(failures) != 0 {
		t.Fatalf("%d concurrent callers observed a mutated tariff", len(failures))
	}
	finalTariff, ok := snapshot.Tariff(identity)
	if !ok || optionalRateString(finalTariff.Rates(101).Input) != "4" {
		t.Fatalf("Tariff() after concurrent returned-vector mutation = %#v, %t; want original tier", finalTariff, ok)
	}
}

func TestSnapshotTariffSelectsStructuredContextTiersAtStrictBoundaries(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"openai":{"id":"openai","models":{}},
		"anthropic":{"id":"anthropic","models":{}},
		"google":{"id":"google","models":{"tiered":{"id":"tiered","cost":{
			"input":1,"output":2,"cache_write":10,
			"context_over_200k":{"input":80,"output":90},
			"tiers":[
				{"input":5,"output":6,"cache_read":0.5,"tier":{"type":"context","size":200}},
				{"input":3,"output":4,"tier":{"type":"context","size":100}}
			]
		}}}},
		"xai":{"id":"xai","models":{}}
	}`)

	snapshot, err := pricing.ParseSnapshot(context.Background(), raw)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "google", Model: "tiered"})
	if !ok {
		t.Fatal("tiered identity has no tariff")
	}
	read05, write10 := "0.5", "10"
	tests := []struct {
		name                  string
		input                 uint64
		wantInput, wantOutput string
		wantRead, wantWrite   *string
	}{
		{name: "below first threshold", input: 99, wantInput: "1", wantOutput: "2", wantWrite: &write10},
		{name: "equal first threshold", input: 100, wantInput: "1", wantOutput: "2", wantWrite: &write10},
		{name: "above first threshold", input: 101, wantInput: "3", wantOutput: "4"},
		{name: "equal second threshold uses first tier", input: 200, wantInput: "3", wantOutput: "4"},
		{name: "above second threshold", input: 201, wantInput: "5", wantOutput: "6", wantRead: &read05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rates := tariff.Rates(tc.input)
			if optionalRateString(rates.Input) != tc.wantInput || optionalRateString(rates.Output) != tc.wantOutput || optionalRateString(rates.CacheRead) != optionalString(tc.wantRead) || optionalRateString(rates.CacheWrite) != optionalString(tc.wantWrite) {
				t.Fatalf("Rates(%d) = input %q output %q read %q write %q; want %q/%q/%q/%q", tc.input, optionalRateString(rates.Input), optionalRateString(rates.Output), optionalRateString(rates.CacheRead), optionalRateString(rates.CacheWrite), tc.wantInput, tc.wantOutput, optionalString(tc.wantRead), optionalString(tc.wantWrite))
			}
		})
	}
}

func TestSnapshotTariffUsesLegacyContextRatesOnlyWithoutStructuredTiers(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"openai":{"id":"openai","models":{
			"structured":{"id":"structured","cost":{
				"input":1,"output":2,
				"context_over_200k":{"input":8,"output":9},
				"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":272000}}]
			}},
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
	assertRates := func(model string, completeInput uint64, input, output string, cacheRead, cacheWrite *string) {
		t.Helper()
		tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: model})
		if !ok {
			t.Fatalf("%s has no tariff", model)
		}
		got := tariff.Rates(completeInput)
		if optionalRateString(got.Input) != input || optionalRateString(got.Output) != output || optionalRateString(got.CacheRead) != optionalString(cacheRead) || optionalRateString(got.CacheWrite) != optionalString(cacheWrite) {
			t.Fatalf("%s rates at %d input = input %q output %q read %q write %q; want %q/%q/%q/%q", model, completeInput, optionalRateString(got.Input), optionalRateString(got.Output), optionalRateString(got.CacheRead), optionalRateString(got.CacheWrite), input, output, optionalString(cacheRead), optionalString(cacheWrite))
		}
	}
	write8, read015 := "8", "0.15"
	assertRates("structured", 200001, "1", "2", nil, nil)
	assertRates("structured", 272001, "3", "4", nil, nil)
	assertRates("legacy", 200000, "1", "2", nil, nil)
	assertRates("legacy", 200001, "6", "7", nil, &write8)
	assertRates("base", ^uint64(0), "1.5", "2.5", &read015, nil)
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
		{name: "empty rate row is valid", cost: `{}`, ok: true},
		{name: "negative rate", cost: `{"input":-1,"output":2}`},
		{name: "excessive rate", cost: `{"input":1000000000000000000}`},
		{name: "wrong rate type", cost: `{"input":1,"output":"2"}`},
		{name: "null input rate", cost: `{"input":null,"output":2}`},
		{name: "null optional rate", cost: `{"input":1,"output":2,"cache_read":null}`},
		{name: "malformed recognized unselected rate", cost: `{"input":1,"output":2,"reasoning":{},"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":100}}]}`},
		{name: "missing output is valid", cost: `{"input":1}`, ok: true},
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

func TestSnapshotCancellationInterruptsLargeTierProjection(t *testing.T) {
	raw := highCardinalitySnapshot(100_000)
	if len(raw) > 8<<20 {
		t.Fatalf("cancellation fixture is %d bytes, exceeds remote decoded-body contract", len(raw))
	}

	started := time.Now()
	if _, err := pricing.ParseSnapshot(context.Background(), raw); err != nil {
		t.Fatalf("baseline ParseSnapshot() error = %v", err)
	}
	baseline := time.Since(started)

	// ParseSnapshot has no progress callback by design. Calibrating cancellation
	// against the same bounded fixture avoids a fixed host-speed deadline, but
	// this remains overlap evidence rather than a deterministic latency proof.
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(baseline/2, cancel)
	defer timer.Stop()
	snapshot, err := pricing.ParseSnapshot(ctx, raw)
	if ctx.Err() == nil {
		cancel()
		t.Fatal("fixture completed before calibrated cancellation; cancellation overlap was not established")
	}
	if !errors.Is(err, context.Canceled) || snapshot != nil {
		t.Fatalf("ParseSnapshot() after mid-projection cancellation = %#v, %v; want nil, context canceled", snapshot, err)
	}
}

func TestSnapshotComparesBoundedExactExponentThresholds(t *testing.T) {
	t.Parallel()

	valid := snapshotFixture(`{"input":1,"output":2,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":1e2}},{"input":5,"output":6,"tier":{"type":"context","size":2e2}}]}`)
	snapshot, err := pricing.ParseSnapshot(context.Background(), valid)
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
	rates := tariff.Rates(201)
	if !ok || optionalRateString(rates.Input) != "5" || optionalRateString(rates.Output) != "6" {
		t.Fatalf("rates above threshold 2e2 = %#v, %t; want its exact row", rates, ok)
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

func highCardinalitySnapshot(tiers int) []byte {
	var raw strings.Builder
	raw.Grow(tiers * 64)
	raw.WriteString(`{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{"large":{"id":"large","cost":{"input":1,"output":2,"tiers":[`)
	for index := range tiers {
		if index != 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{"input":1,"output":2,"tier":{"type":"context","size":`)
		raw.WriteString(strconv.Itoa(index))
		raw.WriteString(`}}`)
	}
	raw.WriteString(`]}}}}}`)
	return []byte(raw.String())
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
