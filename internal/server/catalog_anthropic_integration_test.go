package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
)

func TestAnthropicModelCatalogOverRealListener(t *testing.T) {
	captured, err := os.ReadFile("../catalog/testdata/copilot-models-2026-07-18.json")
	if err != nil {
		t.Fatalf("read captured catalog: %v", err)
	}
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/models" || r.URL.RawQuery != "" {
			t.Errorf("upstream request = %s %s, want GET /models without query", r.Method, r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(captured)
	}))
	defer upstream.Close()

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}, true)
	forwarder := forward.New(provider, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, nil, NewStreamOutcomeCounter()))

	do := func(method, target, keyHeader, key string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+target, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if keyHeader != "" {
			req.Header.Set(keyHeader, key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s %s: %v", method, target, err)
		}
		return resp, body
	}

	getResp, getBody := do(http.MethodGet, "/anthropic/v1/models?limit=1&after_id=ignored", "Authorization", "Bearer "+testAPIKey)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", getResp.StatusCode, getBody)
	}
	if getResp.Header.Get("Content-Type") != "application/json" || getResp.Header.Get("Content-Length") != strconv.Itoa(len(getBody)) {
		t.Errorf("GET representation headers = %v, want JSON and length %d", getResp.Header, len(getBody))
	}
	var catalog struct {
		Data []struct {
			ID           string `json:"id"`
			Type         string `json:"type"`
			Capabilities struct {
				Thinking *struct {
					Supported bool `json:"supported"`
				} `json:"thinking"`
			} `json:"capabilities"`
		} `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}
	if err := json.Unmarshal(getBody, &catalog); err != nil {
		t.Fatalf("GET did not deserialize as Anthropic catalog: %v\n%s", err, getBody)
	}
	if len(catalog.Data) != 7 || catalog.Data[0].ID != "claude-opus-4.6" || catalog.Data[6].ID != "claude-haiku-4.5" || catalog.Data[0].Type != "model" {
		t.Errorf("Anthropic membership/order = %+v, want seven capture-ordered Claude models", catalog.Data)
	}
	if catalog.HasMore || catalog.FirstID == nil || *catalog.FirstID != catalog.Data[0].ID || catalog.LastID == nil || *catalog.LastID != catalog.Data[6].ID {
		t.Errorf("Anthropic envelope boundaries = has_more:%v first:%v last:%v", catalog.HasMore, catalog.FirstID, catalog.LastID)
	}
	if catalog.Data[0].Capabilities.Thinking == nil || !catalog.Data[0].Capabilities.Thinking.Supported {
		t.Errorf("evidence-backed capabilities are not visible through the served Catalog: %+v", catalog.Data[0].Capabilities)
	}

	headResp, headBody := do(http.MethodHead, "/anthropic/v1/models", "X-Api-Key", testAPIKey)
	if headResp.StatusCode != http.StatusOK || len(headBody) != 0 || headResp.Header.Get("Content-Length") != strconv.Itoa(len(getBody)) {
		t.Errorf("HEAD = status %d length %q body %q, want GET-equivalent headers and no body", headResp.StatusCode, headResp.Header.Get("Content-Length"), headBody)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("GET+HEAD upstream hits = %d, want 2", got)
	}

	for _, request := range []struct {
		method string
		target string
		status int
	}{
		{http.MethodPost, "/anthropic/v1/models", http.StatusMethodNotAllowed},
		{http.MethodGet, "/anthropic/models", http.StatusNotFound},
	} {
		resp, _ := do(request.method, request.target, "Authorization", "Bearer "+testAPIKey)
		if resp.StatusCode != request.status {
			t.Errorf("%s %s status = %d, want %d", request.method, request.target, resp.StatusCode, request.status)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("unregistered routes reached upstream; hits = %d, want 2", got)
	}
}
