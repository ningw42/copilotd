package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

// readyStub returns a ready Static provider whose Credential points at baseURL
// and carries a fixed impersonation set and Copilot token.
func readyStub(baseURL string) *identity.Static {
	return identity.NewStatic(identity.Credential{
		BaseURL: baseURL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
}

// TestForwardVerbatimAndHeaderPolicy is the round-trip proof: the original body
// reaches the upstream unchanged, the denylist strips the inbound API key while
// setting the Copilot Authorization, impersonation, and correlation id, other
// client headers pass through, and the upstream response is copied back verbatim.
func TestForwardVerbatimAndHeaderPolicy(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "present")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_1","ok":true}`)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(), 5*time.Second, 1<<20)

	const reqBody = `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer inbound-api-key") // must be stripped
	req.Header.Set("X-Api-Key", "inbound-api-key")            // must be stripped
	req.Header.Set("Content-Type", "application/json")        // passthrough
	req.Header.Set("Anthropic-Version", "2023-06-01")         // passthrough
	req.Header.Set("Editor-Version", "client-supplied")       // replaced by impersonation
	req.Header.Set("Connection", "X-Drop-Me")                 // hop-by-hop listing
	req.Header.Set("X-Drop-Me", "should-not-cross")
	req = req.WithContext(logging.WithRequestID(req.Context(), "rid-fwd"))

	rec := httptest.NewRecorder()
	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	// Response copied back verbatim.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"id":"msg_1","ok":true}` {
		t.Errorf("body = %q, want the upstream body verbatim", got)
	}
	if m := rec.Header().Get("X-Upstream-Marker"); m != "present" {
		t.Errorf("upstream response header not copied back: %q", m)
	}

	// Original bytes and path.
	if string(gotBody) != reqBody {
		t.Errorf("upstream body = %q, want the original bytes %q", gotBody, reqBody)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}

	// Header policy.
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer copilot-token" {
		t.Errorf("upstream Authorization = %q, want Bearer copilot-token", auth)
	}
	if k := gotHeaders.Get("X-Api-Key"); k != "" {
		t.Errorf("upstream X-Api-Key = %q, want it stripped", k)
	}
	if v := gotHeaders.Get("Copilot-Integration-Id"); v != "vscode-chat" {
		t.Errorf("upstream Copilot-Integration-Id = %q, want vscode-chat", v)
	}
	if v := gotHeaders.Get("Editor-Version"); v != "vscode/1.104.1" {
		t.Errorf("upstream Editor-Version = %q, want the impersonation value", v)
	}
	if v := gotHeaders.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("upstream Anthropic-Version = %q, want it passed through", v)
	}
	if v := gotHeaders.Get("X-Request-Id"); v != "rid-fwd" {
		t.Errorf("upstream X-Request-Id = %q, want rid-fwd", v)
	}
	if v := gotHeaders.Get("X-Drop-Me"); v != "" {
		t.Errorf("Connection-listed header leaked upstream: %q", v)
	}

	// The inbound API key must appear nowhere upstream.
	for name, vals := range gotHeaders {
		for _, v := range vals {
			if strings.Contains(v, "inbound-api-key") {
				t.Errorf("inbound API key leaked upstream in %s: %q", name, v)
			}
		}
	}
}

// TestForwardUpstreamErrorVerbatim proves a non-2xx upstream response (400/429/
// 500) is copied back unchanged rather than re-wrapped by apierror.
func TestForwardUpstreamErrorVerbatim(t *testing.T) {
	const upstreamErr = `{"type":"error","error":{"type":"api_error","message":"upstream boom"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, upstreamErr)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(), 5*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (verbatim)", rec.Code)
	}
	if got := rec.Body.String(); got != upstreamErr {
		t.Errorf("body = %q, want the upstream error verbatim", got)
	}
}

func TestForwardSynchronousOnlyPeek(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	f := New(readyStub(upstream.URL), NewClient(), 5*time.Second, 1<<20)

	t.Run("stream true rejected without reaching upstream", func(t *testing.T) {
		hits = 0
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
		rec := httptest.NewRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if hits != 0 {
			t.Errorf("upstream hit %d times, want 0 for a rejected stream request", hits)
		}
	})

	t.Run("stream false forwarded", func(t *testing.T) {
		hits = 0
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":false}`))
		rec := httptest.NewRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusOK || hits != 1 {
			t.Errorf("status = %d, hits = %d; want 200 forwarded once", rec.Code, hits)
		}
	})

	t.Run("non-JSON body forwarded, not treated as streaming", func(t *testing.T) {
		hits = 0
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`not json at all`))
		rec := httptest.NewRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusOK || hits != 1 {
			t.Errorf("status = %d, hits = %d; want a malformed body forwarded once", rec.Code, hits)
		}
	})
}

func TestForwardBodyBounding(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(), 5*time.Second, 8) // 8-byte cap
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"way too long"}`))
	rec := httptest.NewRecorder()
	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream hit %d times, want 0 for an over-limit body", hits)
	}
}

func TestForwardProxyOriginErrors(t *testing.T) {
	t.Run("unreachable upstream yields 502", func(t *testing.T) {
		// 127.0.0.1:1 refuses connections immediately.
		f := New(readyStub("http://127.0.0.1:1"), NewClient(), 2*time.Second, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("upstream slower than the deadline yields 504", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		f := New(readyStub(upstream.URL), NewClient(), 100*time.Millisecond, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", rec.Code)
		}
	})
}

// TestForwardClientCancelPropagates proves a client disconnect cancels the
// upstream call and the handler returns promptly without synthesizing a 504.
func TestForwardClientCancelPropagates(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server's background read can detect the client
		// closing the connection and cancel this request's context.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			close(upstreamCancelled)
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(), 5*time.Second, 1<<20)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let the outbound request reach the upstream
	cancel()

	select {
	case <-upstreamCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe the client cancellation")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return promptly after client cancel")
	}
	if rec.Code == http.StatusGatewayTimeout {
		t.Errorf("client cancel synthesized a 504, want no error body")
	}
}
