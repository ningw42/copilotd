package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
)

const testAPIKey = "test-api-key"

// e2eConfig is a resolved ServeConfig with the impersonation defaults, a set API
// key, and the given inline OAuth token — the shape runServe would hand
// buildServeProvider, minus the flag/env/file plumbing.
func e2eConfig(oauthToken string) config.ServeConfig {
	return config.ServeConfig{
		Addr:                         "127.0.0.1:0",
		LogLevel:                     "info",
		LogFormat:                    "text",
		ShutdownTimeout:              2 * time.Second,
		APIKey:                       testAPIKey,
		GithubOAuthToken:             oauthToken,
		OutboundTimeout:              5 * time.Second,
		StreamIdleTimeout:            5 * time.Second,
		StreamKeepaliveInterval:      15 * time.Second,
		WriteTimeout:                 5 * time.Second,
		ResponseHeaderTimeout:        5 * time.Second,
		MaxRequestBytes:              1 << 20,
		MaxBufferedResponseBytes:     1 << 20,
		StartupMintRetries:           0, // deterministic against stubs; no retries needed
		VSCodeVersionFallback:        "1.104.1",
		PluginVersionFallback:        "0.26.7",
		CopilotIntegrationID:         "vscode-chat",
		GithubAPIVersion:             "2025-04-01",
		ImpersonationRefreshInterval: 24 * time.Hour,
	}
}

// discardLogger returns a logger writing to io.Discard so tests stay quiet.
func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, err := logging.NewWithWriter(io.Discard, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	return l
}

// startTestServer runs srv on an ephemeral loopback listener and returns its base
// URL, tearing it down on cleanup. Mirrors server_integration_test's helper.
func startTestServer(t *testing.T, srv *server.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down within the grace period")
		}
	})

	base := "http://" + ln.Addr().String()
	for range 50 {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // test setup poll
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return base
}

// copilotStub is an httptest fake of the Copilot inference upstream capturing the
// forwarder's outbound request.
type copilotStub struct {
	server *httptest.Server
	auth   string
	hdr    http.Header
	body   []byte
	path   string
}

func newCopilotStub(t *testing.T, respBody string) *copilotStub {
	s := &copilotStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.auth = r.Header.Get("Authorization")
		s.hdr = r.Header.Clone()
		s.body, _ = io.ReadAll(r.Body)
		s.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// newGitHubExchangeStub fakes GitHub's token endpoint, minting copilotToken with
// endpoints.api pointing at apiURL. It captures the exchange request headers.
func newGitHubExchangeStub(t *testing.T, copilotToken, apiURL string, gotAuth, gotUA *string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      copilotToken,
			"expires_at": time.Now().Add(25 * time.Minute).Unix(),
			"refresh_in": 1500,
			"endpoints":  map[string]any{"api": apiURL},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// TestServeFirstRealCallEndToEnd is Phase 1.5's outcome: the REAL identity.Manager
// does a REAL token exchange against a stubbed GitHub, then the REAL forward path
// round-trips a non-streaming JSON request END TO END on BOTH surfaces against a
// stubbed Copilot. It asserts the minted Copilot bearer + impersonation headers
// reached Copilot and the body round-tripped verbatim.
func TestServeFirstRealCallEndToEnd(t *testing.T) {
	const (
		oauth        = "gho-inline-secret"
		copilotToken = "copilot-minted-token"
	)
	copilot := newCopilotStub(t, `{"id":"msg_1","role":"assistant"}`)

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, copilotToken, copilot.server.URL, &exchangeAuth, &exchangeUA)

	cfg := e2eConfig(oauth)
	logger := discardLogger(t)

	mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), productionDiscoveryEdge())
	if err != nil {
		t.Fatalf("buildServeProvider: %v", err)
	}
	// Mint synchronously so readiness is warm before the first request (production
	// does this in a goroutine; here we want determinism).
	mgr.StartupMint(context.Background())
	if !mgr.Ready() {
		t.Fatalf("manager not ready after a successful startup mint")
	}

	fwd := forward.New(mgr, forward.NewClient(cfg.ResponseHeaderTimeout), cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, cfg.MaxBufferedResponseBytes, nil, forward.WithLogger(logger))
	base := startTestServer(t, server.New(cfg, logger, mgr, imp, fwd, newTestWSProxy(mgr), server.NewStreamOutcomeCounter()))

	assertImpersonation := func(t *testing.T) {
		t.Helper()
		if copilot.auth != "Bearer "+copilotToken {
			t.Errorf("upstream Authorization = %q, want the minted Copilot bearer", copilot.auth)
		}
		if strings.Contains(copilot.auth, testAPIKey) || copilot.hdr.Get("X-Api-Key") != "" {
			t.Errorf("inbound API key leaked upstream (auth=%q)", copilot.auth)
		}
		if copilot.hdr.Get("Copilot-Integration-Id") != "vscode-chat" ||
			copilot.hdr.Get("Editor-Version") != "vscode/1.104.1" ||
			copilot.hdr.Get("User-Agent") != "GitHubCopilotChat/0.26.7" ||
			copilot.hdr.Get("X-Github-Api-Version") != "2025-04-01" {
			t.Errorf("impersonation headers missing upstream: %v", copilot.hdr)
		}
	}

	t.Run("anthropic surface round-trips", func(t *testing.T) {
		const reqBody = `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`
		resp, respBody := post(t, base+"/anthropic/v1/messages", reqBody)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if respBody != `{"id":"msg_1","role":"assistant"}` {
			t.Errorf("response body = %q, want the upstream body verbatim", respBody)
		}
		if copilot.path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", copilot.path)
		}
		if string(copilot.body) != reqBody {
			t.Errorf("upstream body = %q, want the original bytes", copilot.body)
		}
		assertImpersonation(t)
	})

	t.Run("openai surface round-trips", func(t *testing.T) {
		const reqBody = `{"model":"gpt-4o","input":"hi"}`
		resp, respBody := post(t, base+"/openai/v1/responses", reqBody)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if respBody != `{"id":"msg_1","role":"assistant"}` {
			t.Errorf("response body = %q, want the upstream body verbatim", respBody)
		}
		// The /v1 asymmetry: OpenAI drops /v1 upstream.
		if copilot.path != "/responses" {
			t.Errorf("upstream path = %q, want /responses (not /v1/responses)", copilot.path)
		}
		if string(copilot.body) != reqBody {
			t.Errorf("upstream body = %q, want the original bytes", copilot.body)
		}
		assertImpersonation(t)
	})

	// The exchange itself carried the OAuth token (token scheme) and the
	// impersonation UA the token endpoint's allowlist checks.
	if exchangeAuth != "token "+oauth {
		t.Errorf("exchange Authorization = %q, want %q", exchangeAuth, "token "+oauth)
	}
	if exchangeUA != "GitHubCopilotChat/"+cfg.PluginVersionFallback {
		t.Errorf("exchange User-Agent = %q, want %q", exchangeUA, "GitHubCopilotChat/"+cfg.PluginVersionFallback)
	}
}

// TestServeDiscoveredVersionsEndToEnd proves the bound serve lifecycle carries
// successful startup discovery through the first exchange and the first
// forwarded inference request, and reports the same effective values on
// /readyz. Every outbound edge is stubbed; no Microsoft or GitHub host is used.
func TestServeDiscoveredVersionsEndToEnd(t *testing.T) {
	const (
		discoveredVSCode = "1.140.2"
		discoveredPlugin = "0.61.3"
	)
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/stable":
			_, _ = io.WriteString(w, `["`+discoveredVSCode+`"]`)
		case "/_apis/public/gallery/extensionquery":
			_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"`+discoveredPlugin+`","properties":[]}]}]}]}`)
		default:
			t.Errorf("unexpected discovery path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(discovery.Close)

	upstream := newCopilotStub(t, `{"ok":true}`)
	exchangeHeaders := make(chan http.Header, 1)
	github := lifecycleExchangeStub(t, upstream.server.URL, exchangeHeaders)
	cfg := e2eConfig("gho-discovery-e2e")
	// Make fallback values observably different so every assertion below proves
	// that startup discovery, rather than static configuration, supplied them.
	cfg.VSCodeVersionFallback = "9.8.7"
	cfg.PluginVersionFallback = "6.5.4"
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
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runBoundServe after cancellation: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("bound serve did not stop within the grace period")
		}
	})

	base := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, base+"/readyz", http.StatusOK)

	select {
	case exchange := <-exchangeHeaders:
		if got, want := exchange.Get("Editor-Version"), "vscode/"+discoveredVSCode; got != want {
			t.Errorf("exchange Editor-Version = %q, want discovered %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("startup exchange did not run after discovery")
	}

	resp, _ := post(t, base+"/anthropic/v1/messages", `{"model":"test"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward status = %d, want 200", resp.StatusCode)
	}
	if got, want := upstream.hdr.Get("Editor-Plugin-Version"), "copilot-chat/"+discoveredPlugin; got != want {
		t.Errorf("forwarded Editor-Plugin-Version = %q, want discovered %q", got, want)
	}
	if got, want := upstream.hdr.Get("User-Agent"), "GitHubCopilotChat/"+discoveredPlugin; got != want {
		t.Errorf("forwarded User-Agent = %q, want discovered %q", got, want)
	}

	assertReadyzImpersonation(t, base, discoveredVSCode, discoveredPlugin, "discovered")
}

// TestServeDegradedWindow proves the readiness gate: before the first mint and
// after a mint failure, Surface endpoints return 503 and /readyz is not-ready.
func TestServeDegradedWindow(t *testing.T) {
	t.Run("pre-first-mint window: 503 + not-ready", func(t *testing.T) {
		copilot := newCopilotStub(t, `{"ok":true}`)
		var a, u string
		github := newGitHubExchangeStub(t, "copilot-token", copilot.server.URL, &a, &u)

		cfg := e2eConfig("gho-secret")
		logger := discardLogger(t)
		mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), productionDiscoveryEdge())
		if err != nil {
			t.Fatalf("buildServeProvider: %v", err)
		}
		// Deliberately do NOT run StartupMint: the daemon is in its pre-first-mint
		// window, so Ready() is false.
		if mgr.Ready() {
			t.Fatalf("Ready() = true before any mint, want false")
		}
		fwd := forward.New(mgr, forward.NewClient(cfg.ResponseHeaderTimeout), cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, cfg.MaxBufferedResponseBytes, nil, forward.WithLogger(logger))
		base := startTestServer(t, server.New(cfg, logger, mgr, imp, fwd, newTestWSProxy(mgr), server.NewStreamOutcomeCounter()))

		assertNotReady(t, base)

		// The readiness gate short-circuits before the forwarder, so no exchange
		// fires and no request reaches Copilot.
		if copilot.path != "" {
			t.Errorf("Copilot was reached %q while degraded; readiness gate leaked", copilot.path)
		}
	})

	t.Run("after a mint failure: 503 + not-ready", func(t *testing.T) {
		// GitHub returns 401 (auth-class): the startup mint short-circuits and the
		// daemon stays degraded.
		github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(github.Close)

		cfg := e2eConfig("gho-secret")
		logger := discardLogger(t)
		mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), productionDiscoveryEdge())
		if err != nil {
			t.Fatalf("buildServeProvider: %v", err)
		}
		mgr.StartupMint(context.Background()) // fails, short-circuits on auth-class
		if mgr.Ready() {
			t.Fatalf("Ready() = true after a failed startup mint, want false")
		}
		fwd := forward.New(mgr, forward.NewClient(cfg.ResponseHeaderTimeout), cfg.OutboundTimeout, cfg.WriteTimeout, cfg.StreamIdleTimeout, cfg.StreamKeepaliveInterval, cfg.MaxRequestBytes, cfg.MaxBufferedResponseBytes, nil, forward.WithLogger(logger))
		base := startTestServer(t, server.New(cfg, logger, mgr, imp, fwd, newTestWSProxy(mgr), server.NewStreamOutcomeCounter()))

		assertNotReady(t, base)
	})
}

// TestRunServeFailsFastWithoutOAuthToken drives the CLI: with a valid config but
// no OAuth token from any source, `serve` exits non-zero with the "run copilotd
// login" message BEFORE binding a listener (never logs "listening").
func TestRunServeFailsFastWithoutOAuthToken(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "serve.log")
	missingTokenFile := filepath.Join(dir, "no-such-token-file")

	code := run([]string{
		"serve",
		"--apikey", "some-key",
		"--github-oauth-token-file", missingTokenFile,
		"--log-file", logFile,
		"--addr", "127.0.0.1:0",
	}, noEnv(), io.Discard, io.Discard)

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (fail-fast)", code)
	}
	logs, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(logs), "copilotd login") {
		t.Errorf("logs missing the 'run copilotd login' guidance:\n%s", logs)
	}
	if strings.Contains(string(logs), "listening") {
		t.Errorf("daemon bound a listener despite the missing token (should fail before bind):\n%s", logs)
	}
}

// --- small helpers ----------------------------------------------------------

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// assertNotReady checks /readyz reports not-ready (503) and an authenticated
// provider request is refused with 503.
func assertNotReady(t *testing.T, base string) {
	t.Helper()
	resp, err := http.Get(base + "/readyz") //nolint:noctx // test poll
	if err != nil {
		t.Fatalf("/readyz: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(rb), "not ready") {
		t.Errorf("/readyz body = %q, want not-ready", rb)
	}

	pr, _ := post(t, base+"/anthropic/v1/messages", `{"model":"x"}`)
	_ = pr.Body.Close()
	if pr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Surface endpoint status = %d, want 503 while degraded", pr.StatusCode)
	}
}
