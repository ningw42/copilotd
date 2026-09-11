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
	priced, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-priced"})
	if !ok {
		t.Fatal("priced identity has no selected rates")
	}
	if optionalRateString(priced.Input) != "1.25" || optionalRateString(priced.Output) != "10" || optionalRateString(priced.CacheRead) != "0" || priced.CacheWrite != nil {
		t.Fatalf("selected rates = %#v, want input=1.25 output=10 cache_read=0 and absent cache_write", priced)
	}
	if _, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-unpriced"}); ok {
		t.Fatal("unpriced identity reported selected rates")
	}
}

func TestSnapshotRatesAreDetachedForConcurrentCallers(t *testing.T) {
	t.Parallel()

	snapshot, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(`{"input":1,"output":2,"cache_read":0.5,"cache_write":3}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	identity := pricing.Identity{Provider: "openai", Model: "model"}
	replacement, err := pricing.ParseRate("99")
	if err != nil {
		t.Fatalf("ParseRate() error = %v", err)
	}

	first, ok := snapshot.Rates(identity)
	if !ok || first.Input == nil || first.Output == nil || first.CacheRead == nil || first.CacheWrite == nil {
		t.Fatalf("Rates() = %#v, %t; want complete vector", first, ok)
	}
	*first.Input = replacement
	*first.Output = replacement
	*first.CacheRead = replacement
	*first.CacheWrite = replacement
	later, ok := snapshot.Rates(identity)
	if !ok || optionalRateString(later.Input) != "1" || optionalRateString(later.Output) != "2" || optionalRateString(later.CacheRead) != "0.5" || optionalRateString(later.CacheWrite) != "3" {
		t.Fatalf("Rates() after returned-vector mutation = %#v, %t; want original vector", later, ok)
	}

	const callers = 32
	var wait sync.WaitGroup
	failures := make(chan struct{}, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			got, ok := snapshot.Rates(identity)
			if !ok || optionalRateString(got.Input) != "1" || optionalRateString(got.Output) != "2" || optionalRateString(got.CacheRead) != "0.5" || optionalRateString(got.CacheWrite) != "3" {
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
		t.Fatalf("%d concurrent callers observed a mutated vector", len(failures))
	}
	final, ok := snapshot.Rates(identity)
	if !ok || optionalRateString(final.Input) != "1" || optionalRateString(final.Output) != "2" || optionalRateString(final.CacheRead) != "0.5" || optionalRateString(final.CacheWrite) != "3" {
		t.Fatalf("Rates() after concurrent returned-vector mutation = %#v, %t; want original vector", final, ok)
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
		if optionalRateString(got.Input) != input || optionalRateString(got.Output) != output || optionalRateString(got.CacheRead) != optionalString(cacheRead) || optionalRateString(got.CacheWrite) != optionalString(cacheWrite) {
			t.Fatalf("%s rates = input %q output %q read %q write %q; want %q/%q/%q/%q", model, optionalRateString(got.Input), optionalRateString(got.Output), optionalRateString(got.CacheRead), optionalRateString(got.CacheWrite), input, output, optionalString(cacheRead), optionalString(cacheWrite))
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
	rates, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "model"})
	if !ok || optionalRateString(rates.Input) != "5" || optionalRateString(rates.Output) != "6" {
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
