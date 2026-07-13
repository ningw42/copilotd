package server

import (
	"io"
	"log/slog"
	"net/http"
)

const healthPath = "/healthz"

// newHandler builds the router wrapped in the middleware chain
// requestID -> accessLog -> recover (outermost to innermost). RequestID is
// outermost so its context is visible to the inner two; recover is innermost so
// the 500 it produces is what the access log records.
func newHandler(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, handleHealth)
	return requestID(accessLog(logger, recoverMW(logger, mux)))
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
