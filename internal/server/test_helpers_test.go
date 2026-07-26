package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/upstream"
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

func newTestForwarder(provider identity.Provider, client *http.Client, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes, maxBufferedResponseBytes int64, registry shim.Registry, options ...forward.Option) *forward.Forwarder {
	caller := upstream.New(provider, client, outboundTimeout, maxBufferedResponseBytes, slog.Default())
	return forward.New(caller, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval, maxRequestBytes, registry, options...)
}

func newTestForwarderWithLogger(provider identity.Provider, client *http.Client, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes, maxBufferedResponseBytes int64, logger *slog.Logger, registry shim.Registry, options ...forward.Option) *forward.Forwarder {
	caller := upstream.New(provider, client, outboundTimeout, maxBufferedResponseBytes, logger)
	return forward.New(caller, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval, maxRequestBytes, registry, options...)
}

func newTestCatalogSource(provider identity.Provider) *upstream.Caller {
	return newTestCatalogSourceWith(provider, forward.NewClient(time.Second), time.Second, 1<<20, slog.Default())
}

func newTestCatalogSourceWith(provider identity.Provider, client *http.Client, outboundTimeout time.Duration, maxBufferedResponseBytes int64, logger *slog.Logger) *upstream.Caller {
	return upstream.New(provider, client, outboundTimeout, maxBufferedResponseBytes, logger)
}

func newTestWSCaller(provider identity.Provider, logger *slog.Logger) *upstream.Caller {
	return upstream.New(provider, http.DefaultClient, time.Second, 1<<20, logger)
}

func newTestWSProxy(provider identity.Provider) *wsforward.Proxy {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	caller := newTestWSCaller(provider, logger)
	return wsforward.New(
		caller,
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		wsforward.WsMetrics{},
	)
}
