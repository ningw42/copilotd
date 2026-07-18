package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
)

func TestOpenAIModelCatalogMapsFetchFailuresOverRealListener(t *testing.T) {
	tests := []struct {
		name        string
		upstreamErr error
		wantStatus  int
	}{
		{name: "timeout", upstreamErr: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout},
		{name: "unreachable", upstreamErr: errors.New("dial failed"), wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := identity.NewStatic(identity.Credential{
				BaseURL: "https://upstream.invalid",
				Token:   "copilot-token",
			}, true)
			client := &http.Client{Transport: serverRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, tt.upstreamErr
			})}
			forwarder := forward.New(provider, client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
			base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, NewStreamOutcomeCounter()))

			req, err := http.NewRequest(http.MethodGet, base+"/openai/v1/models", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET catalog: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if resp.StatusCode != tt.wantStatus || openaiErrorType(t, body) != "api_error" {
				t.Errorf("fetch failure response = %d %s, want %d OpenAI api_error", resp.StatusCode, body, tt.wantStatus)
			}
		})
	}
}

func TestOpenAIModelCatalogOverRealListener(t *testing.T) {
	captured, err := os.ReadFile("../catalog/testdata/copilot-models-2026-07-18.json")
	if err != nil {
		t.Fatalf("read captured catalog: %v", err)
	}

	var hits atomic.Int32
	var responseMode atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/models" || r.URL.RawQuery != "" {
			t.Errorf("upstream request = %s %s, want GET /models without client pagination query", r.Method, r.URL.RequestURI())
		}
		w.Header().Set("X-Request-Id", "different-upstream-id")
		switch responseMode.Load() {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"copilot":"upstream secret error shape"}`)
		case 2:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<html>not a model catalog</html>`)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(captured)
		}
	}))
	defer upstream.Close()

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}, true)
	forwarder := forward.New(provider, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, NewStreamOutcomeCounter()))

	do := func(method, keyHeader, key string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+"/openai/v1/models?limit=1", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if keyHeader != "" {
			req.Header.Set(keyHeader, key)
		}
		req.Header.Set("X-Request-Id", "catalog-client-id")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s catalog: %v", method, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s catalog: %v", method, err)
		}
		return resp, body
	}

	getResp, getBody := do(http.MethodGet, "Authorization", "Bearer "+testAPIKey)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", getResp.StatusCode, getBody)
	}
	if got := getResp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("GET Content-Type = %q, want application/json", got)
	}
	if got := getResp.Header.Get("Content-Length"); got != strconv.Itoa(len(getBody)) {
		t.Errorf("GET Content-Length = %q, want %d", got, len(getBody))
	}
	if got := getResp.Header.Get("X-Request-Id"); got != "catalog-client-id" {
		t.Errorf("GET X-Request-Id = %q, want client correlation id", got)
	}
	for _, want := range []string{`"object":"list"`, `"id":"gpt-5.3-codex"`, `"owned_by":"Microsoft"`, `"owned_by":"Azure OpenAI"`} {
		if !strings.Contains(string(getBody), want) {
			t.Errorf("GET body missing %s: %s", want, getBody)
		}
	}
	for _, unwanted := range []string{"claude-opus-4.6", "warning_message", "different-upstream-id"} {
		if strings.Contains(string(getBody), unwanted) {
			t.Errorf("GET body leaked %q: %s", unwanted, getBody)
		}
	}

	headResp, headBody := do(http.MethodHead, "X-Api-Key", testAPIKey)
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headResp.StatusCode)
	}
	if len(headBody) != 0 {
		t.Errorf("HEAD wire body = %q, want empty", headBody)
	}
	if got := headResp.Header.Get("Content-Length"); got != strconv.Itoa(len(getBody)) {
		t.Errorf("HEAD Content-Length = %q, want GET representation length %d", got, len(getBody))
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("two catalog calls caused %d upstream fetches, want 2", got)
	}

	postResp, _ := do(http.MethodPost, "Authorization", "Bearer "+testAPIKey)
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", postResp.StatusCode)
	}
	unauthorizedResp, unauthorizedBody := do(http.MethodGet, "", "")
	if unauthorizedResp.StatusCode != http.StatusUnauthorized || openaiErrorType(t, unauthorizedBody) != "invalid_request_error" {
		t.Errorf("unauthorized response = %d %s", unauthorizedResp.StatusCode, unauthorizedBody)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("router/auth rejection reached upstream; hits = %d, want 2", got)
	}

	provider.SetReady(false)
	notReadyResp, notReadyBody := do(http.MethodGet, "X-Api-Key", testAPIKey)
	provider.SetReady(true)
	if notReadyResp.StatusCode != http.StatusServiceUnavailable || openaiErrorType(t, notReadyBody) != "api_error" {
		t.Errorf("not-ready response = %d %s", notReadyResp.StatusCode, notReadyBody)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("readiness rejection reached upstream; hits = %d, want 2", got)
	}
	provider.SetError(errors.New("mint failed"))
	credentialResp, credentialBody := do(http.MethodGet, "X-Api-Key", testAPIKey)
	provider.SetError(nil)
	if credentialResp.StatusCode != http.StatusServiceUnavailable || openaiErrorType(t, credentialBody) != "api_error" {
		t.Errorf("credential failure response = %d %s", credentialResp.StatusCode, credentialBody)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("credential failure reached upstream; hits = %d, want 2", got)
	}

	responseMode.Store(1)
	badStatusResp, badStatusBody := do(http.MethodGet, "X-Api-Key", testAPIKey)
	if badStatusResp.StatusCode != http.StatusBadGateway || openaiErrorType(t, badStatusBody) != "api_error" {
		t.Errorf("upstream non-2xx response = %d %s", badStatusResp.StatusCode, badStatusBody)
	}
	if strings.Contains(string(badStatusBody), "upstream secret") {
		t.Errorf("upstream error shape leaked: %s", badStatusBody)
	}

	responseMode.Store(2)
	malformedResp, malformedBody := do(http.MethodGet, "X-Api-Key", testAPIKey)
	if malformedResp.StatusCode != http.StatusBadGateway || openaiErrorType(t, malformedBody) != "api_error" {
		t.Errorf("malformed upstream response = %d %s", malformedResp.StatusCode, malformedBody)
	}
}
