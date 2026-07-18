package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	phase4GitHubOAuthToken = "phase4-github-oauth-token-sentinel"
	phase4RequestBody      = "phase4-request-body-sentinel"
	phase4ResponseBody     = `{"models":[{"id":"phase4-response-body-sentinel","future":{"opaque":true}}]}`
	phase4UpstreamReqID    = "phase4-upstream-request-id-must-not-cross"
)

type phase4ModelsRequest struct {
	method     string
	requestURI string
	header     http.Header
	body       string
}

type phase4ModelsStub struct {
	server         *httptest.Server
	calls          atomic.Int64
	mu             sync.Mutex
	requests       []phase4ModelsRequest
	concurrentGate *phase4ConcurrentGate
}

type phase4ConcurrentGate struct {
	arrived chan struct{}
	release chan struct{}
}

func newPhase4ModelsStub(t *testing.T) *phase4ModelsStub {
	t.Helper()
	stub := &phase4ModelsStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read stub Copilot request: %v", err)
		}
		call := stub.calls.Add(1)
		stub.mu.Lock()
		stub.requests = append(stub.requests, phase4ModelsRequest{
			method:     r.Method,
			requestURI: r.RequestURI,
			header:     r.Header.Clone(),
			body:       string(body),
		})
		gate := stub.concurrentGate
		stub.mu.Unlock()
		if strings.HasPrefix(r.Header.Get("X-Request-Id"), "phase4-concurrent-") && gate != nil {
			gate.arrived <- struct{}{}
			<-gate.release
		}

		w.Header().Set("ETag", `"phase4-models-v1"`)
		w.Header().Set("X-Phase4-Call", strconv.FormatInt(call, 10))
		w.Header().Set("X-Phase4-Upstream-Method", r.Method)
		w.Header().Add("X-Request-Id", phase4UpstreamReqID)
		w.Header().Add("X-Request-Id", phase4UpstreamReqID+"-second")
		if r.Header.Get("If-None-Match") == `"phase4-models-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(phase4ResponseBody)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, phase4ResponseBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase4ResponseBody)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *phase4ModelsStub) snapshot() []phase4ModelsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]phase4ModelsRequest(nil), s.requests...)
}

func phase4HeaderContains(header http.Header, value string) bool {
	for _, values := range header {
		for _, candidate := range values {
			if strings.Contains(candidate, value) {
				return true
			}
		}
	}
	return false
}

func TestPhase4ModelsOutcomeEndToEnd(t *testing.T) {
	models := newPhase4ModelsStub(t)

	var exchangeMu sync.Mutex
	var exchangeHeaders []http.Header
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeMu.Lock()
		exchangeHeaders = append(exchangeHeaders, r.Header.Clone())
		exchangeMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"`+phase4CopilotToken+`","expires_at":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`,"refresh_in":3600,"endpoints":{"api":"`+models.server.URL+`"}}`)
	}))
	t.Cleanup(exchange.Close)

	cfg := e2eConfig(phase4GitHubOAuthToken)
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	logger := newPhase4Logger(t, &logs)
	provider, err := buildServeProvider(cfg, logger, exchange.URL, exchange.Client())
	if err != nil {
		t.Fatalf("build Phase 4 provider: %v", err)
	}
	provider.StartupMint(context.Background())
	if !provider.Ready() {
		t.Fatal("provider is not ready after the stub startup mint")
	}
	base := startPhase4Server(t, cfg, provider, logger)

	for _, public := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}`},
		{path: "/readyz", wantStatus: http.StatusOK, wantBody: `{"status":"ready"}`},
	} {
		resp, body := doPhase4Request(t, nil, http.MethodGet, base+public.path, nil, nil)
		if resp.StatusCode != public.wantStatus || string(body) != public.wantBody {
			t.Errorf("GET %s = status %d body %q, want %d %q", public.path, resp.StatusCode, body, public.wantStatus, public.wantBody)
		}
	}
	if got := models.calls.Load(); got != 0 {
		t.Fatalf("public probes made %d stub Copilot calls, want zero", got)
	}

	type authCase struct {
		name      string
		method    string
		requestID string
		setAuth   func(*http.Request)
		body      string
		target    string
	}
	authCases := []authCase{
		{
			name:      "GET with Bearer",
			method:    http.MethodGet,
			requestID: "phase4-get-bearer-correlation",
			setAuth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+phase4APIKey) },
			body:      phase4RequestBody,
			target:    "/models?phase4-query-sentinel=%2fkept%2Fraw&dup=first&dup=second&flag",
		},
		{
			name:      "GET with x-api-key",
			method:    http.MethodGet,
			requestID: "phase4-get-x-api-key-correlation",
			setAuth:   func(r *http.Request) { r.Header.Set("X-Api-Key", phase4APIKey) },
			target:    "/models",
		},
		{
			name:      "HEAD with Bearer",
			method:    http.MethodHead,
			requestID: "phase4-head-bearer-correlation",
			setAuth:   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+phase4APIKey) },
			target:    "/models",
		},
		{
			name:      "HEAD with x-api-key",
			method:    http.MethodHead,
			requestID: "phase4-head-x-api-key-correlation",
			setAuth:   func(r *http.Request) { r.Header.Set("X-Api-Key", phase4APIKey) },
			target:    "/models",
		},
	}

	for _, tc := range authCases {
		t.Run(tc.name, func(t *testing.T) {
			var requestBody io.Reader
			if tc.body != "" {
				requestBody = strings.NewReader(tc.body)
			}
			resp, responseBody := doPhase4Request(t, nil, tc.method, base+tc.target, requestBody, func(req *http.Request) {
				tc.setAuth(req)
				req.Header.Set("X-Request-Id", tc.requestID)
			})
			wantStatus := http.StatusOK
			wantBody := phase4ResponseBody
			if tc.method == http.MethodHead {
				wantStatus = http.StatusPartialContent
				wantBody = ""
			}
			if resp.StatusCode != wantStatus || string(responseBody) != wantBody {
				t.Errorf("response = status %d body %q, want %d %q", resp.StatusCode, responseBody, wantStatus, wantBody)
			}
			if got := resp.Header.Get("X-Phase4-Upstream-Method"); got != tc.method {
				t.Errorf("upstream-method proof = %q, want %s", got, tc.method)
			}
			if tc.method == http.MethodHead && resp.Header.Get("Content-Length") != strconv.Itoa(len(phase4ResponseBody)) {
				t.Errorf("HEAD Content-Length = %q, want upstream representation length %d", resp.Header.Get("Content-Length"), len(phase4ResponseBody))
			}
			if got := resp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{tc.requestID}) {
				t.Errorf("downstream X-Request-Id = %q, want sole resolved ID", got)
			}
			for _, secret := range []string{phase4APIKey, phase4GitHubOAuthToken, phase4CopilotToken} {
				if phase4HeaderContains(resp.Header, secret) || strings.Contains(string(responseBody), secret) {
					t.Errorf("secret %q crossed the downstream boundary", secret)
				}
			}
		})
	}

	requests := models.snapshot()
	if len(requests) != len(authCases) {
		t.Fatalf("stub Copilot requests = %d, want %d", len(requests), len(authCases))
	}
	for i, tc := range authCases {
		got := requests[i]
		if got.method != tc.method || got.requestURI != tc.target {
			t.Errorf("stub request %d = %s %s, want %s %s", i, got.method, got.requestURI, tc.method, tc.target)
		}
		if got.header.Get("Authorization") != "Bearer "+phase4CopilotToken || got.header.Get("X-Api-Key") != "" {
			t.Errorf("stub credentials %d = Authorization %q X-Api-Key %q, want only Copilot token", i, got.header.Get("Authorization"), got.header.Get("X-Api-Key"))
		}
		if got.header.Get("X-Request-Id") != tc.requestID {
			t.Errorf("stub request ID %d = %q, want %q", i, got.header.Get("X-Request-Id"), tc.requestID)
		}
		if strings.Contains(got.body, phase4GitHubOAuthToken) || strings.Contains(got.body, phase4APIKey) || phase4HeaderContains(got.header, phase4GitHubOAuthToken) || phase4HeaderContains(got.header, phase4APIKey) {
			t.Errorf("inbound or GitHub OAuth secret crossed request %d to Copilot", i)
		}
	}
	if requests[0].body != phase4RequestBody || requests[0].requestURI != authCases[0].target {
		t.Errorf("client-owned query/body = %q %q, want exact %q %q", requests[0].requestURI, requests[0].body, authCases[0].target, phase4RequestBody)
	}

	conditionalID := "phase4-conditional-correlation"
	beforeConditional := models.calls.Load()
	conditionalResp, conditionalBody := doPhase4Request(t, nil, http.MethodGet, base+"/models", nil, func(req *http.Request) {
		req.Header.Set("X-Api-Key", phase4APIKey)
		req.Header.Set("X-Request-Id", conditionalID)
		req.Header.Set("If-None-Match", `"phase4-models-v1"`)
	})
	if conditionalResp.StatusCode != http.StatusNotModified || len(conditionalBody) != 0 {
		t.Errorf("conditional response = status %d body %q, want raw 304 with empty body", conditionalResp.StatusCode, conditionalBody)
	}
	if got := conditionalResp.Header.Get("ETag"); got != `"phase4-models-v1"` {
		t.Errorf("conditional ETag = %q, want authoritative upstream value", got)
	}
	if got := conditionalResp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{conditionalID}) {
		t.Errorf("conditional X-Request-Id = %q, want sole resolved ID", got)
	}
	if got := models.calls.Load() - beforeConditional; got != 1 {
		t.Errorf("conditional upstream calls = %d, want exactly one", got)
	}

	callGET := func(id string) (string, error) {
		resp, body, err := performPhase4Request(nil, http.MethodGet, base+"/models", nil, func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+phase4APIKey)
			req.Header.Set("X-Request-Id", id)
		})
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK || string(body) != phase4ResponseBody {
			return "", fmt.Errorf("models response = status %d body %q, want 200 %q", resp.StatusCode, body, phase4ResponseBody)
		}
		return resp.Header.Get("X-Phase4-Call"), nil
	}

	beforeIndependent := models.calls.Load()
	seenCalls := make(map[string]struct{})
	for i := range 2 {
		call, err := callGET("phase4-sequential-" + strconv.Itoa(i))
		if err != nil {
			t.Fatalf("sequential call %d: %v", i, err)
		}
		seenCalls[call] = struct{}{}
	}
	type callResult struct {
		call string
		err  error
	}
	const concurrentCalls = 8
	gate := &phase4ConcurrentGate{
		arrived: make(chan struct{}, concurrentCalls),
		release: make(chan struct{}),
	}
	models.mu.Lock()
	models.concurrentGate = gate
	models.mu.Unlock()
	var releaseOnce sync.Once
	releaseConcurrent := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer releaseConcurrent()
	results := make(chan callResult, concurrentCalls)
	for i := range concurrentCalls {
		go func() {
			call, err := callGET("phase4-concurrent-" + strconv.Itoa(i))
			results <- callResult{call: call, err: err}
		}()
	}
	for i := range concurrentCalls {
		select {
		case <-gate.arrived:
		case <-time.After(2 * time.Second):
			releaseConcurrent()
			t.Fatalf("concurrent request %d did not overlap at stub Copilot; possible result singleflight", i+1)
		}
	}
	releaseConcurrent()
	for range concurrentCalls {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent call: %v", result.err)
			continue
		}
		seenCalls[result.call] = struct{}{}
	}
	if got := models.calls.Load() - beforeIndependent; got != 2+concurrentCalls {
		t.Errorf("independent stub calls = %d, want %d", got, 2+concurrentCalls)
	}
	if len(seenCalls) != 2+concurrentCalls {
		t.Errorf("distinct upstream call proofs = %d, want %d", len(seenCalls), 2+concurrentCalls)
	}

	exchangeMu.Lock()
	gotExchangeHeaders := append([]http.Header(nil), exchangeHeaders...)
	exchangeMu.Unlock()
	if len(gotExchangeHeaders) != 1 {
		t.Fatalf("token exchange calls = %d, want one startup mint", len(gotExchangeHeaders))
	}
	if got := gotExchangeHeaders[0].Get("Authorization"); got != "token "+phase4GitHubOAuthToken {
		t.Errorf("exchange Authorization = %q, want GitHub OAuth token", got)
	}
	for _, forbidden := range []string{phase4APIKey, phase4CopilotToken, phase4RequestBody, "phase4-query-sentinel"} {
		if phase4HeaderContains(gotExchangeHeaders[0], forbidden) {
			t.Errorf("material %q crossed the exchange request boundary", forbidden)
		}
	}

	logOutput := logs.String()
	for _, tc := range authCases {
		accessLines := phase4LogLinesContaining(logOutput, "msg=access", "request_id="+tc.requestID)
		if len(accessLines) != 1 {
			t.Errorf("access lines for %s = %d, want exactly one", tc.requestID, len(accessLines))
		}
		if len(accessLines) == 0 {
			continue
		}
		line := accessLines[0]
		wantStatus := "status=200"
		wantBytes := "bytes=" + strconv.Itoa(len(phase4ResponseBody))
		if tc.method == http.MethodHead {
			wantStatus = "status=206"
			wantBytes = "bytes=0"
		}
		for _, want := range []string{"method=" + tc.method, `route="` + tc.method + ` /models"`, wantStatus, wantBytes, "duration="} {
			if !strings.Contains(line, want) {
				t.Errorf("access line for %s missing %q: %s", tc.requestID, want, line)
			}
		}
		correlationLines := phase4LogLinesContaining(logOutput,
			`msg="upstream response correlation"`,
			"request_id="+tc.requestID,
			"upstream_request_id="+phase4UpstreamReqID,
		)
		if len(correlationLines) != 1 {
			t.Errorf("correlation lines for %s = %d, want one with upstream ID:\n%s", tc.requestID, len(correlationLines), strings.Join(correlationLines, "\n"))
		}
	}
	for _, private := range []string{
		phase4APIKey,
		phase4GitHubOAuthToken,
		phase4CopilotToken,
		"phase4-query-sentinel",
		phase4RequestBody,
		phase4ResponseBody,
		"phase4-response-body-sentinel",
		phase4UpstreamReqID + "-second",
	} {
		if strings.Contains(logOutput, private) {
			t.Errorf("private material %q appeared in logs:\n%s", private, logOutput)
		}
	}
}
