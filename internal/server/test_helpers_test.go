package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/wsforward"
)

type staticImpersonationObserver struct {
	header http.Header
}

func (s staticImpersonationObserver) Header() http.Header { return s.header.Clone() }

func newTestReadyObservers() ReadyObservers {
	return ReadyObservers{Impersonation: staticImpersonationObserver{header: http.Header{
		"Copilot-Integration-Id": {"vscode-chat"},
		"Editor-Plugin-Version":  {"copilot-chat/0.26.7"},
		"Editor-Version":         {"vscode/1.104.1"},
		"User-Agent":             {"GitHubCopilotChat/0.26.7"},
		"X-Github-Api-Version":   {"2025-04-01"},
	}}, Caches: staticCacheObserver{}}
}

type staticCacheObserver struct{ statuses []cache.Status }

func (s staticCacheObserver) Observe() []cache.Status {
	return append([]cache.Status(nil), s.statuses...)
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
