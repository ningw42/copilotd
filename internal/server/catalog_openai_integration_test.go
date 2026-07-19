package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
)

func TestCodexCatalogOverRealListener(t *testing.T) {
	const (
		reviewer                 = "gpt-5.4"
		activeModel              = "gpt-5.6-sol"
		upstreamCanary           = "capstone-upstream-non-2xx-canary"
		malformedCanary          = "capstone-upstream-malformed-canary"
		upstreamRequestID        = "capstone-upstream-id-must-not-cross"
		responseModeSuccess      = int32(0)
		responseModeNon2xx       = int32(1)
		responseModeMalformed2xx = int32(2)
	)
	captured, err := os.ReadFile("../catalog/testdata/copilot-models-2026-07-18.json")
	if err != nil {
		t.Fatalf("read captured catalog: %v", err)
	}

	var hits atomic.Int32
	var responseMode atomic.Int32
	var requestIDsMu sync.Mutex
	var upstreamRequestIDs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		requestIDsMu.Lock()
		upstreamRequestIDs = append(upstreamRequestIDs, r.Header.Get("X-Request-Id"))
		requestIDsMu.Unlock()
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/models" {
			t.Errorf("upstream request = %s %s, want GET /models", r.Method, r.URL.RequestURI())
		}
		w.Header().Add("X-Request-Id", upstreamRequestID)
		w.Header().Add("X-Request-Id", upstreamRequestID+"-secondary")
		switch responseMode.Load() {
		case responseModeNon2xx:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"copilot":"`+upstreamCanary+`"}`)
		case responseModeMalformed2xx:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `<html>`+malformedCanary+`</html>`)
		case responseModeSuccess:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(captured)
		default:
			t.Errorf("unexpected upstream response mode %d", responseMode.Load())
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	newStack := func(codex config.CodexConfig, ready bool) (string, *identity.Static) {
		t.Helper()
		cfg := testConfig()
		cfg.Codex = codex
		provider := identity.NewStatic(identity.Credential{
			BaseURL: upstream.URL,
			Token:   "copilot-token",
			Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
		}, ready)
		forwarder := forward.New(provider, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
		return startServer(t, New(cfg, discardLogger(t), provider, forwarder, nil, NewStreamOutcomeCounter())), provider
	}

	requestCatalog := func(base, method, target, keyHeader, key, requestID string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+target, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, target, err)
		}
		if keyHeader != "" {
			req.Header.Set(keyHeader, key)
		}
		if requestID != "" {
			req.Header.Set("X-Request-Id", requestID)
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

	assertOpenAIList := func(body []byte) {
		t.Helper()
		var envelope struct {
			Object string            `json:"object"`
			Data   []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.Object != "list" || len(envelope.Data) == 0 {
			t.Errorf("body = %s, want non-empty OpenAI list: %v", body, err)
		}
	}

	codexBase, provider := newStack(config.CodexConfig{Enabled: true, AutoReviewModel: reviewer}, true)
	startHits := hits.Load()
	getRequestID := "codex-capstone-get"
	getResponse, getBody := requestCatalog(codexBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "Authorization", "Bearer "+testAPIKey, getRequestID)
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("Codex GET = %d %s, want 200", getResponse.StatusCode, getBody)
	}
	if getResponse.Header.Get("Content-Type") != "application/json" || getResponse.Header.Get("Content-Length") != strconv.Itoa(len(getBody)) {
		t.Errorf("Codex GET representation headers = %v, want JSON and length %d", getResponse.Header, len(getBody))
	}
	if got := getResponse.Header.Values("X-Request-Id"); len(got) != 1 || got[0] != getRequestID {
		t.Errorf("Codex GET request IDs = %q, want sole resolved ID", got)
	}
	var codexCatalog struct {
		Models []struct {
			Slug                    string `json:"slug"`
			AutoReviewModelOverride string `json:"auto_review_model_override"`
		} `json:"models"`
	}
	if err := json.Unmarshal(getBody, &codexCatalog); err != nil || len(codexCatalog.Models) == 0 {
		t.Fatalf("Codex GET did not return a non-empty client-shaped catalog: %v\n%s", err, getBody)
	}
	var activeHasReviewer, reviewerIsAdvertised bool
	for _, model := range codexCatalog.Models {
		if model.Slug == activeModel && model.AutoReviewModelOverride == reviewer {
			activeHasReviewer = true
		}
		if model.Slug == reviewer {
			reviewerIsAdvertised = true
		}
	}
	if !activeHasReviewer || !reviewerIsAdvertised {
		t.Errorf("end-to-end reviewer sanity = active override %v, reviewer advertised %v", activeHasReviewer, reviewerIsAdvertised)
	}

	headRequestID := "codex-capstone-head"
	headResponse, headBody := requestCatalog(codexBase, http.MethodHead, "/openai/v1/models?client_version=0.144.5", "X-Api-Key", testAPIKey, headRequestID)
	if headResponse.StatusCode != http.StatusOK || len(headBody) != 0 || headResponse.Header.Get("Content-Length") != strconv.Itoa(len(getBody)) {
		t.Errorf("Codex HEAD = status %d length %q body %q, want GET-equivalent headers and no body", headResponse.StatusCode, headResponse.Header.Get("Content-Length"), headBody)
	}
	if got := headResponse.Header.Values("X-Request-Id"); len(got) != 1 || got[0] != headRequestID {
		t.Errorf("Codex HEAD request IDs = %q, want sole resolved ID", got)
	}
	if got := hits.Load() - startHits; got != 2 {
		t.Errorf("two Codex catalog calls caused %d upstream fetches, want 2", got)
	}
	requestIDsMu.Lock()
	gotUpstreamRequestIDs := append([]string(nil), upstreamRequestIDs...)
	requestIDsMu.Unlock()
	if len(gotUpstreamRequestIDs) < 2 || gotUpstreamRequestIDs[0] != getRequestID || gotUpstreamRequestIDs[1] != headRequestID {
		t.Errorf("upstream request IDs = %q, want first two resolved IDs %q/%q", gotUpstreamRequestIDs, getRequestID, headRequestID)
	}

	// The other three negotiation cases retain the Phase 6a OpenAI shape.
	noQueryResponse, noQueryBody := requestCatalog(codexBase, http.MethodGet, "/openai/v1/models", "Authorization", "Bearer "+testAPIKey, "codex-capstone-no-query")
	if noQueryResponse.StatusCode != http.StatusOK {
		t.Fatalf("no-client-version response = %d %s", noQueryResponse.StatusCode, noQueryBody)
	}
	assertOpenAIList(noQueryBody)

	noInjectionBase, _ := newStack(config.CodexConfig{Enabled: true}, true)
	noInjectionResponse, noInjectionBody := requestCatalog(noInjectionBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "Authorization", "Bearer "+testAPIKey, "codex-capstone-no-injection")
	if noInjectionResponse.StatusCode != http.StatusOK {
		t.Fatalf("no-injection response = %d %s", noInjectionResponse.StatusCode, noInjectionBody)
	}
	assertOpenAIList(noInjectionBody)

	disabledBase, _ := newStack(config.CodexConfig{AutoReviewModel: reviewer}, true)
	disabledResponse, disabledBody := requestCatalog(disabledBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "Authorization", "Bearer "+testAPIKey, "codex-capstone-disabled")
	if disabledResponse.StatusCode != http.StatusOK {
		t.Fatalf("disabled response = %d %s", disabledResponse.StatusCode, disabledBody)
	}
	assertOpenAIList(disabledBody)

	provider.SetReady(false)
	beforeGuardHits := hits.Load()
	unauthorizedResponse, unauthorizedBody := requestCatalog(codexBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "", "", "codex-capstone-unauthorized")
	if unauthorizedResponse.StatusCode != http.StatusUnauthorized || openaiErrorType(t, unauthorizedBody) != "invalid_request_error" {
		t.Errorf("invalid auth while not ready = %d %s, want OpenAI 401", unauthorizedResponse.StatusCode, unauthorizedBody)
	}
	notReadyResponse, notReadyBody := requestCatalog(codexBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "X-Api-Key", testAPIKey, "codex-capstone-not-ready")
	if notReadyResponse.StatusCode != http.StatusServiceUnavailable || openaiErrorType(t, notReadyBody) != "api_error" {
		t.Errorf("not-ready response = %d %s, want OpenAI 503", notReadyResponse.StatusCode, notReadyBody)
	}
	if got := hits.Load(); got != beforeGuardHits {
		t.Errorf("auth/readiness rejection reached upstream: hits = %d, want %d", got, beforeGuardHits)
	}
	provider.SetReady(true)

	for _, failure := range []struct {
		name   string
		mode   int32
		canary string
	}{
		{name: "non-2xx", mode: responseModeNon2xx, canary: upstreamCanary},
		{name: "malformed 2xx", mode: responseModeMalformed2xx, canary: malformedCanary},
	} {
		t.Run(failure.name, func(t *testing.T) {
			responseMode.Store(failure.mode)
			response, body := requestCatalog(codexBase, http.MethodGet, "/openai/v1/models?client_version=0.144.5", "Authorization", "Bearer "+testAPIKey, "codex-capstone-"+strings.ReplaceAll(failure.name, " ", "-"))
			if response.StatusCode != http.StatusBadGateway || openaiErrorType(t, body) != "api_error" {
				t.Errorf("response = %d %s, want OpenAI 502", response.StatusCode, body)
			}
			if strings.Contains(string(body), failure.canary) {
				t.Errorf("upstream canary leaked in response: %s", body)
			}
		})
	}
}

func TestCodexCatalogConfigWiringWarningAndAccessLogConfidentiality(t *testing.T) {
	const (
		querySecret     = "client-version-secret-59"
		modelBodySecret = "model-body-secret-59"
		vendorSecret    = "vendor-body-secret-59"
		copilotToken    = "copilot-token-secret-59"
		reviewer        = "configured-missing-reviewer"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/models" {
			t.Errorf("upstream request = %s %s, want GET /models", r.Method, r.URL.RequestURI())
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"`+modelBodySecret+`","vendor":"`+vendorSecret+`","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	}))
	defer upstream.Close()

	logger, logs := bufferLogger(t, "info")
	cfg := testConfig()
	cfg.Codex = config.CodexConfig{Enabled: true, AutoReviewModel: reviewer}
	provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: copilotToken}, true)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	base := startServer(t, New(cfg, logger, provider, forwarder, nil, NewStreamOutcomeCounter()))

	requestCatalog := func(target string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+target, nil)
		if err != nil {
			t.Fatalf("build catalog request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET catalog: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read catalog: %v", err)
		}
		return resp, body
	}

	codexResponse, codexBody := requestCatalog("/openai/v1/models?client_version=" + querySecret)
	openAIResponse, openAIBody := requestCatalog("/openai/v1/models")
	if codexResponse.StatusCode != http.StatusOK || string(codexBody) != `{"models":[]}` {
		t.Fatalf("Codex response = %d %s, want 200 client-shaped empty intersection", codexResponse.StatusCode, codexBody)
	}
	if openAIResponse.StatusCode != http.StatusOK || !strings.HasPrefix(string(openAIBody), `{"object":"list","data":[`) {
		t.Fatalf("OpenAI fallback response = %d %s, want unchanged provider shape", openAIResponse.StatusCode, openAIBody)
	}
	for name := range codexResponse.Header {
		if strings.Contains(strings.ToLower(name), "catalog") {
			t.Errorf("internal catalog metadata leaked in response header %q", name)
		}
	}

	output := logs.String()
	if got := strings.Count(output, `msg="Codex catalog reviewer was skipped"`); got != 1 {
		t.Errorf("reviewer-skip warning count = %d, want exactly 1:\n%s", got, output)
	}
	for _, want := range []string{
		"level=WARN", "reviewer=" + reviewer,
		"catalog_shape=codex", "catalog_shape=openai",
		`route="GET /openai/v1/models"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("catalog logs missing %q:\n%s", want, output)
		}
	}
	if got := strings.Count(output, "msg=access"); got != 2 {
		t.Errorf("access-log count = %d, want one per request:\n%s", got, output)
	}
	for _, forbidden := range []string{querySecret, modelBodySecret, vendorSecret, copilotToken, testAPIKey, string(codexBody), string(openAIBody)} {
		if strings.Contains(output, forbidden) {
			t.Errorf("catalog logs leaked %q:\n%s", forbidden, output)
		}
	}
}

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
			base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, nil, NewStreamOutcomeCounter()))

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
	base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, nil, NewStreamOutcomeCounter()))

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
