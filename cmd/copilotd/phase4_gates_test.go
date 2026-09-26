package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPhase4ModelsGatesAndRouterEndToEnd covers the gates reachable on a
// production server, which is always locally ready. Not-ready gate cases live
// in internal/server's gate tests.
func TestPhase4ModelsGatesAndRouterEndToEnd(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	cfg := e2eConfig(phase4GitHubOAuthToken)
	cfg.APIKey = phase4APIKey
	base := startPhase4Lifecycle(t, cfg, newPhase4ExchangeStub(t, upstream.URL), newPhase4Logger(t, newUsageReportLogs()))

	for _, public := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{path: "/readyz", wantStatus: http.StatusOK, wantBody: phase4ReadyzBody(cfg)},
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
			t.Run(method+" rejects "+auth.name+" auth", func(t *testing.T) {
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
