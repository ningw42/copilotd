package main

import (
	"io"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/upstream"
	"github.com/ningw42/copilotd/internal/wsforward"
)

const testReadyImpersonationJSON = `"caches":{},"impersonation":{"effective_headers":{"Editor-Version":"vscode/1.136.1","Editor-Plugin-Version":"copilot-chat/0.48.1","User-Agent":"GitHubCopilotChat/0.48.1","Copilot-Integration-Id":"vscode-chat","X-GitHub-Api-Version":"2025-04-01"}}`

func newTestImpersonationObserver() *impersonation.Set {
	registry := cache.NewRegistry()
	return impersonation.New(impersonation.Config{
		VSCodeVersionFallback: "1.136.1",
		PluginVersionFallback: "0.48.1",
		CopilotIntegrationID:  "vscode-chat",
		GithubAPIVersion:      "2025-04-01",
	}, impersonation.Edge{}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestCacheObserver() *cache.Registry { return cache.NewRegistry() }

func newTestDependencyErrorLog() *log.Logger { return log.New(io.Discard, "", 0) }

func newTestReadyObservers() server.ReadyObservers {
	return server.ReadyObservers{
		Impersonation: newTestImpersonationObserver(),
		Caches:        newTestCacheObserver(),
	}
}

func newTestForwarderWithLogger(provider identity.Provider, client *http.Client, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes, maxBufferedResponseBytes int64, logger *slog.Logger, registry shim.Registry, options ...forward.Option) *forward.Forwarder {
	caller := upstream.New(provider, client, outboundTimeout, maxBufferedResponseBytes, logger)
	return forward.New(caller, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval, maxRequestBytes, registry, logger, logger, 0, options...)
}

func newTestCatalogSource(provider identity.Provider) *upstream.Caller {
	return upstream.New(provider, forward.NewClient(time.Second), time.Second, 1<<20, slog.Default())
}

func newTestWSProxy(provider identity.Provider) *wsforward.Proxy {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	caller := upstream.New(provider, http.DefaultClient, time.Second, 1<<20, logger)
	return wsforward.New(
		caller,
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		wsforward.WsMetrics{},
	)
}
