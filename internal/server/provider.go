package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
)

// handleReady reports readiness — identity's last mint outcome, distinct from
// /healthz liveness: 200 {"status":"ready"} when ready, else 503
// {"status":"not ready"}. It is unauthenticated by design (it leaks no secret and
// exposes only a coarse ready bit). The GET pattern also serves HEAD, for which
// no body is written.
func handleReady(provider identity.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if provider.Ready() {
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, `{"status":"ready"}`)
			}
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"status":"not ready"}`)
		}
	}
}

// authMW gates a Surface endpoint on the inbound API key. The presented key
// (Authorization: Bearer <k> or x-api-key: <k>) is compared to the configured key
// in constant time over their SHA-256 digests — fixed length, so no length leak.
// A missing, empty, or mismatched key yields a provider-shaped 401; the key is
// never logged. authMW wraps readinessMW, so an unauthenticated caller always
// gets 401 — never a 503 that would leak readiness state.
func authMW(apikey string, surface endpoint.Surface, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(apikey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := presentedKey(r)
		got := sha256.Sum256([]byte(presented))
		if presented == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			apierror.Write(w, surface, apierror.Unauthorized, "missing or invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentedKey extracts the inbound API key from either accepted scheme:
// Authorization: Bearer <k> (scheme case-insensitive) or x-api-key: <k>.
func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// readinessMW gates a Surface endpoint on identity readiness, applied after auth.
// When not ready it returns a provider-shaped 503; the next request can
// transparently re-mint once identity recovers.
func readinessMW(provider identity.Provider, surface endpoint.Surface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !provider.Ready() {
			apierror.Write(w, surface, apierror.NotReady, "service not ready")
			return
		}
		next.ServeHTTP(w, r)
	})
}
