package pricing_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
)

func TestCachedSourceServesValidatedEmbeddedFloorWhenRefreshIsPinned(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	remote := pricing.NewRemote(pricing.ModelsDevURL, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		t.Fatal("pricing remote called while refresh is pinned")
		return nil, nil
	}))
	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{}, remote, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got := len(snapshot.Identities()); got != 113 {
		t.Fatalf("embedded projected identities = %d, want literal audited count 113", got)
	}
	tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	baseRates, contextRates := tariff.Rates(272000), tariff.Rates(272001)
	if !ok || rateString(baseRates.Input) != "4" || rateString(baseRates.Output) != "20" || rateString(baseRates.CacheRead) != "0.4" || rateString(baseRates.CacheWrite) != "5" || rateString(contextRates.Input) != "8" || rateString(contextRates.Output) != "30" || rateString(contextRates.CacheRead) != "0.8" || rateString(contextRates.CacheWrite) != "10" {
		t.Fatalf("embedded gpt-5.6-sol tariff = base %#v context %#v, %t; want audited base 4/20/0.4/5 and above-272k 8/30/0.8/10", baseRates, contextRates, ok)
	}
	priority := "priority"
	zero := int64(0)
	for _, tc := range []struct {
		name       string
		input      int64
		wantAmount string
	}{
		{name: "explicit Fast base", input: 272000, wantAmount: "2.176"},
		{name: "derived Fast context", input: 272001, wantAmount: "4.352016"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contribution, err := tariff.CalculateOpenAI(usage.OpenAIUsage{InputTokens: tc.input, CachedTokens: &zero, CacheWriteTokens: &zero}, &priority)
			if err != nil {
				t.Fatal(err)
			}
			if contribution.Reason != "" || contribution.Amount.String() != tc.wantAmount {
				t.Fatalf("embedded Fast contribution = {amount:%s reason:%q}, want %s", contribution.Amount.String(), contribution.Reason, tc.wantAmount)
			}
		})
	}
	if status.Source != "fallback" || status.Version != "sha256:db685655368231dce789b060e493a21899cb861c9930cc52521795776ab6e3b5" || status.LastSuccess != nil {
		t.Fatalf("snapshot status = %#v, want cold identified fallback", status)
	}
	observed := registry.Observe()
	if len(observed) != 1 || observed[0].Name != "usage_prices" || observed[0].Source != "fallback" || observed[0].Version != status.Version {
		t.Fatalf("cache observation = %#v, want registered usage_prices floor", observed)
	}
	if requests.Load() != 0 {
		t.Fatalf("pricing requests = %d, want 0", requests.Load())
	}
}

func TestCachedSourceReusesParsedSnapshotForContentVersion(t *testing.T) {
	t.Parallel()

	const artifact = `{"openai":{"id":"openai","models":{"cached":{"id":"cached","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	source := cachedSourceForArtifact(t, []byte(artifact))
	limit := pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20}

	first, firstStatus, err := source.Current(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	second, secondStatus, err := source.Current(context.Background(), limit)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("unchanged pricing content was parsed into a second immutable snapshot")
	}
	if !reflect.DeepEqual(secondStatus, firstStatus) {
		t.Fatalf("reused snapshot status = %#v, want current %#v", secondStatus, firstStatus)
	}
	if snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{}); !errors.Is(err, pricing.ErrProjectionLimit) || snapshot != nil || status != (pricing.SnapshotStatus{}) {
		t.Fatalf("cached snapshot under zero-byte limit = %#v, %#v, %v; want projection-limit rejection", snapshot, status, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if snapshot, status, err := source.Current(cancelled, limit); !errors.Is(err, context.Canceled) || snapshot != nil || status != (pricing.SnapshotStatus{}) {
		t.Fatalf("cached snapshot under canceled context = %#v, %#v, %v; want cancellation", snapshot, status, err)
	}
}

func TestCachedSourceConcurrentMissDoesNotBlockCanceledWaiter(t *testing.T) {
	const artifact = `{"openai":{"id":"openai","models":{"cached":{"id":"cached","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	limit := pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20}
	for _, test := range []struct {
		name   string
		limit  pricing.ProjectionLimit
		cancel bool
		err    error
	}{
		{name: "successful projection", limit: limit},
		{name: "canceled projection", limit: limit, cancel: true, err: context.Canceled},
		{name: "limited projection", err: pricing.ErrProjectionLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := cachedSourceForArtifact(t, []byte(artifact))
			type result struct {
				snapshot *pricing.Snapshot
				status   pricing.SnapshotStatus
				err      error
			}
			var workers sync.WaitGroup
			start := func(ctx context.Context, limit pricing.ProjectionLimit) <-chan result {
				done := make(chan result, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					snapshot, status, err := source.Current(ctx, limit)
					done <- result{snapshot, status, err}
				}()
				return done
			}

			leaderBase, cancelLeader := context.WithCancel(context.Background())
			release := make(chan struct{})
			releaseLeader := sync.OnceFunc(func() { close(release) })
			t.Cleanup(func() {
				cancelLeader()
				releaseLeader()
				workers.Wait()
			})
			// Current checks cancellation before and after acquisition. Pause
			// the next checkpoint, inside projection of the tiny accepted miss.
			leader := &currentCheckpointContext{Context: leaderBase, at: 3, entered: make(chan struct{}), release: release}
			leaderDone := start(leader, test.limit)
			<-leader.entered

			waiterBase, cancelWaiter := context.WithCancel(context.Background())
			t.Cleanup(cancelWaiter)
			waiter := &currentCheckpointContext{Context: waiterBase, at: 1, entered: make(chan struct{})}
			waiterDone := start(waiter, limit)
			<-waiter.entered
			// The waiter's initial Err already sampled nil. Cancellation must
			// therefore interrupt acquisition, not take the pre-canceled path.
			cancelWaiter()
			select {
			case got := <-waiterDone:
				if !errors.Is(got.err, context.Canceled) || got.snapshot != nil || got.status != (pricing.SnapshotStatus{}) {
					t.Fatalf("canceled waiter = %#v; want no snapshot/status and context.Canceled", got)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled waiter did not return while the first projection remained paused")
			}

			follower := &currentCheckpointContext{Context: context.Background(), at: 1, entered: make(chan struct{})}
			followerDone := start(follower, limit)
			<-follower.entered
			if test.cancel {
				cancelLeader()
			}
			releaseLeader()
			first, next := <-leaderDone, <-followerDone
			if !errors.Is(first.err, test.err) {
				t.Fatalf("first projection error = %v, want %v", first.err, test.err)
			}
			if test.err != nil && (first.snapshot != nil || first.status != (pricing.SnapshotStatus{})) {
				t.Fatalf("failed projection returned partial data: %#v", first)
			}
			if next.err != nil || next.snapshot == nil || next.status.Source != "fetched" || next.status.LastSuccess == nil {
				t.Fatalf("successful follower = %#v", next)
			}
			wantIdentities := []pricing.Identity{{Provider: "openai", Model: "cached"}}
			if got := next.snapshot.Identities(); !reflect.DeepEqual(got, wantIdentities) {
				t.Fatalf("follower identities = %#v, want complete projection %#v", got, wantIdentities)
			}
			tariff, ok := next.snapshot.Tariff(wantIdentities[0])
			rates := tariff.Rates(0)
			if !ok || rateString(rates.Input) != "1" || rateString(rates.Output) != "2" {
				t.Fatalf("follower reused incomplete tariff: %#v, %t", rates, ok)
			}
			if test.err == nil && (first.snapshot != next.snapshot || !reflect.DeepEqual(first.status, next.status)) {
				t.Fatal("successful concurrent misses did not reuse the same immutable snapshot/status")
			}
			again, status, err := source.Current(context.Background(), limit)
			if err != nil || again != next.snapshot || !reflect.DeepEqual(status, next.status) {
				t.Fatalf("unchanged Current = %p, %#v, %v; want reused %p, %#v", again, status, err, next.snapshot, next.status)
			}
		})
	}
}

func TestCachedSourceRefreshReplacesTheWholeSnapshotWithoutCredentials(t *testing.T) {
	t.Parallel()

	const fetched = `{"openai":{"id":"openai","models":{"new":{"id":"new","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		for _, header := range []string{"Authorization", "X-Api-Key", "Cookie", "Editor-Version", "Editor-Plugin-Version", "Copilot-Integration-Id"} {
			if got := r.Header.Get(header); got != "" {
				t.Errorf("%s = %q, want credential-free request", header, got)
			}
		}
		_, _ = io.WriteString(w, fetched)
	}))
	t.Cleanup(server.Close)

	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())

	snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot.Identities(), []pricing.Identity{{Provider: "openai", Model: "new"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fetched identities = %#v, want full replacement %#v", got, want)
	}
	if _, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "gpt-5.6-sol"}); ok {
		t.Fatal("deleted floor model survived fetched full-snapshot replacement")
	}
	if status.Source != "fetched" || status.Version != "sha256:eb603ea80cffa7d28ced8860aca64564d6caf08362b06e91df159b07a368c53d" || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want fetched content identity and success time", status)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests after refresh and Current = %d, want 1 (Current must not fetch)", requests.Load())
	}
}

func TestCachedSourceProjectionLimitStopsBeforeSelectedKeyAllocationGrows(t *testing.T) {
	largeArtifact := selectedModelsArtifact(2_000, 1_024)
	if len(largeArtifact) > 8<<20 {
		t.Fatalf("large artifact is %d bytes, exceeds remote acceptance cap", len(largeArtifact))
	}
	small := cachedSourceForArtifact(t, selectedModelsArtifact(8, 1_024))
	large := cachedSourceForArtifact(t, largeArtifact)

	projectionAllocations := func(source *pricing.CachedSource) float64 {
		t.Helper()
		var snapshot *pricing.Snapshot
		var status pricing.SnapshotStatus
		var err error
		allocations := testing.AllocsPerRun(5, func() {
			snapshot, status, err = source.Current(context.Background(), pricing.ProjectionLimit{})
		})
		if !errors.Is(err, pricing.ErrProjectionLimit) || snapshot != nil || status != (pricing.SnapshotStatus{}) {
			t.Fatalf("zero-byte projection = %#v, %#v, %v; want projection-limit rejection", snapshot, status, err)
		}
		return allocations
	}

	smallAllocations := projectionAllocations(small)
	largeAllocations := projectionAllocations(large)
	t.Logf("zero-byte Current allocation counts: 8 selected models=%.0f, 2,000 selected models=%.0f", smallAllocations, largeAllocations)
	// This is allocation-count evidence, not a peak-live-memory bound. A broad
	// fixed margin allows runtime bookkeeping noise while rejecting work that
	// grows with all 2,000 selected keys before applying the zero-byte limit.
	if largeAllocations > smallAllocations+200 {
		t.Fatalf("zero-byte Current allocations grew with unselected remainder: small=%.0f large=%.0f", smallAllocations, largeAllocations)
	}
}

func TestCachedSourceProjectionLimitDoesNotRejectAcceptedValue(t *testing.T) {
	t.Parallel()

	const fetched = `{"openai":{"id":"openai","models":{"retained":{"id":"retained","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, fetched)
	}))
	t.Cleanup(server.Close)

	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())

	if snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{}); !errors.Is(err, pricing.ErrProjectionLimit) || snapshot != nil || status != (pricing.SnapshotStatus{}) {
		t.Fatalf("zero-byte report projection = %#v, %#v, %v; want isolated projection-limit failure", snapshot, status, err)
	}
	snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil || len(snapshot.Identities()) != 1 || snapshot.IdentityBytes() != len("openai")+len("retained") || status.Source != "fetched" {
		t.Fatalf("accepted value after small report = %#v, %#v, %v", snapshot, status, err)
	}
	observed := registry.Observe()
	if len(observed) != 1 || observed[0].Source != "fetched" || requests.Load() != 1 {
		t.Fatalf("small report changed cache acceptance or fetched again: observation=%#v requests=%d", observed, requests.Load())
	}
}

func TestCachedSourceRefreshAcceptsReplacementWithDeletedRates(t *testing.T) {
	t.Parallel()

	const complete = `{"openai":{"id":"openai","models":{"deleted":{"id":"deleted","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	const replacement = `{"openai":{"id":"openai","models":{"deleted":{"id":"deleted","cost":{"input":1}},"absent-base":{"id":"absent-base","cost":{"cache_read":0}},"tiered":{"id":"tiered","cost":{"input":7,"output":8,"tiers":[{"input":3,"output":4,"tier":{"type":"context","size":100}},{"output":5,"tier":{"type":"context","size":200}}]}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	var request atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if request.Add(1) == 1 {
			_, _ = io.WriteString(w, complete)
			return
		}
		_, _ = io.WriteString(w, replacement)
	}))
	t.Cleanup(server.Close)

	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())
	firstSnapshot, firstStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}

	registry.Prime(context.Background())
	snapshot, replacementStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	wantIdentities := []pricing.Identity{
		{Provider: "openai", Model: "absent-base"},
		{Provider: "openai", Model: "deleted"},
		{Provider: "openai", Model: "tiered"},
	}
	if got := snapshot.Identities(); !reflect.DeepEqual(got, wantIdentities) {
		t.Fatalf("replacement identities = %#v, want %#v", got, wantIdentities)
	}
	if replacementStatus.Source != "fetched" || replacementStatus.Version == firstStatus.Version || replacementStatus.LastSuccess == nil {
		t.Fatalf("replacement status = %#v, first = %#v; want successful distinct fetched replacement", replacementStatus, firstStatus)
	}
	if snapshot == firstSnapshot {
		t.Fatal("distinct pricing content reused the previous parsed snapshot")
	}
	deletedTariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "deleted"})
	deleted := deletedTariff.Rates(0)
	if !ok || rateString(deleted.Input) != "1" || deleted.Output != nil {
		t.Fatalf("rate-deleted model = %#v, %t; want input 1 and absent output", deleted, ok)
	}
	absentTariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "absent-base"})
	absentBase := absentTariff.Rates(0)
	if !ok || absentBase.Input != nil || absentBase.Output != nil || rateString(absentBase.CacheRead) != "0" || absentBase.CacheWrite != nil {
		t.Fatalf("absent-base model = %#v, %t; want absent input/output/write and explicit-zero read", absentBase, ok)
	}
	tieredTariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "tiered"})
	tieredBase, tieredFirst, tieredSecond := tieredTariff.Rates(100), tieredTariff.Rates(101), tieredTariff.Rates(201)
	if !ok || rateString(tieredBase.Input) != "7" || rateString(tieredBase.Output) != "8" || rateString(tieredFirst.Input) != "3" || rateString(tieredFirst.Output) != "4" || tieredSecond.Input != nil || rateString(tieredSecond.Output) != "5" || tieredSecond.CacheRead != nil || tieredSecond.CacheWrite != nil {
		t.Fatalf("replacement tiered tariff = base %#v first %#v second %#v, %t; want all authoritative vectors without backfill", tieredBase, tieredFirst, tieredSecond, ok)
	}
	observed := registry.Observe()
	if len(observed) != 1 || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptSuccess {
		t.Fatalf("observation = %#v, want successful rate-deleting replacement", observed)
	}
}

func TestCachedSourceReplacesDeletedAndUnpricedFastDeclarationsAuthoritatively(t *testing.T) {
	t.Parallel()

	artifacts := []string{
		`{"openai":{"id":"openai","models":{"model":{"id":"model","cost":{"input":1,"output":2},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`,
		`{"openai":{"id":"openai","models":{"model":{"id":"model","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`,
		`{"openai":{"id":"openai","models":{"model":{"id":"model","cost":{"input":1,"output":2},"experimental":{"modes":{"fast":{}}}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`,
	}
	var request atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(request.Add(1)) - 1
		_, _ = io.WriteString(w, artifacts[index])
	}))
	t.Cleanup(server.Close)
	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	priority := "priority"
	zero := int64(0)
	native := usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: &zero, CacheWriteTokens: &zero}
	for index, want := range []struct {
		amount string
		reason pricing.ExclusionReason
	}{
		{amount: "0.000006"},
		{amount: "0.000003"},
		{amount: "0", reason: pricing.ExclusionMissingRate},
	} {
		registry.Prime(context.Background())
		snapshot, _, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		tariff, ok := snapshot.Tariff(pricing.Identity{Provider: "openai", Model: "model"})
		if !ok {
			t.Fatalf("revision %d has no tariff", index)
		}
		contribution, err := tariff.CalculateOpenAI(native, &priority)
		if err != nil {
			t.Fatal(err)
		}
		if contribution.Amount.String() != want.amount || contribution.Reason != want.reason {
			t.Fatalf("revision %d contribution = {amount:%s reason:%q}, want {%s %q}", index, contribution.Amount.String(), contribution.Reason, want.amount, want.reason)
		}
	}
}

func TestCachedSourceRejectsMalformedRefreshAndHoldsLastGood(t *testing.T) {
	t.Parallel()

	const good = `{"openai":{"id":"openai","models":{"kept":{"id":"kept","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	var request atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if request.Add(1) == 1 {
			_, _ = io.WriteString(w, good)
			return
		}
		_, _ = io.WriteString(w, `{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}}}`)
	}))
	t.Cleanup(server.Close)

	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())
	first, firstStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	registry.Prime(context.Background())
	held, heldStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := held.Identities(), first.Identities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("held identities = %#v, want last-good %#v", got, want)
	}
	if heldStatus.Source != "fetched" || heldStatus.Version != firstStatus.Version || heldStatus.LastSuccess == nil || firstStatus.LastSuccess == nil || !heldStatus.LastSuccess.Equal(*firstStatus.LastSuccess) {
		t.Fatalf("held status = %#v, want unchanged last-good %#v", heldStatus, firstStatus)
	}
	observed := registry.Observe()
	if len(observed) != 1 || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptFailure {
		t.Fatalf("observation = %#v, want failed malformed refresh", observed)
	}
}

func TestCachedSourceHoldsLastGoodAfterMalformedOpenAIModeRefresh(t *testing.T) {
	t.Parallel()

	const good = `{"openai":{"id":"openai","models":{"kept":{"id":"kept","cost":{"input":1,"output":2},"experimental":{"modes":{"fast":{"cost":{"input":2,"output":4}}}}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	oversized := strings.Repeat("x", 1025)
	for _, tc := range []struct {
		name  string
		model string
	}{
		{name: "experimental null", model: `{"id":"kept","experimental":null}`},
		{name: "experimental wrong type", model: `{"id":"kept","experimental":[]}`},
		{name: "modes null", model: `{"id":"kept","experimental":{"modes":null}}`},
		{name: "modes wrong type", model: `{"id":"kept","experimental":{"modes":[]}}`},
		{name: "empty mode name", model: `{"id":"kept","experimental":{"modes":{"":{}}}}`},
		{name: "oversized mode name", model: `{"id":"kept","experimental":{"modes":{` + strconv.Quote(oversized) + `:{}}}}`},
		{name: "mode entry null", model: `{"id":"kept","experimental":{"modes":{"fast":null}}}`},
		{name: "mode entry wrong type", model: `{"id":"kept","experimental":{"modes":{"fast":[]}}}`},
		{name: "provider null", model: `{"id":"kept","experimental":{"modes":{"fast":{"provider":null}}}}`},
		{name: "provider wrong type", model: `{"id":"kept","experimental":{"modes":{"fast":{"provider":[]}}}}`},
		{name: "provider body null", model: `{"id":"kept","experimental":{"modes":{"fast":{"provider":{"body":null}}}}}`},
		{name: "provider body wrong type", model: `{"id":"kept","experimental":{"modes":{"fast":{"provider":{"body":[]}}}}}`},
		{name: "wire tier null", model: `{"id":"kept","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":null}}}}}}`},
		{name: "wire tier empty", model: `{"id":"kept","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":""}}}}}}`},
		{name: "wire tier wrong type", model: `{"id":"kept","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":1}}}}}}`},
		{name: "wire tier oversized", model: `{"id":"kept","experimental":{"modes":{"accelerated":{"provider":{"body":{"service_tier":` + strconv.Quote(oversized) + `}}}}}}`},
		{name: "named candidate contradicts mapping", model: `{"id":"kept","experimental":{"modes":{"fast":{"provider":{"body":{"service_tier":"default"}}}}}}`},
		{name: "reserved name contradicts mapping", model: `{"id":"kept","experimental":{"modes":{"flex":{"provider":{"body":{"service_tier":"priority"}}}}}}`},
		{name: "identical duplicate candidates", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":{"input":2}},"priority":{"cost":{"input":2}}}}}`},
		{name: "conflicting duplicate candidates", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":{"input":2}},"priority":{"cost":{"input":3}}}}}`},
		{name: "case-colliding candidates", model: `{"id":"kept","experimental":{"modes":{"fast":{},"FAST":{}}}}`},
		{name: "Fast cost null", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":null}}}}`},
		{name: "Fast cost wrong type", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":[]}}}}`},
		{name: "invalid Fast rate", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":{"reasoning":null}}}}}`},
		{name: "invalid ignored mode rate", model: `{"id":"kept","experimental":{"modes":{"batch":{"cost":{"input":null}}}}}`},
		{name: "unsupported Fast tiers", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":{"tiers":[]}}}}}`},
		{name: "unsupported Fast legacy context", model: `{"id":"kept","experimental":{"modes":{"fast":{"cost":{"context_over_200k":{}}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			malformed := `{"openai":{"id":"openai","models":{"kept":` + tc.model + `}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
			var request atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if request.Add(1) == 1 {
					_, _ = io.WriteString(w, good)
					return
				}
				_, _ = io.WriteString(w, malformed)
			}))
			t.Cleanup(server.Close)
			registry := cache.NewRegistry()
			source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
			limit := pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20}
			registry.Prime(context.Background())
			first, firstStatus, err := source.Current(context.Background(), limit)
			if err != nil {
				t.Fatal(err)
			}
			registry.Prime(context.Background())
			held, heldStatus, err := source.Current(context.Background(), limit)
			if err != nil {
				t.Fatal(err)
			}
			if held != first || heldStatus.Version != firstStatus.Version {
				t.Fatalf("malformed mode refresh replaced last good: first=%p/%+v held=%p/%+v", first, firstStatus, held, heldStatus)
			}
			observed := registry.Observe()
			if len(observed) != 1 || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptFailure {
				t.Fatalf("observation = %#v, want failed refresh retaining last good", observed)
			}
		})
	}
}

func TestCachedSourceRejectsArtifactSizedIdentitiesWithoutArtifactSizedDiagnostics(t *testing.T) {
	const good = `{"openai":{"id":"openai","models":{"kept":{"id":"kept","cost":{"input":1,"output":2}}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`

	tests := []struct {
		name        string
		warm        bool
		invalid     func(string) string
		wantContext string
		wantReason  string
		wantSource  string
	}{
		{
			name: "oversized selected model key holds floor",
			invalid: func(attacker string) string {
				return `{"openai":{"id":"openai","models":{"` + attacker + `":{"id":"kept"}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
			},
			wantContext: `provider "openai"`,
			wantReason:  "keyed identity must be non-empty valid UTF-8 of at most 1024 bytes",
			wantSource:  "fallback",
		},
		{
			name: "mismatched model id holds last good",
			warm: true,
			invalid: func(attacker string) string {
				return `{"openai":{"id":"openai","models":{"kept":{"id":"` + attacker + `"}}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
			},
			wantContext: `model "openai"/"kept"`,
			wantReason:  "does not match keyed identity",
			wantSource:  "fetched",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var diagnostics []string
			for _, size := range []int{2 << 10, 1 << 20} {
				attacker := strings.Repeat("x", size)
				invalid := test.invalid(attacker)
				var request atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if test.warm && request.Add(1) == 1 {
						_, _ = io.WriteString(w, good)
						return
					}
					_, _ = io.WriteString(w, invalid)
				}))
				t.Cleanup(server.Close)

				var logs bytes.Buffer
				logger := logging.ForComponent(slog.New(slog.NewJSONHandler(&logs, nil)), "internal/cache")
				registry := cache.NewRegistry()
				source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, logger)
				if test.warm {
					registry.Prime(context.Background())
					logs.Reset()
				}
				registry.Prime(context.Background())

				snapshot, status, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
				if err != nil {
					t.Fatalf("Current() error = %v", err)
				}
				if status.Source != test.wantSource {
					t.Fatalf("effective source = %q, want held %s", status.Source, test.wantSource)
				}
				if test.warm {
					want := []pricing.Identity{{Provider: "openai", Model: "kept"}}
					if got := snapshot.Identities(); !reflect.DeepEqual(got, want) {
						t.Fatalf("identities after rejected refresh = %#v, want last-good %#v", got, want)
					}
				}
				observed := registry.Observe()
				if len(observed) != 1 || observed[0].Source != test.wantSource || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptFailure {
					t.Fatalf("cache observation = %#v, want failed attempt holding %s", observed, test.wantSource)
				}

				var record map[string]any
				decoder := json.NewDecoder(&logs)
				if err := decoder.Decode(&record); err != nil {
					t.Fatalf("decode refresh failure log: %v", err)
				}
				if record["msg"] != "cached value refresh failed" || record["component"] != "internal/cache" || record["cached_value"] != "usage_prices" {
					t.Fatalf("refresh failure log = %#v", record)
				}
				diagnostic, ok := record["error"].(string)
				if !ok {
					t.Fatalf("refresh failure diagnostic = %#v, want string", record["error"])
				}
				if !strings.Contains(diagnostic, test.wantContext) || !strings.Contains(diagnostic, test.wantReason) {
					t.Fatalf("refresh failure diagnostic = %q, want bounded context %q and reason %q", diagnostic, test.wantContext, test.wantReason)
				}
				if strings.Contains(diagnostic, attacker) {
					t.Fatal("refresh failure diagnostic retained the complete rejected identity")
				}
				diagnostics = append(diagnostics, diagnostic)
			}
			if diagnostics[0] != diagnostics[1] {
				t.Fatalf("refresh failure diagnostic grew with rejected identity: small bytes=%d large bytes=%d", len(diagnostics[0]), len(diagnostics[1]))
			}
		})
	}
}

func TestRemoteRefreshRefusesRedirectsAndBoundsDecodedBodies(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var followed atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			followed.Add(1)
		}))
		t.Cleanup(target.Close)
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		t.Cleanup(redirect.Close)

		registry := cache.NewRegistry()
		pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(redirect.URL, redirect.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
		registry.Prime(context.Background())
		observed := registry.Observe()
		if followed.Load() != 0 || len(observed) != 1 || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptFailure {
			t.Fatalf("followed=%d observation=%#v; want refused failed redirect", followed.Load(), observed)
		}
	})

	t.Run("decoded body cap", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			size     int
			accepted bool
		}{
			{name: "exactly at cap", size: 8 << 20, accepted: true},
			{name: "one byte over cap", size: (8 << 20) + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				decoded := validDecodedSnapshot(test.size)
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(decoded); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(compressed.Bytes())
				}))
				t.Cleanup(server.Close)

				registry := cache.NewRegistry()
				source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
				registry.Prime(context.Background())
				_, status, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: int(^uint(0) >> 1)})
				if err != nil {
					t.Fatalf("Current() error = %v", err)
				}
				observed := registry.Observe()
				if len(observed) != 1 || observed[0].LastAttemptResult == nil {
					t.Fatalf("observation = %#v, want one completed decoded-cap attempt", observed)
				}
				if test.accepted {
					if *observed[0].LastAttemptResult != cache.AttemptSuccess || status.Source != "fetched" {
						t.Fatalf("status=%#v observation=%#v; want valid at-cap response accepted", status, observed)
					}
					return
				}
				if *observed[0].LastAttemptResult != cache.AttemptFailure || status.Source != "fallback" {
					t.Fatalf("status=%#v observation=%#v; want valid over-cap response rejected at fallback", status, observed)
				}
			})
		}
	})
}

func TestRemoteRefreshOwnsFiveSecondContextAndHonorsCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	cancelled := make(chan error, 1)
	remote := pricing.NewRemote("https://models.invalid/api.json", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Error("pricing request has no deadline")
		} else if remaining := time.Until(deadline); remaining < 4*time.Second || remaining > 5*time.Second {
			t.Errorf("pricing request deadline remaining = %v, want approximately 5s", remaining)
		}
		close(started)
		<-req.Context().Done()
		cancelled <- req.Context().Err()
		return nil, req.Context().Err()
	}))
	registry := cache.NewRegistry()
	pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, remote, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registry.Prime(ctx)
		close(done)
	}()
	<-started
	cancel()
	select {
	case err := <-cancelled:
		if err != context.Canceled {
			t.Fatalf("request context error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pricing request did not observe registry cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registry prime did not return after cancellation")
	}
}

func cachedSourceForArtifact(t *testing.T, artifact []byte) *pricing.CachedSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(server.Close)
	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(pricing.CacheConfig{RefreshInterval: time.Hour}, pricing.NewRemote(server.URL, server.Client().Transport), registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())
	status := registry.Observe()
	if len(status) != 1 || status[0].Source != "fetched" {
		t.Fatalf("artifact was not accepted before report projection: %#v", status)
	}
	return source
}

func selectedModelsArtifact(count, identityBytes int) []byte {
	var models strings.Builder
	models.WriteByte('{')
	for index := range count {
		if index != 0 {
			models.WriteByte(',')
		}
		prefix := strconv.Itoa(index) + "-"
		name := prefix + strings.Repeat("x", identityBytes-len(prefix))
		models.WriteString(strconv.Quote(name))
		models.WriteString(`:{"id":`)
		models.WriteString(strconv.Quote(name))
		models.WriteByte('}')
	}
	models.WriteByte('}')
	return []byte(`{"openai":{"id":"openai","models":` + models.String() + `},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`)
}

func validDecodedSnapshot(size int) []byte {
	const prefix = `{"padding":"`
	const suffix = `","openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		panic("requested decoded snapshot is too small")
	}
	decoded := make([]byte, 0, size)
	decoded = append(decoded, prefix...)
	decoded = append(decoded, bytes.Repeat([]byte{'x'}, padding)...)
	decoded = append(decoded, suffix...)
	return decoded
}

// currentCheckpointContext synchronizes concurrent Current calls using only
// their public context. Sampling Err before notification prevents a cancellation
// handshake from accidentally testing only Current's pre-canceled fast path.
// The optional pause holds projection independently of fixture size or speed.
type currentCheckpointContext struct {
	context.Context
	at      int32
	checks  atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (c *currentCheckpointContext) Err() error {
	err := c.Context.Err()
	if c.checks.Add(1) == c.at {
		close(c.entered)
		if c.release != nil {
			<-c.release
		}
	}
	return err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func rateString(rate *pricing.Rate) string {
	if rate == nil {
		return "<absent>"
	}
	return rate.String()
}
