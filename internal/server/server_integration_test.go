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
	fwd := forward.New(prov, forward.NewClient(), 5*time.Second, 1<<20)
	return newHandler(testAPIKey, prov, fwd, discardLogger(t)), prov
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
		rec := httptest.NewRecorder()
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
		rec := httptest.NewRecorder()
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

	do := func(setKey func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		if setKey != nil {
			setKey(req)
		}
		rec := httptest.NewRecorder()
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
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 (auth before readiness)", rec.Code)
		}
	})

	t.Run("authenticated gets 503 when not ready", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if typ := anthropicErrorType(t, rec.Body.Bytes()); typ != "api_error" {
			t.Errorf("error.type = %q, want api_error", typ)
		}
	})
}

// TestSynchronousOnlyAtBoundary exercises the stream reject and the body cap
// through the full assembled stack (auth + readiness + forward).
func TestSynchronousOnlyAtBoundary(t *testing.T) {
	h, _ := stack(t, "http://127.0.0.1:1", true) // upstream unreachable; peeks reject before any call

	t.Run("stream true -> 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if typ := anthropicErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
		}
	})
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
	fwd := forward.New(prov, forward.NewClient(), 5*time.Second, 1<<20)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, fwd))

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
	fwd := forward.New(prov, forward.NewClient(), 5*time.Second, 1<<20)
	base := startServer(t, New(testConfig(), discardLogger(t), prov, fwd))

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

// TestOpenAISynchronousOnlyAtBoundary proves the synchronous-only boundary for
// the OpenAI surface through the full assembled stack: stream:true AND
// background:true are each rejected with an OpenAI-shaped 400 before any upstream
// call (the upstream is unreachable, so a leak would surface as 502/504).
func TestOpenAISynchronousOnlyAtBoundary(t *testing.T) {
	h, _ := stack(t, "http://127.0.0.1:1", true)

	reject := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("stream true -> OpenAI-shaped 400", func(t *testing.T) {
		rec := reject(`{"stream":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
		}
	})

	t.Run("background true -> OpenAI-shaped 400", func(t *testing.T) {
		rec := reject(`{"background":true}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if typ := openaiErrorType(t, rec.Body.Bytes()); typ != "invalid_request_error" {
			t.Errorf("error.type = %q, want invalid_request_error", typ)
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
		rec := httptest.NewRecorder()
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
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("authenticated but not ready -> OpenAI-shaped 503", func(t *testing.T) {
		h, _ := stack(t, "", false)
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
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
		fwd := forward.New(prov, forward.NewClient(), time.Second, 8) // 8-byte cap
		h := newHandler(testAPIKey, prov, fwd, discardLogger(t))
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"way too long"}`))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		rec := httptest.NewRecorder()
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
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 (verbatim)", rec.Code)
		}
		if got := rec.Body.String(); got != upstreamErr {
			t.Errorf("body = %q, want the upstream error verbatim", got)
		}
	})
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
