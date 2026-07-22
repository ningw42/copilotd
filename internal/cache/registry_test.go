package cache_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
)

func TestRegistryPrimeFansOutConcurrently(t *testing.T) {
	t.Parallel()

	started := make(chan string, 2)
	release := make(chan struct{})
	makeValue := func(name string) *cache.Value[string] {
		return cache.New(cache.Cacheable[string]{
			Name:            name,
			Fallback:        "embedded",
			FallbackVersion: "v1",
			TTL:             time.Hour,
			Fetch: func(ctx context.Context) (string, string, error) {
				started <- name
				select {
				case <-release:
					return "downloaded", "v2", nil
				case <-ctx.Done():
					return "", "", ctx.Err()
				}
			},
			Hash: func(value string) string { return value },
		})
	}
	registry := cache.NewRegistry()
	registry.Register(makeValue("first"))
	registry.Register(makeValue("second"))
	done := make(chan struct{})
	go func() {
		registry.Prime(context.Background())
		close(done)
	}()

	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatal("both refresh attempts did not start concurrently")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Prime did not return after both attempts settled")
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("started entries = %v, want first and second", seen)
	}
}

func TestRegistryPrimeReturnsEarlyOnContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			close(started)
			<-release
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
	})
	registry := cache.NewRegistry()
	registry.Register(value)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registry.Prime(ctx)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Prime did not start refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Prime did not return after context cancellation")
	}
	close(release)
}

func TestRegistryStartLaunchesEachValuesOwnLoop(t *testing.T) {
	t.Parallel()

	tickerA := &fakeTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	tickerB := &fakeTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	created := make(chan time.Duration, 2)
	fetched := make(chan string, 2)
	makeValue := func(name string, ttl time.Duration, ticker cache.Ticker) *cache.Value[string] {
		return cache.New(cache.Cacheable[string]{
			Name:            name,
			Fallback:        "embedded",
			FallbackVersion: "v1",
			TTL:             ttl,
			Fetch: func(context.Context) (string, string, error) {
				fetched <- name
				return "downloaded", "v2", nil
			},
			Hash: func(value string) string { return value },
		}, cache.WithTicker(func(interval time.Duration) cache.Ticker {
			created <- interval
			return ticker
		}))
	}
	registry := cache.NewRegistry()
	registry.Register(makeValue("short", time.Minute, tickerA))
	registry.Register(makeValue("long", 2*time.Minute, tickerB))
	ctx, cancel := context.WithCancel(context.Background())
	registry.Start(ctx)

	intervals := map[time.Duration]bool{}
	for range 2 {
		select {
		case interval := <-created:
			intervals[interval] = true
		case <-time.After(time.Second):
			t.Fatal("Start did not launch both value loops")
		}
	}
	if !intervals[time.Minute] || !intervals[2*time.Minute] {
		t.Fatalf("ticker intervals = %v, want 1m and 2m", intervals)
	}
	tickerA.ticks <- time.Time{}
	tickerB.ticks <- time.Time{}
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-fetched:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("both value loops did not refresh on their own ticks")
		}
	}
	if !seen["short"] || !seen["long"] {
		t.Fatalf("refreshed values = %v, want short and long", seen)
	}

	cancel()
	for name, stopped := range map[string]<-chan struct{}{
		"short": tickerA.stopped,
		"long":  tickerB.stopped,
	} {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatalf("%s value loop did not stop", name)
		}
	}
}

func TestRegistryObserveCollectsOnlyPublishingEntries(t *testing.T) {
	t.Parallel()

	makeValue := func(name string) *cache.Value[string] {
		return cache.New(cache.Cacheable[string]{
			Name:            name,
			Fallback:        "embedded",
			FallbackVersion: "v1",
			Fetch: func(context.Context) (string, string, error) {
				t.Fatal("disabled value fetched during observation")
				return "", "", nil
			},
			Hash: func(value string) string { return value },
		})
	}
	registry := cache.NewRegistry()
	registry.Register(makeValue("release_notes"))
	registry.Register(makeValue(""))

	statuses := registry.Observe()

	if len(statuses) != 1 {
		t.Fatalf("Observe returned %d statuses, want 1: %#v", len(statuses), statuses)
	}
	if got := statuses[0]; got.Name != "release_notes" || got.Source != "fallback" || got.Version != "v1" || got.LastSuccess != nil {
		t.Fatalf("observed status = %#v, want cold release_notes fallback", got)
	}
}

func TestRegistryConcurrentRegisterAndObserve(t *testing.T) {
	t.Parallel()

	registry := cache.NewRegistry()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := range 200 {
			registry.Register(cache.New(cache.Cacheable[string]{
				Name:            fmt.Sprintf("entry_%d", i),
				Fallback:        "embedded",
				FallbackVersion: "v1",
				Fetch: func(context.Context) (string, string, error) {
					return "", "", nil
				},
				Hash: func(value string) string { return value },
			}))
		}
	}()
	go func() {
		defer workers.Done()
		for range 200 {
			for _, status := range registry.Observe() {
				if status.Name == "" || status.Source != "fallback" || status.Version != "v1" {
					t.Errorf("Observe returned invalid status %#v", status)
					return
				}
			}
		}
	}()
	workers.Wait()
}
