package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
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
// The provider routes carry two additional inner wrappers — auth then readiness —
// applied per route, because Go's ServeMux has no subtree middleware. The full
// order on a provider route is therefore requestID -> accessLog -> recover ->
// auth -> readiness -> forward. /healthz and /readyz are never gated by auth or
// readiness.
func newHandler(apikey string, provider identity.Provider, fwd *forward.Forwarder, logger *slog.Logger, streamOutcomes StreamOutcomeObserver, codexConfig config.CodexConfig, wsProxy *wsforward.Proxy) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, handleHealth)
	mux.HandleFunc("GET "+readyPath, handleReady(provider))

	// guard applies the provider-route-specific inner wrappers in order: auth
	// (outer) then readiness (inner), so auth runs first.
	guard := func(tag apierror.Surface, h http.Handler) http.Handler {
		return authMW(apikey, tag, readinessMW(provider, tag, h))
	}

	// Surface routes: the explicit inbound->upstream map (not a blind prefix
	// strip — note the /v1 asymmetry: Anthropic keeps /v1 upstream, OpenAI drops
	// it). Anthropic requests are not peeked; the OpenAI peek rejects only
	// background:true (tag == apierror.OpenAI).
	mux.Handle("POST /anthropic/v1/messages",
		guard(apierror.Anthropic, fwd.Handler("/v1/messages", apierror.Anthropic)))
	mux.Handle("POST /anthropic/v1/messages/count_tokens",
		guard(apierror.Anthropic, fwd.Handler("/v1/messages/count_tokens", apierror.Anthropic)))
	mux.Handle("POST /openai/v1/responses",
		guard(apierror.OpenAI, fwd.Handler("/responses", apierror.OpenAI)))
	if wsProxy != nil {
		mux.Handle("GET /openai/v1/responses",
			guard(apierror.OpenAI, wsProxy.Handler()))
	}
	anthropicModels := catalog.Handler(catalog.Descriptor{
		Surface:       apierror.Anthropic,
		RequiredRoute: catalog.AnthropicMessagesRoute,
		Render:        catalog.RenderAnthropic,
	}, fwd)
	mux.Handle("GET /anthropic/v1/models", guard(apierror.Anthropic, anthropicModels))
	mux.Handle("HEAD /anthropic/v1/models", guard(apierror.Anthropic, anthropicModels))
	openAIModels := catalog.Handler(catalog.Descriptor{
		Surface:       apierror.OpenAI,
		RequiredRoute: catalog.OpenAIResponsesRoute,
		Render:        catalog.RenderOpenAI,
		Codex: catalog.CodexDescriptor{
			Enabled: codexConfig.Enabled,
			RenderConfig: catalog.CodexRenderConfig{
				AutoReviewModel: codexConfig.AutoReviewModel,
				OverrideLimits:  codexConfig.OverrideLimits,
			},
		},
		Logger: logger,
	}, fwd)
	mux.Handle("GET /openai/v1/models", guard(apierror.OpenAI, openAIModels))
	mux.Handle("HEAD /openai/v1/models", guard(apierror.OpenAI, openAIModels))
	mux.Handle("GET /models",
		guard(apierror.GitHubCopilot,
			fwd.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)))
	mux.Handle("HEAD /models",
		guard(apierror.GitHubCopilot,
			fwd.PassthroughHandler(http.MethodHead, "/models", apierror.GitHubCopilot)))

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
