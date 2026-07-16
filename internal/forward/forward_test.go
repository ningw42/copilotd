package forward

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

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
)

func TestStreamPolicyMapsStallToNativeTerminal(t *testing.T) {
	policy := streamPolicy(apierror.Anthropic, time.Minute, 90*time.Second, 15*time.Second, sse.RealClock{}, nil)
	if policy.IdleTimeout != 90*time.Second {
		t.Fatalf("IdleTimeout = %v, want 90s", policy.IdleTimeout)
	}
	rec := httptest.NewRecorder()
	if err := policy.RenderError(rec, sse.OutcomeStall); err != nil {
		t.Fatalf("RenderError() error = %v", err)
	}
	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream stalled\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestStreamPolicySelectsSurfaceTerminalAndKeepalive(t *testing.T) {
	tests := []struct {
		name          string
		surface       apierror.Surface
		wantKeepalive time.Duration
		terminal      map[string]bool
	}{
		{
			name:          "Anthropic forwards upstream pings without injection",
			surface:       apierror.Anthropic,
			wantKeepalive: 0,
			terminal:      map[string]bool{"message_stop": true, "error": true},
		},
		{
			name:          "OpenAI injects keepalive and recognizes every terminal",
			surface:       apierror.OpenAI,
			wantKeepalive: 17 * time.Second,
			terminal: map[string]bool{
				"response.completed":  true,
				"response.failed":     true,
				"response.incomplete": true,
				"error":               true,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := streamPolicy(tc.surface, time.Minute, 90*time.Second, 17*time.Second, sse.RealClock{}, nil)
			if policy.KeepaliveInterval != tc.wantKeepalive {
				t.Errorf("KeepaliveInterval = %v, want %v", policy.KeepaliveInterval, tc.wantKeepalive)
			}
			for _, eventType := range []string{"message_stop", "response.completed", "response.failed", "response.incomplete", "error", "unknown"} {
				if got, want := policy.Terminal(eventType), tc.terminal[eventType]; got != want {
					t.Errorf("Terminal(%q) = %t, want %t", eventType, got, want)
				}
			}
		})
	}
}

func TestNewUsesConfiguredStreamTimers(t *testing.T) {
	f := New(readyStub("https://upstream.invalid"), http.DefaultClient, time.Minute, time.Minute, 37*time.Second, 13*time.Second, 1<<20)
	if f.streamIdleTimeout != 37*time.Second {
		t.Errorf("streamIdleTimeout = %v, want 37s", f.streamIdleTimeout)
	}
	if f.streamKeepaliveInterval != 13*time.Second {
		t.Errorf("streamKeepaliveInterval = %v, want 13s", f.streamKeepaliveInterval)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestForwardParameterizedEventStreamUsesPump(t *testing.T) {
	const upstreamFrame = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	const synthesized = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream ended before a terminal event\"}}\n\n"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type":      {"text/event-stream; charset=utf-8"},
				"Content-Length":    {"65"},
				"X-Upstream-Marker": {"present"},
				"Connection":        {"X-Upstream-Hop"},
				"X-Upstream-Hop":    {"must-not-cross"},
			},
			Body:    io.NopCloser(strings.NewReader(upstreamFrame)),
			Request: r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	downstream := httptest.NewServer(f.Handler("/v1/messages", apierror.Anthropic))
	defer downstream.Close()

	resp, err := http.Post(downstream.URL, "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want upstream 202", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q, want parameterized upstream value", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want stripped for streaming response", got)
	}
	if got := resp.Header.Get("X-Upstream-Marker"); got != "present" {
		t.Errorf("X-Upstream-Marker = %q, want present", got)
	}
	if got := resp.Header.Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want hop-by-hop header stripped", got)
	}
	if got := resp.Header.Get("X-Upstream-Hop"); got != "" {
		t.Errorf("X-Upstream-Hop = %q, want Connection-listed header stripped", got)
	}
	if got := string(body); got != upstreamFrame+synthesized {
		t.Errorf("body = %q, want upstream frame followed by native synthesized terminal %q", got, upstreamFrame+synthesized)
	}
}

func TestForwardStoresStreamResultOnRequestHolder(t *testing.T) {
	const first = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(first + terminal)),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)

	f.Handler("/v1/messages", apierror.Anthropic)(newDeadlineRecorder(), req)

	got, ok := StreamResultFromContext(ctx)
	if !ok {
		t.Fatal("stream result holder is unset, want pump result")
	}
	want := StreamResult{Surface: "anthropic", Outcome: sse.OutcomeClean, Frames: 2}
	if got != want {
		t.Errorf("stream result = %#v, want %#v", got, want)
	}
}

func TestForwardReportsDataTypeFallbacks(t *testing.T) {
	const terminal = "data: {\"type\":\"message_stop\"}\n\n"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(terminal)),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)

	f.Handler("/v1/messages", apierror.Anthropic)(newDeadlineRecorder(), req)

	if got := f.fallbacks.Count(); got != 1 {
		t.Errorf("fallback count = %d, want 1 for data-only frame", got)
	}
	result, ok := StreamResultFromContext(ctx)
	if !ok {
		t.Fatal("stream result holder is unset")
	}
	if result.Fallbacks != 1 {
		t.Errorf("stream fallback count = %d, want 1 for data-only frame", result.Fallbacks)
	}
}

func TestForwardOpenAITerminalsAreVerbatimAndNeverDoubled(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.failed", "response.incomplete", "error"} {
		t.Run(eventType, func(t *testing.T) {
			raw := "event: " + eventType + "\ndata: {\"type\":\"" + eventType + "\",\"unknown\":true}\n\n"
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(raw)),
					Request:    r,
				}, nil
			})}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
			req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"stream":true}`))
			ctx := WithStreamResultHolder(req.Context())
			req = req.WithContext(ctx)
			rec := newDeadlineRecorder()

			f.Handler("/responses", apierror.OpenAI)(rec, req)

			if got := rec.Body.String(); got != raw {
				t.Errorf("body = %q, want exact terminal %q with no synthesized duplicate", got, raw)
			}
			got, ok := StreamResultFromContext(ctx)
			if !ok {
				t.Fatal("stream result holder is unset")
			}
			if got.Outcome != sse.OutcomeClean || got.Frames != 1 {
				t.Errorf("stream result = %#v, want clean one-frame terminal", got)
			}
		})
	}
}

func TestForwardLeavesStreamResultUnsetForBufferedResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":false}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)

	f.Handler("/v1/messages", apierror.Anthropic)(newDeadlineRecorder(), req)

	if got, ok := StreamResultFromContext(ctx); ok {
		t.Errorf("buffered response stored stream result %#v, want holder unset", got)
	}
}

func TestForwardStoresCanonicalOpenAIStreamSurface(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)

	f.Handler("/responses", apierror.OpenAI)(newDeadlineRecorder(), req)

	got, ok := StreamResultFromContext(ctx)
	if !ok {
		t.Fatal("OpenAI stream result holder is unset")
	}
	if got.Surface != "openai" {
		t.Errorf("OpenAI stream surface = %q, want canonical openai", got.Surface)
	}
}

func anthropicErrorType(t *testing.T, body []byte) string {
	t.Helper()
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Type != "error" {
		t.Fatalf("body is not Anthropic-shaped: %s", body)
	}
	return response.Error.Type
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)

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

	rec := newDeadlineRecorder()
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

// TestForwardJSONErrorToStreamRequestUsesBufferedPath proves the response is
// ground truth: even when the request asks for streaming, an upstream JSON 502
// stays on the buffered path and is copied back rather than re-wrapped.
func TestForwardJSONErrorToStreamRequestUsesBufferedPath(t *testing.T) {
	const upstreamErr = `{"type":"error","error":{"type":"api_error","message":"upstream boom"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, upstreamErr)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	rec := newDeadlineRecorder()
	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want upstream 502 verbatim", rec.Code)
	}
	if got := rec.Body.String(); got != upstreamErr {
		t.Errorf("body = %q, want the upstream error verbatim", got)
	}
}

func TestForwardSurfacePeek(t *testing.T) {
	var hits int
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)

	forward := func(surface apierror.Surface, body string) *deadlineRecorder {
		path := "/anthropic/v1/messages"
		upstreamPath := "/v1/messages"
		if surface == apierror.OpenAI {
			path = "/openai/v1/responses"
			upstreamPath = "/responses"
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := newDeadlineRecorder()
		f.Handler(upstreamPath, surface)(rec, req)
		return rec
	}

	t.Run("Anthropic stream true forwarded verbatim", func(t *testing.T) {
		hits = 0
		const body = `{"model":"claude-3-5-sonnet","stream":true}`
		rec := forward(apierror.Anthropic, body)
		if rec.Code != http.StatusAccepted || hits != 1 {
			t.Errorf("status = %d, hits = %d; want upstream 202 and one hit", rec.Code, hits)
		}
		if string(gotBody) != body {
			t.Errorf("upstream body = %q, want original bytes %q", gotBody, body)
		}
	})

	t.Run("OpenAI stream true forwarded verbatim", func(t *testing.T) {
		hits = 0
		const body = `{"model":"gpt-4.1","stream":true}`
		rec := forward(apierror.OpenAI, body)
		if rec.Code != http.StatusAccepted || hits != 1 {
			t.Errorf("status = %d, hits = %d; want upstream 202 and one hit", rec.Code, hits)
		}
		if string(gotBody) != body {
			t.Errorf("upstream body = %q, want original bytes %q", gotBody, body)
		}
	})

	t.Run("Anthropic background true forwarded without a peek", func(t *testing.T) {
		hits = 0
		rec := forward(apierror.Anthropic, `{"background":true}`)
		if rec.Code != http.StatusAccepted || hits != 1 {
			t.Errorf("status = %d, hits = %d; want upstream 202 and one hit", rec.Code, hits)
		}
	})

	t.Run("OpenAI background true rejected", func(t *testing.T) {
		hits = 0
		rec := forward(apierror.OpenAI, `{"background":true}`)
		if rec.Code != http.StatusBadRequest || hits != 0 {
			t.Errorf("status = %d, hits = %d; want 400 and no upstream hit", rec.Code, hits)
		}
	})

	t.Run("non-JSON OpenAI body forwarded for upstream validation", func(t *testing.T) {
		hits = 0
		rec := forward(apierror.OpenAI, `not json at all`)
		if rec.Code != http.StatusAccepted || hits != 1 {
			t.Errorf("status = %d, hits = %d; want upstream 202 and one hit", rec.Code, hits)
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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 8) // 8-byte cap
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"model":"way too long"}`))
	rec := newDeadlineRecorder()
	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if hits != 0 {
		t.Errorf("upstream hit %d times, want 0 for an over-limit body", hits)
	}
}

func TestForwardProxyOriginErrors(t *testing.T) {
	t.Run("response header timeout yields 504", func(t *testing.T) {
		releaseUpstream := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-releaseUpstream
			w.WriteHeader(http.StatusOK)
		}))
		defer upstream.Close()
		f := New(readyStub(upstream.URL), NewClient(50*time.Millisecond), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := newDeadlineRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		close(releaseUpstream)
		if rec.Code != http.StatusGatewayTimeout {
			t.Errorf("status = %d, want 504", rec.Code)
		}
		if got := anthropicErrorType(t, rec.Body.Bytes()); got != "api_error" {
			t.Errorf("error.type = %q, want api_error", got)
		}
	})

	t.Run("unreachable upstream yields 502", func(t *testing.T) {
		// 127.0.0.1:1 refuses connections immediately.
		f := New(readyStub("http://127.0.0.1:1"), NewClient(5*time.Second), 2*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := newDeadlineRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("outbound timeout remains a buffered response backstop", func(t *testing.T) {
		upstreamCancelled := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(upstreamCancelled)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		}))
		defer upstream.Close()
		f := New(readyStub(upstream.URL), NewClient(5*time.Second), 50*time.Millisecond, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)
		req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
		rec := newDeadlineRecorder()
		f.Handler("/v1/messages", apierror.Anthropic)(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want already-committed upstream 200", rec.Code)
		}
		select {
		case <-upstreamCancelled:
		case <-time.After(time.Second):
			t.Fatal("buffered response was not cancelled by its total backstop")
		}
	})
}

func TestOutboundTimeoutIsNotAppliedToEventStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {}\n\n")
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(time.Second), 50*time.Millisecond, time.Second, 90*time.Second, 15*time.Second, 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
	rec := newDeadlineRecorder()
	f.Handler("/responses", apierror.OpenAI)(rec, req)

	if got := rec.Body.String(); got != "event: response.completed\ndata: {}\n\n" {
		t.Errorf("body = %q, want complete event after outbound timeout elapsed", got)
	}
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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)).WithContext(ctx)
	rec := newDeadlineRecorder()

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

func TestBufferedCopyAbortsWhenClientStopsDraining(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamCancelled)
		chunk := make([]byte, 32<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 100*time.Millisecond, 90*time.Second, 15*time.Second, 1<<20)
	handlerReturned := make(chan struct{})
	downstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerReturned)
		f.Handler("/v1/messages", apierror.Anthropic)(w, r)
	}))
	downstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetWriteBuffer(1024)
			}
		}
	}
	downstream.Start()
	defer downstream.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(downstream.URL, "http://"))
	if err != nil {
		t.Fatalf("dial downstream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: copilotd.test\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatalf("write downstream request: %v", err)
	}

	select {
	case <-handlerReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("buffered handler did not abort after its per-write deadline")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("buffered copy abort did not close the upstream response body")
	}
}
