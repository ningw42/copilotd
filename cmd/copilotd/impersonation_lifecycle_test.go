package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/server"
)

func TestProductionDiscoveryEdgeUsesMicrosoftOriginsAndPlainDedicatedClient(t *testing.T) {
	edge := productionDiscoveryEdge()
	if edge.VSCodeBaseURL != "https://update.code.visualstudio.com" {
		t.Errorf("VS Code discovery base URL = %q", edge.VSCodeBaseURL)
	}
	if edge.MarketplaceBaseURL != "https://marketplace.visualstudio.com" {
		t.Errorf("Marketplace discovery base URL = %q", edge.MarketplaceBaseURL)
	}
	if edge.Client == nil {
		t.Fatal("production discovery client is nil")
	}
	if edge.Client == http.DefaultClient || edge.Client.Transport != nil || edge.Client.Timeout != 0 {
		t.Errorf("production discovery client = %#v, want a dedicated plain client", edge.Client)
	}
}

func TestRunBoundServeIsReadyWhilePrimeWaits(t *testing.T) {
	started := make(chan string, 2)
	cancelled := make(chan string, 2)
	// Never let a failed cancellation assertion strand the blocking stub.
	release := make(chan struct{})
	defer close(release)
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept the Marketplace POST completely before blocking. Leaving its body
		// unread can keep the server-side HTTP/1 request active after the client has
		// cancelled, masking the lifecycle behavior and hanging Server.Close.
		_, _ = io.Copy(io.Discard, r.Body)
		started <- r.URL.Path
		select {
		case <-r.Context().Done():
			cancelled <- r.URL.Path
		case <-release:
		}
	}))
	t.Cleanup(discovery.Close)

	upstream := newCopilotStub(t, `{"ok":true}`)
	github := lifecycleExchangeStub(t, upstream.server.URL, make(chan http.Header, 1))
	cfg := e2eConfig("gho-startup-window")
	cfg.ImpersonationRefreshInterval = time.Hour
	logger := discardLogger(t)
	mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), impersonation.Edge{
		VSCodeBaseURL:      discovery.URL,
		MarketplaceBaseURL: discovery.URL,
		Client:             discovery.Client(),
	})
	if err != nil {
		t.Fatalf("buildServeProvider: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runBoundServe(ctx, cfg, logger, mgr, imp, ln) }()

	startedPaths := make(map[string]bool, 2)
	for range 2 {
		select {
		case path := <-started:
			startedPaths[path] = true
		case <-time.After(time.Second):
			t.Fatal("startup Prime did not begin both discovery requests")
		}
	}

	base := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, base+"/healthz", http.StatusOK)
	assertHTTPStatusEventually(t, base+"/readyz", http.StatusOK)

	cancel()
	cancelledPaths := make(map[string]bool, 2)
	for range 2 {
		select {
		case path := <-cancelled:
			cancelledPaths[path] = true
		case <-time.After(time.Second):
			t.Fatalf("serve context cancellation stopped %v of startup discovery %v", cancelledPaths, startedPaths)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runBoundServe after cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bound serve did not stop after cancellation")
	}
}

func TestServeLifecycleCarriesFallbackAndDiscoveredVersionsOnWire(t *testing.T) {
	tests := []struct {
		name             string
		interval         time.Duration
		discoveryStatus  int
		vscodeResponse   string
		pluginResponse   string
		wantVSCode       string
		wantPlugin       string
		wantSource       string
		wantDiscoveryHit bool
	}{
		{
			name:       "disabled pins configured non-default fallbacks",
			interval:   0,
			wantVSCode: "9.8.7",
			wantPlugin: "6.5.4",
			wantSource: "fallback",
		},
		{
			name:             "failed discovery keeps configured non-default fallbacks",
			interval:         time.Hour,
			discoveryStatus:  http.StatusServiceUnavailable,
			wantVSCode:       "9.8.7",
			wantPlugin:       "6.5.4",
			wantSource:       "fallback",
			wantDiscoveryHit: true,
		},
		{
			name:             "successful discovery wins",
			interval:         time.Hour,
			discoveryStatus:  http.StatusOK,
			vscodeResponse:   `["1.140.2"]`,
			pluginResponse:   `{"results":[{"extensions":[{"versions":[{"version":"0.61.3","properties":[]}]}]}]}`,
			wantVSCode:       "1.140.2",
			wantPlugin:       "0.61.3",
			wantSource:       "discovered",
			wantDiscoveryHit: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var discoveryCalls atomic.Int32
			discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				discoveryCalls.Add(1)
				if tc.discoveryStatus != http.StatusOK {
					w.WriteHeader(tc.discoveryStatus)
					return
				}
				switch r.URL.Path {
				case "/api/releases/stable":
					_, _ = io.WriteString(w, tc.vscodeResponse)
				case "/_apis/public/gallery/extensionquery":
					_, _ = io.WriteString(w, tc.pluginResponse)
				default:
					t.Errorf("unexpected discovery path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(discovery.Close)

			upstream := newCopilotStub(t, `{"ok":true}`)
			exchangeHeaders := make(chan http.Header, 1)
			github := lifecycleExchangeStub(t, upstream.server.URL, exchangeHeaders)
			cfg := e2eConfig("gho-lifecycle")
			cfg.VSCodeVersionFallback = "9.8.7"
			cfg.PluginVersionFallback = "6.5.4"
			cfg.ImpersonationRefreshInterval = tc.interval
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), impersonation.Edge{
				VSCodeBaseURL:      discovery.URL,
				MarketplaceBaseURL: discovery.URL,
				Client:             discovery.Client(),
			})
			if err != nil {
				t.Fatalf("buildServeProvider: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			runServeStartup(ctx, tc.interval, imp, mgr, logger)

			var exchange http.Header
			select {
			case exchange = <-exchangeHeaders:
			case <-time.After(time.Second):
				t.Fatal("startup exchange did not run")
			}
			assertVersionHeaders(t, exchange, tc.wantVSCode, tc.wantPlugin)

			fwd := forward.New(mgr, forward.NewClient(cfg.ResponseHeaderTimeout), cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, cfg.MaxBufferedResponseBytes, nil, forward.WithLogger(logger))
			base := startTestServer(t, server.New(cfg, logger, mgr, imp, fwd, newTestWSProxy(mgr), server.NewStreamOutcomeCounter()))
			assertReadyzImpersonation(t, base, tc.wantVSCode, tc.wantPlugin, tc.wantSource)
			resp, _ := post(t, base+"/anthropic/v1/messages", `{"model":"test"}`)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("forward status = %d, want 200", resp.StatusCode)
			}
			assertVersionHeaders(t, upstream.hdr, tc.wantVSCode, tc.wantPlugin)

			if got := discoveryCalls.Load(); (got > 0) != tc.wantDiscoveryHit {
				t.Errorf("Microsoft discovery calls = %d, want hit=%t", got, tc.wantDiscoveryHit)
			}
			logOutput := logs.String()
			if !strings.Contains(logOutput, "level=INFO") ||
				!strings.Contains(logOutput, "startup impersonation discovery outcome") ||
				!strings.Contains(logOutput, "vscode_source="+tc.wantSource) ||
				!strings.Contains(logOutput, "plugin_source="+tc.wantSource) {
				t.Errorf("startup logs = %q, want info discovery outcome with %s sources", logOutput, tc.wantSource)
			}
		})
	}
}

func assertReadyzImpersonation(t *testing.T, base, vscode, plugin, source string) {
	t.Helper()
	resp, err := http.Get(base + "/readyz") //nolint:noctx // local test server
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	var got struct {
		Status        string `json:"status"`
		Impersonation struct {
			EffectiveHeaders map[string]string `json:"effective_headers"`
			Discovery        struct {
				VSCode struct {
					Source      string     `json:"source"`
					LastSuccess *time.Time `json:"last_success"`
				} `json:"vscode"`
				CopilotChat struct {
					Source      string     `json:"source"`
					LastSuccess *time.Time `json:"last_success"`
				} `json:"copilot_chat"`
			} `json:"discovery"`
		} `json:"impersonation"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode /readyz: %v; body=%s", err, body)
	}
	if got.Status != "ready" {
		t.Errorf("/readyz status field = %q, want ready", got.Status)
	}
	assertVersionHeaders(t, http.Header{
		"Editor-Version":         {got.Impersonation.EffectiveHeaders["Editor-Version"]},
		"Editor-Plugin-Version":  {got.Impersonation.EffectiveHeaders["Editor-Plugin-Version"]},
		"User-Agent":             {got.Impersonation.EffectiveHeaders["User-Agent"]},
		"Copilot-Integration-Id": {got.Impersonation.EffectiveHeaders["Copilot-Integration-Id"]},
		"X-Github-Api-Version":   {got.Impersonation.EffectiveHeaders["X-GitHub-Api-Version"]},
	}, vscode, plugin)
	if got.Impersonation.Discovery.VSCode.Source != source || got.Impersonation.Discovery.CopilotChat.Source != source {
		t.Errorf("/readyz discovery sources = %q/%q, want %q", got.Impersonation.Discovery.VSCode.Source, got.Impersonation.Discovery.CopilotChat.Source, source)
	}
	wantLastSuccess := source == "discovered"
	if (got.Impersonation.Discovery.VSCode.LastSuccess != nil) != wantLastSuccess ||
		(got.Impersonation.Discovery.CopilotChat.LastSuccess != nil) != wantLastSuccess {
		t.Errorf("/readyz last_success = %v/%v, want non-null=%t", got.Impersonation.Discovery.VSCode.LastSuccess, got.Impersonation.Discovery.CopilotChat.LastSuccess, wantLastSuccess)
	}
	if strings.Contains(string(body), "Authorization") || strings.Contains(string(body), "last_error") {
		t.Errorf("/readyz leaked secret/error detail: %s", body)
	}
}

func TestServeLifecycleCancellationStopsPeriodicDiscovery(t *testing.T) {
	var discoveryCalls atomic.Int32
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discoveryCalls.Add(1)
		switch r.URL.Path {
		case "/api/releases/stable":
			_, _ = io.WriteString(w, `["1.140.2"]`)
		case "/_apis/public/gallery/extensionquery":
			_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"0.61.3","properties":[]}]}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(discovery.Close)
	upstream := newCopilotStub(t, `{"ok":true}`)
	github := lifecycleExchangeStub(t, upstream.server.URL, make(chan http.Header, 1))
	cfg := e2eConfig("gho-run-cancel")
	cfg.ImpersonationRefreshInterval = 10 * time.Millisecond
	logger := discardLogger(t)
	mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), impersonation.Edge{
		VSCodeBaseURL:      discovery.URL,
		MarketplaceBaseURL: discovery.URL,
		Client:             discovery.Client(),
	})
	if err != nil {
		t.Fatalf("buildServeProvider: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runServeStartup(ctx, cfg.ImpersonationRefreshInterval, imp, mgr, logger)
	deadline := time.Now().Add(time.Second)
	for discoveryCalls.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if discoveryCalls.Load() < 4 {
		t.Fatalf("periodic Run did not discover after Prime; calls = %d", discoveryCalls.Load())
	}
	cancel()
	time.Sleep(30 * time.Millisecond)
	afterCancel := discoveryCalls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := discoveryCalls.Load(); got != afterCancel {
		t.Errorf("discovery calls after cancellation = %d -> %d, want Run stopped", afterCancel, got)
	}
}

func lifecycleExchangeStub(t *testing.T, upstreamURL string, headers chan<- http.Header) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case headers <- r.Header.Clone():
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "copilot-lifecycle-token",
			"expires_at": time.Now().Add(time.Hour).Unix(),
			"refresh_in": 3600,
			"endpoints":  map[string]any{"api": upstreamURL},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func assertVersionHeaders(t *testing.T, header http.Header, vscode, plugin string) {
	t.Helper()
	want := map[string]string{
		"Editor-Version":         "vscode/" + vscode,
		"Editor-Plugin-Version":  "copilot-chat/" + plugin,
		"User-Agent":             "GitHubCopilotChat/" + plugin,
		"Copilot-Integration-Id": "vscode-chat",
		"X-Github-Api-Version":   "2025-04-01",
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func assertHTTPStatusEventually(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
			err = errors.New(resp.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not reach status %d: %v", url, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
