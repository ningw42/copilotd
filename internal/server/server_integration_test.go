package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
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
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
	return newHandler(testAPIKey, prov, fwd, discardLogger(t), NewStreamOutcomeCounter()), prov
}

type controllerRecorder struct {
	*httptest.ResponseRecorder
}

func newControllerRecorder() *controllerRecorder {
	return &controllerRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *controllerRecorder) SetWriteDeadline(time.Time) error { return nil }

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
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		close(release)
		t.Fatalf("read flushed first chunk: %v", err)
	}
	if got := string(first); got != "first\n" {
		close(release)
		t.Fatalf("first chunk = %q, want first\\n", got)
	}
	close(release)

	rest, err := io.ReadAll(resp.Body)
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

func TestReadyzReflectsReadiness(t *testing.T) {
	h, prov := stack(t, "", true)

	t.Run("ready", func(t *testing.T) {
		rec := newControllerRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != `{"status":"ready"}` {
			t.Errorf("body = %q, want ready", rec.Body.String())
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
		if rec.Body.String() != `{"status":"not ready"}` {
			t.Errorf("body = %q, want not ready", rec.Body.String())
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
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, fwd, NewStreamOutcomeCounter()))

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
	fwd := forward.New(prov, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, fwd, NewStreamOutcomeCounter()))

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
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 8) // 8-byte cap
		h := newHandler(testAPIKey, prov, fwd, discardLogger(t), NewStreamOutcomeCounter())
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
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
		return startServer(t, New(testConfig(), discardLogger(t), prov, fwd, NewStreamOutcomeCounter()))
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
		rest, err := io.ReadAll(resp.Body)
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
		body, err := io.ReadAll(resp.Body)
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
		fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 2*time.Second, keepalive, 1<<20)
		outcomes := NewStreamOutcomeCounter()
		return startServer(t, New(testConfig(), discardLogger(t), prov, fwd, outcomes)), outcomes
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

		base, outcomes := serveAgainst(t, upstream.URL, time.Second)
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
		rest, err := io.ReadAll(resp.Body)
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
		body, err := io.ReadAll(resp.Body)
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
		if _, err := io.ReadFull(resp.Body, comment); err != nil {
			close(releaseTerminal)
			t.Fatalf("read keepalive: %v", err)
		}
		if got := string(comment); got != ":\n\n" {
			close(releaseTerminal)
			t.Fatalf("keepalive = %q, want SSE comment", got)
		}
		close(releaseTerminal)
		rest, err := io.ReadAll(resp.Body)
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
	fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, fwd, NewStreamOutcomeCounter()))
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
	if _, err := io.ReadFull(resp.Body, got); err != nil {
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
