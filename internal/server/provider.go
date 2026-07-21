package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
)

// ImpersonationObserver supplies the non-secret snapshot rendered by /readyz.
// The interface lives at the consuming HTTP boundary; impersonation owns only
// the observed state and remains unaware of handlers and JSON rendering.
type ImpersonationObserver interface {
	Observe() impersonation.Observed
}

type readyResponse struct {
	Status        string                     `json:"status"`
	Impersonation readyImpersonationResponse `json:"impersonation"`
}

type readyImpersonationResponse struct {
	EffectiveHeaders readyEffectiveHeaders `json:"effective_headers"`
	Discovery        readyDiscovery        `json:"discovery"`
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

type readyDiscovery struct {
	VSCode      readyDiscoveryFact `json:"vscode"`
	CopilotChat readyDiscoveryFact `json:"copilot_chat"`
}

type readyDiscoveryFact struct {
	Source      string     `json:"source"`
	LastSuccess *time.Time `json:"last_success"`
}

// handleReady reports whether identity has the local prerequisites needed to
// attempt service, distinct from /healthz liveness and independent of Copilot
// token mint outcomes. Its unauthenticated body includes only the allowlisted
// effective impersonation headers and per-fact discovery source/last-success.
// The GET pattern also serves HEAD, for which no body is written.
func handleReady(provider identity.Provider, observer ImpersonationObserver) http.HandlerFunc {
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

		body, _ := json.Marshal(newReadyResponse(status, observer.Observe()))
		_, _ = w.Write(body)
	}
}

func newReadyResponse(status string, observed impersonation.Observed) readyResponse {
	headers := observed.EffectiveHeaders
	return readyResponse{
		Status: status,
		Impersonation: readyImpersonationResponse{
			EffectiveHeaders: readyEffectiveHeaders{
				EditorVersion:        headers.Get("Editor-Version"),
				EditorPluginVersion:  headers.Get("Editor-Plugin-Version"),
				UserAgent:            headers.Get("User-Agent"),
				CopilotIntegrationID: headers.Get("Copilot-Integration-Id"),
				GithubAPIVersion:     headers.Get("X-GitHub-Api-Version"),
			},
			Discovery: readyDiscovery{
				VSCode: readyDiscoveryFact{
					Source:      observed.Discovery.VSCode.Source,
					LastSuccess: observed.Discovery.VSCode.LastSuccess,
				},
				CopilotChat: readyDiscoveryFact{
					Source:      observed.Discovery.CopilotChat.Source,
					LastSuccess: observed.Discovery.CopilotChat.LastSuccess,
				},
			},
		},
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
