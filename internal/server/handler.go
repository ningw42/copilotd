package server

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

const (
	healthPath = "/healthz"
	readyPath  = "/readyz"
)

// newHandler builds the router wrapped in requestID -> accessLog -> recover.
// Each mounted binding then derives its logging scope between the mux and the
// auth/readiness guards, so rejected Endpoint requests retain the binding's
// scope. The full Endpoint order is requestID -> accessLog -> recover -> mux ->
// scoped -> auth -> local readiness -> handler. Probes use scoped -> handler and
// are never gated by auth or readiness. The local report mount also bypasses
// those guards, but receives ordinary non-probe access classification.
func newHandler(apikey string, provider identity.Provider, observers ReadyObservers, logger *slog.Logger, streamOutcomes StreamOutcomeObserver, mounts []Mount) http.Handler {
	mux := http.NewServeMux()
	registerProbe := func(pattern string, handler http.Handler) {
		attrs := []slog.Attr{slog.String(logging.InboundKey, pattern)}
		mux.Handle(pattern, scoped(attrs, true, handler))
	}
	registerProbe("GET "+healthPath, http.HandlerFunc(handleHealth))
	registerProbe("GET "+readyPath, handleReady(provider, observers.Impersonation, observers.Caches))

	for _, mount := range mounts {
		handler := mount.handler
		if mount.gated {
			// Auth (outer) then local readiness (inner), so auth runs first.
			handler = authMW(apikey, mount.surface, readinessMW(provider, mount.surface, handler))
		}
		for _, route := range mount.routes {
			mux.Handle(route.pattern, scoped(route.scope, false, handler))
		}
	}

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
