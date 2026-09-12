package pricing_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
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

func TestSnapshotProjectsOpenAIFastDeclarationsWithoutInventingAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		model      string
		wantTariff bool
	}{
		{
			name:       "named Fast without provider metadata or normal cost",
			model:      `{"id":"model","experimental":{"modes":{"FAST":{"cost":{"input":2,"output":4}}}}}`,
			wantTariff: true,
		},
		{
			name:       "named priority",
			model:      `{"id":"model","experimental":{"modes":{"PrIoRiTy":{}}}}`,
			wantTariff: true,
		},
		{
			name:       "differently named wire candidate",
			model:      `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":"PRIORITY"}},"cost":{"input":2,"output":4}}}}}`,
			wantTariff: true,
		},
		{
			name:  "non-Fast mode remains outside projection",
			model: `{"id":"model","experimental":{"modes":{"batch":{"cost":{"input":2,"output":4,"tiers":{"ignored":true}}}}}}`,
		},
		{
			name:  "empty modes",
			model: `{"id":"model","experimental":{"modes":{}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := pricing.ParseSnapshot(t.Context(), snapshotModelFixture(tc.model))
			if err != nil {
				t.Fatalf("ParseSnapshot() error = %v", err)
			}
			if snapshot.IdentityBytes() != len("openai")+len("model") {
				t.Fatalf("IdentityBytes() = %d, want provider/model bytes only", snapshot.IdentityBytes())
			}
			_, gotTariff := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
			if gotTariff != tc.wantTariff {
				t.Fatalf("Tariff() present = %t, want %t", gotTariff, tc.wantTariff)
			}
		})
	}
}

func TestSnapshotRejectsMalformedOrContradictoryOpenAIModes(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", 1025)
	tests := []struct {
		name  string
		model string
		ok    bool
	}{
		{name: "absent experimental", model: `{"id":"model"}`, ok: true},
		{name: "empty experimental", model: `{"id":"model","experimental":{}}`, ok: true},
		{name: "experimental null", model: `{"id":"model","experimental":null}`},
		{name: "experimental wrong type", model: `{"id":"model","experimental":[]}`},
		{name: "modes null", model: `{"id":"model","experimental":{"modes":null}}`},
		{name: "modes wrong type", model: `{"id":"model","experimental":{"modes":[]}}`},
		{name: "empty mode name", model: `{"id":"model","experimental":{"modes":{"":{}}}}`},
		{name: "oversized mode name", model: `{"id":"model","experimental":{"modes":{` + strconv.Quote(oversized) + `:{}}}}`},
		{name: "mode null", model: `{"id":"model","experimental":{"modes":{"fast":null}}}`},
		{name: "mode wrong type", model: `{"id":"model","experimental":{"modes":{"fast":[]}}}`},
		{name: "provider null", model: `{"id":"model","experimental":{"modes":{"fast":{"provider":null}}}}`},
		{name: "provider wrong type", model: `{"id":"model","experimental":{"modes":{"fast":{"provider":[]}}}}`},
		{name: "body null", model: `{"id":"model","experimental":{"modes":{"fast":{"provider":{"body":null}}}}}`},
		{name: "body wrong type", model: `{"id":"model","experimental":{"modes":{"fast":{"provider":{"body":[]}}}}}`},
		{name: "wire null", model: `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":null}}}}}}`},
		{name: "wire empty", model: `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":""}}}}}}`},
		{name: "wire wrong type", model: `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":1}}}}}}`},
		{name: "wire oversized", model: `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":` + strconv.Quote(oversized) + `}}}}}}`},
		{name: "named Fast contradicts wire", model: `{"id":"model","experimental":{"modes":{"fast":{"provider":{"body":{"service_tier":"default"}}}}}}`},
		{name: "reserved default claims Fast wire", model: `{"id":"model","experimental":{"modes":{"DEFAULT":{"provider":{"body":{"service_tier":"priority"}}}}}}`},
		{name: "reserved auto claims Fast wire", model: `{"id":"model","experimental":{"modes":{"auto":{"provider":{"body":{"service_tier":"fast"}}}}}}`},
		{name: "reserved flex claims Fast wire", model: `{"id":"model","experimental":{"modes":{"flex":{"provider":{"body":{"service_tier":"fast"}}}}}}`},
		{name: "reserved scale claims Fast wire", model: `{"id":"model","experimental":{"modes":{"scale":{"provider":{"body":{"service_tier":"fast"}}}}}}`},
		{name: "reserved ultrafast claims Fast wire", model: `{"id":"model","experimental":{"modes":{"ultrafast":{"provider":{"body":{"service_tier":"fast"}}}}}}`},
		{name: "identical duplicate candidates", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":{"input":2}},"priority":{"cost":{"input":2}}}}}`},
		{name: "conflicting duplicate candidates", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":{"input":2}},"priority":{"cost":{"input":3}}}}}`},
		{name: "case-colliding duplicate candidates", model: `{"id":"model","experimental":{"modes":{"fast":{},"FAST":{}}}}`},
		{name: "wire and named duplicate candidates", model: `{"id":"model","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":"priority"}}},"fast":{}}}}`},
		{name: "Fast cost null", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":null}}}}`},
		{name: "Fast cost wrong type", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":[]}}}}`},
		{name: "malformed Fast rate", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":{"reasoning":null}}}}}`},
		{name: "malformed ignored mode rate", model: `{"id":"model","experimental":{"modes":{"batch":{"cost":{"output_audio":false}}}}}`},
		{name: "unsupported Fast tiers", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":{"tiers":[]}}}}}`},
		{name: "unsupported Fast legacy context", model: `{"id":"model","experimental":{"modes":{"fast":{"cost":{"context_over_200k":{}}}}}}`},
		{name: "unknown fields and valid non-Fast mode", model: `{"id":"model","experimental":{"modes":{"batch":{"provider":{"body":{"service_tier":"batch"},"future":true},"cost":{"input":1,"tiers":{"ignored":true}},"future":true}},"future":true}}`, ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pricing.ParseSnapshot(t.Context(), snapshotModelFixture(tc.model))
			if tc.ok && err != nil {
				t.Fatalf("ParseSnapshot() error = %v, want accepted", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ParseSnapshot() error = nil, want rejection")
			}
		})
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

func TestSnapshotFastTariffsRemainImmutableForConcurrentCallers(t *testing.T) {
	t.Parallel()

	snapshot, err := pricing.ParseSnapshot(t.Context(), snapshotModelFixture(`{"id":"model","cost":{
		"input":1,"output":2,"tiers":[{"input":2,"output":3,"tier":{"type":"context","size":100}}]
	},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
	if !ok {
		t.Fatal("Fast fixture has no tariff")
	}
	normal := tariff.Rates(101)
	replacement, err := pricing.ParseRate("99")
	if err != nil {
		t.Fatal(err)
	}
	*normal.Input = replacement
	*normal.Output = replacement

	zero := int64(0)
	priority := "priority"
	native := usage.OpenAIUsage{InputTokens: 150, OutputTokens: 10, CachedTokens: &zero, CacheWriteTokens: &zero}
	const callers = 32
	var wait sync.WaitGroup
	failures := make(chan string, callers)
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			gotTariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
			if !ok {
				failures <- "missing tariff"
				return
			}
			contribution, err := gotTariff.CalculateOpenAI(native, &priority)
			if err != nil || contribution.Reason != "" || contribution.Amount.String() != "0.00066" {
				failures <- fmt.Sprintf("amount=%s reason=%q error=%v", contribution.Amount.String(), contribution.Reason, err)
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("concurrent Fast calculation: %s", failure)
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
	assertRates("legacy", 199999, "1", "2", nil, nil)
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

func TestSnapshotCancellationInterruptsDerivedFastTierConstruction(t *testing.T) {
	const tiers = 5_000
	raw := highCardinalityFastSnapshot(tiers)
	baseline := &countingContext{Context: context.Background()}
	if _, err := pricing.ParseSnapshot(baseline, raw); err != nil {
		t.Fatalf("baseline ParseSnapshot() error = %v", err)
	}
	if baseline.checks <= tiers {
		t.Fatalf("baseline context checks = %d, want more than %d derived-tier checks", baseline.checks, tiers)
	}

	cancelAt := baseline.checks - tiers/2
	cancelled := &countingContext{Context: context.Background(), cancelAt: cancelAt}
	snapshot, err := pricing.ParseSnapshot(cancelled, raw)
	if !errors.Is(err, context.Canceled) || snapshot != nil {
		t.Fatalf("ParseSnapshot() at context check %d = %#v, %v; want nil, context canceled", cancelAt, snapshot, err)
	}
}

func TestSnapshotAcceptsFullUint64ContextThresholds(t *testing.T) {
	t.Parallel()

	snapshot, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(`{
		"input":1,"output":2,
		"tiers":[
			{"input":3,"output":4,"tier":{"type":"context","size":1000000000000000000}},
			{"input":5,"output":6,"tier":{"type":"context","size":18446744073709551615}}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseSnapshot() error = %v", err)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
	atFirst := tariff.Rates(1_000_000_000_000_000_000)
	aboveFirst := tariff.Rates(1_000_000_000_000_000_001)
	atMaximum := tariff.Rates(^uint64(0))
	if !ok || optionalRateString(atFirst.Input) != "1" || optionalRateString(aboveFirst.Input) != "3" || optionalRateString(atMaximum.Input) != "3" {
		t.Fatalf("full-width threshold tariff = at first %#v above first %#v at maximum %#v, %t", atFirst, aboveFirst, atMaximum, ok)
	}
	if _, err := pricing.ParseSnapshot(context.Background(), snapshotFixture(`{"tiers":[{"tier":{"type":"context","size":18446744073709551616}}]}`)); err == nil {
		t.Fatal("ParseSnapshot() accepted a context threshold above uint64")
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
	return snapshotModelFixture(`{"id":"model","cost":` + cost + `}`)
}

func snapshotModelFixture(model string) []byte {
	return []byte(`{"openai":{"id":"openai","models":{"model":` + model + `}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`)
}

func highCardinalityFastSnapshot(tiers int) []byte {
	var raw strings.Builder
	raw.Grow(tiers * 64)
	raw.WriteString(`{"openai":{"id":"openai","models":{"large":{"id":"large","cost":{"input":1,"output":2,"tiers":[`)
	for index := range tiers {
		if index != 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{"input":1,"output":2,"tier":{"type":"context","size":`)
		raw.WriteString(strconv.Itoa(index))
		raw.WriteString(`}}`)
	}
	raw.WriteString(`]},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`)
	return []byte(raw.String())
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

type countingContext struct {
	context.Context
	checks, cancelAt int
}

func (c *countingContext) Err() error {
	c.checks++
	if c.cancelAt > 0 && c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
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
