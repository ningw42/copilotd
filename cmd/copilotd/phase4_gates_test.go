package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ningw42/copilotd/internal/identity"
)

func TestPhase4ModelsGatesAndRouterEndToEnd(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "phase4-copilot-token",
	}, false)
	cfg := e2eConfig("unused-oauth-token")
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	base := startPhase4Server(t, cfg, provider, newPhase4Logger(t, &logs))

	for _, public := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"not ready",` + testReadyImpersonationJSON + `}`},
	} {
		resp, body := doPhase4Request(t, nil, http.MethodGet, base+public.path, nil, nil)
		if resp.StatusCode != public.wantStatus || string(body) != public.wantBody {
			t.Errorf("unauthenticated GET %s = status %d body %q, want %d %q", public.path, resp.StatusCode, body, public.wantStatus, public.wantBody)
		}
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, auth := range []struct {
			name      string
			configure func(*http.Request)
		}{
			{name: "missing"},
			{name: "wrong Bearer", configure: func(req *http.Request) { req.Header.Set("Authorization", "Bearer wrong-phase4-key") }},
			{name: "wrong x-api-key", configure: func(req *http.Request) { req.Header.Set("X-Api-Key", "wrong-phase4-key") }},
		} {
			t.Run(method+" rejects "+auth.name+" auth before readiness", func(t *testing.T) {
				resp, body := doPhase4Request(t, nil, method, base+"/models", nil, auth.configure)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401", resp.StatusCode)
				}
				if method == http.MethodGet && !strings.Contains(string(body), `"type":"authentication_error"`) {
					t.Errorf("GET body = %q, want Anthropic-shaped authentication error", body)
				}
				if method == http.MethodHead && len(body) != 0 {
					t.Errorf("HEAD wire body = %q, want empty", body)
				}
			})
		}

		t.Run(method+" exposes readiness only after valid auth", func(t *testing.T) {
			resp, body := doPhase4Request(t, nil, method, base+"/models", nil, func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+phase4APIKey)
			})
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if method == http.MethodGet && !strings.Contains(string(body), `"message":"service not ready"`) {
				t.Errorf("GET body = %q, want readiness error", body)
			}
			if method == http.MethodHead && len(body) != 0 {
				t.Errorf("HEAD wire body = %q, want empty", body)
			}
		})
	}

	resp, _ := doPhase4Request(t, nil, http.MethodPost, base+"/models", nil, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+phase4APIKey)
	})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /models status = %d, want standard 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("POST /models Allow = %q, want %q", got, "GET, HEAD")
	}

	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("stub Copilot calls = %d, want zero for gate and router failures", got)
	}
}
