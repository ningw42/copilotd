package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/wsforward"
)

const (
	healthPath = "/healthz"
	readyPath  = "/readyz"
)

// newHandler builds the router wrapped in the middleware chain
// requestID -> accessLog -> recover (outermost to innermost). RequestID is
// outermost so its context is visible to the inner two; recover is innermost so
// the 500 it produces is what the access log records.
//
// Surface endpoints carry two additional inner wrappers — auth then local
// readiness — applied per route, because Go's ServeMux has no subtree
// middleware. The full order on a Surface endpoint is therefore requestID ->
// accessLog -> recover -> auth -> local readiness -> forward. /healthz and
// /readyz are never gated by auth or readiness.
func newHandler(apikey string, provider identity.Provider, observer ImpersonationObserver, fwd *forward.Forwarder, logger *slog.Logger, streamOutcomes StreamOutcomeObserver, codexConfig config.CodexConfig, wsProxy *wsforward.Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, handleHealth)
	mux.HandleFunc("GET "+readyPath, handleReady(provider, observer))
	codexDesc := catalog.CodexDescriptor{
		Enabled: codexConfig.Enabled,
		RenderConfig: catalog.CodexRenderConfig{
			AutoReviewModel: codexConfig.AutoReviewModel,
			OverrideLimits:  codexConfig.OverrideLimits,
		},
	}

	// guard applies the Surface-endpoint-specific inner wrappers in order: auth
	// (outer) then local readiness (inner), so auth runs first.
	guard := func(surface endpoint.Surface, h http.Handler) http.Handler {
		return authMW(apikey, surface, readinessMW(provider, surface, h))
	}
	mount := func(ep endpoint.Endpoint, h http.Handler) {
		guarded := guard(ep.Surface(), h)
		for _, pattern := range ep.Patterns() {
			mux.Handle(pattern, guarded)
		}
	}
	registerForward := func(ep endpoint.HTTPForward) { mount(ep, fwd.Handler(ep)) }
	registerWS := func(ep endpoint.WSForward) { mount(ep, wsProxy.Handler(ep)) }
	registerPassthrough := func(ep endpoint.Passthrough) { mount(ep, fwd.PassthroughHandler(ep)) }
	registerCatalog := func(ep endpoint.Catalog, rendering catalog.Rendering) {
		mount(ep, catalog.Handler(ep, rendering, fwd))
	}

	registerForward(endpoint.AnthropicMessages())
	registerForward(endpoint.AnthropicCountTokens())
	registerForward(endpoint.OpenAIResponsesHTTP())
	registerWS(endpoint.OpenAIResponsesWS())
	registerPassthrough(endpoint.Models())
	registerCatalog(endpoint.AnthropicCatalog(), catalog.Rendering{Render: catalog.RenderAnthropic})
	registerCatalog(endpoint.OpenAICatalog(), catalog.Rendering{Render: catalog.RenderOpenAI, Codex: codexDesc, Logger: logger})

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
