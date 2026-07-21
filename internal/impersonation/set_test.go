package impersonation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewBindsConfiguredFallbacksStaticIdentifiersAndDiscoveryEdge(t *testing.T) {
	t.Parallel()

	var vscodeCalls atomic.Int32
	var marketplaceCalls atomic.Int32
	edge := Edge{
		VSCodeBaseURL:      "https://vscode.test",
		MarketplaceBaseURL: "https://marketplace.test",
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Host {
			case "vscode.test":
				vscodeCalls.Add(1)
				body = `["1.140.0"]`
			case "marketplace.test":
				marketplaceCalls.Add(1)
				body = `{"results":[{"extensions":[{"versions":[{"version":"0.60.0","properties":[]}]}]}]}`
			default:
				return nil, errors.New("unexpected discovery host " + r.URL.Host)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})},
	}
	set := New(Config{
		VSCodeVersionFallback: "1.120.0",
		PluginVersionFallback: "0.40.0",
		CopilotIntegrationID:  "configured-integration",
		GithubAPIVersion:      "2026-01-01",
	}, edge, slog.New(slog.NewTextHandler(io.Discard, nil)))

	fallback := set.Header()
	if got := fallback.Get("Editor-Version"); got != "vscode/1.120.0" {
		t.Fatalf("configured fallback Editor-Version = %q", got)
	}
	if got := fallback.Get("Editor-Plugin-Version"); got != "copilot-chat/0.40.0" {
		t.Fatalf("configured fallback Editor-Plugin-Version = %q", got)
	}
	if got := fallback.Get("Copilot-Integration-Id"); got != "configured-integration" {
		t.Fatalf("configured integration id = %q", got)
	}
	if got := fallback.Get("X-GitHub-Api-Version"); got != "2026-01-01" {
		t.Fatalf("configured API version = %q", got)
	}

	set.Prime(context.Background())
	discovered := set.Header()
	if got := discovered.Get("Editor-Version"); got != "vscode/1.140.0" {
		t.Fatalf("discovered Editor-Version = %q", got)
	}
	if got := discovered.Get("User-Agent"); got != "GitHubCopilotChat/0.60.0" {
		t.Fatalf("discovered User-Agent = %q", got)
	}
	if vscodeCalls.Load() != 1 || marketplaceCalls.Load() != 1 {
		t.Fatalf("edge calls = (%d, %d), want one each", vscodeCalls.Load(), marketplaceCalls.Load())
	}
}

func TestSetHeaderAssemblesFallbackAndDiscoveredVersions(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("discovery unavailable")
	tests := []struct {
		name         string
		vscodeResult string
		vscodeErr    error
		pluginResult string
		pluginErr    error
		wantVscode   string
		wantPlugin   string
	}{
		{
			name:         "both discovered",
			vscodeResult: "1.129.1",
			pluginResult: "0.49.2",
			wantVscode:   "1.129.1",
			wantPlugin:   "0.49.2",
		},
		{
			name:         "VS Code only",
			vscodeResult: "1.129.1",
			pluginErr:    wantErr,
			wantVscode:   "1.129.1",
			wantPlugin:   "0.26.7",
		},
		{
			name:         "Copilot Chat only",
			vscodeErr:    wantErr,
			pluginResult: "0.49.2",
			wantVscode:   "1.104.1",
			wantPlugin:   "0.49.2",
		},
		{
			name:       "neither uses exact default derivation",
			vscodeErr:  wantErr,
			pluginErr:  wantErr,
			wantVscode: "1.104.1",
			wantPlugin: "0.26.7",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			set := testSet(
				func(context.Context) (string, error) { return test.vscodeResult, test.vscodeErr },
				func(context.Context) (string, error) { return test.pluginResult, test.pluginErr },
			)
			set.Prime(context.Background())

			want := http.Header{
				"Copilot-Integration-Id": {"vscode-chat"},
				"Editor-Plugin-Version":  {"copilot-chat/" + test.wantPlugin},
				"Editor-Version":         {"vscode/" + test.wantVscode},
				"User-Agent":             {"GitHubCopilotChat/" + test.wantPlugin},
				"X-Github-Api-Version":   {"2025-04-01"},
			}
			if got := set.Header(); !reflect.DeepEqual(got, want) {
				t.Fatalf("Header() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestSetHeaderReturnsFreshMapAndReflectsDiscovery(t *testing.T) {
	t.Parallel()

	set := testSet(
		func(context.Context) (string, error) { return "1.129.1", nil },
		func(context.Context) (string, error) { return "0.49.2", nil },
	)
	first := set.Header()
	first.Set("Editor-Version", "mutated")
	first["User-Agent"][0] = "mutated"

	beforePrime := set.Header()
	if got := beforePrime.Get("Editor-Version"); got != "vscode/1.104.1" {
		t.Fatalf("fresh Header() Editor-Version = %q, want fallback", got)
	}
	if got := beforePrime.Get("User-Agent"); got != "GitHubCopilotChat/0.26.7" {
		t.Fatalf("fresh Header() User-Agent = %q, want fallback", got)
	}

	set.Prime(context.Background())
	afterPrime := set.Header()
	if got := afterPrime.Get("Editor-Version"); got != "vscode/1.129.1" {
		t.Fatalf("discovered Header() Editor-Version = %q, want discovered", got)
	}
	if got := afterPrime.Get("Editor-Plugin-Version"); got != "copilot-chat/0.49.2" {
		t.Fatalf("discovered Header() Editor-Plugin-Version = %q, want discovered", got)
	}
}

func TestSetObserveContainsOnlyEffectiveHeadersAndFactFreshness(t *testing.T) {
	t.Parallel()

	set := testSet(
		func(context.Context) (string, error) { return "1.129.1", nil },
		func(context.Context) (string, error) { return "", errors.New("URL containing internal detail") },
	)
	set.Prime(context.Background())

	got := set.Observe()
	if got.EffectiveHeaders.Get("Editor-Version") != "vscode/1.129.1" {
		t.Fatalf("effective Editor-Version = %q, want discovered", got.EffectiveHeaders.Get("Editor-Version"))
	}
	if got.Discovery.VSCode.Source != "discovered" || got.Discovery.VSCode.LastSuccess == nil {
		t.Fatalf("VS Code observation = %+v, want discovered with last success", got.Discovery.VSCode)
	}
	if got.Discovery.CopilotChat.Source != "fallback" || got.Discovery.CopilotChat.LastSuccess != nil {
		t.Fatalf("Copilot Chat observation = %+v, want cold fallback", got.Discovery.CopilotChat)
	}

	assertExportedFields(t, reflect.TypeOf(got), []string{"EffectiveHeaders", "Discovery"})
	assertExportedFields(t, reflect.TypeOf(got.Discovery), []string{"VSCode", "CopilotChat"})
	assertExportedFields(t, reflect.TypeOf(got.Discovery.VSCode), []string{"Source", "LastSuccess"})

	got.EffectiveHeaders.Set("Editor-Version", "mutated")
	if next := set.Observe().EffectiveHeaders.Get("Editor-Version"); next != "vscode/1.129.1" {
		t.Fatalf("Observe() shares effective headers: next Editor-Version = %q", next)
	}
}

func TestSetPrimeDiscoversFactsConcurrently(t *testing.T) {
	t.Parallel()

	started := make(chan string, 2)
	release := make(chan struct{})
	discover := func(name, value string) func(context.Context) (string, error) {
		return func(ctx context.Context) (string, error) {
			started <- name
			select {
			case <-release:
				return value, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	set := testSet(discover("vscode", "1.129.1"), discover("plugin", "0.49.2"))
	done := make(chan struct{})
	go func() {
		set.Prime(context.Background())
		close(done)
	}()

	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("Prime did not start both discoveries concurrently")
		}
	}
	if !seen["vscode"] || !seen["plugin"] {
		t.Fatalf("started discoveries = %v, want both facts", seen)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Prime did not return after both discoveries settled")
	}
}

func TestSetPrimeBoundsCombinedWaitAndKeepsTimedOutFallback(t *testing.T) {
	t.Parallel()

	set := testSet(
		func(context.Context) (string, error) { return "1.129.1", nil },
		func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	)
	set.primeTimeout = 25 * time.Millisecond

	started := time.Now()
	set.Prime(context.Background())
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Prime elapsed = %v, want bounded combined wait", elapsed)
	}
	got := set.Header()
	if got.Get("Editor-Version") != "vscode/1.129.1" {
		t.Fatalf("Editor-Version = %q, want successful discovery", got.Get("Editor-Version"))
	}
	if got.Get("Editor-Plugin-Version") != "copilot-chat/0.26.7" {
		t.Fatalf("Editor-Plugin-Version = %q, want timed-out fallback", got.Get("Editor-Plugin-Version"))
	}
}

func TestSetRunSharesTickerWaitsForTickAndStopsOnCancellation(t *testing.T) {
	t.Parallel()

	vscodeCalls := make(chan struct{}, 2)
	pluginCalls := make(chan struct{}, 2)
	set := testSet(
		func(context.Context) (string, error) {
			vscodeCalls <- struct{}{}
			return "1.129.1", nil
		},
		func(context.Context) (string, error) {
			pluginCalls <- struct{}{}
			return "0.49.2", nil
		},
	)
	ticker := &manualSetTicker{ticks: make(chan time.Time, 1), stopped: make(chan struct{})}
	const interval = 24 * time.Hour
	set.newTicker = func(got time.Duration) setTicker {
		if got != interval {
			t.Errorf("ticker interval = %v, want %v", got, interval)
		}
		return ticker
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		set.Run(ctx, interval)
		close(done)
	}()

	assertNoCall(t, vscodeCalls, "VS Code discovery attempted immediately")
	assertNoCall(t, pluginCalls, "Copilot Chat discovery attempted immediately")
	ticker.ticks <- time.Now()
	assertCall(t, vscodeCalls, "VS Code discovery was not attempted on shared tick")
	assertCall(t, pluginCalls, "Copilot Chat discovery was not attempted on shared tick")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
	select {
	case <-ticker.stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop its ticker")
	}
}

func TestSetColdPrimeFailureDoesNotRetryBeforeRunTick(t *testing.T) {
	t.Parallel()

	var vscodeCalls atomic.Int32
	var pluginCalls atomic.Int32
	fail := func(calls *atomic.Int32) func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			calls.Add(1)
			return "", errors.New("offline")
		}
	}
	set := testSet(fail(&vscodeCalls), fail(&pluginCalls))
	set.Prime(context.Background())
	if vscodeCalls.Load() != 1 || pluginCalls.Load() != 1 {
		t.Fatalf("Prime calls = (%d, %d), want one each", vscodeCalls.Load(), pluginCalls.Load())
	}

	ticker := &manualSetTicker{ticks: make(chan time.Time, 1), stopped: make(chan struct{})}
	set.newTicker = func(time.Duration) setTicker { return ticker }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		set.Run(ctx, time.Hour)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	if vscodeCalls.Load() != 1 || pluginCalls.Load() != 1 {
		t.Fatalf("calls before tick = (%d, %d), want no cold retry", vscodeCalls.Load(), pluginCalls.Load())
	}
	ticker.ticks <- time.Now()
	deadline := time.Now().Add(time.Second)
	for (vscodeCalls.Load() != 2 || pluginCalls.Load() != 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if vscodeCalls.Load() != 2 || pluginCalls.Load() != 2 {
		t.Fatalf("calls after tick = (%d, %d), want two each", vscodeCalls.Load(), pluginCalls.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}

func testSet(
	vscodeDiscover func(context.Context) (string, error),
	pluginDiscover func(context.Context) (string, error),
) *Set {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Set{
		vscode:        newVersionFact("1.104.1", vscodeDiscover, withLogger(logger)),
		plugin:        newVersionFact("0.26.7", pluginDiscover, withLogger(logger)),
		integrationID: "vscode-chat",
		apiVersion:    "2025-04-01",
		primeTimeout:  5 * time.Second,
		newTicker:     newRealSetTicker,
	}
}

func assertExportedFields(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	var got []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.IsExported() {
			got = append(got, field.Name)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported fields on %s = %v, want only %v", typ, got, want)
	}
}

func assertNoCall(t *testing.T, calls <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-calls:
		t.Fatal(message)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertCall(t *testing.T, calls <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

type manualSetTicker struct {
	ticks   chan time.Time
	stopped chan struct{}
}

func (t *manualSetTicker) C() <-chan time.Time { return t.ticks }

func (t *manualSetTicker) Stop() { close(t.stopped) }
