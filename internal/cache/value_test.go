package cache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
)

func TestValueColdServesFallback(t *testing.T) {
	t.Parallel()

	value := cache.New(cache.Cacheable[string]{
		Name:            "release_notes",
		Fallback:        "embedded",
		FallbackVersion: "v1",
		Fetch: func(context.Context) (string, string, error) {
			t.Fatal("fetch called before a refresh attempt")
			return "", "", nil
		},
		Hash: func(value string) string { return value },
	})

	got, status := value.Current()
	if got != "embedded" {
		t.Fatalf("Current value = %q, want fallback %q", got, "embedded")
	}
	if status.Name != "release_notes" {
		t.Fatalf("status name = %q, want %q", status.Name, "release_notes")
	}
	if status.Source != "fallback" {
		t.Fatalf("status source = %q, want fallback", status.Source)
	}
	if status.Version != "v1" {
		t.Fatalf("status version = %q, want %q", status.Version, "v1")
	}
	if status.LastSuccess != nil {
		t.Fatalf("status last success = %v, want nil", status.LastSuccess)
	}
	if status.LastAttempt != nil || status.LastAttemptResult != nil {
		t.Fatalf("status attempt = (%v, %v), want (nil, nil)", status.LastAttempt, status.LastAttemptResult)
	}
}

func TestValueAttemptAtZeroTimeIsNotNeverAttempted(t *testing.T) {
	t.Parallel()

	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
	}, cache.WithClock(func() time.Time { return time.Time{} }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	_, status := value.Current()
	assertAttempt(t, status, time.Time{}, cache.AttemptSuccess)
}

func TestValueRefreshLogsSuccessAndFailureLevels(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	attempts := 0
	value := cache.New(cache.Cacheable[string]{
		Name:            "release_notes",
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			attempts++
			if attempts == 1 {
				return "downloaded", "v2", nil
			}
			return "", "", errors.New("upstream unavailable")
		},
		Hash: func(value string) string { return value },
	}, cache.WithLogger(logger))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	registry.Prime(context.Background())

	decoder := json.NewDecoder(&logs)
	for _, want := range []struct {
		level   string
		message string
	}{
		{"DEBUG", "cached value refresh succeeded"},
		{"WARN", "cached value refresh failed"},
	} {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode %s log: %v", want.level, err)
		}
		if record["level"] != want.level || record["msg"] != want.message {
			t.Fatalf("log = %#v, want level %s message %q", record, want.level, want.message)
		}
		if record["cached_value"] != "release_notes" {
			t.Fatalf("cached value log attribute = %v, want release_notes", record["cached_value"])
		}
	}
	var extra map[string]any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra log record: %v (%#v)", err, extra)
	}
}

func TestValueRunWaitsForTickerAndStopsOnCancellation(t *testing.T) {
	t.Parallel()

	created := make(chan time.Duration, 1)
	ticker := &fakeTicker{ticks: make(chan time.Time), stopped: make(chan struct{})}
	fetches := make(chan struct{}, 1)
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             37 * time.Minute,
		Fetch: func(context.Context) (string, string, error) {
			fetches <- struct{}{}
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
	}, cache.WithTicker(func(interval time.Duration) cache.Ticker {
		created <- interval
		return ticker
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		value.Run(ctx)
		close(done)
	}()

	select {
	case interval := <-created:
		if interval != 37*time.Minute {
			t.Fatalf("ticker interval = %v, want %v", interval, 37*time.Minute)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not create its ticker")
	}
	select {
	case <-fetches:
		t.Fatal("Run fetched before its first tick")
	default:
	}
	ticker.ticks <- time.Time{}
	select {
	case <-fetches:
	case <-time.After(time.Second):
		t.Fatal("Run did not fetch after a tick")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("Run did not stop its ticker")
	}
}

func TestValueDisabledTTLDoesNotPrimeOrRun(t *testing.T) {
	t.Parallel()

	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             0,
		Version: func(context.Context) (string, error) {
			t.Fatal("Version called while refresh is disabled")
			return "", nil
		},
		Fetch: func(context.Context) (string, string, error) {
			t.Fatal("Fetch called while refresh is disabled")
			return "", "", nil
		},
		Hash: func(value string) string { return value },
	}, cache.WithTicker(func(time.Duration) cache.Ticker {
		t.Fatal("ticker created while refresh is disabled")
		return nil
	}))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	value.Run(context.Background())

	got, status := value.Current()
	if got != "embedded" || status.Source != "fallback" || status.LastSuccess != nil {
		t.Fatalf("Current = %q, %#v; want untouched cold fallback", got, status)
	}
	if status.LastAttempt != nil || status.LastAttemptResult != nil {
		t.Fatalf("status attempt = (%v, %v), want (nil, nil)", status.LastAttempt, status.LastAttemptResult)
	}
}

func TestValueConcurrentCurrentAndRefresh(t *testing.T) {
	t.Parallel()

	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
	})
	registry := cache.NewRegistry()
	registry.Register(value)

	var workers sync.WaitGroup
	for range 4 {
		workers.Add(2)
		go func() {
			defer workers.Done()
			for range 200 {
				got, status := value.Current()
				switch status.Source {
				case "fallback":
					if got != "embedded" || status.Version != "v1" {
						t.Errorf("fallback Current = %q, %#v", got, status)
						return
					}
				case "fetched":
					if got != "downloaded" || status.Version != "v2" || status.LastSuccess == nil {
						t.Errorf("fetched Current = %q, %#v", got, status)
						return
					}
				default:
					t.Errorf("Current returned unknown source %q", status.Source)
					return
				}
			}
		}()
		go func() {
			defer workers.Done()
			for range 50 {
				registry.Prime(context.Background())
			}
		}()
	}
	workers.Wait()
}

type fakeTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

func (t *fakeTicker) C() <-chan time.Time { return t.ticks }
func (t *fakeTicker) Stop()               { close(t.stopped) }

func TestValueWarmFetchFailureKeepsLastGood(t *testing.T) {
	t.Parallel()

	firstSuccess := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	now := firstSuccess
	attempts := 0
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			attempts++
			if attempts == 1 {
				return "downloaded", "v2", nil
			}
			return "", "", errors.New("upstream unavailable")
		},
		Hash: func(value string) string { return value },
	}, cache.WithClock(func() time.Time { return now }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	now = now.Add(24 * time.Hour)
	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "downloaded" {
		t.Fatalf("Current value = %q, want last-good %q", got, "downloaded")
	}
	if status.Source != "fetched" || status.Version != "v2" {
		t.Fatalf("status = %#v, want fetched v2", status)
	}
	if status.LastSuccess == nil || !status.LastSuccess.Equal(firstSuccess) {
		t.Fatalf("status last success = %v, want original success %v", status.LastSuccess, firstSuccess)
	}
	assertAttempt(t, status, now, cache.AttemptFailure)
}

func TestValueUnchangedVersionSkipsFetchAndRecordsAttempt(t *testing.T) {
	t.Parallel()

	attemptCompleted := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	versionCalls := 0
	clockCalls := 0
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Version: func(context.Context) (string, error) {
			versionCalls++
			return "v1", nil
		},
		Fetch: func(context.Context) (string, string, error) {
			t.Fatal("Fetch called for unchanged version")
			return "", "", nil
		},
		Hash: func(value string) string { return value },
	}, cache.WithClock(func() time.Time {
		clockCalls++
		return attemptCompleted
	}))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "embedded" || status.Source != "fallback" || status.Version != "v1" {
		t.Fatalf("Current = %q, %#v; want fallback at v1", got, status)
	}
	if status.LastSuccess != nil {
		t.Fatalf("status last success = %v, want nil without a validated fetch", status.LastSuccess)
	}
	assertAttempt(t, status, attemptCompleted, cache.AttemptSuccess)
	if versionCalls != 1 {
		t.Fatalf("Version calls = %d, want 1", versionCalls)
	}
	if clockCalls != 1 {
		t.Fatalf("clock calls = %d, want 1 for the recorded attempt", clockCalls)
	}
}

func TestValueChangedVersionContinuesThroughFetchHashAndValidate(t *testing.T) {
	t.Parallel()

	attemptCompleted := time.Date(2026, 7, 22, 10, 15, 0, 0, time.UTC)
	versionCalls := 0
	fetchCalls := 0
	hashCalls := 0
	validateCalls := 0
	fetchCompleted := false
	validateCompleted := false
	clockObservedCompletion := false
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Version: func(context.Context) (string, error) {
			versionCalls++
			return "v2", nil
		},
		Fetch: func(context.Context) (string, string, error) {
			fetchCalls++
			fetchCompleted = true
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string {
			hashCalls++
			return value
		},
		Validate: func(string) error {
			validateCalls++
			validateCompleted = true
			return nil
		},
	}, cache.WithClock(func() time.Time {
		clockObservedCompletion = fetchCompleted && validateCompleted
		return attemptCompleted
	}))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "downloaded" || status.Source != "fetched" || status.Version != "v2" || status.LastSuccess == nil {
		t.Fatalf("Current = %q, %#v; want validated fetched v2", got, status)
	}
	assertAttempt(t, status, attemptCompleted, cache.AttemptSuccess)
	if !clockObservedCompletion {
		t.Fatal("attempt timestamp recorded before fetch and validation completed")
	}
	if versionCalls != 1 || fetchCalls != 1 || hashCalls != 2 || validateCalls != 1 {
		t.Fatalf("calls: Version=%d Fetch=%d Hash=%d Validate=%d; want 1, 1, 2, 1", versionCalls, fetchCalls, hashCalls, validateCalls)
	}
}

func TestValueVersionFailureSkipsFetchAndKeepsLastGood(t *testing.T) {
	t.Parallel()

	attemptCompleted := time.Date(2026, 7, 22, 10, 20, 0, 0, time.UTC)
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Version: func(context.Context) (string, error) {
			return "", errors.New("version unavailable")
		},
		Fetch: func(context.Context) (string, string, error) {
			t.Fatal("Fetch called after Version failed")
			return "", "", nil
		},
		Hash: func(value string) string { return value },
	}, cache.WithClock(func() time.Time { return attemptCompleted }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "embedded" || status.Source != "fallback" || status.Version != "v1" || status.LastSuccess != nil {
		t.Fatalf("Current = %q, %#v; want untouched cold fallback", got, status)
	}
	assertAttempt(t, status, attemptCompleted, cache.AttemptFailure)
}

func TestValueRefreshLadderRekeysThenDropsFetchedValueBackToFloor(t *testing.T) {
	t.Parallel()

	floor := []byte("floor")
	now := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	fetches := []struct {
		value   []byte
		version string
	}{
		{[]byte("ahead"), "v2"},
		{[]byte("ahead"), "v3"},
		{[]byte("floor"), "v4"},
	}
	fetchIndex := 0
	validated := 0
	value := cache.New(cache.Cacheable[[]byte]{
		Fallback:        floor,
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) ([]byte, string, error) {
			fetched := fetches[fetchIndex]
			fetchIndex++
			return fetched.value, fetched.version, nil
		},
		Hash: func(value []byte) string { return string(value) },
		Validate: func([]byte) error {
			validated++
			return nil
		},
	}, cache.WithClock(func() time.Time { return now }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	now = now.Add(time.Hour)
	registry.Prime(context.Background())
	got, status := value.Current()
	if string(got) != "ahead" || status.Source != "fetched" || status.Version != "v3" {
		t.Fatalf("after re-key Current = %q, %#v; want fetched ahead at v3", got, status)
	}
	if validated != 1 {
		t.Fatalf("validate calls after same-hash re-key = %d, want 1", validated)
	}
	assertAttempt(t, status, now, cache.AttemptSuccess)

	now = now.Add(time.Hour)
	registry.Prime(context.Background())
	got, status = value.Current()
	if string(got) != "floor" || status.Source != "fallback" || status.Version != "v4" {
		t.Fatalf("after floor return Current = %q, %#v; want fallback floor at v4", got, status)
	}
	if validated != 1 {
		t.Fatalf("validate calls after floor-hash return = %d, want 1", validated)
	}
	assertAttempt(t, status, now, cache.AttemptSuccess)
}

func TestValueRejectedFetchKeepsLastGood(t *testing.T) {
	t.Parallel()

	firstSuccess := time.Date(2026, 7, 22, 10, 30, 0, 0, time.UTC)
	now := firstSuccess
	fetches := 0
	value := cache.New(cache.Cacheable[string]{
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			fetches++
			if fetches == 1 {
				return "good", "v2", nil
			}
			return "malformed", "v3", nil
		},
		Hash: func(value string) string { return value },
		Validate: func(value string) error {
			if value == "malformed" {
				return errors.New("invalid payload")
			}
			return nil
		},
	}, cache.WithClock(func() time.Time { return now }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())
	now = now.Add(time.Hour)
	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "good" || status.Source != "fetched" || status.Version != "v2" {
		t.Fatalf("Current = %q, %#v; want last-good fetched v2", got, status)
	}
	if status.LastSuccess == nil || !status.LastSuccess.Equal(firstSuccess) {
		t.Fatalf("status last success = %v, want original success %v", status.LastSuccess, firstSuccess)
	}
	assertAttempt(t, status, now, cache.AttemptFailure)
}

func assertAttempt(t *testing.T, status cache.Status, completed time.Time, result cache.AttemptResult) {
	t.Helper()
	if status.LastAttempt == nil || !status.LastAttempt.Equal(completed) {
		t.Fatalf("status last attempt = %v, want %v", status.LastAttempt, completed)
	}
	if status.LastAttemptResult == nil || *status.LastAttemptResult != result {
		t.Fatalf("status last attempt result = %v, want %q", status.LastAttemptResult, result)
	}
}

func TestValuePrimeServesDistinctValidatedFetch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 22, 8, 30, 0, 0, time.UTC)
	validated := 0
	value := cache.New(cache.Cacheable[string]{
		Name:            "release_notes",
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			return "downloaded", "v2", nil
		},
		Hash: func(value string) string { return value },
		Validate: func(value string) error {
			validated++
			return nil
		},
	}, cache.WithClock(func() time.Time { return now }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "downloaded" {
		t.Fatalf("Current value = %q, want fetched value %q", got, "downloaded")
	}
	if status.Source != "fetched" {
		t.Fatalf("status source = %q, want fetched", status.Source)
	}
	if status.Version != "v2" {
		t.Fatalf("status version = %q, want %q", status.Version, "v2")
	}
	if status.LastSuccess == nil || !status.LastSuccess.Equal(now) {
		t.Fatalf("status last success = %v, want %v", status.LastSuccess, now)
	}
	if validated != 1 {
		t.Fatalf("validate calls = %d, want 1", validated)
	}
}

func TestValuePrimeKeepsFallbackWhenFetchHasFloorHash(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	validated := 0
	value := cache.New(cache.Cacheable[string]{
		Name:            "release_notes",
		Fallback:        "embedded",
		FallbackVersion: "v1",
		TTL:             time.Hour,
		Fetch: func(context.Context) (string, string, error) {
			return "embedded-copy", "v2", nil
		},
		Hash: func(string) string { return "same-content" },
		Validate: func(string) error {
			validated++
			return nil
		},
	}, cache.WithClock(func() time.Time { return now }))
	registry := cache.NewRegistry()
	registry.Register(value)

	registry.Prime(context.Background())

	got, status := value.Current()
	if got != "embedded" {
		t.Fatalf("Current value = %q, want embedded fallback", got)
	}
	if status.Source != "fallback" {
		t.Fatalf("status source = %q, want fallback", status.Source)
	}
	if status.Version != "v2" {
		t.Fatalf("status version = %q, want fetched label %q", status.Version, "v2")
	}
	if status.LastSuccess == nil || !status.LastSuccess.Equal(now) {
		t.Fatalf("status last success = %v, want %v", status.LastSuccess, now)
	}
	if validated != 0 {
		t.Fatalf("validate calls = %d, want 0 for hash-identical content", validated)
	}
}

func TestNewPanicsWhenRequiredFunctionIsNil(t *testing.T) {
	t.Parallel()

	tests := map[string]cache.Cacheable[string]{
		"hash": {
			Fetch: func(context.Context) (string, string, error) { return "", "", nil },
		},
		"fetch": {
			Hash: func(value string) string { return value },
		},
	}
	for name, recipe := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("New with nil %s did not panic", name)
				}
			}()
			cache.New(recipe)
		})
	}
}
