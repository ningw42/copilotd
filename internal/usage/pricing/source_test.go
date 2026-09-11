package pricing_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
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
	snapshot, status, err := source.Current(context.Background())
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got := len(snapshot.Identities()); got != 113 {
		t.Fatalf("embedded projected identities = %d, want literal audited count 113", got)
	}
	rates, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-5.6-sol"})
	if !ok || rateString(rates.Input) != "8" || rateString(rates.Output) != "30" || rateString(rates.CacheRead) != "0.8" || rateString(rates.CacheWrite) != "10" {
		t.Fatalf("embedded gpt-5.6-sol selected rates = %#v, %t; want audited highest-tier 8/30/0.8/10", rates, ok)
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

	snapshot, status, err := source.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := snapshot.Identities(), []pricing.Identity{{Provider: "openai", Model: "new"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fetched identities = %#v, want full replacement %#v", got, want)
	}
	if _, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "gpt-5.6-sol"}); ok {
		t.Fatal("deleted floor model survived fetched full-snapshot replacement")
	}
	if status.Source != "fetched" || status.Version != "sha256:eb603ea80cffa7d28ced8860aca64564d6caf08362b06e91df159b07a368c53d" || status.LastSuccess == nil {
		t.Fatalf("status = %#v, want fetched content identity and success time", status)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests after refresh and Current = %d, want 1 (Current must not fetch)", requests.Load())
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
	_, firstStatus, err := source.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	registry.Prime(context.Background())
	snapshot, replacementStatus, err := source.Current(context.Background())
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
	deleted, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "deleted"})
	if !ok || rateString(deleted.Input) != "1" || deleted.Output != nil {
		t.Fatalf("rate-deleted model = %#v, %t; want input 1 and absent output", deleted, ok)
	}
	absentBase, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "absent-base"})
	if !ok || absentBase.Input != nil || absentBase.Output != nil || rateString(absentBase.CacheRead) != "0" || absentBase.CacheWrite != nil {
		t.Fatalf("absent-base model = %#v, %t; want absent input/output/write and explicit-zero read", absentBase, ok)
	}
	tiered, ok := snapshot.Rates(pricing.Identity{Provider: "openai", Model: "tiered"})
	if !ok || tiered.Input != nil || rateString(tiered.Output) != "5" || tiered.CacheRead != nil || tiered.CacheWrite != nil {
		t.Fatalf("incomplete highest tier = %#v, %t; want authoritative absent input and output 5 without backfill", tiered, ok)
	}
	observed := registry.Observe()
	if len(observed) != 1 || observed[0].LastAttemptResult == nil || *observed[0].LastAttemptResult != cache.AttemptSuccess {
		t.Fatalf("observation = %#v, want successful rate-deleting replacement", observed)
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
	first, firstStatus, err := source.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry.Prime(context.Background())
	held, heldStatus, err := source.Current(context.Background())
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
				_, status, err := source.Current(context.Background())
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func rateString(rate *pricing.Rate) string {
	if rate == nil {
		return "<absent>"
	}
	return rate.String()
}
