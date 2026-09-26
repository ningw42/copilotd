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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
)

const testAPIKey = "test-api-key"

// e2eConfig is a resolved ServeConfig with stable synthetic impersonation
// fixtures, a set API key, and the given inline OAuth token — the shape runServe
// would hand the serve lifecycle, minus the flag/env/file plumbing.
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
		VSCodeVersionFallback:        "1.2.3",
		PluginVersionFallback:        "4.5.6",
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

// gitHubExchangeStub fakes GitHub's token endpoint, minting one Copilot token
// whose endpoints.api is apiURL. It keeps the latest exchange request headers.
type gitHubExchangeStub struct {
	server *httptest.Server
	mu     sync.Mutex
	header http.Header
}

func newGitHubExchangeStub(t *testing.T, copilotToken, apiURL string) *gitHubExchangeStub {
	stub := &gitHubExchangeStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.header = r.Header.Clone()
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      copilotToken,
			"expires_at": time.Now().Add(25 * time.Minute).Unix(),
			"refresh_in": 1500,
			"endpoints":  map[string]any{"api": apiURL},
		})
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *gitHubExchangeStub) lastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.Clone()
}

// newSequencedGitHubExchangeStub returns the supplied statuses in order, then
// succeeds on every later exchange. The call count lets assembled-server tests
// prove which inbound requests reached credential acquisition.
func newSequencedGitHubExchangeStub(t *testing.T, apiURL string, statuses ...int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(calls.Add(1))
		status := http.StatusOK
		if attempt <= len(statuses) {
			status = statuses[attempt-1]
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "copilot-recovery-token",
			"expires_at": time.Now().Add(25 * time.Minute).Unix(),
			"refresh_in": 1500,
			"endpoints":  map[string]any{"api": apiURL},
		})
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

type suppliedRegistryShim struct{}

var (
	_ shim.BufferedTransformer      = (*suppliedRegistryShim)(nil)
	_ shim.ServerMessageTransformer = (*suppliedRegistryShim)(nil)
)

func (*suppliedRegistryShim) TransformBuffered(_ context.Context, body *shim.Body) error {
	body.Bytes = []byte(`{"registry":"supplied-http"}`)
	return nil
}

func (*suppliedRegistryShim) TransformServerMessage(_ context.Context, message *shim.Message) bool {
	message.Data = []byte(`{"registry":"supplied-websocket"}`)
	return true
}

func TestServeLifecycleUsesSuppliedShimRegistryForHTTPAndWebSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept upstream WebSocket: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			messageType, _, err := conn.Read(r.Context())
			if err != nil {
				t.Errorf("read upstream WebSocket message: %v", err)
				return
			}
			if err := conn.Write(r.Context(), messageType, []byte(`{"registry":"upstream-websocket"}`)); err != nil {
				t.Errorf("write upstream WebSocket message: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"registry":"upstream-http"}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := lifecycleConfig("gho-supplied-registry")
	cfg.WebSocketHandshakeTimeout = 5 * time.Second
	github := lifecycleExchangeStub(t, upstream.URL, make(chan http.Header, 1))
	registry := shim.Registry{{
		Name:    "supplied-registry-probe",
		Enabled: true,
		Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
			return surface == endpoint.OpenAI && route == endpoint.RouteOpenAIResponses
		},
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return &suppliedRegistryShim{}
		},
	}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	run := startServeLifecycle(t, discardLogger(t), serveInput{
		Config:   cfg,
		Edges:    exchangeServeEdges(github),
		Listener: ln,
		// This test's point is the supplied registry, so it replaces the
		// configured one rather than decorating it.
		DecorateRegistry: func(shim.Registry) shim.Registry { return registry },
	})

	base := "http://" + ln.Addr().String()
	run.awaitHealthy(t, base)

	resp, body := post(t, base+"/openai/v1/responses", `{"model":"gpt"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || body != `{"registry":"supplied-http"}` {
		t.Errorf("HTTP response = %d %q, want supplied registry transform", resp.StatusCode, body)
	}

	webSocketURL := "ws" + strings.TrimPrefix(base, "http") + "/openai/v1/responses"
	conn, response, err := websocket.Dial(context.Background(), webSocketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testAPIKey}},
	})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial WebSocket transport: %v", err)
	}
	wsCtx, wsCancel := context.WithTimeout(context.Background(), time.Second)
	defer wsCancel()
	if err := conn.Write(wsCtx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write WebSocket message: %v", err)
	}
	_, message, err := conn.Read(wsCtx)
	if err != nil {
		t.Fatalf("read WebSocket message: %v", err)
	}
	if got := string(message); got != `{"registry":"supplied-websocket"}` {
		t.Errorf("WebSocket message = %q, want supplied registry transform", got)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	http.DefaultClient.CloseIdleConnections()
	run.cancel()
	if result := run.await(t, 5*time.Second); result.Outcome != serveClean || result.Err != nil {
		t.Errorf("serve lifecycle after cancellation = %v (%v), want clean", result.Outcome, result.Err)
	}
}

// startRecoveryLifecycle serves through the production lifecycle with the
// GitHub exchange at github. With holdStartup, startup is held in the discovery
// edge, before its startup mint, so a request can mint on demand first;
// otherwise discovery is disabled and startup mints right away.
func startRecoveryLifecycle(t *testing.T, github *httptest.Server, holdStartup bool) (string, *usageReportLogs) {
	t.Helper()
	cfg := lifecycleConfig("gho-secret")
	edges := exchangeServeEdges(github)
	if holdStartup {
		held := make(chan struct{})
		discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-held:
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(discovery.Close)
		t.Cleanup(func() { close(held) })
		cfg.ImpersonationRefreshInterval = time.Hour
		edges.Discovery = impersonation.Edge{
			VSCodeBaseURL:      discovery.URL,
			MarketplaceBaseURL: discovery.URL,
			Client:             discovery.Client(),
		}
	}
	logs := newUsageReportLogs()
	return startServedLifecycle(t, newPhase4Logger(t, logs), serveInput{Config: cfg, Edges: edges}), logs
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

	exchange := newGitHubExchangeStub(t, copilotToken, copilot.server.URL)

	cfg := lifecycleConfig(oauth)
	logs := newUsageReportLogs()
	base := startServedLifecycle(t, newPhase4Logger(t, logs), serveInput{Config: cfg, Edges: exchangeServeEdges(exchange.server)})
	// Wait for the background startup mint so the credential cache is warm
	// before the first request.
	logs.await(t, `msg="minted copilot token"`, "trigger=startup")

	assertImpersonation := func(t *testing.T) {
		t.Helper()
		if copilot.auth != "Bearer "+copilotToken {
			t.Errorf("upstream Authorization = %q, want the minted Copilot bearer", copilot.auth)
		}
		if strings.Contains(copilot.auth, testAPIKey) || copilot.hdr.Get("X-Api-Key") != "" {
			t.Errorf("inbound API key leaked upstream (auth=%q)", copilot.auth)
		}
		if copilot.hdr.Get("Copilot-Integration-Id") != "vscode-chat" ||
			copilot.hdr.Get("Editor-Version") != "vscode/1.2.3" ||
			copilot.hdr.Get("User-Agent") != "GitHubCopilotChat/4.5.6" ||
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
	exchangeHeader := exchange.lastHeader()
	if got := exchangeHeader.Get("Authorization"); got != "token "+oauth {
		t.Errorf("exchange Authorization = %q, want %q", got, "token "+oauth)
	}
	if got := exchangeHeader.Get("User-Agent"); got != "GitHubCopilotChat/4.5.6" {
		t.Errorf("exchange User-Agent = %q, want %q", got, "GitHubCopilotChat/4.5.6")
	}
}

// TestServeDiscoveredVersionsEndToEnd proves the bound serve lifecycle carries
// successful startup discovery through the first exchange and the first
// forwarded inference request, and reports the same effective values on
// /readyz. Every outbound edge is stubbed; no Microsoft or GitHub host is used.
func TestServeDiscoveredVersionsEndToEnd(t *testing.T) {
	const (
		discoveredVSCode = "7.8.9"
		discoveredPlugin = "6.5.4"
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
	cfg.VSCodeVersionFallback = "1.2.3"
	cfg.PluginVersionFallback = "4.5.6"
	cfg.ImpersonationRefreshInterval = time.Hour
	edges := exchangeServeEdges(github)
	edges.Discovery = impersonation.Edge{
		VSCodeBaseURL:      discovery.URL,
		MarketplaceBaseURL: discovery.URL,
		Client:             discovery.Client(),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	run := startServeLifecycle(t, discardLogger(t), serveInput{Config: cfg, Edges: edges, Listener: ln})
	base := "http://" + ln.Addr().String()

	// The startup exchange runs after Prime, so its headers are the barrier for
	// discovery having completed.
	select {
	case exchange := <-exchangeHeaders:
		if got, want := exchange.Get("Editor-Version"), "vscode/7.8.9"; got != want {
			t.Errorf("exchange Editor-Version = %q, want discovered %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup exchange did not run after discovery")
	}

	resp, _ := post(t, base+"/anthropic/v1/messages", `{"model":"test"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forward status = %d, want 200", resp.StatusCode)
	}
	if got, want := upstream.hdr.Get("Editor-Plugin-Version"), "copilot-chat/6.5.4"; got != want {
		t.Errorf("forwarded Editor-Plugin-Version = %q, want discovered %q", got, want)
	}
	if got, want := upstream.hdr.Get("User-Agent"), "GitHubCopilotChat/6.5.4"; got != want {
		t.Errorf("forwarded User-Agent = %q, want discovered %q", got, want)
	}

	assertReadyzImpersonation(t, base, discoveredVSCode, discoveredPlugin, "fetched", true)
	http.DefaultClient.CloseIdleConnections()
	run.cancel()
	if result := run.await(t, 5*time.Second); result.Outcome != serveClean || result.Err != nil {
		t.Errorf("serve lifecycle after cancellation = %v (%v), want clean", result.Outcome, result.Err)
	}
}

func TestServeFreshCodexCatalogAndReadinessEndToEnd(t *testing.T) {
	const (
		tag    = "rust-v1.2.3"
		commit = "1234567890abcdef1234567890abcdef12345678"
		oauth  = "gho-codex-freshness"
	)
	fresh := completeCodexModelsBytes(t, "fresh-model", "fresh release prompt")
	upstream := newCopilotStub(t, `{"data":[{"id":"fresh-model","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)

	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			if got := r.Header.Get("Authorization"); got != "token "+oauth {
				t.Errorf("exchange Authorization = %q, want GitHub OAuth token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "copilot-codex-token",
				"expires_at": time.Now().Add(25 * time.Minute).Unix(),
				"refresh_in": 1500,
				"endpoints":  map[string]any{"api": upstream.server.URL},
			})
		case "/repos/openai/codex/releases/latest":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Codex release peek carried Authorization %q", got)
			}
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case "/repos/openai/codex/commits/" + tag:
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Codex commit resolution carried Authorization %q", got)
			}
			if got := r.Header.Get("Accept"); got != "application/vnd.github.sha" {
				t.Errorf("Codex commit Accept = %q, want GitHub SHA media type", got)
			}
			_, _ = io.WriteString(w, commit)
		case "/repos/openai/codex/contents/codex-rs/models-manager/models.json":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("Codex models fetch carried Authorization %q", got)
			}
			if got := r.URL.Query().Get("ref"); got != commit {
				t.Errorf("Codex models ref = %q, want peeled commit %q", got, commit)
			}
			_, _ = w.Write(fresh)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/releases/stable":
			_, _ = io.WriteString(w, `["7.8.9"]`)
		case "/_apis/public/gallery/extensionquery":
			_, _ = io.WriteString(w, `{"results":[{"extensions":[{"versions":[{"version":"6.5.4","properties":[]}]}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(discovery.Close)

	cfg := e2eConfig(oauth)
	cfg.CodexCatalogEnabled = true
	cfg.CodexOverrideLimits = true
	cfg.CodexCatalogRefreshInterval = time.Hour
	edges := exchangeServeEdges(github)
	edges.Discovery = impersonation.Edge{
		VSCodeBaseURL:      discovery.URL,
		MarketplaceBaseURL: discovery.URL,
		Client:             discovery.Client(),
	}
	edges.CodexModels = catalog.ModelsEdge{BaseURL: github.URL, Client: github.Client()}

	base := startServedLifecycle(t, discardLogger(t), serveInput{Config: cfg, Edges: edges})

	awaitCachedValue(t, base, "codex_models", "fetched", tag)
	resp, err := http.Get(base + "/readyz") //nolint:noctx // local e2e server
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	var readiness struct {
		Caches map[string]json.RawMessage `json:"caches"`
	}
	err = json.NewDecoder(resp.Body).Decode(&readiness)
	_ = resp.Body.Close()
	if err != nil || len(readiness.Caches) != 3 {
		t.Fatalf("readiness caches = %v (%v), want vscode, copilot_chat, and codex_models", readiness.Caches, err)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/openai/v1/models?client_version=fixture", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET Codex catalog: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Codex catalog status = %d, want 200: %s", resp.StatusCode, body)
	}
	var rendered struct {
		Models []struct {
			Slug             string `json:"slug"`
			BaseInstructions string `json:"base_instructions"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &rendered); err != nil {
		t.Fatalf("decode Codex catalog: %v; body=%s", err, body)
	}
	if len(rendered.Models) != 1 || rendered.Models[0].Slug != "fresh-model" || rendered.Models[0].BaseInstructions != "fresh release prompt" {
		t.Fatalf("rendered Codex models = %#v, want fetched release entry", rendered.Models)
	}
}

// TestServeRequestDrivenMintRecoveryEndToEnd proves that readiness and request
// admission depend only on the local prerequisites already resolved before the
// server binds. Every authenticated request can therefore reach the real
// identity manager, including before startup warm-up and after either class of
// exchange failure.
func TestServeRequestDrivenMintRecoveryEndToEnd(t *testing.T) {
	t.Run("request before startup warm-up mints and forwards", func(t *testing.T) {
		copilot := newCopilotStub(t, `{"ok":true}`)
		github, exchanges := newSequencedGitHubExchangeStub(t, copilot.server.URL)

		// Startup is held before its mint. The first authenticated request is
		// allowed to perform the on-demand mint itself.
		base, _ := startRecoveryLifecycle(t, github, true)

		assertReadyzImpersonation(t, base, "1.2.3", "4.5.6", "fallback", false)
		resp, _ := post(t, base+"/anthropic/v1/messages", `{"model":"x"}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Surface endpoint status = %d, want 200", resp.StatusCode)
		}
		if got := exchanges.Load(); got != 1 {
			t.Errorf("exchange calls = %d, want 1 on-demand mint", got)
		}
	})

	t.Run("failed startup warm-up does not block a request mint", func(t *testing.T) {
		copilot := newCopilotStub(t, `{"ok":true}`)
		github, exchanges := newSequencedGitHubExchangeStub(t, copilot.server.URL, http.StatusUnauthorized, http.StatusOK)
		base, logs := startRecoveryLifecycle(t, github, false)
		logs.await(t, "startup mint short-circuited")

		assertReadyzImpersonation(t, base, "1.2.3", "4.5.6", "fallback", false)
		resp, _ := post(t, base+"/anthropic/v1/messages", `{"model":"x"}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Surface endpoint status = %d, want 200 after startup failure", resp.StatusCode)
		}
		if got := exchanges.Load(); got != 2 {
			t.Errorf("exchange calls = %d, want failed startup + successful on-demand", got)
		}
	})

	for _, firstFailure := range []struct {
		name   string
		status int
	}{
		{name: "transient", status: http.StatusInternalServerError},
		{name: "auth-class", status: http.StatusUnauthorized},
	} {
		t.Run(firstFailure.name+" on-demand failure is request-scoped", func(t *testing.T) {
			copilot := newCopilotStub(t, `{"ok":true}`)
			github, exchanges := newSequencedGitHubExchangeStub(t, copilot.server.URL, firstFailure.status, http.StatusOK)
			// Startup is held before its mint, so the first failure status
			// belongs to the first authenticated request.
			base, _ := startRecoveryLifecycle(t, github, true)

			unauthenticated, err := http.Post(base+"/anthropic/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`)) //nolint:noctx // local test server
			if err != nil {
				t.Fatalf("unauthenticated request: %v", err)
			}
			_ = unauthenticated.Body.Close()
			if unauthenticated.StatusCode != http.StatusUnauthorized {
				t.Errorf("unauthenticated status = %d, want 401", unauthenticated.StatusCode)
			}
			if got := exchanges.Load(); got != 0 {
				t.Fatalf("unauthenticated request caused %d exchanges, want 0", got)
			}

			failed, failedBody := post(t, base+"/anthropic/v1/messages", `{"model":"x"}`)
			_ = failed.Body.Close()
			if failed.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("failed-mint request status = %d, want 503", failed.StatusCode)
			}
			if want := `{"type":"error","error":{"type":"api_error","message":"no upstream credential available"}}`; failedBody != want {
				t.Errorf("failed-mint response = %q, want %q", failedBody, want)
			}
			if copilot.path != "" {
				t.Errorf("failed-mint request reached Copilot path %q", copilot.path)
			}
			if got := exchanges.Load(); got != 1 {
				t.Fatalf("exchange calls after failed request = %d, want 1", got)
			}
			assertReadyzImpersonation(t, base, "1.2.3", "4.5.6", "fallback", false)

			recovered, _ := post(t, base+"/anthropic/v1/messages", `{"model":"x"}`)
			_ = recovered.Body.Close()
			if recovered.StatusCode != http.StatusOK {
				t.Fatalf("recovery request status = %d, want 200", recovered.StatusCode)
			}
			if got := exchanges.Load(); got != 2 {
				t.Errorf("exchange calls after recovery = %d, want 2", got)
			}
		})
	}
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

func completeCodexModelsBytes(t *testing.T, slug, prompt string) []byte {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"models": []any{map[string]any{
			"slug":                         slug,
			"display_name":                 "Fresh model",
			"supported_reasoning_levels":   []any{map[string]any{"effort": "medium", "description": "Balanced"}},
			"shell_type":                   "shell_command",
			"visibility":                   "list",
			"supported_in_api":             true,
			"priority":                     1,
			"base_instructions":            prompt,
			"supports_reasoning_summaries": true,
			"support_verbosity":            true,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls": true,
			"experimental_supported_tools": []string{},
			"model_messages": map[string]any{
				"instructions_template":  "{{ instructions }}",
				"instructions_variables": map[string]string{"personality_default": ""},
				"approvals":              nil,
			},
		}},
	})
	if err != nil {
		t.Fatalf("encode complete Codex models bytes: %v", err)
	}
	return encoded
}
