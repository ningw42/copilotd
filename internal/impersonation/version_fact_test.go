package impersonation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionFactColdServesFallback(t *testing.T) {
	t.Parallel()

	fact := newVersionFact("1.104.1", func(context.Context) (string, error) {
		t.Fatal("discover called before an attempt")
		return "", nil
	})

	got, state := fact.current()
	if got != "1.104.1" {
		t.Fatalf("current value = %q, want fallback %q", got, "1.104.1")
	}
	if state.source != sourceFallback {
		t.Fatalf("source = %q, want %q", state.source, sourceFallback)
	}
	if !state.lastSuccess.IsZero() {
		t.Fatalf("lastSuccess = %v, want zero", state.lastSuccess)
	}
	if !state.lastAttempt.IsZero() {
		t.Fatalf("lastAttempt = %v, want zero", state.lastAttempt)
	}
	if state.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty", state.lastErr)
	}
}

func TestVersionFactDiscoveryAttemptSuccess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	fact := newVersionFact(
		"1.104.1",
		func(context.Context) (string, error) { return "1.129.1", nil },
		withClock(func() time.Time { return now }),
	)

	if err := fact.attemptDiscovery(context.Background()); err != nil {
		t.Fatalf("discovery attempt returned error: %v", err)
	}

	got, state := fact.current()
	if got != "1.129.1" {
		t.Fatalf("current value = %q, want discovered %q", got, "1.129.1")
	}
	if state.source != sourceDiscovered {
		t.Fatalf("source = %q, want %q", state.source, sourceDiscovered)
	}
	if !state.lastSuccess.Equal(now) {
		t.Fatalf("lastSuccess = %v, want %v", state.lastSuccess, now)
	}
	if !state.lastAttempt.Equal(now) {
		t.Fatalf("lastAttempt = %v, want %v", state.lastAttempt, now)
	}
	if state.lastErr != "" {
		t.Fatalf("lastErr = %q, want empty", state.lastErr)
	}
}

func TestVersionFactWarmFailureKeepsLastGoodValue(t *testing.T) {
	t.Parallel()

	firstAttempt := time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC)
	secondAttempt := firstAttempt.Add(24 * time.Hour)
	now := firstAttempt
	wantErr := errors.New("discovery unavailable")
	attempt := 0
	fact := newVersionFact(
		"1.104.1",
		func(context.Context) (string, error) {
			attempt++
			if attempt == 1 {
				return "1.129.1", nil
			}
			return "", wantErr
		},
		withClock(func() time.Time { return now }),
	)

	if err := fact.attemptDiscovery(context.Background()); err != nil {
		t.Fatalf("first discovery attempt returned error: %v", err)
	}
	now = secondAttempt
	if err := fact.attemptDiscovery(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("second discovery attempt error = %v, want %v", err, wantErr)
	}

	got, state := fact.current()
	if got != "1.129.1" {
		t.Fatalf("current value = %q, want last-good %q", got, "1.129.1")
	}
	if state.source != sourceDiscovered {
		t.Fatalf("source = %q, want %q", state.source, sourceDiscovered)
	}
	if !state.lastSuccess.Equal(firstAttempt) {
		t.Fatalf("lastSuccess = %v, want original success %v", state.lastSuccess, firstAttempt)
	}
	if !state.lastAttempt.Equal(secondAttempt) {
		t.Fatalf("lastAttempt = %v, want %v", state.lastAttempt, secondAttempt)
	}
	if state.lastErr != wantErr.Error() {
		t.Fatalf("lastErr = %q, want %q", state.lastErr, wantErr)
	}
}

func TestVersionFactRunWaitsForTicksAndStopsOnCancellation(t *testing.T) {
	t.Parallel()

	const interval = 30 * time.Millisecond
	calls := make(chan struct{}, 4)
	fact := newVersionFact("1.104.1", func(context.Context) (string, error) {
		calls <- struct{}{}
		return "1.129.1", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fact.run(ctx, interval)
	}()

	select {
	case <-calls:
		t.Fatal("run attempted discovery immediately before the first tick")
	case <-time.After(interval / 3):
	}

	for tick := 1; tick <= 2; tick++ {
		select {
		case <-calls:
		case <-time.After(10 * interval):
			t.Fatalf("discovery attempt %d did not arrive", tick)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * interval):
		t.Fatal("run did not stop after context cancellation")
	}
	select {
	case <-calls:
		t.Fatal("run attempted discovery after context cancellation")
	case <-time.After(2 * interval):
	}
}

func TestVersionFactDiscoveryAttemptLogsOutcomeLevels(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	wantErr := errors.New("discovery unavailable")
	var attempts atomic.Int32
	fact := newVersionFact(
		"1.104.1",
		func(context.Context) (string, error) {
			if attempts.Add(1) == 1 {
				return "1.129.1", nil
			}
			return "", wantErr
		},
		withLogger(logger),
	)

	if err := fact.attemptDiscovery(context.Background()); err != nil {
		t.Fatalf("successful discovery attempt returned error: %v", err)
	}
	if err := fact.attemptDiscovery(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("failed discovery attempt error = %v, want %v", err, wantErr)
	}

	decoder := json.NewDecoder(&logs)
	assertLogRecord := func(wantLevel, wantMessage string) {
		t.Helper()
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if got := record["level"]; got != wantLevel {
			t.Fatalf("log level = %v, want %q", got, wantLevel)
		}
		if got := record["msg"]; got != wantMessage {
			t.Fatalf("log message = %v, want %q", got, wantMessage)
		}
	}
	assertLogRecord("DEBUG", "impersonation version discovery succeeded")
	assertLogRecord("WARN", "impersonation version discovery failed")
	var extra map[string]any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra log record: %v (record %v)", err, extra)
	}
}

func TestVersionFactConcurrentCurrentAndDiscoveryAttempts(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fact := newVersionFact(
		"1.104.1",
		func(context.Context) (string, error) { return "1.129.1", nil },
		withLogger(logger),
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		fact.run(ctx, time.Millisecond)
	}()

	var workers sync.WaitGroup
	for range 4 {
		workers.Add(2)
		go func() {
			defer workers.Done()
			for range 200 {
				value, state := fact.current()
				switch state.source {
				case sourceFallback:
					if value != "1.104.1" {
						t.Errorf("fallback source returned value %q", value)
						return
					}
				case sourceDiscovered:
					if value != "1.129.1" {
						t.Errorf("discovered source returned value %q", value)
						return
					}
				default:
					t.Errorf("current returned unknown source %q", state.source)
					return
				}
			}
		}()
		go func() {
			defer workers.Done()
			for range 50 {
				if err := fact.attemptDiscovery(ctx); err != nil {
					t.Errorf("discovery attempt returned error: %v", err)
					return
				}
			}
		}()
	}

	workers.Wait()
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("run did not stop after cancellation")
	}
}
