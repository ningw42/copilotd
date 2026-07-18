package forward

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
)

func TestIsolatedPassthroughClientPreservesAutomaticHTTP2(t *testing.T) {
	var gotProtocol string
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProtocol = r.Proto
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	client, closeIdleConnections := isolatedPassthroughClient(NewClient(time.Second))
	defer closeIdleConnections()

	transport := client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport.TLSClientConfig.RootCAs = roots

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("automatic HTTP/2 request through isolated client: %v", err)
	}
	defer resp.Body.Close()
	if gotProtocol != "HTTP/2.0" {
		t.Errorf("upstream protocol = %q, want HTTP/2.0 preserved from the injected client policy", gotProtocol)
	}
}

func TestPassthroughPreservesRawQueryAndForceQuery(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		wantTarget string
	}{
		{
			name:       "ordering duplicates escaping and valueless parameters",
			target:     "/models?dup=first&escaped=%2f%2F&flag&empty=&dup=second",
			wantTarget: "/models?dup=first&escaped=%2f%2F&flag&empty=&dup=second",
		},
		{name: "bare trailing question mark", target: "/models?", wantTarget: "/models?"},
		{name: "no query", target: "/models", wantTarget: "/models"},
	}

	for _, tc := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				var gotTarget string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotTarget = r.RequestURI
					w.WriteHeader(http.StatusNoContent)
				}))
				defer upstream.Close()

				f := New(readyStub(upstream.URL), NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
				req := httptest.NewRequest(method, tc.target, nil)
				rec := newDeadlineRecorder()

				f.PassthroughHandler(method, "/models", apierror.GitHubCopilot)(rec, req)

				if rec.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want 204", rec.Code)
				}
				if gotTarget != tc.wantTarget {
					t.Errorf("upstream request target = %q, want %q", gotTarget, tc.wantTarget)
				}
			})
		}
	}
}

func TestPassthroughEnforcesRequestHeaderOwnership(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			testPassthroughEnforcesRequestHeaderOwnership(t, method)
		})
	}
}

func testPassthroughEnforcesRequestHeaderOwnership(t *testing.T, method string) {
	t.Helper()
	const requestBody = "unusual request body that exceeds the one-byte inference cap"
	var gotHeader http.Header
	var gotContentLength int64
	var gotHost string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotHeader = r.Header.Clone()
		gotContentLength = r.ContentLength
		gotHost = r.Host
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if got := string(body); got != requestBody {
			t.Errorf("upstream request body = %q, want %q", got, requestBody)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	req := httptest.NewRequest(method, "/models", nil)
	req.Body = io.NopCloser(strings.NewReader(requestBody))
	req.ContentLength = int64(len(requestBody))
	req.Host = "client.example"
	req.Header = http.Header{
		"Accept":                    {"application/vnd.github+json", "application/json;q=0.5"},
		"Accept-Encoding":           {"br;q=1.0, gzip;q=0.8", "identity;q=0.1"},
		"Accept-Language":           {"en-US"},
		"Authorization":             {"Bearer inbound-api-key-sentinel"},
		"Connection":                {"X-Connection-Only, Editor-Version"},
		"Content-Length":            {"999999"},
		"Copilot-Integration-Id":    {"client-integration"},
		"Editor-Version":            {"client-editor"},
		"Host":                      {"client-header.example"},
		"If-Modified-Since":         {"Sat, 18 Jul 2026 08:00:00 GMT"},
		"If-None-Match":             {`"models-v7"`},
		"Keep-Alive":                {"timeout=5"},
		"Proxy-Authenticate":        {"Basic realm=client"},
		"Proxy-Authorization":       {"Basic client-secret"},
		"TE":                        {"trailers"},
		"Trailer":                   {"X-Checksum"},
		"Transfer-Encoding":         {"chunked"},
		"Upgrade":                   {"websocket"},
		"X-Api-Key":                 {"inbound-api-key-sentinel"},
		"X-Connection-Only":         {"must-not-cross"},
		"X-End-To-End-Client-Value": {"preserved-one", "preserved-two"},
		"X-Request-Id":              {"unresolved-client-id"},
	}
	req = req.WithContext(logging.WithRequestID(req.Context(), "resolved-request-id"))
	rec := newDeadlineRecorder()

	f.PassthroughHandler(method, "/models", apierror.GitHubCopilot)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if gotContentLength != int64(len(requestBody)) {
		t.Errorf("structured ContentLength = %d, want %d", gotContentLength, len(requestBody))
	}
	if gotHost == "client.example" || gotHost == "client-header.example" {
		t.Errorf("structured Host = %q, want upstream origin", gotHost)
	}
	for _, name := range []string{
		"Connection", "Content-Length", "Host", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
		"X-Api-Key", "X-Connection-Only",
	} {
		if values, ok := gotHeader[http.CanonicalHeaderKey(name)]; ok {
			t.Errorf("%s survived upstream with values %q", name, values)
		}
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer copilot-token" {
		t.Errorf("Authorization = %q, want Copilot token", got)
	}
	if got := gotHeader.Get("Copilot-Integration-Id"); got != "vscode-chat" {
		t.Errorf("Copilot-Integration-Id = %q, want impersonation value", got)
	}
	if got := gotHeader.Get("Editor-Version"); got != "vscode/1.104.1" {
		t.Errorf("Editor-Version = %q, want impersonation value", got)
	}
	if got := gotHeader.Get("X-Request-Id"); got != "resolved-request-id" {
		t.Errorf("X-Request-Id = %q, want resolved request ID", got)
	}
	for name, want := range (http.Header{
		"Accept":                    {"application/vnd.github+json", "application/json;q=0.5"},
		"Accept-Encoding":           {"br;q=1.0, gzip;q=0.8", "identity;q=0.1"},
		"Accept-Language":           {"en-US"},
		"If-Modified-Since":         {"Sat, 18 Jul 2026 08:00:00 GMT"},
		"If-None-Match":             {`"models-v7"`},
		"X-End-To-End-Client-Value": {"preserved-one", "preserved-two"},
	}) {
		if got := gotHeader.Values(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s values = %q, want %q", name, got, want)
		}
	}
}

func TestPassthroughLeavesAbsentAcceptEncodingAbsent(t *testing.T) {
	var gotValues []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotValues = append([]string(nil), r.Header.Values("Accept-Encoding")...)
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)

	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(newDeadlineRecorder(), httptest.NewRequest(http.MethodGet, "/models", nil))

	if len(gotValues) != 0 {
		t.Errorf("upstream Accept-Encoding values = %q, want absent", gotValues)
	}
}

func TestPassthroughHandlerMapsMethodAndRouteAndCopiesBasicResponse(t *testing.T) {
	tests := []struct {
		method   string
		wantBody string
	}{
		{method: http.MethodGet, wantBody: "opaque upstream body"},
		{method: http.MethodHead, wantBody: ""},
	}

	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			const requestBody = "request body larger than the one-byte inference cap"
			inboundBody := &observedReadCloser{reader: strings.NewReader(requestBody)}
			upstreamBody := &observedReadCloser{reader: strings.NewReader("opaque upstream body")}
			var calls int
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if inboundBody.reads != 0 {
					t.Errorf("request body reads before RoundTrip = %d, want zero", inboundBody.reads)
				}
				if r.Method != tc.method {
					t.Errorf("upstream method = %q, want %q", r.Method, tc.method)
				}
				if r.URL.Path != "/models" {
					t.Errorf("upstream Route = %q, want /models", r.URL.Path)
				}
				if r.ContentLength != int64(len(requestBody)) {
					t.Errorf("upstream ContentLength = %d, want %d", r.ContentLength, len(requestBody))
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read upstream request body: %v", err)
				}
				if string(body) != requestBody {
					t.Errorf("upstream body = %q, want %q", body, requestBody)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer copilot-token" {
					t.Errorf("Authorization = %q, want current Copilot token", got)
				}
				if got := r.Header.Get("X-Api-Key"); got != "" {
					t.Errorf("X-Api-Key leaked upstream: %q", got)
				}
				if got := r.Header.Get("Host"); got != "" || r.Host == "client.example" || r.Host == "client-header.example" {
					t.Errorf("inbound Host survived upstream: header=%q structured=%q", got, r.Host)
				}
				return &http.Response{
					StatusCode: http.StatusPartialContent,
					Header: http.Header{
						"Content-Type":      {"text/event-stream"},
						"Content-Length":    {strconv.Itoa(len("opaque upstream body"))},
						"X-Upstream-Marker": {"present"},
					},
					Body:    upstreamBody,
					Request: r,
				}, nil
			})}
			registry := shim.Registry{{
				Name:    "must-not-run",
				Enabled: true,
				New: func(context.Context, apierror.Surface, shim.Route) any {
					panic("passthrough instantiated the shim onion")
				},
			}}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1, 1, registry)
			req := httptest.NewRequest(tc.method, "/models", nil)
			req.Body = inboundBody
			req.ContentLength = int64(len(requestBody))
			req.Host = "client.example"
			req.Header.Set("Host", "client-header.example")
			req.Header.Set("Authorization", "Bearer inbound-api-key")
			req.Header.Set("X-Api-Key", "inbound-api-key")
			rec := &commitObservingRecorder{deadlineRecorder: newDeadlineRecorder(), body: upstreamBody}

			f.PassthroughHandler(tc.method, "/models", apierror.GitHubCopilot)(rec, req)

			if calls != 1 {
				t.Errorf("upstream calls = %d, want exactly 1", calls)
			}
			if rec.Code != http.StatusPartialContent {
				t.Errorf("status = %d, want upstream 206", rec.Code)
			}
			if got := rec.Header().Get("X-Upstream-Marker"); got != "present" {
				t.Errorf("X-Upstream-Marker = %q, want present", got)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len("opaque upstream body")) {
				t.Errorf("Content-Length = %q, want preserved upstream representation length", got)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if rec.readsAtCommit != 0 {
				t.Errorf("upstream response reads at commit = %d, want zero", rec.readsAtCommit)
			}
			if !upstreamBody.closed {
				t.Error("upstream response body was not closed")
			}
			if tc.method == http.MethodHead && upstreamBody.reads != 0 {
				t.Errorf("upstream HEAD response body reads = %d, want zero", upstreamBody.reads)
			}
		})
	}
}

func TestPassthroughCurrentFailureUsesGitHubCopilotRendererWithoutCallingUpstream(t *testing.T) {
	provider := readyStub("https://upstream.invalid")
	provider.SetError(errors.New("mint failed"))
	clientCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		clientCalls++
		return nil, errors.New("must not be called")
	})}
	f := New(provider, client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	rec := newDeadlineRecorder()

	f.PassthroughHandler(http.MethodHead, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodHead, "/models", nil))

	if clientCalls != 0 {
		t.Errorf("upstream calls = %d, want zero", clientCalls)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	const want = `{"type":"error","error":{"type":"api_error","message":"no upstream credential available"}}`
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want GitHub Copilot's approved Anthropic shape %q", got, want)
	}
}

func TestPassthroughPreservesAuthoritativeResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "2xx unknown model fields", status: http.StatusOK, body: []byte(`{"models":[{"future_field":{"nested":true}}]}`)},
		{name: "3xx first response", status: http.StatusTemporaryRedirect, body: []byte("redirect body")},
		{name: "4xx", status: http.StatusTooManyRequests, body: []byte(`{"upstream":"rate-limited"}`)},
		{name: "5xx encoded opaque bytes", status: http.StatusBadGateway, body: []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0x00, 0x7f}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := &cancelAwareBody{chunks: [][]byte{tc.body}}
			client := &http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					body.ctx = r.Context()
					return &http.Response{
						StatusCode: tc.status,
						Header: http.Header{
							"Cache-Control":         {"private", "max-age=0"},
							"Connection":            {"X-Connection-Only, X-Second-Hop"},
							"Content-Encoding":      {"gzip"},
							"Content-Type":          {"text/event-stream"},
							"Keep-Alive":            {"timeout=5"},
							"Location":              {"/redirect-target"},
							"Proxy-Authenticate":    {"Basic realm=upstream"},
							"Proxy-Authorization":   {"Basic secret"},
							"TE":                    {"trailers"},
							"Trailer":               {"X-Checksum"},
							"Transfer-Encoding":     {"chunked"},
							"Upgrade":               {"websocket"},
							"X-Connection-Only":     {"drop"},
							"X-End-To-End-Upstream": {"first", "second"},
							"X-Request-Id":          {"upstream-one", "upstream-two"},
							"X-Second-Hop":          {"drop-too"},
						},
						Body:    body,
						Request: r,
					}, nil
				})}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
			rec := newDeadlineRecorder()
			rec.Header().Set("X-Request-Id", "resolved-request-id")

			f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))

			if calls != 1 {
				t.Errorf("upstream calls = %d, want exactly 1", calls)
			}
			if rec.Code != tc.status {
				t.Errorf("status = %d, want authoritative upstream %d", rec.Code, tc.status)
			}
			if got := rec.Body.Bytes(); !bytes.Equal(got, tc.body) {
				t.Errorf("body = %q, want byte-exact %q", got, tc.body)
			}
			for name, want := range (http.Header{
				"Cache-Control":         {"private", "max-age=0"},
				"Content-Encoding":      {"gzip"},
				"Content-Type":          {"text/event-stream"},
				"Location":              {"/redirect-target"},
				"X-End-To-End-Upstream": {"first", "second"},
			}) {
				if got := rec.Header().Values(name); !reflect.DeepEqual(got, want) {
					t.Errorf("%s values = %q, want %q", name, got, want)
				}
			}
			for _, name := range []string{
				"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
				"TE", "Trailer", "Transfer-Encoding", "Upgrade", "X-Connection-Only", "X-Second-Hop",
			} {
				if got := rec.Header().Values(name); len(got) != 0 {
					t.Errorf("hop-by-hop %s survived with values %q", name, got)
				}
			}
			if got := rec.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"resolved-request-id"}) {
				t.Errorf("X-Request-Id values = %q, want sole resolved value", got)
			}
			assertBodyClosed(t, body)
		})
	}
}

func TestPassthroughTransportPreservesEncodedBytesAndFirstRedirect(t *testing.T) {
	t.Run("encoded body is not transparently decoded", func(t *testing.T) {
		encoded := []byte{0x1f, 0x8b, 0x08, 0x00, 0x6f, 0x70, 0x61, 0x71, 0x75, 0x65}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
				t.Errorf("Accept-Encoding = %q, want client value gzip", got)
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encoded)
		}))
		defer upstream.Close()

		f := New(readyStub(upstream.URL), NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
		req := httptest.NewRequest(http.MethodGet, "/models", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := newDeadlineRecorder()

		f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip", got)
		}
		if got := rec.Body.Bytes(); !bytes.Equal(got, encoded) {
			t.Errorf("body = %v, want encoded bytes %v", got, encoded)
		}
	})

	t.Run("redirect returns the first response without another call", func(t *testing.T) {
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Path != "/models" {
				t.Errorf("redirect was followed to %q", r.URL.Path)
			}
			w.Header().Set("Location", "/redirect-target")
			w.WriteHeader(http.StatusPermanentRedirect)
			_, _ = io.WriteString(w, "authoritative redirect body")
		}))
		defer upstream.Close()

		f := New(readyStub(upstream.URL), NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
		rec := newDeadlineRecorder()
		f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))

		if calls != 1 {
			t.Errorf("upstream calls = %d, want exactly 1", calls)
		}
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "/redirect-target" || rec.Body.String() != "authoritative redirect body" {
			t.Errorf("redirect response = status %d location %q body %q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	})
}

func TestPassthroughDoesNotReplayModelsAfterResponseFailure(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			var transportAttempts atomic.Int32
			var usedCachedConnection atomic.Bool
			var modelsCalls atomic.Int32

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				modelsCalls.Add(1)
				panic(http.ErrAbortHandler)
			}))
			defer upstream.Close()

			client := NewClient(time.Second)
			f := New(readyStub(upstream.URL), client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				transportAttempts.Add(1)
				usedCachedConnection.Store(info.Reused)
			}}
			req := httptest.NewRequest(method, "/models", nil)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
			rec := newDeadlineRecorder()
			f.PassthroughHandler(method, "/models", apierror.GitHubCopilot)(rec, req)

			if usedCachedConnection.Load() {
				t.Error("support request used a cached connection, want a fresh single-use connection")
			}
			if got := transportAttempts.Load(); got != 1 {
				t.Errorf("transport attempts = %d, want exactly one with no transparent replay", got)
			}
			if got := modelsCalls.Load(); got != 1 {
				t.Errorf("upstream /models calls = %d, want exactly one with no transparent replay", got)
			}
			const wantBody = `{"type":"error","error":{"type":"api_error","message":"could not reach the upstream"}}`
			if rec.Code != http.StatusBadGateway || rec.Body.String() != wantBody {
				t.Errorf("response failure = status %d body %q, want 502 %q", rec.Code, rec.Body.String(), wantBody)
			}
		})
	}
}

func TestPassthroughUsesFreshSingleUseConnection(t *testing.T) {
	tests := []struct {
		name             string
		tls              bool
		expectedProtocol string
	}{
		{name: "HTTP1", expectedProtocol: "HTTP/1.1"},
		{name: "HTTP2", tls: true, expectedProtocol: "HTTP/2.0"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acceptedConnections := make(chan net.Conn, 4)
			var seedProtocol string
			var transportAttempts atomic.Int32
			var usedCachedConnection atomic.Bool
			var modelsCalls atomic.Int32

			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/seed-idle-connection":
					seedProtocol = r.Proto
					w.WriteHeader(http.StatusNoContent)
				case "/models":
					modelsCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			upstream.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
				acceptedConnections <- conn
				return ctx
			}
			if test.tls {
				upstream.EnableHTTP2 = true
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			defer upstream.Close()

			client := NewClient(time.Second)
			if test.tls {
				transport := client.Transport.(*http.Transport)
				transport.TLSClientConfig = upstream.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				transport.ForceAttemptHTTP2 = true
			}
			seedResponse, err := client.Get(upstream.URL + "/seed-idle-connection")
			if err != nil {
				t.Fatalf("seed %s idle connection: %v", test.name, err)
			}
			if err := seedResponse.Body.Close(); err != nil {
				t.Fatalf("close seed response body: %v", err)
			}
			seededServerConnection := <-acceptedConnections
			if seedProtocol != test.expectedProtocol {
				t.Fatalf("seed protocol = %q, want %q", seedProtocol, test.expectedProtocol)
			}

			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				transportAttempts.Add(1)
				usedCachedConnection.Store(info.Reused)
				if info.Reused {
					_ = seededServerConnection.Close()
					return
				}
				freshServerConnection := <-acceptedConnections
				if !test.tls {
					if tcp, ok := freshServerConnection.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
				}
				_ = freshServerConnection.Close()
			}}
			req := httptest.NewRequest(http.MethodGet, "/models", nil)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
			f := New(readyStub(upstream.URL), client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
			rec := newDeadlineRecorder()
			f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, req)

			if usedCachedConnection.Load() {
				t.Errorf("%s /models request reused the injected client's connection, want an isolated fresh connection", test.name)
			}
			if got := transportAttempts.Load(); got != 1 {
				t.Errorf("%s transport attempts = %d, want exactly one with no transparent replay", test.name, got)
			}
			if got := modelsCalls.Load(); got != 0 {
				t.Errorf("delivered upstream %s /models calls = %d, want zero after the fresh connection failed", test.name, got)
			}
			const wantBody = `{"type":"error","error":{"type":"api_error","message":"could not reach the upstream"}}`
			if rec.Code != http.StatusBadGateway || rec.Body.String() != wantBody {
				t.Errorf("%s connection failure = status %d body %q, want 502 %q", test.name, rec.Code, rec.Body.String(), wantBody)
			}
		})
	}
}

func TestPassthroughPreHeaderFailuresUseLocalErrorPolicy(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		roundTrip roundTripFunc
		wantCode  int
		wantBody  string
		wantCalls int
	}{
		{
			name:      "request construction",
			baseURL:   "http://[::1",
			wantCode:  http.StatusBadGateway,
			wantBody:  `{"type":"error","error":{"type":"api_error","message":"could not build the upstream request"}}`,
			wantCalls: 0,
		},
		{
			name:    "reachability",
			baseURL: "https://upstream.invalid",
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network unreachable")
			},
			wantCode:  http.StatusBadGateway,
			wantBody:  `{"type":"error","error":{"type":"api_error","message":"could not reach the upstream"}}`,
			wantCalls: 1,
		},
		{
			name:    "deadline",
			baseURL: "https://upstream.invalid",
			roundTrip: func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			},
			wantCode:  http.StatusGatewayTimeout,
			wantBody:  `{"type":"error","error":{"type":"api_error","message":"the upstream request timed out"}}`,
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			provider := &countingProvider{cred: identity.Credential{BaseURL: tc.baseURL, Token: "copilot-token"}}
			roundTrip := tc.roundTrip
			if roundTrip == nil {
				roundTrip = func(*http.Request) (*http.Response, error) {
					t.Fatal("RoundTrip called after construction failure")
					return nil, nil
				}
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return roundTrip(r)
			})}
			f := New(provider, client, time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
			rec := newDeadlineRecorder()

			f.PassthroughHandler(http.MethodHead, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodHead, "/models", nil))

			if calls != tc.wantCalls {
				t.Errorf("upstream calls = %d, want %d", calls, tc.wantCalls)
			}
			if provider.calls != 1 {
				t.Errorf("Provider.Current calls = %d, want one with no remint/fallback retry", provider.calls)
			}
			if rec.Code != tc.wantCode || rec.Body.String() != tc.wantBody {
				t.Errorf("local error = status %d body %q, want %d %q", rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
		})
	}
}

func TestPassthroughResponseHeaderTimeoutUsesConfiguredClient(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(upstreamStarted)
		<-r.Context().Done()
	}))
	defer upstream.Close()

	f := New(readyStub(upstream.URL), NewClient(20*time.Millisecond), time.Second, time.Second, time.Second, time.Second, 1, 1, nil)
	rec := newDeadlineRecorder()
	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))
	<-upstreamStarted

	const want = `{"type":"error","error":{"type":"api_error","message":"the upstream request timed out"}}`
	if rec.Code != http.StatusGatewayTimeout || rec.Body.String() != want {
		t.Errorf("response-header timeout = status %d body %q, want 504 %q", rec.Code, rec.Body.String(), want)
	}
}

func TestPassthroughPostCommitReadFailureCancelsThenClosesWithoutSynthesis(t *testing.T) {
	readErr := errors.New("upstream body failed")
	body := &cancelAwareBody{chunks: [][]byte{[]byte("event: model.future\ndata: {\"opaque\":true}\n\n")}, terminal: readErr}
	client := bodyClient(body, http.Header{"Content-Type": {"text/event-stream"}})
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
	rec := newDeadlineRecorder()

	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))

	const want = "event: model.future\ndata: {\"opaque\":true}\n\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("post-read-failure response = status %d body %q, want committed 200 and raw prefix %q", rec.Code, rec.Body.String(), want)
	}
	assertBodyCleanup(t, body, true)
}

func TestPassthroughOutboundTimeoutStopsCommittedRawBody(t *testing.T) {
	body := &cancelAwareBody{chunks: [][]byte{[]byte("raw-prefix")}, blockAfterChunks: true}
	client := bodyClient(body, http.Header{"Content-Type": {"application/octet-stream"}})
	f := New(readyStub("https://upstream.invalid"), client, 20*time.Millisecond, time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
	rec := newDeadlineRecorder()

	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "raw-prefix" {
		t.Errorf("body-timeout response = status %d body %q, want committed 200 raw-prefix only", rec.Code, rec.Body.String())
	}
	assertBodyCleanup(t, body, true)
}

func TestPassthroughWriteFailureCancelsAndClosesWithoutReplacement(t *testing.T) {
	body := &cancelAwareBody{chunks: [][]byte{[]byte("model-response-data-must-not-be-logged")}}
	client := bodyClient(body, http.Header{"Content-Type": {"application/json"}})
	f := New(readyStub("https://upstream.invalid"), client, time.Second, 37*time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
	w := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("client stopped reading")}
	started := time.Now()

	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(w, httptest.NewRequest(http.MethodGet, "/models", nil))

	if w.status != http.StatusOK || len(w.written) != 0 {
		t.Errorf("write-failure response = status %d body %q, want committed 200 with no replacement", w.status, w.written)
	}
	if len(w.deadlines) != 1 {
		t.Errorf("write deadline calls = %d, want one configured per-write deadline", len(w.deadlines))
	} else if got := w.deadlines[0].Sub(started); got < 36*time.Second || got > 38*time.Second {
		t.Errorf("write deadline offset = %v, want configured 37s", got)
	}
	assertBodyCleanup(t, body, true)
}

func TestPassthroughClientCancelStopsCopyAndReleasesBody(t *testing.T) {
	prefixRead := make(chan struct{})
	body := &cancelAwareBody{chunks: [][]byte{[]byte("client-visible-prefix")}, blockAfterChunks: true, firstRead: prefixRead}
	client := bodyClient(body, http.Header{"Content-Type": {"application/json"}})
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Nanosecond, time.Nanosecond, 1, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/models", nil).WithContext(ctx)
	rec := newDeadlineRecorder()
	done := make(chan struct{})
	go func() {
		f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, req)
		close(done)
	}()

	select {
	case <-prefixRead:
	case <-time.After(time.Second):
		t.Fatal("upstream prefix was not read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("passthrough handler did not return after client cancellation")
	}

	if rec.Code != http.StatusOK || rec.Body.String() != "client-visible-prefix" {
		t.Errorf("client-cancel response = status %d body %q, want raw prefix only", rec.Code, rec.Body.String())
	}
	assertBodyCleanup(t, body, true)
}

func TestPassthroughSSELookingBodyDoesNotUseSSETimers(t *testing.T) {
	const raw = "data: {\"unknown\":true}\n\nnot even a complete SSE frame"
	body := &cancelAwareBody{chunks: [][]byte{[]byte(raw)}, delay: 30 * time.Millisecond}
	client := bodyClient(body, http.Header{"Content-Type": {"text/event-stream"}})
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Millisecond, time.Millisecond, 1, 1, nil)
	rec := newDeadlineRecorder()

	f.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)(rec, httptest.NewRequest(http.MethodGet, "/models", nil))

	if rec.Body.String() != raw {
		t.Errorf("SSE-looking raw body = %q, want byte-exact %q", rec.Body.String(), raw)
	}
	assertBodyClosed(t, body)
}

type cancelAwareBody struct {
	mu               sync.Mutex
	ctx              context.Context
	chunks           [][]byte
	terminal         error
	blockAfterChunks bool
	delay            time.Duration
	firstRead        chan struct{}
	firstReadOnce    sync.Once
	closed           bool
	canceledAtClose  bool
}

type countingProvider struct {
	cred  identity.Credential
	calls int
}

func (p *countingProvider) Current(context.Context) (identity.Credential, error) {
	p.calls++
	return p.cred, nil
}

func (*countingProvider) Ready() bool { return true }

func (b *cancelAwareBody) Read(p []byte) (int, error) {
	if b.delay > 0 {
		time.Sleep(b.delay)
		b.delay = 0
	}
	b.mu.Lock()
	if len(b.chunks) > 0 {
		chunk := b.chunks[0]
		b.chunks = b.chunks[1:]
		b.mu.Unlock()
		n := copy(p, chunk)
		if n < len(chunk) {
			b.mu.Lock()
			b.chunks = append([][]byte{append([]byte(nil), chunk[n:]...)}, b.chunks...)
			b.mu.Unlock()
		}
		if b.firstRead != nil {
			b.firstReadOnce.Do(func() { close(b.firstRead) })
		}
		return n, nil
	}
	ctx := b.ctx
	block := b.blockAfterChunks
	terminal := b.terminal
	b.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if terminal != nil {
		return 0, terminal
	}
	return 0, io.EOF
}

func (b *cancelAwareBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.canceledAtClose = b.ctx != nil && b.ctx.Err() != nil
	return nil
}

func bodyClient(body *cancelAwareBody, header http.Header) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body.mu.Lock()
		body.ctx = r.Context()
		body.mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body, Request: r}, nil
	})}
}

func assertBodyCleanup(t *testing.T, body *cancelAwareBody, wantCanceledAtClose bool) {
	t.Helper()
	assertBodyClosed(t, body)
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.canceledAtClose != wantCanceledAtClose {
		t.Errorf("outbound context canceled at body close = %t, want %t", body.canceledAtClose, wantCanceledAtClose)
	}
}

func assertBodyClosed(t *testing.T, body *cancelAwareBody) {
	t.Helper()
	body.mu.Lock()
	defer body.mu.Unlock()
	if !body.closed {
		t.Error("upstream response body was not closed")
	}
}

type failingResponseWriter struct {
	header    http.Header
	status    int
	written   []byte
	writeErr  error
	deadlines []time.Time
}

func (w *failingResponseWriter) Header() http.Header { return w.header }

func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.written = append(w.written, p...)
	return len(p), nil
}

func (w *failingResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
