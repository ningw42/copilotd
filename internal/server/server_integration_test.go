package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/sse"
)

// stack builds the assembled handler wired to a Static provider (pointing at
// upstreamURL, with the given readiness) and a forwarder with a 1 MiB cap.
func stack(t *testing.T, upstreamURL string, ready bool) (http.Handler, *identity.Static) {
	t.Helper()
	prov := identity.NewStatic(identity.Credential{
		BaseURL: upstreamURL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, ready)
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	return newHandler(testAPIKey, prov, newTestImpersonationObserver(), fwd, discardLogger(t), NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(prov)), prov
}

type controllerRecorder struct {
	*httptest.ResponseRecorder
}

type serverRoundTripFunc func(*http.Request) (*http.Response, error)

func (f serverRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newControllerRecorder() *controllerRecorder {
	return &controllerRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *controllerRecorder) SetWriteDeadline(time.Time) error { return nil }

func readFullWithin(body io.ReadCloser, dst []byte, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(body, dst)
		done <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = body.Close()
		return context.DeadlineExceeded
	}
}

func readAllWithin(body io.ReadCloser, timeout time.Duration) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := io.ReadAll(body)
		done <- result{body: body, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.body, result.err
	case <-timer.C:
		_ = body.Close()
		return nil, context.DeadlineExceeded
	}
}

func TestResponseControllerThroughMiddlewareChain(t *testing.T) {
	release := make(chan struct{})
	handlerErrors := make(chan error, 3)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := sse.NewWriter(w, time.Second, time.Now)
		if _, err := writer.Write([]byte("first\n")); err != nil {
			handlerErrors <- err
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			handlerErrors <- err
			return
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			handlerErrors <- context.DeadlineExceeded
			return
		}
		if _, err := writer.Write([]byte("second\n")); err != nil {
			handlerErrors <- err
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			handlerErrors <- err
		}
	})

	logger := discardLogger(t)
	h := requestID(accessLog(logger, NewStreamOutcomeCounter(), recoverMW(logger, inner)))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		close(release)
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	first := make([]byte, len("first\n"))
	if err := readFullWithin(resp.Body, first, time.Second); err != nil {
		close(release)
		t.Fatalf("read flushed first chunk: %v", err)
	}
	if got := string(first); got != "first\n" {
		close(release)
		t.Fatalf("first chunk = %q, want first\\n", got)
	}
	close(release)

	rest, err := readAllWithin(resp.Body, time.Second)
	if err != nil {
		t.Fatalf("read second chunk: %v", err)
	}
	if got := string(rest); got != "second\n" {
		t.Errorf("second chunk = %q, want second\\n", got)
	}
	select {
	case err := <-handlerErrors:
		t.Errorf("ResponseController through middleware: %v", err)
	default:
	}
}

// anthropicErrorType decodes the Anthropic error envelope and returns its inner
// error.type, failing the test if the body is not Anthropic-shaped.
func anthropicErrorType(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Type != "error" {
		t.Fatalf("body is not Anthropic-shaped: %s", body)
	}
	return e.Error.Type
}

// openaiErrorType decodes the OpenAI error envelope
// ({"error":{"message","type","code","param":null}}) and returns its inner
// error.type, failing the test if the body is not OpenAI-shaped. It also asserts
// the always-null "param" key is present, so the shape stays distinct from the
// Anthropic envelope.
func openaiErrorType(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error *struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    *string `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
		t.Fatalf("body is not OpenAI-shaped: %s", body)
	}
	if !strings.Contains(string(body), `"param":null`) {
		t.Errorf("OpenAI error body missing the nullable param key: %s", body)
	}
	return e.Error.Type
}

func TestReadyzReflectsReadinessAndImpersonation(t *testing.T) {
	prov := identity.NewStatic(identity.Credential{}, true)
	lastSuccess := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	observer := staticImpersonationObserver{observed: impersonation.Observed{
		EffectiveHeaders: http.Header{
			"Authorization":          {"secret-that-must-not-render"},
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Plugin-Version":  {"copilot-chat/0.48.1"},
			"Editor-Version":         {"vscode/1.129.1"},
			"User-Agent":             {"GitHubCopilotChat/0.48.1"},
			"X-Github-Api-Version":   {"2025-04-01"},
		},
		Discovery: impersonation.ObservedDiscovery{
			VSCode:      impersonation.ObservedFact{Source: "discovered", LastSuccess: &lastSuccess},
			CopilotChat: impersonation.ObservedFact{Source: "fallback"},
		},
	}}
	h := handleReady(prov, observer)
	wantImpersonation := `"impersonation":{"effective_headers":{"Editor-Version":"vscode/1.129.1","Editor-Plugin-Version":"copilot-chat/0.48.1","User-Agent":"GitHubCopilotChat/0.48.1","Copilot-Integration-Id":"vscode-chat","X-GitHub-Api-Version":"2025-04-01"},"discovery":{"vscode":{"source":"discovered","last_success":"2026-07-20T12:00:00Z"},"copilot_chat":{"source":"fallback","last_success":null}}}`

	t.Run("ready", func(t *testing.T) {
		rec := newControllerRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if want := `{"status":"ready",` + wantImpersonation + `}`; rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}
		if strings.Contains(rec.Body.String(), "secret-that-must-not-render") {
			t.Errorf("body leaked a non-impersonation header: %s", rec.Body.String())
		}
	})

	t.Run("not ready", func(t *testing.T) {
		prov.SetReady(false)
		defer prov.SetReady(true)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if want := `{"status":"not ready",` + wantImpersonation + `}`; rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}

		head := newControllerRecorder()
		h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/readyz", nil))
		if head.Code != http.StatusServiceUnavailable {
			t.Errorf("HEAD status = %d, want 503", head.Code)
		}
		if head.Body.Len() != 0 {
			t.Errorf("HEAD body = %q, want empty", head.Body.String())
		}
	})

	t.Run("ready HEAD has no body", func(t *testing.T) {
		rec := newControllerRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body = %q, want empty", rec.Body.String())
		}
	})
}

// TestAuthOnProviderRoute covers both accepted schemes for a valid key and the
// provider-shaped 401 for missing/wrong keys. The valid cases forward to a stub
// upstream returning 200.
func TestAuthOnProviderRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	h, _ := stack(t, upstream.URL, true)

	do := func(setKey func(*http.Request)) *controllerRecorder {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		if setKey != nil {
			setKey(req)
		}
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("valid Bearer forwards", func(t *testing.T) {
		rec := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testAPIKey) })
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("valid x-api-key forwards", func(t *testing.T) {
		rec := do(func(r *http.Request) { r.Header.Set("X-Api-Key", testAPIKey) })
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("missing key rejected 401 (Anthropic-shaped)", func(t *testing.T) {
		rec := do(nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if typ := anthropicErrorType(t, rec.Body.Bytes()); typ != "authentication_error" {
			t.Errorf("error.type = %q, want authentication_error", typ)
		}
	})
	t.Run("wrong key rejected 401", func(t *testing.T) {
		rec := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong-key") })
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// TestAuthBeforeReadiness proves the ordering: when not ready, an unauthenticated
// caller still gets 401 (auth runs first), while an authenticated caller gets the
// 503 readiness signal.
func TestAuthBeforeReadiness(t *testing.T) {
	h, _ := stack(t, "", false) // not ready

	t.Run("unauthenticated gets 401 even when not ready", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (auth before readiness)", rec.Code)
		}
	})

	t.Run("authenticated gets 503 when not ready", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if typ := anthropicErrorType(t, rec.Body.Bytes()); typ != "api_error" {
			t.Errorf("error.type = %q, want api_error", typ)
		}
	})
}

func TestModelsTracerMapsExplicitGETAndHEADAtAssembledBoundary(t *testing.T) {
	type upstreamRequest struct {
		method string
		route  string
		auth   string
		apiKey string
		host   string
	}
	var got []upstreamRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, upstreamRequest{
			method: r.Method,
			route:  r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			apiKey: r.Header.Get("X-Api-Key"),
			host:   r.Host,
		})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Marker", r.Method)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "opaque body without an SSE terminal")
	}))
	defer upstream.Close()
	h, _ := stack(t, upstream.URL, true)

	t.Run("GET maps to one upstream GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/models", nil)
		req.Host = "inbound.example"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Errorf("status = %d, want upstream 202", rec.Code)
		}
		if got := rec.Header().Get("X-Upstream-Marker"); got != http.MethodGet {
			t.Errorf("X-Upstream-Marker = %q, want GET", got)
		}
		if got := rec.Body.String(); got != "opaque body without an SSE terminal" {
			t.Errorf("body = %q, want opaque upstream body without synthesis", got)
		}
	})

	t.Run("HEAD maps to one upstream HEAD and writes no body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/models", nil)
		req.Host = "inbound.example"
		req.Header.Set("X-Api-Key", testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Errorf("status = %d, want upstream 202", rec.Code)
		}
		if got := rec.Header().Get("X-Upstream-Marker"); got != http.MethodHead {
			t.Errorf("X-Upstream-Marker = %q, want HEAD", got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body = %q, want empty", rec.Body.String())
		}
	})

	if len(got) != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2", len(got))
	}
	for i, wantMethod := range []string{http.MethodGet, http.MethodHead} {
		if got[i].method != wantMethod || got[i].route != "/models" {
			t.Errorf("upstream call %d = %s Route %s, want %s Route /models", i, got[i].method, got[i].route, wantMethod)
		}
		if got[i].auth != "Bearer copilot-token" || got[i].apiKey != "" {
			t.Errorf("upstream credentials %d = auth %q x-api-key %q, want only Copilot token", i, got[i].auth, got[i].apiKey)
		}
		if got[i].host == "inbound.example" {
			t.Errorf("upstream Host %d = inbound Host %q", i, got[i].host)
		}
	}
}

func TestModelsRequestOwnershipAndIdentityBoundariesAtAssembledServer(t *testing.T) {
	const (
		apiKeySentinel           = "inbound-api-key-sentinel-43"
		githubOAuthTokenSentinel = "github-oauth-token-sentinel-43"
		copilotTokenSentinel     = "copilot-token-sentinel-43"
		requestBodySentinel      = "unusual-models-get-body-sentinel-43"
		requestID                = "models-request-id-43"
		requestTarget            = "/models?dup=query-first-sentinel-43&escaped=query%2fescaped%2Fsentinel-43&flag&empty=&dup=query-second-sentinel-43"
	)

	type modelsRequest struct {
		requestURI    string
		header        http.Header
		body          string
		contentLength int64
		host          string
	}
	modelsRequests := make(chan modelsRequest, 1)
	modelsUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read models request body: %v", err)
		}
		modelsRequests <- modelsRequest{
			requestURI:    r.RequestURI,
			header:        r.Header.Clone(),
			body:          string(body),
			contentLength: r.ContentLength,
			host:          r.Host,
		}
		w.Header().Set("X-Models-Result", "raw")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer modelsUpstream.Close()

	exchangeRequests := make(chan http.Header, 1)
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeRequests <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      copilotTokenSentinel,
			"expires_at": time.Now().Add(time.Hour).Unix(),
			"refresh_in": 3600,
			"endpoints":  map[string]string{"api": modelsUpstream.URL},
		})
	}))
	defer exchange.Close()

	logger, logs := bufferLogger(t, "info")
	provider := identity.NewManager(identity.ManagerConfig{
		OAuthToken:    githubOAuthTokenSentinel,
		GitHubBaseURL: exchange.URL,
		HTTPClient:    exchange.Client(),
		Impersonation: identity.StaticImpersonation(http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		}),
		Logger: logger,
	})
	provider.StartupMint(context.Background())
	exchangeHeader := <-exchangeRequests
	if got := exchangeHeader.Get("Authorization"); got != "token "+githubOAuthTokenSentinel {
		t.Fatalf("exchange Authorization = %q, want GitHub OAuth token", got)
	}

	fwd := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	h := newHandler(apiKeySentinel, provider, newTestImpersonationObserver(), fwd, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(provider))
	req := httptest.NewRequest(http.MethodGet, requestTarget, nil)
	req.Body = io.NopCloser(strings.NewReader(requestBodySentinel))
	req.ContentLength = int64(len(requestBodySentinel))
	req.Host = "client-owned-host.invalid"
	req.Header = http.Header{
		"Accept":                 {"application/vnd.github+json"},
		"Accept-Encoding":        {"br;q=1.0, gzip;q=0.8"},
		"Authorization":          {"Bearer " + apiKeySentinel},
		"Connection":             {"X-Connection-Only"},
		"Content-Length":         {"999999"},
		"Copilot-Integration-Id": {"client-integration"},
		"Editor-Version":         {"client-editor"},
		"Host":                   {"client-header-host.invalid"},
		"If-None-Match":          {`"models-v7"`},
		"X-Api-Key":              {apiKeySentinel},
		"X-Connection-Only":      {"must-not-cross"},
		"X-End-To-End":           {"client-owned"},
		"X-Request-Id":           {requestID},
	}
	rec := newControllerRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want upstream 204", rec.Code)
	}
	if got := rec.Header().Get("X-Models-Result"); got != "raw" {
		t.Errorf("X-Models-Result = %q, want raw", got)
	}
	got := <-modelsRequests
	if got.requestURI != requestTarget {
		t.Errorf("upstream request target = %q, want %q", got.requestURI, requestTarget)
	}
	if got.body != requestBodySentinel {
		t.Errorf("upstream body = %q, want sentinel body", got.body)
	}
	if got.contentLength != int64(len(requestBodySentinel)) {
		t.Errorf("upstream ContentLength = %d, want %d", got.contentLength, len(requestBodySentinel))
	}
	if got.host == "client-owned-host.invalid" || got.host == "client-header-host.invalid" {
		t.Errorf("upstream Host = %q, want models origin", got.host)
	}
	for _, name := range []string{"Connection", "Host", "X-Api-Key", "X-Connection-Only"} {
		if values, ok := got.header[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("%s survived upstream with values %q", name, values)
		}
	}
	if value := got.header.Get("Content-Length"); value != strconv.Itoa(len(requestBodySentinel)) {
		t.Errorf("wire Content-Length = %q, want structured length %d", value, len(requestBodySentinel))
	}
	for name, want := range map[string]string{
		"Accept":                 "application/vnd.github+json",
		"Accept-Encoding":        "br;q=1.0, gzip;q=0.8",
		"Authorization":          "Bearer " + copilotTokenSentinel,
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Version":         "vscode/1.104.1",
		"If-None-Match":          `"models-v7"`,
		"X-End-To-End":           "client-owned",
		"X-Request-Id":           requestID,
	} {
		if value := got.header.Get(name); value != want {
			t.Errorf("upstream %s = %q, want %q", name, value, want)
		}
	}

	headerContains := func(header http.Header, sentinel string) bool {
		for _, values := range header {
			for _, value := range values {
				if strings.Contains(value, sentinel) {
					return true
				}
			}
		}
		return false
	}
	for _, secret := range []string{apiKeySentinel, copilotTokenSentinel} {
		if headerContains(exchangeHeader, secret) {
			t.Errorf("secret %q crossed the GitHub token exchange boundary", secret)
		}
	}
	for _, secret := range []string{apiKeySentinel, githubOAuthTokenSentinel} {
		if headerContains(got.header, secret) || strings.Contains(got.body, secret) {
			t.Errorf("secret %q crossed the models boundary", secret)
		}
	}
	for _, secret := range []string{apiKeySentinel, githubOAuthTokenSentinel, copilotTokenSentinel} {
		if headerContains(rec.Header(), secret) || strings.Contains(rec.Body.String(), secret) {
			t.Errorf("secret %q crossed the downstream boundary", secret)
		}
	}
	for _, private := range []string{
		apiKeySentinel,
		githubOAuthTokenSentinel,
		copilotTokenSentinel,
		"query-first-sentinel-43",
		"query%2fescaped%2Fsentinel-43",
		"query/escaped/sentinel-43",
		"query-second-sentinel-43",
		requestBodySentinel,
	} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("private request material %q appeared in logs:\n%s", private, logs.String())
		}
	}
}

func TestModelsHEADPreservesRequestAndResponseContractAtRealListener(t *testing.T) {
	const (
		requestBody   = "unusual-models-head-body-sentinel-45"
		requestID     = "models-head-request-id-45"
		requestTarget = "/models?dup=head-first&escaped=head%2fescaped%2Fvalue&flag&empty=&dup=head-second"
	)

	type upstreamRequest struct {
		method        string
		requestURI    string
		header        http.Header
		body          string
		contentLength int64
		host          string
	}
	requests := make(chan upstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream HEAD body: %v", err)
		}
		requests <- upstreamRequest{
			method:        r.Method,
			requestURI:    r.RequestURI,
			header:        r.Header.Clone(),
			body:          string(body),
			contentLength: r.ContentLength,
			host:          r.Host,
		}
		w.Header().Add("Cache-Control", "private")
		w.Header().Add("Cache-Control", "max-age=0")
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("Content-Length", "4242")
		w.Header().Set("Content-Type", "application/vnd.github+json")
		w.Header().Set("X-End-To-End-Upstream", "preserved")
		w.Header().Set("X-Request-Id", "upstream-head-id-must-not-cross")
		w.Header().Set("X-Upstream-Hop", "drop")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "upstream HEAD representation must not reach either wire")
	}))
	defer upstream.Close()

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	fwd := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	logger, logs := bufferLogger(t, "info")
	server := httptest.NewServer(newHandler(testAPIKey, provider, newTestImpersonationObserver(), fwd, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(provider)))
	defer server.Close()

	req, err := http.NewRequest(http.MethodHead, server.URL+requestTarget, strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build HEAD: %v", err)
	}
	req.Host = "client-owned-host.invalid"
	req.Header = http.Header{
		"Accept-Encoding":        {"br, gzip"},
		"Authorization":          {"Bearer " + testAPIKey},
		"Connection":             {"X-Connection-Only"},
		"Copilot-Integration-Id": {"client-integration"},
		"Editor-Version":         {"client-editor"},
		"If-None-Match":          {`"models-head-v1"`},
		"X-Api-Key":              {testAPIKey},
		"X-Connection-Only":      {"drop"},
		"X-End-To-End":           {"client-owned"},
		"X-Request-Id":           {requestID},
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD /models: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read downstream HEAD response: %v", err)
	}

	if resp.StatusCode != http.StatusPartialContent || len(body) != 0 {
		t.Errorf("downstream HEAD = status %d body %q, want upstream 206 with no wire body", resp.StatusCode, body)
	}
	for name, want := range (http.Header{
		"Cache-Control":         {"private", "max-age=0"},
		"Content-Length":        {"4242"},
		"Content-Type":          {"application/vnd.github+json"},
		"X-End-To-End-Upstream": {"preserved"},
	}) {
		if got := resp.Header.Values(name); !reflect.DeepEqual(got, want) {
			t.Errorf("downstream %s = %q, want %q", name, got, want)
		}
	}
	if got := resp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{requestID}) {
		t.Errorf("downstream X-Request-Id = %q, want sole resolved ID", got)
	}
	for _, name := range []string{"Connection", "X-Upstream-Hop"} {
		if got := resp.Header.Values(name); len(got) != 0 {
			t.Errorf("hop-by-hop downstream %s survived with values %q", name, got)
		}
	}

	got := <-requests
	if got.method != http.MethodHead || got.requestURI != requestTarget {
		t.Errorf("upstream request = %s %s, want HEAD %s", got.method, got.requestURI, requestTarget)
	}
	if got.body != requestBody || got.contentLength != int64(len(requestBody)) {
		t.Errorf("upstream body/length = %q/%d, want %q/%d", got.body, got.contentLength, requestBody, len(requestBody))
	}
	if got.host == "client-owned-host.invalid" {
		t.Errorf("inbound Host survived upstream: %q", got.host)
	}
	for name, want := range map[string]string{
		"Accept-Encoding":        "br, gzip",
		"Authorization":          "Bearer copilot-token",
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Version":         "vscode/1.104.1",
		"If-None-Match":          `"models-head-v1"`,
		"X-End-To-End":           "client-owned",
		"X-Request-Id":           requestID,
	} {
		if value := got.header.Get(name); value != want {
			t.Errorf("upstream %s = %q, want %q", name, value, want)
		}
	}
	for _, name := range []string{"Connection", "X-Api-Key", "X-Connection-Only"} {
		if values := got.header.Values(name); len(values) != 0 {
			t.Errorf("owned upstream %s survived with values %q", name, values)
		}
	}
	if log := logs.String(); !strings.Contains(log, `route="HEAD /models"`) || !strings.Contains(log, "status=206") || !strings.Contains(log, "bytes=0") || !strings.Contains(log, "request_id="+requestID) {
		t.Errorf("HEAD access log lacks explicit route/status/zero-byte/correlation metadata:\n%s", log)
	}
}

func TestModelsAuthoritativeResponseAtAssembledBoundaryOmitsResponseDataFromLogs(t *testing.T) {
	const (
		requestID          = "resolved-models-response-id-44"
		upstreamRequestID  = "upstream-models-response-id-44"
		responseData       = "event: future.model.event\ndata: {\"unknown_model_field\":\"response-data-sentinel-44\"}\n\n"
		responseHeaderData = "response-header-sentinel-44"
	)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Add("Cache-Control", "private")
		w.Header().Add("Cache-Control", "max-age=0")
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Models-Metadata", responseHeaderData)
		w.Header().Set("X-Request-Id", upstreamRequestID)
		w.Header().Set("X-Upstream-Hop", "must-not-cross")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, responseData)
	}))
	defer upstream.Close()

	provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true)
	fwd := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
	logger, logs := bufferLogger(t, "info")
	h := newHandler(testAPIKey, provider, newTestImpersonationObserver(), fwd, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(provider))
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", requestID)
	rec := newControllerRecorder()

	h.ServeHTTP(rec, req)

	if calls != 1 {
		t.Errorf("upstream calls = %d, want exactly 1", calls)
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want authoritative upstream 418", rec.Code)
	}
	if got := rec.Body.String(); got != responseData {
		t.Errorf("body = %q, want opaque upstream bytes %q", got, responseData)
	}
	if got := rec.Header().Values("Cache-Control"); len(got) != 2 || got[0] != "private" || got[1] != "max-age=0" {
		t.Errorf("Cache-Control values = %q, want [private max-age=0]", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want opaque text/event-stream", got)
	}
	if got := rec.Header().Get("X-Models-Metadata"); got != responseHeaderData {
		t.Errorf("X-Models-Metadata = %q, want %q", got, responseHeaderData)
	}
	if got := rec.Header().Values("X-Request-Id"); len(got) != 1 || got[0] != requestID {
		t.Errorf("X-Request-Id values = %q, want sole resolved id", got)
	}
	for _, name := range []string{"Connection", "X-Upstream-Hop"} {
		if got := rec.Header().Values(name); len(got) != 0 {
			t.Errorf("hop-by-hop %s survived with values %q", name, got)
		}
	}
	for _, private := range []string{responseData, "response-data-sentinel-44", responseHeaderData, upstreamRequestID} {
		if strings.Contains(logs.String(), private) {
			t.Errorf("response data %q appeared in logs:\n%s", private, logs.String())
		}
	}
	if got := logs.String(); !strings.Contains(got, `route="GET /models"`) || !strings.Contains(got, "status=418") || !strings.Contains(got, "request_id="+requestID) {
		t.Errorf("access log lacks route/status/correlation metadata:\n%s", got)
	}
}

func TestModelsTracerGateFailuresAndRouterBehavior(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h, provider := stack(t, upstream.URL, false)

	assertError := func(t *testing.T, req *http.Request, wantStatus int, wantBody string) {
		t.Helper()
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Errorf("status = %d, want %d", rec.Code, wantStatus)
		}
		if got := rec.Body.String(); got != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
		if upstreamCalls != 0 {
			t.Errorf("upstream calls = %d, want zero", upstreamCalls)
		}
	}

	const unauthorized = `{"type":"error","error":{"type":"authentication_error","message":"missing or invalid API key"}}`
	assertError(t, httptest.NewRequest(http.MethodGet, "/models", nil), http.StatusUnauthorized, unauthorized)
	wrong := httptest.NewRequest(http.MethodGet, "/models", nil)
	wrong.Header.Set("Authorization", "Bearer wrong")
	assertError(t, wrong, http.StatusUnauthorized, unauthorized)

	const notReady = `{"type":"error","error":{"type":"api_error","message":"service not ready"}}`
	authenticated := httptest.NewRequest(http.MethodGet, "/models", nil)
	authenticated.Header.Set("Authorization", "Bearer "+testAPIKey)
	assertError(t, authenticated, http.StatusServiceUnavailable, notReady)

	provider.SetReady(true)
	provider.SetError(errors.New("mint failed"))
	const noCredential = `{"type":"error","error":{"type":"api_error","message":"no upstream credential available"}}`
	currentFailure := httptest.NewRequest(http.MethodGet, "/models", nil)
	currentFailure.Header.Set("Authorization", "Bearer "+testAPIKey)
	assertError(t, currentFailure, http.StatusServiceUnavailable, noCredential)
	provider.SetError(nil)

	for _, tc := range []struct {
		method     string
		target     string
		wantStatus int
	}{
		{method: http.MethodPost, target: "/models", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodGet, target: "/models/", wantStatus: http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.target, rec.Code, tc.wantStatus)
		}
		if upstreamCalls != 0 {
			t.Errorf("%s %s made %d upstream calls, want zero", tc.method, tc.target, upstreamCalls)
		}
	}
}

func TestModelsHEADLocalFailuresHaveNoWireBody(t *testing.T) {
	tests := []struct {
		name          string
		credential    identity.Credential
		ready         bool
		providerError error
		authorize     bool
		roundTrip     serverRoundTripFunc
		wantStatus    int
		wantBody      string
		wantCalls     int
	}{
		{
			name:       "auth before readiness",
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"type":"error","error":{"type":"authentication_error","message":"missing or invalid API key"}}`,
		},
		{
			name:       "not ready",
			authorize:  true,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"service not ready"}}`,
		},
		{
			name:          "current credential failure",
			credential:    identity.Credential{BaseURL: "https://upstream.invalid", Token: "copilot-token"},
			ready:         true,
			providerError: errors.New("mint failed"),
			authorize:     true,
			wantStatus:    http.StatusServiceUnavailable,
			wantBody:      `{"type":"error","error":{"type":"api_error","message":"no upstream credential available"}}`,
		},
		{
			name:       "request construction",
			credential: identity.Credential{BaseURL: "http://[::1", Token: "copilot-token"},
			ready:      true,
			authorize:  true,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"could not build the upstream request"}}`,
		},
		{
			name:       "reachability",
			credential: identity.Credential{BaseURL: "https://upstream.invalid", Token: "copilot-token"},
			ready:      true,
			authorize:  true,
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unreachable")
			},
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"could not reach the upstream"}}`,
			wantCalls:  1,
		},
		{
			name:       "deadline",
			credential: identity.Credential{BaseURL: "https://upstream.invalid", Token: "copilot-token"},
			ready:      true,
			authorize:  true,
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			},
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"the upstream request timed out"}}`,
			wantCalls:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := identity.NewStatic(tc.credential, tc.ready)
			provider.SetError(tc.providerError)
			calls := 0
			roundTrip := tc.roundTrip
			if roundTrip == nil {
				roundTrip = func(*http.Request) (*http.Response, error) {
					t.Error("unexpected upstream call")
					return nil, errors.New("unexpected upstream call")
				}
			}
			client := &http.Client{Transport: serverRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return roundTrip(r)
			})}
			fwd := forward.New(provider, client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
			logger, logs := bufferLogger(t, "info")
			server := httptest.NewServer(newHandler(testAPIKey, provider, newTestImpersonationObserver(), fwd, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(provider)))
			defer server.Close()

			req, err := http.NewRequest(http.MethodHead, server.URL+"/models", nil)
			if err != nil {
				t.Fatalf("build HEAD: %v", err)
			}
			if tc.authorize {
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("HEAD: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read HEAD response: %v", err)
			}
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if calls != tc.wantCalls {
				t.Errorf("upstream calls = %d, want %d", calls, tc.wantCalls)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(tc.wantBody)) {
				t.Errorf("Content-Length = %q, want HEAD representation length %d", got, len(tc.wantBody))
			}
			if len(body) != 0 {
				t.Errorf("wire body = %q, want empty for HEAD", body)
			}
			if log := logs.String(); !strings.Contains(log, `route="HEAD /models"`) || !strings.Contains(log, "status="+strconv.Itoa(tc.wantStatus)) || !strings.Contains(log, "bytes=0") {
				t.Errorf("HEAD failure access log lacks explicit route/status/zero bytes:\n%s", log)
			}
		})
	}
}

func TestModelsExplicitPatternsReachAccessLog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
	}, true)
	fwd := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	logger, logs := bufferLogger(t, "info")
	h := newHandler(testAPIKey, provider, newTestImpersonationObserver(), fwd, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(provider))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequest(method, "/models", nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		h.ServeHTTP(newControllerRecorder(), req)
	}

	for _, route := range []string{`route="GET /models"`, `route="HEAD /models"`} {
		if count := strings.Count(logs.String(), route); count != 1 {
			t.Errorf("access log count for %s = %d, want 1:\n%s", route, count, logs.String())
		}
	}
}

// TestAnthropicStreamForwardedAtBoundary replaces the Phase 1 stream reject
// proof: stream:true now crosses the full assembled stack unchanged, and the
// upstream response is copied back on the existing buffered path.
func TestAnthropicStreamForwardedAtBoundary(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Upstream-Marker", "present")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"msg_stream"}`)
	}))
	defer upstream.Close()
	h, _ := stack(t, upstream.URL, true)

	const reqBody = `{"model":"claude-3-5-sonnet","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := newControllerRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want upstream 202", rec.Code)
	}
	if string(gotBody) != reqBody {
		t.Errorf("upstream body = %q, want original bytes %q", gotBody, reqBody)
	}
	if got := rec.Body.String(); got != `{"id":"msg_stream"}` {
		t.Errorf("response body = %q, want upstream body", got)
	}
	if got := rec.Header().Get("X-Upstream-Marker"); got != "present" {
		t.Errorf("X-Upstream-Marker = %q, want present", got)
	}
}

func TestInferenceRoutesReturnFirstUpstreamRedirect(t *testing.T) {
	tests := []struct {
		name          string
		inboundPath   string
		upstreamRoute string
	}{
		{
			name:          "Anthropic",
			inboundPath:   "/anthropic/v1/messages",
			upstreamRoute: "/v1/messages",
		},
		{
			name:          "OpenAI",
			inboundPath:   "/openai/v1/responses",
			upstreamRoute: "/responses",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.URL.Path != tc.upstreamRoute {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "redirect was followed")
					return
				}
				w.Header().Set("Location", "/redirect-target")
				w.Header().Set("X-Upstream-Marker", "first-response")
				w.WriteHeader(http.StatusTemporaryRedirect)
				_, _ = io.WriteString(w, "first response body")
			}))
			defer upstream.Close()

			h, _ := stack(t, upstream.URL, true)
			req := httptest.NewRequest(http.MethodPost, tc.inboundPath, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			rec := newControllerRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want first upstream 307", rec.Code)
			}
			if hits != 1 {
				t.Errorf("upstream hits = %d, want exactly 1", hits)
			}
			if got := rec.Header().Get("Location"); got != "/redirect-target" {
				t.Errorf("Location = %q, want /redirect-target", got)
			}
			if got := rec.Header().Get("X-Upstream-Marker"); got != "first-response" {
				t.Errorf("X-Upstream-Marker = %q, want first-response", got)
			}
			if got := rec.Body.String(); got != "first response body" {
				t.Errorf("body = %q, want first response body", got)
			}
		})
	}
}

func TestInferenceResponsesKeepOnlyResolvedRequestID(t *testing.T) {
	tests := []struct {
		name        string
		inboundPath string
	}{
		{name: "Anthropic", inboundPath: "/anthropic/v1/messages"},
		{name: "OpenAI", inboundPath: "/openai/v1/responses"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Request-Id", "upstream-first")
				w.Header().Add("X-Request-Id", "upstream-second")
				w.Header().Set("X-Upstream-Marker", "preserved")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "opaque upstream body")
			}))
			defer upstream.Close()

			h, _ := stack(t, upstream.URL, true)
			req := httptest.NewRequest(http.MethodPost, tc.inboundPath, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("X-Request-Id", "resolved-request-id")
			rec := newControllerRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Errorf("status = %d, want 202", rec.Code)
			}
			if got := rec.Header().Values("X-Request-Id"); len(got) != 1 || got[0] != "resolved-request-id" {
				t.Errorf("X-Request-Id values = %q, want exactly [resolved-request-id]", got)
			}
			if got := rec.Header().Get("X-Upstream-Marker"); got != "preserved" {
				t.Errorf("X-Upstream-Marker = %q, want preserved", got)
			}
			if got := rec.Body.String(); got != "opaque upstream body" {
				t.Errorf("body = %q, want opaque upstream body", got)
			}
		})
	}
}

// TestEndToEndForwardViaRun is the Phase 1 outcome as an automated test: the
// server is driven over a real ephemeral listener with a real HTTP client, the
// forwarder points at a stub "Copilot", and a valid key round-trips verbatim
// while a wrong key is rejected — asserting the impersonated outbound credential
// and that the inbound API key never leaks upstream.
func TestEndToEndForwardViaRun(t *testing.T) {
	var gotAuth, gotIntegration, gotEditor, gotReqID, gotAPIKeyHeader string
	var gotBody []byte
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIntegration = r.Header.Get("Copilot-Integration-Id")
		gotEditor = r.Header.Get("Editor-Version")
		gotReqID = r.Header.Get("X-Request-Id")
		gotAPIKeyHeader = r.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_abc","role":"assistant"}`)
	}))
	defer upstream.Close()

	prov := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-secret-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, newTestImpersonationObserver(), fwd, newTestWSProxy(prov), NewStreamOutcomeCounter()))

	const reqBody = `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`

	t.Run("valid key round-trips verbatim", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if string(respBody) != `{"id":"msg_abc","role":"assistant"}` {
			t.Errorf("response body = %q, want the upstream body verbatim", respBody)
		}
		if resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining") != "42" {
			t.Errorf("upstream response header not copied back")
		}

		// Upstream saw the impersonated outbound credential, not the inbound key.
		if gotAuth != "Bearer copilot-secret-token" {
			t.Errorf("upstream Authorization = %q, want Bearer copilot-secret-token", gotAuth)
		}
		if strings.Contains(gotAuth, testAPIKey) || gotAPIKeyHeader != "" {
			t.Errorf("inbound API key leaked upstream (auth=%q x-api-key=%q)", gotAuth, gotAPIKeyHeader)
		}
		if gotIntegration != "vscode-chat" || gotEditor != "vscode/1.104.1" {
			t.Errorf("impersonation headers missing upstream: integration=%q editor=%q", gotIntegration, gotEditor)
		}
		if gotReqID == "" {
			t.Errorf("upstream did not receive an X-Request-Id")
		}
		if string(gotBody) != reqBody {
			t.Errorf("upstream body = %q, want the original bytes", gotBody)
		}
		if gotPath != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", gotPath)
		}
	})

	t.Run("wrong key rejected without reaching upstream", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Authorization", "Bearer nope")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestOpenAIResponsesForwardVerbatim is the OpenAI-surface analogue of the
// Anthropic end-to-end test: driven over a real listener with a real client, a
// valid key round-trips verbatim to the stubbed Copilot. It nails the /v1
// asymmetry — inbound POST /openai/v1/responses forwards to upstream /responses,
// NOT /v1/responses — while carrying the impersonated outbound credential and
// never leaking the inbound API key.
func TestOpenAIResponsesForwardVerbatim(t *testing.T) {
	var gotAuth, gotIntegration, gotEditor, gotReqID, gotAPIKeyHeader string
	var gotBody []byte
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIntegration = r.Header.Get("Copilot-Integration-Id")
		gotEditor = r.Header.Get("Editor-Version")
		gotReqID = r.Header.Get("X-Request-Id")
		gotAPIKeyHeader = r.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(r.Body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "present")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"resp_abc","object":"response"}`)
	}))
	defer upstream.Close()

	prov := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-secret-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, newTestImpersonationObserver(), fwd, newTestWSProxy(prov), NewStreamOutcomeCounter()))

	const reqBody = `{"model":"gpt-4o","input":"hi"}`
	req, _ := http.NewRequest(http.MethodPost, base+"/openai/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(respBody) != `{"id":"resp_abc","object":"response"}` {
		t.Errorf("response body = %q, want the upstream body verbatim", respBody)
	}
	if resp.Header.Get("X-Upstream-Marker") != "present" {
		t.Errorf("upstream response header not copied back")
	}

	// The /v1 asymmetry: the OpenAI surface drops /v1 upstream.
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses (not /v1/responses)", gotPath)
	}
	// Upstream saw the impersonated outbound credential, not the inbound key.
	if gotAuth != "Bearer copilot-secret-token" {
		t.Errorf("upstream Authorization = %q, want Bearer copilot-secret-token", gotAuth)
	}
	if strings.Contains(gotAuth, testAPIKey) || gotAPIKeyHeader != "" {
		t.Errorf("inbound API key leaked upstream (auth=%q x-api-key=%q)", gotAuth, gotAPIKeyHeader)
	}
	if gotIntegration != "vscode-chat" || gotEditor != "vscode/1.104.1" {
		t.Errorf("impersonation headers missing upstream: integration=%q editor=%q", gotIntegration, gotEditor)
	}
	if gotReqID == "" {
		t.Errorf("upstream did not receive an X-Request-Id")
	}
	if string(gotBody) != reqBody {
		t.Errorf("upstream body = %q, want the original bytes", gotBody)
	}
}

// TestOpenAIStreamAndBackgroundAtBoundary proves the OpenAI peek now ignores
// stream:true while retaining the surface-shaped background:true rejection.
func TestOpenAIStreamAndBackgroundAtBoundary(t *testing.T) {
	var hits int
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"id":"resp_stream"}`)
	}))
	defer upstream.Close()
	h, _ := stack(t, upstream.URL, true)

	request := func(body string) *controllerRecorder {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("stream true forwarded verbatim", func(t *testing.T) {
		hits = 0
		const reqBody = `{"model":"gpt-4.1","stream":true}`
		rec := request(reqBody)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want upstream 202", rec.Code)
		}
		if hits != 1 {
			t.Errorf("upstream hits = %d, want 1", hits)
		}
		if string(gotBody) != reqBody {
			t.Errorf("upstream body = %q, want original bytes %q", gotBody, reqBody)
		}
		if got := rec.Body.String(); got != `{"id":"resp_stream"}` {
			t.Errorf("response body = %q, want upstream body", got)
		}
	})

	t.Run("background true -> OpenAI-shaped 400", func(t *testing.T) {
		hits = 0
		rec := request(`{"background":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
		}
		if hits != 0 {
			t.Errorf("upstream hits = %d, want 0", hits)
		}
	})
}

// TestOpenAIAuthAndReadiness confirms the shared auth/readiness gates render
// OpenAI-shaped errors on this surface: missing/wrong key -> 401, and an
// authenticated caller against a not-ready identity -> 503.
func TestOpenAIAuthAndReadiness(t *testing.T) {
	t.Run("missing key -> OpenAI-shaped 401", func(t *testing.T) {
		h, _ := stack(t, "http://127.0.0.1:1", true)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
		}
	})

	t.Run("wrong key -> 401", func(t *testing.T) {
		h, _ := stack(t, "http://127.0.0.1:1", true)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer wrong-key")
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("authenticated but not ready -> OpenAI-shaped 503", func(t *testing.T) {
		h, _ := stack(t, "", false)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "api_error" {
			t.Errorf("error.type = %q, want api_error", typ)
		}
	})
}

// TestOpenAIBodyCapAndUpstreamPassthrough covers the body cap (over the limit ->
// OpenAI-shaped 413) and verbatim upstream passthrough (an upstream 429 is copied
// back unchanged, never re-wrapped by apierror) on the OpenAI surface.
func TestOpenAIBodyCapAndUpstreamPassthrough(t *testing.T) {
	t.Run("over cap -> OpenAI-shaped 413", func(t *testing.T) {
		prov := identity.NewStatic(identity.Credential{BaseURL: "http://127.0.0.1:1", Token: "t"}, true)
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 8, 1<<20, nil) // 8-byte request cap
		h := newHandler(testAPIKey, prov, newTestImpersonationObserver(), fwd, discardLogger(t), NewStreamOutcomeCounter(), config.CodexConfig{}, newTestWSProxy(prov))
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"way too long"}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
		}
	})

	t.Run("upstream 429 copied back verbatim", func(t *testing.T) {
		const upstreamErr = `{"error":{"message":"rate limited","type":"rate_limit_exceeded","code":"rate_limit_exceeded","param":null}}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, upstreamErr)
		}))
		defer upstream.Close()
		h, _ := stack(t, upstream.URL, true)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := newControllerRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 (verbatim)", rec.Code)
		}
		if got := rec.Body.String(); got != upstreamErr {
			t.Errorf("body = %q, want the upstream error verbatim", got)
		}
	})
}

func TestAnthropicStreamingEndToEnd(t *testing.T) {
	const first = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const synthesized = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream ended before a terminal event\"}}\n\n"

	serveAgainst := func(t *testing.T, upstreamURL string) string {
		t.Helper()
		prov := identity.NewStatic(identity.Credential{
			BaseURL: upstreamURL,
			Token:   "copilot-token",
			Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
		}, true)
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
		return startServer(t, New(testConfig(), discardLogger(t), prov, newTestImpersonationObserver(), fwd, newTestWSProxy(prov), NewStreamOutcomeCounter()))
	}

	request := func(t *testing.T, base string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("stream request: %v", err)
		}
		return resp
	}

	t.Run("frames arrive incrementally and clean terminal is not doubled", func(t *testing.T) {
		firstFlushed := make(chan struct{})
		releaseTerminal := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, first)
			_ = http.NewResponseController(w).Flush()
			close(firstFlushed)
			<-releaseTerminal
			_, _ = io.WriteString(w, terminal)
			_ = http.NewResponseController(w).Flush()
		}))
		defer upstream.Close()

		resp := request(t, serveAgainst(t, upstream.URL))
		defer func() { _ = resp.Body.Close() }()
		select {
		case <-firstFlushed:
		case <-time.After(time.Second):
			close(releaseTerminal)
			t.Fatal("stub did not flush the first upstream frame")
		}

		firstRead := make(chan error, 1)
		gotFirst := make([]byte, len(first))
		go func() {
			_, err := io.ReadFull(resp.Body, gotFirst)
			firstRead <- err
		}()
		select {
		case err := <-firstRead:
			if err != nil {
				close(releaseTerminal)
				t.Fatalf("read first frame: %v", err)
			}
		case <-time.After(time.Second):
			close(releaseTerminal)
			t.Fatal("client did not receive the first frame before the terminal was released")
		}
		if got := string(gotFirst); got != first {
			close(releaseTerminal)
			t.Fatalf("first frame = %q, want exact upstream bytes %q", got, first)
		}

		close(releaseTerminal)
		rest, err := readAllWithin(resp.Body, time.Second)
		if err != nil {
			t.Fatalf("read terminal: %v", err)
		}
		if got := string(rest); got != terminal {
			t.Errorf("remaining bytes = %q, want one upstream terminal only %q", got, terminal)
		}
	})

	t.Run("truncated stream gets native terminal and request id", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, first)
			_ = http.NewResponseController(w).Flush()
		}))
		defer upstream.Close()

		resp := request(t, serveAgainst(t, upstream.URL))
		defer func() { _ = resp.Body.Close() }()
		body, err := readAllWithin(resp.Body, time.Second)
		if err != nil {
			t.Fatalf("read truncated response: %v", err)
		}
		if got := string(body); got != first+synthesized {
			t.Errorf("body = %q, want upstream frame plus native terminal %q", got, first+synthesized)
		}
		if requestID := resp.Header.Get("X-Request-Id"); requestID == "" {
			t.Error("synthesized streaming response is missing X-Request-Id")
		}
	})
}

func TestOpenAIStreamingEndToEnd(t *testing.T) {
	const first = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n"
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1}\n\n"
	const synthesized = "event: error\ndata: {\"type\":\"error\",\"code\":null,\"message\":\"copilotd: upstream stream ended before a terminal event\",\"param\":null}\n\n"

	serveAgainst := func(t *testing.T, upstreamURL string, keepalive time.Duration) (string, *StreamOutcomeCounter) {
		t.Helper()
		prov := identity.NewStatic(identity.Credential{
			BaseURL: upstreamURL,
			Token:   "copilot-token",
			Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
		}, true)
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 2*time.Second, keepalive, 1<<20, 1<<20, nil)
		outcomes := NewStreamOutcomeCounter()
		return startServer(t, New(testConfig(), discardLogger(t), prov, newTestImpersonationObserver(), fwd, newTestWSProxy(prov), outcomes)), outcomes
	}

	request := func(t *testing.T, base string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("stream request: %v", err)
		}
		return resp
	}

	waitForOutcome := func(t *testing.T, outcomes *StreamOutcomeCounter, outcome sse.Outcome) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for outcomes.Count("openai", outcome) != 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := outcomes.Count("openai", outcome); got != 1 {
			t.Errorf("OpenAI %q outcome count = %d, want 1", outcome, got)
		}
	}

	t.Run("frames arrive incrementally and clean terminal is not doubled", func(t *testing.T) {
		releaseTerminal := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, first)
			_ = http.NewResponseController(w).Flush()
			<-releaseTerminal
			_, _ = io.WriteString(w, terminal)
			_ = http.NewResponseController(w).Flush()
		}))
		defer upstream.Close()

		base, outcomes := serveAgainst(t, upstream.URL, 30*time.Second)
		resp := request(t, base)
		defer func() { _ = resp.Body.Close() }()
		gotFirst := make([]byte, len(first))
		firstRead := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(resp.Body, gotFirst)
			firstRead <- err
		}()
		select {
		case err := <-firstRead:
			if err != nil {
				close(releaseTerminal)
				t.Fatalf("read first frame: %v", err)
			}
		case <-time.After(time.Second):
			close(releaseTerminal)
			t.Fatal("client did not receive the first OpenAI frame incrementally")
		}
		if got := string(gotFirst); got != first {
			close(releaseTerminal)
			t.Fatalf("first frame = %q, want %q", got, first)
		}

		close(releaseTerminal)
		rest, err := readAllWithin(resp.Body, time.Second)
		if err != nil {
			t.Fatalf("read terminal: %v", err)
		}
		if got := string(rest); got != terminal {
			t.Errorf("remaining bytes = %q, want one verbatim terminal %q", got, terminal)
		}
		waitForOutcome(t, outcomes, sse.OutcomeClean)
	})

	t.Run("truncated stream gets OpenAI native terminal", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, first)
			_ = http.NewResponseController(w).Flush()
		}))
		defer upstream.Close()

		base, outcomes := serveAgainst(t, upstream.URL, time.Second)
		resp := request(t, base)
		defer func() { _ = resp.Body.Close() }()
		body, err := readAllWithin(resp.Body, time.Second)
		if err != nil {
			t.Fatalf("read truncated response: %v", err)
		}
		if got := string(body); got != first+synthesized {
			t.Errorf("body = %q, want frame plus OpenAI terminal %q", got, first+synthesized)
		}
		waitForOutcome(t, outcomes, sse.OutcomeSynthesized)
	})

	t.Run("idle gap emits keepalive comment before terminal", func(t *testing.T) {
		releaseTerminal := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-releaseTerminal
			_, _ = io.WriteString(w, terminal)
			_ = http.NewResponseController(w).Flush()
		}))
		defer upstream.Close()

		base, outcomes := serveAgainst(t, upstream.URL, 40*time.Millisecond)
		resp := request(t, base)
		defer func() { _ = resp.Body.Close() }()
		comment := make([]byte, len(":\n\n"))
		if err := readFullWithin(resp.Body, comment, time.Second); err != nil {
			close(releaseTerminal)
			t.Fatalf("read keepalive: %v", err)
		}
		if got := string(comment); got != ":\n\n" {
			close(releaseTerminal)
			t.Fatalf("keepalive = %q, want SSE comment", got)
		}
		close(releaseTerminal)
		rest, err := readAllWithin(resp.Body, time.Second)
		if err != nil {
			t.Fatalf("read terminal after keepalive: %v", err)
		}
		if got := string(rest); got != terminal {
			t.Errorf("remaining bytes = %q, want one terminal after keepalive %q", got, terminal)
		}
		waitForOutcome(t, outcomes, sse.OutcomeClean)
	})
}

func TestStreamingClientHangupCancelsCopilotEndToEnd(t *testing.T) {
	const first = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	prov := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}, true)
	fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	outcomes := NewStreamOutcomeCounter()
	base := startServer(t, New(testConfig(), discardLogger(t), prov, newTestImpersonationObserver(), fwd, newTestWSProxy(prov), outcomes))
	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	got := make([]byte, len(first))
	if err := readFullWithin(resp.Body, got, time.Second); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read first flushed frame: %v", err)
	}
	if string(got) != first {
		_ = resp.Body.Close()
		t.Fatalf("first frame = %q, want %q", got, first)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("hang up streaming client: %v", err)
	}

	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("stub Copilot did not promptly observe cancellation after the streaming client hung up")
	}
	deadline := time.Now().Add(time.Second)
	for outcomes.Count("anthropic", sse.OutcomeClientCancel) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := outcomes.Count("anthropic", sse.OutcomeClientCancel); got != 1 {
		t.Errorf("Anthropic client_cancel outcome count = %d, want 1", got)
	}
}

// startServer runs srv on an ephemeral loopback listener and returns its base
// URL, tearing it down on test cleanup.
func startServer(t *testing.T, srv *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down within the grace period")
		}
	})

	base := "http://" + ln.Addr().String()
	// Wait until the listener is actually serving.
	for range 50 {
		resp, err := http.Get(base + "/healthz") //nolint:noctx // test setup poll
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return base
}
