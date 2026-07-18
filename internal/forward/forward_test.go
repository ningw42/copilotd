package forward

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
)

type requestMutationShim struct {
	query string
}

var _ shim.RequestTransformer = (*requestMutationShim)(nil)

func (s *requestMutationShim) TransformRequest(_ context.Context, r *shim.Request) error {
	s.query = r.Query()
	r.Body = []byte(`{"shimmed":true}`)
	r.Header = http.Header{
		"Content-Type":   {"application/shim+json"},
		"Editor-Version": {"shim-must-lose"},
		"X-Shim":         {"applied"},
	}
	return nil
}

func TestForwardRequestShimMutationsReachUpstreamAndQueryStaysCoreOwned(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	var gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	instance := &requestMutationShim{}
	var gotSurface apierror.Surface
	var gotRoute shim.Route
	registry := shim.Registry{{
		Name:    "request-mutation",
		Enabled: true,
		New: func(_ context.Context, surface apierror.Surface, route shim.Route) any {
			gotSurface, gotRoute = surface, route
			return instance
		},
	}}
	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages?tag=first&escaped=%2f%2F&flag", strings.NewReader(`{"original":true}`))
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want upstream 204", rec.Code)
	}
	if gotSurface != apierror.Anthropic || gotRoute != "/v1/messages" {
		t.Errorf("factory endpoint = (%v, %q), want (Anthropic, /v1/messages)", gotSurface, gotRoute)
	}
	if instance.query != "tag=first&escaped=%2f%2F&flag" {
		t.Errorf("shim Query() = %q, want exact inbound raw query", instance.query)
	}
	if gotRequestURI != "/v1/messages?tag=first&escaped=%2f%2F&flag" {
		t.Errorf("upstream RequestURI = %q, want core-owned verbatim query", gotRequestURI)
	}
	if string(gotBody) != `{"shimmed":true}` {
		t.Errorf("upstream body = %q, want shimmed body", gotBody)
	}
	if gotHeader.Get("Content-Type") != "application/shim+json" || gotHeader.Get("X-Shim") != "applied" {
		t.Errorf("upstream headers = %v, want shim mutations", gotHeader)
	}
	if gotHeader.Get("Editor-Version") != "vscode/1.104.1" {
		t.Errorf("Editor-Version = %q, want impersonation to override shim", gotHeader.Get("Editor-Version"))
	}
}

type preludeMutationShim struct{}

var _ shim.PreludeTransformer = (*preludeMutationShim)(nil)

func (*preludeMutationShim) TransformPrelude(_ context.Context, p *shim.Prelude) error {
	p.Status = http.StatusMultiStatus
	p.Header.Set("X-Prelude", "applied")
	p.Header.Del("X-Remove")
	return nil
}

type bufferedResponseShim struct {
	calls int
	body  []byte
	err   error
}

var _ shim.BufferedTransformer = (*bufferedResponseShim)(nil)

func (s *bufferedResponseShim) TransformBuffered(_ context.Context, b *shim.Body) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	b.Bytes = append([]byte(nil), s.body...)
	return nil
}

func TestForwardBufferedShimTransformsBodyAndRecomputesContentLength(t *testing.T) {
	instance := &bufferedResponseShim{body: []byte(`{"transformed":true}`)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type":   {"application/json"},
				"Content-Length": {"8"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"ok":1}`)),
			Request: r,
		}, nil
	})}
	registry := shim.Registry{{
		Name:    "buffered",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return instance
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	rec := newDeadlineRecorder()

	f.Handler("/responses", apierror.OpenAI)(rec, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`)))

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
	if got, want := rec.Body.String(), `{"transformed":true}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Content-Length"), "20"; got != want {
		t.Errorf("Content-Length = %q, want transformed length %q", got, want)
	}
	if instance.calls != 1 {
		t.Errorf("TransformBuffered calls = %d, want 1", instance.calls)
	}
}

type commitObservingRecorder struct {
	*deadlineRecorder
	body          *observedReadCloser
	readsAtCommit int
}

func (r *commitObservingRecorder) WriteHeader(status int) {
	r.readsAtCommit = r.body.reads
	r.deadlineRecorder.WriteHeader(status)
}

func TestForwardWithoutBufferedHookCommitsBeforeReadingVerbatimBody(t *testing.T) {
	const upstream = `{"opaque":true}`
	body := &observedReadCloser{reader: strings.NewReader(upstream)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type":   {"application/json"},
				"Content-Length": {strconv.Itoa(len(upstream))},
			},
			Body:    body,
			Request: r,
		}, nil
	})}
	registry := shim.CanonicalRegistry()
	registry[0].Enabled = true
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1, registry)
	rec := &commitObservingRecorder{deadlineRecorder: newDeadlineRecorder(), body: body}

	f.Handler("/v1/messages", apierror.Anthropic)(rec, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)))

	if rec.readsAtCommit != 0 {
		t.Errorf("upstream body reads at commit = %d, want zero for unbuffered path", rec.readsAtCommit)
	}
	if got := rec.Body.String(); got != upstream {
		t.Errorf("body = %q, want byte-exact %q", got, upstream)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(upstream)) {
		t.Errorf("Content-Length = %q, want untouched %d", got, len(upstream))
	}
}

func TestForwardBufferedShimSkipsNonIdentityEncodedResponse(t *testing.T) {
	upstream := []byte("\x1f\x8bopaque")
	instance := &bufferedResponseShim{body: []byte("must-not-appear")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     {"application/json"},
				"Content-Encoding": {"gzip"},
				"Content-Length":   {strconv.Itoa(len(upstream))},
			},
			Body:    io.NopCloser(bytes.NewReader(upstream)),
			Request: r,
		}, nil
	})}
	registry := shim.Registry{{
		Name:    "buffered",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return instance
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1, registry)
	rec := newDeadlineRecorder()

	f.Handler("/responses", apierror.OpenAI)(rec, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`)))

	if instance.calls != 0 {
		t.Errorf("TransformBuffered calls = %d, want zero for encoded response", instance.calls)
	}
	if !bytes.Equal(rec.Body.Bytes(), upstream) {
		t.Errorf("body = %q, want encoded bytes %q", rec.Body.Bytes(), upstream)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip untouched", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(upstream)) {
		t.Errorf("Content-Length = %q, want untouched %d", got, len(upstream))
	}
}

func TestForwardBufferedResponseOverCapRendersBeforeCommit(t *testing.T) {
	instance := &bufferedResponseShim{body: []byte("must-not-appear")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type": {"application/json"},
				"X-Upstream":   {"must-not-leak"},
			},
			Body:    io.NopCloser(strings.NewReader("123456789")),
			Request: r,
		}, nil
	})}
	registry := shim.Registry{{
		Name:    "buffered",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return instance
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 8, registry)
	rec := newDeadlineRecorder()

	f.Handler("/responses", apierror.OpenAI)(rec, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	const wantBody = `{"error":{"message":"upstream response body exceeds the maximum allowed size","type":"invalid_request_error","code":null,"param":null}}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want native cap error %q", got, wantBody)
	}
	if instance.calls != 0 {
		t.Errorf("TransformBuffered calls = %d, want zero for over-cap body", instance.calls)
	}
	if got := rec.Header().Get("X-Upstream"); got != "" {
		t.Errorf("X-Upstream = %q, want no upstream header committed", got)
	}
}

func TestForwardBufferedShimFailureRendersBeforeCommit(t *testing.T) {
	instance := &bufferedResponseShim{err: errors.New("private transform failure")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type": {"application/json"},
				"X-Upstream":   {"must-not-leak"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request: r,
		}, nil
	})}
	registry := shim.Registry{{
		Name:    "buffered",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return instance
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	const wantBody = `{"type":"error","error":{"type":"api_error","message":"copilotd: shim failed"}}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want native shim error %q", got, wantBody)
	}
	if got := rec.Header().Get("X-Upstream"); got != "" {
		t.Errorf("X-Upstream = %q, want no upstream header committed", got)
	}
}

type firstWriteRecorder struct {
	*deadlineRecorder
	firstStatus int
	firstHeader http.Header
}

func newFirstWriteRecorder() *firstWriteRecorder {
	return &firstWriteRecorder{deadlineRecorder: newDeadlineRecorder()}
}

func (r *firstWriteRecorder) Write(p []byte) (int, error) {
	if r.firstHeader == nil {
		r.firstStatus = r.Code
		r.firstHeader = r.Header().Clone()
	}
	return r.ResponseRecorder.Write(p)
}

func TestForwardPreludeShimCommitsMutationsBeforeBodyOnBothPaths(t *testing.T) {
	const bufferedBody = `{"ok":true}`
	const streamBody = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "buffered", contentType: "application/json", body: bufferedBody},
		{name: "stream", contentType: "text/event-stream", body: streamBody},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header: http.Header{
						"Content-Type": {tc.contentType},
						"X-Remove":     {"upstream"},
					},
					Body:    io.NopCloser(strings.NewReader(tc.body)),
					Request: r,
				}, nil
			})}
			registry := shim.Registry{{
				Name:    "prelude-mutation",
				Enabled: true,
				New: func(context.Context, apierror.Surface, shim.Route) any {
					return &preludeMutationShim{}
				},
			}}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
			rec := newFirstWriteRecorder()

			f.Handler("/v1/messages", apierror.Anthropic)(rec, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)))

			if rec.firstStatus != http.StatusMultiStatus {
				t.Errorf("status at first body byte = %d, want prelude-mutated 207", rec.firstStatus)
			}
			if rec.firstHeader.Get("X-Prelude") != "applied" || rec.firstHeader.Get("X-Remove") != "" {
				t.Errorf("headers at first body byte = %v, want prelude mutations", rec.firstHeader)
			}
			if got := rec.Body.String(); got != tc.body {
				t.Errorf("body = %q, want verbatim %q", got, tc.body)
			}
		})
	}
}

type rejectingRequestShim struct {
	err error
}

var _ shim.RequestTransformer = (*rejectingRequestShim)(nil)

func (s *rejectingRequestShim) TransformRequest(context.Context, *shim.Request) error {
	return s.err
}

func TestForwardRequestShimRejectionUsesNativeShapeWithoutCallingUpstream(t *testing.T) {
	tests := []struct {
		name       string
		surface    apierror.Surface
		err        error
		wantStatus int
		wantType   string
		wantBody   string
	}{
		{
			name:       "bare Anthropic failure defaults to shim error",
			surface:    apierror.Anthropic,
			err:        errors.New("private implementation detail"),
			wantStatus: http.StatusInternalServerError,
			wantType:   "api_error",
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"copilotd: shim failed"}}`,
		},
		{
			name:       "deliberate OpenAI rejection keeps selected kind and message",
			surface:    apierror.OpenAI,
			err:        apierror.Reject(apierror.InvalidRequest, "unsupported option"),
			wantStatus: http.StatusBadRequest,
			wantType:   "invalid_request_error",
			wantBody:   `{"error":{"message":"unsupported option","type":"invalid_request_error","code":null,"param":null}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstreamCalls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls++
				return nil, errors.New("must not be called")
			})}
			registry := shim.Registry{{
				Name:    "reject",
				Enabled: true,
				New: func(context.Context, apierror.Surface, shim.Route) any {
					return &rejectingRequestShim{err: tc.err}
				},
			}}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
			rec := newDeadlineRecorder()

			f.Handler("/route", tc.surface)(rec, httptest.NewRequest(http.MethodPost, "/provider/route", strings.NewReader(`{}`)))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want native shape %q", got, tc.wantBody)
			}
			if upstreamCalls != 0 {
				t.Errorf("upstream calls = %d, want zero", upstreamCalls)
			}
			if tc.surface == apierror.Anthropic {
				if got := anthropicErrorType(t, rec.Body.Bytes()); got != tc.wantType {
					t.Errorf("error.type = %q, want %q", got, tc.wantType)
				}
			}
		})
	}
}

type rejectingPreludeShim struct{}

var _ shim.PreludeTransformer = (*rejectingPreludeShim)(nil)

func (*rejectingPreludeShim) TransformPrelude(context.Context, *shim.Prelude) error {
	return apierror.Reject(apierror.InvalidRequest, "response envelope rejected")
}

func TestForwardPreludeShimRejectionReplacesUpstreamResponseBeforeCommit(t *testing.T) {
	upstreamBody := &observedReadCloser{reader: strings.NewReader("must-not-leak")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}, "X-Upstream": {"must-not-leak"}},
			Body:       upstreamBody,
			Request:    r,
		}, nil
	})}
	registry := shim.Registry{{
		Name:    "reject-prelude",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return &rejectingPreludeShim{}
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	rec := newDeadlineRecorder()

	f.Handler("/responses", apierror.OpenAI)(rec, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	const wantBody = `{"error":{"message":"response envelope rejected","type":"invalid_request_error","code":null,"param":null}}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if got := rec.Header().Get("X-Upstream"); got != "" {
		t.Errorf("X-Upstream = %q, want prelude rejection before upstream headers commit", got)
	}
	if upstreamBody.reads != 0 || !upstreamBody.closed {
		t.Errorf("upstream body reads/closed = %d/%t, want 0/true", upstreamBody.reads, upstreamBody.closed)
	}
}

func TestForwardEnabledNopIsByteExactWithEmptyChainOnBothPaths(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "buffered", contentType: "application/json", body: `{"unknown":{"nested":true}}`},
		{name: "stream", contentType: "text/event-stream", body: "event: message_stop\r\ndata: {\"type\":\"message_stop\",\"unknown\":true}\r\n\r\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := func(registry shim.Registry) *deadlineRecorder {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusAccepted,
						Header:     http.Header{"Content-Type": {tc.contentType}, "X-Upstream": {"preserved"}},
						Body:       io.NopCloser(strings.NewReader(tc.body)),
						Request:    r,
					}, nil
				})}
				f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
				rec := newDeadlineRecorder()
				f.Handler("/v1/messages", apierror.Anthropic)(rec, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"raw":true}`)))
				return rec
			}

			empty := run(nil)
			nopRegistry := shim.CanonicalRegistry()
			nopRegistry[0].Enabled = true
			nop := run(nopRegistry)

			if nop.Code != empty.Code || !bytes.Equal(nop.Body.Bytes(), empty.Body.Bytes()) || !reflect.DeepEqual(nop.Header(), empty.Header()) {
				t.Errorf("enabled nop response = status %d headers %v body %q; empty chain = status %d headers %v body %q", nop.Code, nop.Header(), nop.Body.Bytes(), empty.Code, empty.Header(), empty.Body.Bytes())
			}
			if got := nop.Body.String(); got != tc.body {
				t.Errorf("nop body = %q, want exact upstream bytes %q", got, tc.body)
			}
		})
	}
}

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

func TestStreamPolicyMapsShimFailureToNativeTerminal(t *testing.T) {
	policy := streamPolicy(apierror.Anthropic, time.Minute, 90*time.Second, 15*time.Second, sse.RealClock{}, nil)
	rec := httptest.NewRecorder()

	if err := policy.RenderError(rec, sse.OutcomeShimError); err != nil {
		t.Fatalf("RenderError() error = %v", err)
	}

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: shim failed\"}}\n\n"
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
	f := New(readyStub("https://upstream.invalid"), http.DefaultClient, time.Minute, time.Minute, 37*time.Second, 13*time.Second, 1<<20, 1<<20, nil)
	if f.streamIdleTimeout != 37*time.Second {
		t.Errorf("streamIdleTimeout = %v, want 37s", f.streamIdleTimeout)
	}
	if f.streamKeepaliveInterval != 13*time.Second {
		t.Errorf("streamKeepaliveInterval = %v, want 13s", f.streamKeepaliveInterval)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type observedReadCloser struct {
	reader io.Reader
	reads  int
	closed bool
}

func (r *observedReadCloser) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func (r *observedReadCloser) Close() error {
	r.closed = true
	return nil
}

type failingEventShim struct {
	err error
}

type holdingStreamShim struct {
	held []sse.Frame
}

func (s *holdingStreamShim) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	s.held = append(s.held, frame)
	return nil, nil
}

func (s *holdingStreamShim) Finalize(context.Context) ([]sse.Frame, error) {
	return s.held, nil
}

type alteringFailingFinalizerShim struct {
	err error
}

type observingEventShim struct {
	seen []sse.Frame
}

func (s *observingEventShim) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	s.seen = append(s.seen, frame)
	return []sse.Frame{frame}, nil
}

type failingFinalizerOnlyShim struct {
	frames []sse.Frame
	err    error
}

func (s *failingFinalizerOnlyShim) Finalize(context.Context) ([]sse.Frame, error) {
	return s.frames, s.err
}

func (*alteringFailingFinalizerShim) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	frame.Raw = append([]byte(": outer-transform\n"), frame.Raw...)
	return []sse.Frame{frame}, nil
}

func (s *alteringFailingFinalizerShim) Finalize(context.Context) ([]sse.Frame, error) {
	return nil, s.err
}

var _ shim.EventTransformer = (*failingEventShim)(nil)

func (s *failingEventShim) TransformEvent(context.Context, sse.Frame) ([]sse.Frame, error) {
	return nil, s.err
}

func TestForwardStreamShimFailureRendersNativeTerminalAndReleasesUpstream(t *testing.T) {
	const upstreamFrame = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"secret\":\"must-not-leak\"}\n\n"
	body := &observedReadCloser{reader: strings.NewReader(upstreamFrame)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})}
	instance := &failingEventShim{err: errors.New("private shim internals")}
	registry := shim.Registry{{
		Name:    "failing-event",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return instance
		},
	}}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: shim failed\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want one native shim terminal %q", got, want)
	}
	result, ok := StreamResultFromContext(ctx)
	if !ok || result.Outcome != sse.OutcomeShimError || result.Frames != 0 {
		t.Errorf("stream result = %#v, %t, want shim_error with zero transformed frames", result, ok)
	}
	if !body.closed {
		t.Error("upstream body is open after stream shim failure")
	}
}

func TestForwardSuppressesOuterFinalizeErrorAfterFullyComposedTerminal(t *testing.T) {
	const upstreamTerminal = "event: message_stop\ndata: {\"type\":\"message_stop\",\"payload\":\"frame-secret\"}\n\n"
	body := &observedReadCloser{reader: strings.NewReader(upstreamTerminal)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})}
	outer := &alteringFailingFinalizerShim{err: errors.New("private-finalize-error")}
	inner := &holdingStreamShim{}
	registry := shim.Registry{
		{Name: "outer", Enabled: true, New: func(context.Context, apierror.Surface, shim.Route) any { return outer }},
		{Name: "inner", Enabled: true, New: func(context.Context, apierror.Surface, shim.Route) any { return inner }},
	}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	var logOutput bytes.Buffer
	f.logger = slog.New(slog.NewTextHandler(&logOutput, nil))
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if got, want := rec.Body.String(), ": outer-transform\n"+upstreamTerminal; got != want {
		t.Errorf("body = %q, want fully composed terminal %q", got, want)
	}
	result, ok := StreamResultFromContext(ctx)
	if !ok || result.Outcome != sse.OutcomeClean || result.Frames != 1 {
		t.Errorf("stream result = %#v, %t, want clean with one transformed frame", result, ok)
	}
	if got := f.suppressedShimErrors.Count(); got != 1 {
		t.Errorf("suppressed shim errors = %d, want 1", got)
	}
	logText := logOutput.String()
	if !strings.Contains(logText, "suppressed post-terminal shim error") || !strings.Contains(logText, "stage=finalize") {
		t.Errorf("warning = %q, want suppression metadata", logText)
	}
	for _, secret := range []string{"frame-secret", "private-finalize-error"} {
		if strings.Contains(logText, secret) {
			t.Errorf("warning leaked %q: %s", secret, logText)
		}
	}
	if !body.closed {
		t.Error("upstream body is open after suppressed finalize failure")
	}
}

func TestForwardDiscardsPartiallyComposedMiddleFinalizeOutput(t *testing.T) {
	body := &observedReadCloser{reader: strings.NewReader("")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       body,
			Request:    r,
		}, nil
	})}
	outer := &observingEventShim{}
	middle := &failingFinalizerOnlyShim{
		frames: []sse.Frame{{Type: "delta", Raw: []byte("partially-composed-secret")}},
		err:    errors.New("middle failed"),
	}
	inner := &failingFinalizerOnlyShim{}
	registry := shim.Registry{
		{Name: "outer-A", Enabled: true, New: func(context.Context, apierror.Surface, shim.Route) any { return outer }},
		{Name: "middle-B", Enabled: true, New: func(context.Context, apierror.Surface, shim.Route) any { return middle }},
		{Name: "inner-C", Enabled: true, New: func(context.Context, apierror.Surface, shim.Route) any { return inner }},
	}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, registry)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	ctx := WithStreamResultHolder(req.Context())
	req = req.WithContext(ctx)
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: shim failed\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want shim terminal without partial frame %q", got, want)
	}
	if len(outer.seen) != 0 {
		t.Errorf("outer event hook saw %#v, want middle output discarded at failure", outer.seen)
	}
	result, ok := StreamResultFromContext(ctx)
	if !ok || result.Outcome != sse.OutcomeShimError || result.Frames != 0 {
		t.Errorf("stream result = %#v, %t, want shim_error with no partial frames", result, ok)
	}
	if !body.closed {
		t.Error("upstream body is open after middle finalize failure")
	}
}

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
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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

func TestForwardRemovesExplicitIdentityEncodingFromEventStream(t *testing.T) {
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header: http.Header{
				"Content-Type":     {"text/event-stream"},
				"Content-Encoding": {"  IdEnTiTy\t"},
			},
			Body:    io.NopCloser(strings.NewReader(terminal)),
			Request: r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want upstream 202", rec.Code)
	}
	if got := rec.Header().Values("Content-Encoding"); len(got) != 0 {
		t.Errorf("downstream Content-Encoding values = %q, want absent for explicit identity", got)
	}
	if got := rec.Body.String(); got != terminal {
		t.Errorf("body = %q, want terminal frame verbatim %q", got, terminal)
	}
}

func TestForwardRejectsUnsupportedEventStreamEncodingBeforeCommit(t *testing.T) {
	const message = "upstream returned unsupported Content-Encoding for an event stream"
	const anthropicError = `{"type":"error","error":{"type":"api_error","message":"` + message + `"}}`
	const openAIError = `{"error":{"message":"` + message + `","type":"api_error","code":null,"param":null}}`
	tests := []struct {
		name      string
		surface   apierror.Surface
		encodings []string
		wantBody  string
	}{
		{
			name:      "Anthropic rejects gzip",
			surface:   apierror.Anthropic,
			encodings: []string{"gzip"},
			wantBody:  anthropicError,
		},
		{
			name:      "OpenAI rejects another coding in its native envelope",
			surface:   apierror.OpenAI,
			encodings: []string{"br"},
			wantBody:  openAIError,
		},
		{
			name:      "rejects comma-separated coding chain",
			surface:   apierror.Anthropic,
			encodings: []string{"identity, gzip"},
			wantBody:  anthropicError,
		},
		{
			name:      "rejects explicit empty value",
			surface:   apierror.Anthropic,
			encodings: []string{""},
			wantBody:  anthropicError,
		},
		{
			name:      "rejects repeated identity fields",
			surface:   apierror.Anthropic,
			encodings: []string{"identity", "identity"},
			wantBody:  anthropicError,
		},
		{
			name:      "rejects repeated different fields",
			surface:   apierror.Anthropic,
			encodings: []string{"identity", "gzip"},
			wantBody:  anthropicError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstreamBody := &observedReadCloser{reader: strings.NewReader("upstream-secret-body")}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTeapot,
					Header: http.Header{
						"Content-Type":      {"text/event-stream"},
						"Content-Encoding":  append([]string(nil), tc.encodings...),
						"X-Upstream-Secret": {"must-not-leak"},
					},
					Body:    upstreamBody,
					Request: r,
				}, nil
			})}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
			req := httptest.NewRequest(http.MethodPost, "/provider/route", strings.NewReader(`{"stream":true}`))
			rec := newDeadlineRecorder()

			f.Handler("/upstream", tc.surface)(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want copilotd-originated 502", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Values("Content-Encoding"); len(got) != 0 {
				t.Errorf("rejected upstream Content-Encoding leaked downstream: %q", got)
			}
			if got := rec.Header().Get("X-Upstream-Secret"); got != "" {
				t.Errorf("rejected upstream header leaked downstream: %q", got)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want native error %q", got, tc.wantBody)
			}
			if upstreamBody.reads != 0 {
				t.Errorf("upstream body reads = %d, want zero before rejection", upstreamBody.reads)
			}
			if !upstreamBody.closed {
				t.Error("upstream body was not closed after rejection")
			}
		})
	}
}

func TestNewClientLeavesCompressionNegotiationAndDecodingToCaller(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"opaque":true}`)); err != nil {
		t.Fatalf("compress fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("finish compressed fixture: %v", err)
	}
	wantBody := append([]byte(nil), compressed.Bytes()...)

	var gotAcceptEncoding []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding = append([]string(nil), r.Header.Values("Accept-Encoding")...)
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wantBody)
	}))
	defer upstream.Close()

	resp, err := NewClient(5 * time.Second).Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if len(gotAcceptEncoding) != 0 {
		t.Errorf("transport-supplied Accept-Encoding = %q, want absent", gotAcceptEncoding)
	}
	if resp.Uncompressed {
		t.Error("response marked transparently decompressed, want encoded response")
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if !bytes.Equal(body, wantBody) {
		t.Errorf("body = %x, want encoded bytes %x", body, wantBody)
	}
}

func TestForwardKeepsCompressedBufferedResponseOpaque(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte(`{"opaque":true}`)); err != nil {
		t.Fatalf("compress fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("finish compressed fixture: %v", err)
	}
	wantBody := append([]byte(nil), compressed.Bytes()...)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("X-Upstream-Marker", "present")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(wantBody)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`))
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Errorf("status = %d, want upstream 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("X-Upstream-Marker"); got != "present" {
		t.Errorf("X-Upstream-Marker = %q, want present", got)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, wantBody) {
		t.Errorf("body = %x, want compressed upstream bytes %x", got, wantBody)
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
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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

func TestForwardPreservesRawQueryOnCurrentRoutes(t *testing.T) {
	var gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	tests := []struct {
		name           string
		inboundTarget  string
		upstreamPath   string
		surface        apierror.Surface
		fragment       string
		wantRequestURI string
	}{
		{
			name:           "Anthropic messages",
			inboundTarget:  "/anthropic/v1/messages?beta=true",
			upstreamPath:   "/v1/messages",
			surface:        apierror.Anthropic,
			wantRequestURI: "/v1/messages?beta=true",
		},
		{
			name:           "Anthropic token counting keeps order duplicates escapes and value forms",
			inboundTarget:  "/anthropic/v1/messages/count_tokens?tag=first&tag=second&escaped=%2f%2F&flag&empty=",
			upstreamPath:   "/v1/messages/count_tokens",
			surface:        apierror.Anthropic,
			wantRequestURI: "/v1/messages/count_tokens?tag=first&tag=second&escaped=%2f%2F&flag&empty=",
		},
		{
			name:           "OpenAI Responses",
			inboundTarget:  "/openai/v1/responses?include=output%2ftext&include=usage",
			upstreamPath:   "/responses",
			surface:        apierror.OpenAI,
			wantRequestURI: "/responses?include=output%2ftext&include=usage",
		},
		{
			name:           "no query stays absent and fragment is omitted",
			inboundTarget:  "/openai/v1/responses",
			upstreamPath:   "/responses",
			surface:        apierror.OpenAI,
			fragment:       "client-only",
			wantRequestURI: "/responses",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotRequestURI = ""
			req := httptest.NewRequest(http.MethodPost, tc.inboundTarget, strings.NewReader(`{}`))
			req.URL.Fragment = tc.fragment
			rec := newDeadlineRecorder()

			f.Handler(tc.upstreamPath, tc.surface)(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want upstream 204", rec.Code)
			}
			if gotRequestURI != tc.wantRequestURI {
				t.Errorf("upstream RequestURI = %q, want %q", gotRequestURI, tc.wantRequestURI)
			}
		})
	}
}

func TestForwardPreservesBareQueryMarker(t *testing.T) {
	var gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages?", strings.NewReader(`{}`))
	rec := newDeadlineRecorder()

	f.Handler("/v1/messages", apierror.Anthropic)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want upstream 204", rec.Code)
	}
	if gotRequestURI != "/v1/messages?" {
		t.Errorf("upstream RequestURI = %q, want %q", gotRequestURI, "/v1/messages?")
	}
}

func TestForwardRequestsIdentityEncodingOnCurrentRoutes(t *testing.T) {
	var gotAcceptEncoding []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEncoding = append([]string(nil), r.Header.Values("Accept-Encoding")...)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	tests := []struct {
		name          string
		inboundTarget string
		upstreamPath  string
		surface       apierror.Surface
		clientValues  []string
	}{
		{
			name:          "Anthropic messages replaces a mixed client value",
			inboundTarget: "/anthropic/v1/messages",
			upstreamPath:  "/v1/messages",
			surface:       apierror.Anthropic,
			clientValues:  []string{"gzip, br"},
		},
		{
			name:          "Anthropic token counting replaces repeated client values",
			inboundTarget: "/anthropic/v1/messages/count_tokens",
			upstreamPath:  "/v1/messages/count_tokens",
			surface:       apierror.Anthropic,
			clientValues:  []string{"gzip", "identity"},
		},
		{
			name:          "OpenAI Responses sets identity when client value is absent",
			inboundTarget: "/openai/v1/responses",
			upstreamPath:  "/responses",
			surface:       apierror.OpenAI,
		},
		{
			name:          "client value is not parsed or rejected",
			inboundTarget: "/openai/v1/responses",
			upstreamPath:  "/responses",
			surface:       apierror.OpenAI,
			clientValues:  []string{"definitely-not-a-content-coding"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAcceptEncoding = nil
			req := httptest.NewRequest(http.MethodPost, tc.inboundTarget, strings.NewReader(`{}`))
			if tc.clientValues != nil {
				req.Header["Accept-Encoding"] = append([]string(nil), tc.clientValues...)
			}
			rec := newDeadlineRecorder()

			f.Handler(tc.upstreamPath, tc.surface)(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want upstream 204", rec.Code)
			}
			if len(gotAcceptEncoding) != 1 || gotAcceptEncoding[0] != "identity" {
				t.Errorf("upstream Accept-Encoding values = %q, want exactly [identity]", gotAcceptEncoding)
			}
		})
	}
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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)

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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)

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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 8, 1<<20, nil) // 8-byte request cap
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
		f := New(readyStub(upstream.URL), NewClient(50*time.Millisecond), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
		f := New(readyStub("http://127.0.0.1:1"), NewClient(5*time.Second), 2*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
		f := New(readyStub(upstream.URL), NewClient(5*time.Second), 50*time.Millisecond, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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

	f := New(readyStub(upstream.URL), NewClient(time.Second), 50*time.Millisecond, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)

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

	f := New(readyStub(upstream.URL), NewClient(5*time.Second), 5*time.Second, 100*time.Millisecond, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
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
