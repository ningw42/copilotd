package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
)

// ImpersonationObserver supplies the non-secret effective headers rendered by
// /readyz. The interface lives at the consuming HTTP boundary.
type ImpersonationObserver interface {
	Header() http.Header
}

// CacheObserver supplies uniform non-secret cached-value freshness for
// /readyz.
type CacheObserver interface {
	Observe() []cache.Status
}

// ReadyObservers groups the non-secret observations rendered by /readyz.
type ReadyObservers struct {
	Impersonation ImpersonationObserver
	Caches        CacheObserver
}

type readyResponse struct {
	Status        string                     `json:"status"`
	Caches        map[string]readyCache      `json:"caches"`
	Impersonation readyImpersonationResponse `json:"impersonation"`
}

type readyImpersonationResponse struct {
	EffectiveHeaders readyEffectiveHeaders `json:"effective_headers"`
}

// readyEffectiveHeaders is deliberately an allowlist. Even if an observer is
// accidentally handed a broader header map, /readyz cannot render a credential
// or another unexpected value from it.
type readyEffectiveHeaders struct {
	EditorVersion        string `json:"Editor-Version"`
	EditorPluginVersion  string `json:"Editor-Plugin-Version"`
	UserAgent            string `json:"User-Agent"`
	CopilotIntegrationID string `json:"Copilot-Integration-Id"`
	GithubAPIVersion     string `json:"X-GitHub-Api-Version"`
}

type readyCache struct {
	Source      string     `json:"source"`
	Version     string     `json:"version"`
	LastSuccess *time.Time `json:"last_success"`
}

// handleReady reports whether identity has the local prerequisites needed to
// attempt service, distinct from /healthz liveness and independent of Copilot
// token mint outcomes. Its unauthenticated body includes only the allowlisted
// effective impersonation headers and uniform cached-value freshness.
// The GET pattern also serves HEAD, for which no body is written.
func handleReady(provider identity.Provider, impersonation ImpersonationObserver, caches CacheObserver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "not ready"
		statusCode := http.StatusServiceUnavailable
		if provider.Ready() {
			status = "ready"
			statusCode = http.StatusOK
		}
		w.WriteHeader(statusCode)
		if r.Method == http.MethodHead {
			return
		}

		body, _ := json.Marshal(newReadyResponse(status, impersonation.Header(), caches.Observe()))
		_, _ = w.Write(body)
	}
}

func newReadyResponse(status string, headers http.Header, statuses []cache.Status) readyResponse {
	response := readyResponse{
		Status: status,
		Caches: make(map[string]readyCache, len(statuses)),
		Impersonation: readyImpersonationResponse{
			EffectiveHeaders: readyEffectiveHeaders{
				EditorVersion:        headers.Get("Editor-Version"),
				EditorPluginVersion:  headers.Get("Editor-Plugin-Version"),
				UserAgent:            headers.Get("User-Agent"),
				CopilotIntegrationID: headers.Get("Copilot-Integration-Id"),
				GithubAPIVersion:     headers.Get("X-GitHub-Api-Version"),
			},
		},
	}
	for _, status := range statuses {
		response.Caches[status.Name] = readyCache{
			Source:      status.Source,
			Version:     status.Version,
			LastSuccess: status.LastSuccess,
		}
	}
	return response
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

// readinessMW gates a Surface endpoint on local identity prerequisites, applied
// after auth. Remote mint outcomes never change this signal, so authenticated
// requests continue to credential acquisition after an exchange failure.
func readinessMW(provider identity.Provider, surface endpoint.Surface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !provider.Ready() {
			apierror.Write(w, surface, apierror.NotReady, "service not ready")
			return
		}
		next.ServeHTTP(w, r)
	})
}
