package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/wsforward"
)

const (
	healthPath = "/healthz"
	readyPath  = "/readyz"
)

// newHandler builds the router wrapped in requestID -> accessLog -> recover.
// Each registered binding then derives its logging scope between the mux and
// the auth/readiness guards, so rejected Endpoint requests retain the binding's
// scope. The full Endpoint order is requestID -> accessLog -> recover -> mux ->
// scoped -> auth -> local readiness -> handler. Probes use scoped -> handler and
// are never gated by auth or readiness.
// Invariant: catalog settings cross the render seam only through catalogs.
func newHandler(apikey string, provider identity.Provider, observers ReadyObservers, fwd *forward.Forwarder, source catalog.Source, logger, catalogLogger *slog.Logger, streamOutcomes StreamOutcomeObserver, catalogs catalog.RenderDescriptors, wsProxy *wsforward.Proxy) http.Handler {
	mux := http.NewServeMux()
	registerProbe := func(pattern string, handler http.Handler) {
		attrs := []slog.Attr{slog.String(logging.InboundKey, pattern)}
		mux.Handle(pattern, scoped(attrs, true, handler))
	}
	registerProbe("GET "+healthPath, http.HandlerFunc(handleHealth))
	registerProbe("GET "+readyPath, handleReady(provider, observers.Impersonation, observers.Caches))

	// guard applies the Surface-endpoint-specific inner wrappers in order: auth
	// (outer) then local readiness (inner), so auth runs first.
	guard := func(surface endpoint.Surface, h http.Handler) http.Handler {
		return authMW(apikey, surface, readinessMW(provider, surface, h))
	}
	mount := func(ep endpoint.Endpoint, h http.Handler) {
		guarded := guard(ep.Surface(), h)
		for _, pattern := range ep.Patterns() {
			attrs := []slog.Attr{
				slog.String(logging.InboundKey, pattern),
				slog.String(logging.SurfaceKey, ep.Surface().String()),
			}
			mux.Handle(pattern, scoped(attrs, false, guarded))
		}
	}
	registerForward := func(ep endpoint.HTTPForward) { mount(ep, fwd.Handler(ep)) }
	registerWS := func(ep endpoint.WSForward) {
		guarded := guard(ep.Surface(), wsProxy.Handler(ep))
		for _, pattern := range ep.Patterns() {
			attrs := []slog.Attr{
				slog.String(logging.InboundKey, pattern),
				slog.String(logging.SurfaceKey, ep.Surface().String()),
				slog.Bool(logging.WSKey, true),
			}
			mux.Handle(pattern, scoped(attrs, false, guarded))
		}
	}
	registerPassthrough := func(ep endpoint.Passthrough) { mount(ep, fwd.PassthroughHandler(ep)) }
	registerCatalog := func(ep endpoint.Catalog, rendering catalog.Rendering) {
		mount(ep, catalog.Handler(catalogLogger, ep, rendering, source))
	}

	registerForward(endpoint.AnthropicMessages())
	registerForward(endpoint.AnthropicCountTokens())
	registerForward(endpoint.OpenAIResponsesHTTP())
	registerWS(endpoint.OpenAIResponsesWS())
	registerPassthrough(endpoint.Models())
	registerCatalog(endpoint.AnthropicCatalog(), catalog.Rendering{Render: func(models []catalog.Model) ([]byte, error) {
		return catalog.RenderAnthropicWithConfig(models, catalogs.Anthropic)
	}})
	registerCatalog(endpoint.OpenAICatalog(), catalog.Rendering{Render: catalog.RenderOpenAI, Codex: catalogs.Codex})

	return requestID(accessLog(logger, streamOutcomes, recoverMW(logger, mux)))
}

// handleHealth reports liveness only: 200 with {"status":"ok"}. It deliberately
// does not expose the build version on this unauthenticated endpoint. The GET
// pattern also serves HEAD, for which no body is written.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}
