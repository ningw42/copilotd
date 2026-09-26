package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/logging"
)

const (
	phase4APIKey       = "phase4-inbound-api-key-sentinel"
	phase4CopilotToken = "phase4-copilot-token-sentinel"
)

func newPhase4Logger(t *testing.T, dst io.Writer) *slog.Logger {
	t.Helper()
	logger, err := logging.NewWithWriter(dst, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build Phase 4 logger: %v", err)
	}
	return logger
}

func phase4LogLinesContaining(logOutput string, fragments ...string) []string {
	var matches []string
	for _, line := range strings.Split(logOutput, "\n") {
		match := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				match = false
				break
			}
		}
		if match {
			matches = append(matches, line)
		}
	}
	return matches
}

// newPhase4ExchangeStub mints phase4CopilotToken with its API base at upstreamURL.
func newPhase4ExchangeStub(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+phase4CopilotToken+`","expires_at":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`,"refresh_in":3600,"endpoints":{"api":"`+upstreamURL+`"}}`)
	}))
	t.Cleanup(exchange.Close)
	return exchange
}

// startPhase4Lifecycle serves cfg through the production lifecycle with the
// GitHub exchange at exchange and discovery disabled.
func startPhase4Lifecycle(t *testing.T, cfg config.ServeConfig, exchange *httptest.Server, logger *slog.Logger) string {
	t.Helper()
	cfg.ImpersonationRefreshInterval = 0
	return startServedLifecycle(t, logger, serveInput{Config: cfg, Edges: exchangeServeEdges(exchange)})
}

// phase4ReadyzBody is /readyz's exact ready body for cfg with discovery
// disabled: the configured impersonation facts, and both registered
// impersonation cached values still on their configured fallbacks.
func phase4ReadyzBody(cfg config.ServeConfig) string {
	cached := func(version string) string {
		return `{"source":"fallback","version":"` + version + `","last_success":null,"last_attempt":null,"last_attempt_result":null}`
	}
	return `{"status":"ready","caches":{"copilot_chat":` + cached(cfg.PluginVersionFallback) + `,"vscode":` + cached(cfg.VSCodeVersionFallback) + `},` +
		`"impersonation":{"effective_headers":{"Editor-Version":"vscode/` + cfg.VSCodeVersionFallback +
		`","Editor-Plugin-Version":"copilot-chat/` + cfg.PluginVersionFallback +
		`","User-Agent":"GitHubCopilotChat/` + cfg.PluginVersionFallback +
		`","Copilot-Integration-Id":"` + cfg.CopilotIntegrationID +
		`","X-GitHub-Api-Version":"` + cfg.GithubAPIVersion + `"}}}`
}

func performPhase4Request(
	client *http.Client,
	method string,
	url string,
	body io.Reader,
	configure func(*http.Request),
) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if configure != nil {
		configure(req)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read %s %s response: %w", method, url, err)
	}
	return resp, responseBody, nil
}

func doPhase4Request(
	t *testing.T,
	client *http.Client,
	method string,
	url string,
	body io.Reader,
	configure func(*http.Request),
) (*http.Response, []byte) {
	t.Helper()
	resp, responseBody, err := performPhase4Request(client, method, url, body, configure)
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}
