package server

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/upstream"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/wsforward"
)

type staticImpersonationObserver struct {
	header http.Header
}

func (s staticImpersonationObserver) Header() http.Header { return s.header.Clone() }

func newTestDependencyErrorLog() *log.Logger { return log.New(io.Discard, "", 0) }

func serverLogLinesContaining(output string, fragments ...string) []string {
	var matched []string
	for _, line := range strings.Split(output, "\n") {
		include := true
		for _, fragment := range fragments {
			if !strings.Contains(line, fragment) {
				include = false
				break
			}
		}
		if include {
			matched = append(matched, line)
		}
	}
	return matched
}

const testShutdownTimeout = 2 * time.Second

// newTestServer builds a Server from the test API key, shutdown timeout and
// readiness observers with base's internal/server child, routing only mounts.
func newTestServer(base *slog.Logger, provider identity.Provider, mounts ...Mount) *Server {
	return newObservedTestServer(base, provider, NewStreamOutcomeCounter(), mounts...)
}

// newObservedTestServer is newTestServer reporting stream outcomes to
// streamOutcomes.
func newObservedTestServer(base *slog.Logger, provider identity.Provider, streamOutcomes StreamOutcomeObserver, mounts ...Mount) *Server {
	return New(testAPIKey, testShutdownTimeout, logging.ForComponent(base, "internal/server"), newTestDependencyErrorLog(), provider, newTestReadyObservers(), streamOutcomes, mounts...)
}

// newTestHandler is newTestServer's router alone, for tests that drive it
// without a listener.
func newTestHandler(base *slog.Logger, provider identity.Provider, mounts ...Mount) http.Handler {
	return newHandler(testAPIKey, provider, newTestReadyObservers(), logging.ForComponent(base, "internal/server"), NewStreamOutcomeCounter(), mounts)
}

// forwardMount, passthroughMount, catalogMount and webSocketMount each mount
// one Endpoint over a test dependency, the way newServeServer does.
func forwardMount(fwd *forward.Forwarder, ep endpoint.HTTPForward) Mount {
	return MountHTTPForward(ep, fwd.Handler(ep))
}

func passthroughMount(fwd *forward.Forwarder) Mount {
	return MountPassthrough(endpoint.Models(), fwd.PassthroughHandler(endpoint.Models()))
}

func catalogMount(base *slog.Logger, ep endpoint.Catalog, catalogs catalog.RenderDescriptors, source catalog.Source) Mount {
	return MountCatalog(ep, catalog.Handler(logging.ForComponent(base, "internal/catalog"), ep, catalogs, source, func(ctx context.Context, shape catalog.Shape) {
		requestsummary.RecordCatalogShape(ctx, string(shape))
	}))
}

func webSocketMount(proxy *wsforward.Proxy) Mount {
	return MountWebSocket(endpoint.OpenAIResponsesWS(), proxy.Handler(endpoint.OpenAIResponsesWS()), proxy)
}

// productionMounts is the full production-shaped route set over test
// dependencies: every Endpoint plus the disabled Usage report.
func productionMounts(base *slog.Logger, fwd *forward.Forwarder, source catalog.Source, wsProxy *wsforward.Proxy, catalogs catalog.RenderDescriptors) []Mount {
	return []Mount{
		forwardMount(fwd, endpoint.AnthropicMessages()),
		forwardMount(fwd, endpoint.AnthropicCountTokens()),
		forwardMount(fwd, endpoint.OpenAIResponsesHTTP()),
		webSocketMount(wsProxy),
		passthroughMount(fwd),
		catalogMount(base, endpoint.AnthropicCatalog(), catalogs, source),
		catalogMount(base, endpoint.OpenAICatalog(), catalogs, source),
		MountReport(reporthttp.Handler(nil)),
	}
}

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
	logger := slog.Default()
	caller := upstream.New(provider, client, outboundTimeout, maxBufferedResponseBytes, logger)
	return forward.New(caller, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval, maxRequestBytes, registry, logger, logger, 0, options...)
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
		logger,
		0,
		wsforward.WsMetrics{},
	)
}
