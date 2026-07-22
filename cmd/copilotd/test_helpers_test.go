package main

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/wsforward"
)

const testReadyImpersonationJSON = `"caches":{},"impersonation":{"effective_headers":{"Editor-Version":"vscode/1.104.1","Editor-Plugin-Version":"copilot-chat/0.26.7","User-Agent":"GitHubCopilotChat/0.26.7","Copilot-Integration-Id":"vscode-chat","X-GitHub-Api-Version":"2025-04-01"}}`

func newTestImpersonationObserver() *impersonation.Set {
	registry := cache.NewRegistry()
	return impersonation.New(impersonation.Config{
		VSCodeVersionFallback: "1.104.1",
		PluginVersionFallback: "0.26.7",
		CopilotIntegrationID:  "vscode-chat",
		GithubAPIVersion:      "2025-04-01",
	}, impersonation.Edge{}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestCacheObserver() *cache.Registry { return cache.NewRegistry() }

func newTestReadyObservers() server.ReadyObservers {
	return server.ReadyObservers{
		Impersonation: newTestImpersonationObserver(),
		Caches:        newTestCacheObserver(),
	}
}

func newTestWSProxy(provider identity.Provider) *wsforward.Proxy {
	return wsforward.New(
		provider,
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		wsforward.WsMetrics{},
	)
}
