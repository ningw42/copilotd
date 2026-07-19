package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
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

func startPhase4Server(t *testing.T, cfg config.ServeConfig, provider identity.Provider, logger *slog.Logger) string {
	t.Helper()
	forwarder := forward.New(
		provider,
		forward.NewClient(cfg.ResponseHeaderTimeout),
		cfg.OutboundTimeout,
		cfg.WriteTimeout,
		cfg.StreamIdleTimeout,
		cfg.StreamKeepaliveInterval,
		cfg.MaxRequestBytes,
		cfg.MaxBufferedResponseBytes,
		configuredShimRegistry(cfg),
		forward.WithLogger(logger),
	)
	return startTestServer(t, server.New(cfg, logger, provider, forwarder, nil, server.NewStreamOutcomeCounter()))
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
