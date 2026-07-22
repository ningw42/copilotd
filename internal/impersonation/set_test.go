package impersonation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
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
	registry := cache.NewRegistry()
	set := New(Config{
		VSCodeVersionFallback: "1.120.0",
		PluginVersionFallback: "0.40.0",
		CopilotIntegrationID:  "configured-integration",
		GithubAPIVersion:      "2026-01-01",
		RefreshInterval:       time.Hour,
	}, edge, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	registry.Prime(context.Background())
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

func TestHeaderAssemblesAllFallbackAndFetchedStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		vscodeStatus int
		pluginStatus int
		wantVSCode   string
		wantPlugin   string
	}{
		{name: "both fetched", vscodeStatus: http.StatusOK, pluginStatus: http.StatusOK, wantVSCode: "1.140.0", wantPlugin: "0.60.0"},
		{name: "VS Code only", vscodeStatus: http.StatusOK, pluginStatus: http.StatusServiceUnavailable, wantVSCode: "1.140.0", wantPlugin: "0.26.7"},
		{name: "Copilot Chat only", vscodeStatus: http.StatusServiceUnavailable, pluginStatus: http.StatusOK, wantVSCode: "1.104.1", wantPlugin: "0.60.0"},
		{name: "both fallback", vscodeStatus: http.StatusServiceUnavailable, pluginStatus: http.StatusServiceUnavailable, wantVSCode: "1.104.1", wantPlugin: "0.26.7"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case vscodeStableReleasesPath:
					w.WriteHeader(test.vscodeStatus)
					if test.vscodeStatus == http.StatusOK {
						_, _ = io.WriteString(w, `["1.140.0"]`)
					}
				case marketplaceQueryPath:
					w.WriteHeader(test.pluginStatus)
					if test.pluginStatus == http.StatusOK {
						_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"0.60.0","properties":[]}]}]}]}`)
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer discovery.Close()

			registry := cache.NewRegistry()
			set := New(Config{
				VSCodeVersionFallback: "1.104.1",
				PluginVersionFallback: "0.26.7",
				CopilotIntegrationID:  "vscode-chat",
				GithubAPIVersion:      "2025-04-01",
				RefreshInterval:       time.Hour,
			}, Edge{
				VSCodeBaseURL:      discovery.URL,
				MarketplaceBaseURL: discovery.URL,
				Client:             discovery.Client(),
			}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
			registry.Prime(context.Background())

			want := http.Header{
				"Copilot-Integration-Id": {"vscode-chat"},
				"Editor-Plugin-Version":  {"copilot-chat/" + test.wantPlugin},
				"Editor-Version":         {"vscode/" + test.wantVSCode},
				"User-Agent":             {"GitHubCopilotChat/" + test.wantPlugin},
				"X-Github-Api-Version":   {"2025-04-01"},
			}
			if got := set.Header(); !headersEqual(got, want) {
				t.Fatalf("Header() = %#v, want %#v", got, want)
			}
		})
	}
}

func headersEqual(got, want http.Header) bool {
	if len(got) != len(want) {
		return false
	}
	for key, values := range want {
		if got.Get(key) != values[0] {
			return false
		}
	}
	return true
}

func TestHeaderReflectsCacheSwapOnNextCall(t *testing.T) {
	t.Parallel()

	var generation atomic.Int32
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second := generation.Load() > 0
		switch r.URL.Path {
		case vscodeStableReleasesPath:
			if second {
				_, _ = io.WriteString(w, `["1.141.0"]`)
			} else {
				_, _ = io.WriteString(w, `["1.140.0"]`)
			}
		case marketplaceQueryPath:
			version := "0.60.0"
			if second {
				version = "0.61.0"
			}
			_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"`+version+`","properties":[]}]}]}]}`)
		}
	}))
	defer discovery.Close()

	registry := cache.NewRegistry()
	set := New(Config{
		VSCodeVersionFallback: "1.104.1",
		PluginVersionFallback: "0.26.7",
		CopilotIntegrationID:  "vscode-chat",
		GithubAPIVersion:      "2025-04-01",
		RefreshInterval:       time.Hour,
	}, Edge{VSCodeBaseURL: discovery.URL, MarketplaceBaseURL: discovery.URL, Client: discovery.Client()}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	first := set.Header()
	first.Set("Editor-Version", "mutated")
	generation.Add(1)
	registry.Prime(context.Background())

	got := set.Header()
	if got.Get("Editor-Version") != "vscode/1.141.0" || got.Get("User-Agent") != "GitHubCopilotChat/0.61.0" {
		t.Fatalf("Header() after swap = %v, want second fetched versions", got)
	}
}

func TestDisabledRefreshRegistersFallbacksWithoutCallingDiscovery(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	edge := Edge{
		VSCodeBaseURL:      "https://vscode.test",
		MarketplaceBaseURL: "https://marketplace.test",
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("discovery must stay disabled")
		})},
	}
	registry := cache.NewRegistry()
	set := New(Config{
		VSCodeVersionFallback: "1.104.1",
		PluginVersionFallback: "0.26.7",
		CopilotIntegrationID:  "vscode-chat",
		GithubAPIVersion:      "2025-04-01",
		RefreshInterval:       0,
	}, edge, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	registry.Prime(ctx)
	registry.Start(ctx)
	cancel()
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled discovery calls = %d, want zero", got)
	}
	if got := set.Header(); got.Get("Editor-Version") != "vscode/1.104.1" || got.Get("User-Agent") != "GitHubCopilotChat/0.26.7" {
		t.Fatalf("disabled Header() = %v, want configured fallbacks", got)
	}
	statuses := registry.Observe()
	if len(statuses) != 2 {
		t.Fatalf("registered cache statuses = %d, want 2", len(statuses))
	}
	for _, status := range statuses {
		if status.Source != "fallback" || status.LastSuccess != nil {
			t.Errorf("disabled cache %q = %+v, want cold fallback", status.Name, status)
		}
	}
}

func TestCacheValidationGateRejectsInvalidDiscoveredVersions(t *testing.T) {
	t.Parallel()

	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case vscodeStableReleasesPath:
			_, _ = io.WriteString(w, `["release-1.140.0"]`)
		case marketplaceQueryPath:
			_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"0.60","properties":[]}]}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer discovery.Close()

	registry := cache.NewRegistry()
	set := New(Config{
		VSCodeVersionFallback: "1.104.1",
		PluginVersionFallback: "0.26.7",
		CopilotIntegrationID:  "vscode-chat",
		GithubAPIVersion:      "2025-04-01",
		RefreshInterval:       time.Hour,
	}, Edge{
		VSCodeBaseURL:      discovery.URL,
		MarketplaceBaseURL: discovery.URL,
		Client:             discovery.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))

	registry.Prime(context.Background())
	got := set.Header()
	if got.Get("Editor-Version") != "vscode/1.104.1" ||
		got.Get("Editor-Plugin-Version") != "copilot-chat/0.26.7" {
		t.Fatalf("Header() = %v, want both fallbacks after validation rejection", got)
	}
	for _, status := range registry.Observe() {
		if status.Source != "fallback" || status.LastSuccess != nil {
			t.Errorf("cache %q status = %+v, want cold fallback after rejection", status.Name, status)
		}
	}
}
