package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ningw42/copilotd/internal/identity"
)

func TestPhase4InferenceCorrelationAndRedirectRegressionsEndToEnd(t *testing.T) {
	type upstreamRequest struct {
		route     string
		requestID string
	}
	var (
		requestsMu sync.Mutex
		requests   []upstreamRequest
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests = append(requests, upstreamRequest{
			route:     r.URL.Path,
			requestID: r.Header.Get("X-Request-Id"),
		})
		requestsMu.Unlock()
		w.Header().Set("Location", "/phase4-redirect-target-must-not-be-followed")
		w.Header().Add("X-Request-Id", "phase4-inference-upstream-id-one")
		w.Header().Add("X-Request-Id", "phase4-inference-upstream-id-two")
		w.Header().Set("X-Phase4-First-Response", r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "phase4 first redirect body for "+r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   phase4CopilotToken,
	}, true)
	cfg := e2eConfig("unused-oauth-token")
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	logger := newPhase4Logger(t, &logs)
	base := startPhase4Server(t, cfg, provider, logger)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	tests := []struct {
		name          string
		inboundPath   string
		upstreamRoute string
		requestID     string
	}{
		{
			name:          "Anthropic",
			inboundPath:   "/anthropic/v1/messages",
			upstreamRoute: "/v1/messages",
			requestID:     "phase4-anthropic-regression-correlation",
		},
		{
			name:          "OpenAI",
			inboundPath:   "/openai/v1/responses",
			upstreamRoute: "/responses",
			requestID:     "phase4-openai-regression-correlation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doPhase4Request(t, client, http.MethodPost, base+tc.inboundPath, strings.NewReader(`{"opaque":"phase4-inference-request"}`), func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+phase4APIKey)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Request-Id", tc.requestID)
			})
			if resp.StatusCode != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want first upstream 307", resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/phase4-redirect-target-must-not-be-followed" {
				t.Errorf("Location = %q, want first upstream Location", got)
			}
			if got := resp.Header.Get("X-Phase4-First-Response"); got != tc.upstreamRoute {
				t.Errorf("first-response proof = %q, want %q", got, tc.upstreamRoute)
			}
			if got := string(body); got != "phase4 first redirect body for "+tc.upstreamRoute {
				t.Errorf("body = %q, want first upstream body", got)
			}
			if got := resp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{tc.requestID}) {
				t.Errorf("downstream X-Request-Id = %q, want sole resolved ID", got)
			}
		})
	}

	requestsMu.Lock()
	gotRequests := append([]upstreamRequest(nil), requests...)
	requestsMu.Unlock()
	if len(gotRequests) != len(tests) {
		t.Fatalf("inference upstream calls = %d, want exactly %d first-response calls", len(gotRequests), len(tests))
	}
	for i, tc := range tests {
		if gotRequests[i].route != tc.upstreamRoute || gotRequests[i].requestID != tc.requestID {
			t.Errorf("upstream request %d = Route %q request ID %q, want %q %q", i, gotRequests[i].route, gotRequests[i].requestID, tc.upstreamRoute, tc.requestID)
		}
		if !strings.Contains(logs.String(), "request_id="+tc.requestID) {
			t.Errorf("access logs lack resolved correlation %q:\n%s", tc.requestID, logs.String())
		}
		lines := phase4LogLinesContaining(logs.String(),
			"upstream_request_id=phase4-inference-upstream-id-one",
			"request_id="+tc.requestID,
		)
		if len(lines) != 1 {
			t.Errorf("correlation lines for %q = %d, want one:\n%s", tc.requestID, len(lines), strings.Join(lines, "\n"))
		}
	}
	if strings.Contains(logs.String(), "phase4-inference-upstream-id-two") {
		t.Errorf("secondary upstream request ID appeared in logs:\n%s", logs.String())
	}
}
