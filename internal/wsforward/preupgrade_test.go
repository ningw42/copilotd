package wsforward

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/upstream"
)

func TestProxyRejectsInvalidUpgradeBeforeCredentialOrDial(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		http.Error(w, "upstream handshake rejected", http.StatusBadRequest)
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
	}, true)
	provider.SetError(errors.New("credential resolution must not run"))
	proxy := newPreupgradeTestProxy(provider, http.DefaultClient, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	tests := []struct {
		name    string
		request func() *http.Request
	}{
		{
			name: "plain GET",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
			},
		},
		{
			name: "empty WebSocket key",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Header.Set("Sec-WebSocket-Key", " \t")
				return request
			},
		},
		{
			name: "missing Connection upgrade token",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Header.Del("Connection")
				return request
			},
		},
		{
			name: "HTTP before 1.1",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Proto = "HTTP/1.0"
				request.ProtoMajor = 1
				request.ProtoMinor = 0
				return request
			},
		},
		{
			name: "invalid WebSocket key",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Header.Set("Sec-WebSocket-Key", "not-base64")
				return request
			},
		},
		{
			name: "multiple WebSocket keys",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Header.Add("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
				return request
			},
		},
		{
			name: "unsupported WebSocket version",
			request: func() *http.Request {
				request := validUpgradeRequest()
				request.Header.Set("Sec-WebSocket-Version", "12")
				return request
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, test.request())

			if recorder.Code != http.StatusUpgradeRequired {
				t.Errorf("status = %d, want 426", recorder.Code)
			}
			const wantBody = `{"error":{"message":"request is not a WebSocket upgrade","type":"invalid_request_error","code":null,"param":null}}`
			if got := recorder.Body.String(); got != wantBody {
				t.Errorf("body = %q, want %q", got, wantBody)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
	if got := upstreamRequests.Load(); got != 0 {
		t.Errorf("upstream handshakes = %d, want 0", got)
	}
}

func TestProxyReturnsNotReadyForTokenWiseUpgradeWhenCredentialResolutionFails(t *testing.T) {
	provider := identity.NewStatic(identity.Credential{}, true)
	provider.SetError(errors.New("credential failure with secret details"))
	proxy := newPreupgradeTestProxy(provider, http.DefaultClient, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	request := validUpgradeRequest()
	request.Header.Set("Upgrade", "h2c, WebSocket")
	recorder := httptest.NewRecorder()
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
	const wantBody = `{"error":{"message":"no upstream credential available","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Errorf("credential error details leaked in body: %q", recorder.Body.String())
	}
}

func TestProxyReturnsBadGatewayBeforeAcceptWhenUpstreamDialIsRefused(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused with secret details")
	})}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true)
	proxy := newPreupgradeTestProxy(provider, client, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	recorder := httptest.NewRecorder()
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest())

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 before any downstream 101", recorder.Code)
	}
	const wantBody = `{"error":{"message":"could not reach the upstream","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Errorf("upstream dial details leaked in body: %q", recorder.Body.String())
	}
}

func TestProxyReturnsGatewayTimeoutBeforeAcceptWhenUpstreamDialTimesOut(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true)
	proxy := newPreupgradeTestProxy(provider, client, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	recorder := httptest.NewRecorder()
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest())

	if recorder.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 before any downstream 101", recorder.Code)
	}
	const wantBody = `{"error":{"message":"the upstream request timed out","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

func TestProxyRelaysUpstreamHandshakeRejection(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := `{"error":{"message":"copilot rejected the handshake","code":"` + strconv.Itoa(status) + `"}}`
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(upstreamServer.Close)

			recorder, accepts := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

			if recorder.Code != status {
				t.Errorf("status = %d, want relayed %d without a downstream 101", recorder.Code, status)
			}
			for name, want := range map[string]string{
				"Content-Type":   "application/json",
				"Retry-After":    "7",
				"Content-Length": strconv.Itoa(len(body)),
			} {
				if got := recorder.Header().Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if got := recorder.Body.String(); got != body {
				t.Errorf("body = %q, want Copilot's %q", got, body)
			}
			if len(accepts) != 1 || accepts[0] != AcceptDialFailed {
				t.Errorf("accept observations = %v, want [%s]", accepts, AcceptDialFailed)
			}
		})
	}
}

func TestProxyRelaysHandshakeRejectionHeadersThroughResponsePolicy(t *testing.T) {
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 429 Too Many Requests\r\n"+
		"Connection: X-Hop-Listed\r\n"+
		"X-Hop-Listed: connection-scoped\r\n"+
		"Keep-Alive: timeout=5\r\n"+
		"Proxy-Authenticate: Basic\r\n"+
		"X-Request-Id: upstream-rejection-id\r\n"+
		"Retry-After: 7\r\n"+
		"X-Copilot-Detail: end-to-end\r\n"+
		"Content-Length: 2\r\n"+
		"\r\n"+
		"{}")

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want relayed 429", recorder.Code)
	}
	for _, name := range []string{"Connection", "X-Hop-Listed", "Keep-Alive", "Proxy-Authenticate", "X-Request-Id"} {
		if got := recorder.Header().Values(name); len(got) != 0 {
			t.Errorf("%s = %v, want not relayed", name, got)
		}
	}
	for name, want := range map[string]string{"Retry-After": "7", "X-Copilot-Detail": "end-to-end"} {
		if got := recorder.Header().Get(name); got != want {
			t.Errorf("%s = %q, want relayed %q", name, got, want)
		}
	}
}

func TestProxyOmitsHandshakeRejectionBodyAboveRetentionCap(t *testing.T) {
	body := strings.Repeat("x", 2048)
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 429 Too Many Requests\r\n"+
		"Retry-After: 7\r\n"+
		"Content-Length: 2048\r\n"+
		"\r\n"+
		body)

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	assertRelayedWithoutBody(t, recorder, http.StatusTooManyRequests)
	if got := recorder.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want relayed 7", got)
	}
}

func TestProxyRelaysHandshakeRejectionBodyExactlyAtRetentionCap(t *testing.T) {
	body := strings.Repeat("y", 1024)
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 403 Forbidden\r\n"+
		"Content-Length: 1024\r\n"+
		"\r\n"+
		body)

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want relayed 403", recorder.Code)
	}
	if got := recorder.Body.String(); got != body {
		t.Errorf("body = %d bytes, want Copilot's complete 1024-byte body", len(got))
	}
	if got := recorder.Header().Get("Content-Length"); got != "1024" {
		t.Errorf("Content-Length = %q, want 1024", got)
	}
}

func TestProxyOmitsHandshakeRejectionBodyClosedBeforeDeclaredLength(t *testing.T) {
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 429 Too Many Requests\r\n"+
		"Content-Length: 100\r\n"+
		"\r\n"+
		"short!!")

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	assertRelayedWithoutBody(t, recorder, http.StatusTooManyRequests)
}

func TestProxyOmitsCompleteHandshakeRejectionBodyOfUnknownLength(t *testing.T) {
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 429 Too Many Requests\r\n"+
		"Transfer-Encoding: chunked\r\n"+
		"\r\n"+
		"7\r\n{\"a\":1}\r\n"+
		"0\r\n\r\n")

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	// Complete, but nothing proves it: the omission is deliberate.
	assertRelayedWithoutBody(t, recorder, http.StatusTooManyRequests)
}

func TestProxyOmitsIncompleteHandshakeRejectionBodyOfUnknownLength(t *testing.T) {
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 429 Too Many Requests\r\n"+
		"Transfer-Encoding: chunked\r\n"+
		"\r\n"+
		"7\r\n{\"a\":1}\r\n")

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	assertRelayedWithoutBody(t, recorder, http.StatusTooManyRequests)
}

func TestProxyOmitsHandshakeRejectionBodyDecompressedByDialTransport(t *testing.T) {
	var encoded bytes.Buffer
	gzipWriter := gzip.NewWriter(&encoded)
	_, _ = io.WriteString(gzipWriter, `{"error":{"message":"rate limited"}}`)
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip rejection body: %v", err)
	}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !headerContainsToken(r.Header, "Accept-Encoding", "gzip") {
			t.Errorf("handshake Accept-Encoding = %q, want the dial transport's automatic gzip", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(encoded.Len()))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(encoded.Bytes())
	}))
	t.Cleanup(upstreamServer.Close)

	recorder, _ := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	assertRelayedWithoutBody(t, recorder, http.StatusTooManyRequests)
	if got := recorder.Header().Values("Content-Encoding"); len(got) != 0 {
		t.Errorf("Content-Encoding = %v, want none for the transport-decoded rejection", got)
	}
}

func TestProxyTimesOutHandshakeRejectionWhoseBodyStallsPastDialDeadline(t *testing.T) {
	recorder, accepts := serveUpgradeAgainst(t, "http://upstream.invalid", stalledRejectionClient(make(chan struct{})), 20*time.Millisecond)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504: the deadline wins over the rejection", recorder.Code)
	}
	const wantBody = `{"error":{"message":"the upstream request timed out","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if len(accepts) != 1 || accepts[0] != AcceptDialFailed {
		t.Errorf("accept observations = %v, want [%s]", accepts, AcceptDialFailed)
	}
}

func TestProxyWritesNothingWhenClientLeavesDuringHandshakeRejectionBody(t *testing.T) {
	bodyEntered := make(chan struct{})
	provider := identity.NewStatic(identity.Credential{BaseURL: "http://upstream.invalid", Token: "copilot-token"}, true)
	observed := &recordingWsMetrics{}
	proxy := newAdmissionTestProxy(provider, stalledRejectionClient(bodyEntered), WsMetrics{Accept: observed, SessionTerminal: observed})
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	recorder := httptest.NewRecorder()
	recorder.Code = 0 // distinguish untouched from an explicit WriteHeader(200)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest().WithContext(requestCtx))
	}()
	select {
	case <-bodyEntered:
	case <-time.After(time.Second):
		t.Fatal("dial did not start reading the rejection body")
	}

	cancelRequest()

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after the client left mid-rejection")
	}
	assertNoPreUpgradeResponse(t, recorder, observed)
}

func TestProxyTimesOutDeadlineBearingDialErrorAfterClientLeaves(t *testing.T) {
	requestEntered := make(chan struct{})
	dialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestEntered)
		<-request.Context().Done()
		return nil, fmt.Errorf("upstream handshake: %w", context.DeadlineExceeded)
	})}
	provider := identity.NewStatic(identity.Credential{BaseURL: "http://upstream.invalid", Token: "copilot-token"}, true)
	observed := &recordingWsMetrics{}
	proxy := newAdmissionTestProxy(provider, dialClient, WsMetrics{Accept: observed, SessionTerminal: observed})
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest().WithContext(requestCtx))
	}()
	select {
	case <-requestEntered:
	case <-time.After(time.Second):
		t.Fatal("upstream handshake did not start")
	}

	cancelRequest()

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after the client left")
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504: a deadline-bearing error wins over cancellation", recorder.Code)
	}
	if accepts, _ := observed.snapshot(); len(accepts) != 1 || accepts[0] != AcceptDialFailed {
		t.Errorf("accept observations = %v, want [%s]", accepts, AcceptDialFailed)
	}
}

func TestProxyReturnsBadGatewayWhenUpstream101FailsHandshakeVerification(t *testing.T) {
	upstreamServer := rawHandshakeUpstream(t, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Connection: Upgrade\r\n"+
		"Upgrade: websocket\r\n"+
		"Sec-WebSocket-Accept: not-the-expected-accept\r\n"+
		"\r\n")

	recorder, accepts := serveUpgradeAgainst(t, upstreamServer.URL, upstreamServer.Client(), time.Second)

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a 101 that fails verification", recorder.Code)
	}
	const wantBody = `{"error":{"message":"could not reach the upstream","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
	if len(accepts) != 1 || accepts[0] != AcceptDialFailed {
		t.Errorf("accept observations = %v, want [%s]", accepts, AcceptDialFailed)
	}
}

func TestProxyLogsRelayedHandshakeRejectionOnlyInCorrelatedAccessRecord(t *testing.T) {
	const requestID = "copilotd-handshake-rejected"
	for _, tc := range []struct {
		name              string
		status            int
		upstreamRequestID string
		wantLevel         string
		wantCorrelation   bool
	}{
		{name: "different id", status: http.StatusForbidden, upstreamRequestID: "upstream-handshake-rejected", wantLevel: "INFO", wantCorrelation: true},
		{name: "identical id", status: http.StatusForbidden, upstreamRequestID: requestID, wantLevel: "INFO"},
		{name: "absent id", status: http.StatusForbidden, wantLevel: "INFO"},
		{name: "server error status", status: http.StatusServiceUnavailable, upstreamRequestID: "upstream-handshake-unavailable", wantLevel: "WARN", wantCorrelation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.upstreamRequestID != "" {
					w.Header().Set(upstream.RequestIDHeader, tc.upstreamRequestID)
				}
				http.Error(w, "upstream handshake rejected", tc.status)
			}))
			t.Cleanup(upstreamServer.Close)

			var logs bytes.Buffer
			base, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "json"})
			if err != nil {
				t.Fatalf("build logger: %v", err)
			}
			provider := identity.NewStatic(identity.Credential{
				BaseURL: upstreamServer.URL,
				Token:   "copilot-token",
			}, true)
			observed := &recordingWsMetrics{}
			proxy := New(newTestCaller(provider, logging.ForComponent(base, "internal/upstream")),
				upstreamServer.Client(), time.Second, time.Second, 1<<20, nil,
				logging.ForComponent(base, "internal/wsforward"), logging.ForComponent(base, "internal/shim"), 0,
				WsMetrics{Accept: observed, SessionTerminal: observed})
			t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

			request := validUpgradeRequest()
			ctx, summary := requestsummary.Begin(logging.WithRequestID(request.Context(), requestID), telemetryStreamObserver{})
			recorder := httptest.NewRecorder()
			proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, request.WithContext(ctx))
			if recorder.Code != tc.status {
				t.Errorf("status = %d, want relayed %d", recorder.Code, tc.status)
			}
			if accepts, _ := observed.snapshot(); len(accepts) != 1 || accepts[0] != AcceptDialFailed {
				t.Errorf("accept observations = %v, want [%s]", accepts, AcceptDialFailed)
			}
			publication := summary.Finish(requestsummary.ResponseResult{Method: request.Method, Status: recorder.Code})
			logging.ForComponent(base, "internal/server").LogAttrs(publication.Context, publication.Level, "access", publication.Attrs...)

			// A relayed rejection is an ordinary upstream answer: no failure
			// record, only the access record.
			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(lines) != 1 {
				t.Fatalf("log records = %d, want only the access record: %s", len(lines), logs.String())
			}
			var record map[string]any
			if err := json.Unmarshal(lines[0], &record); err != nil {
				t.Fatalf("decode access record: %v: %s", err, lines[0])
			}
			for key, want := range map[string]any{
				"level":      tc.wantLevel,
				"component":  "internal/server",
				"request_id": requestID,
				"status":     float64(tc.status),
			} {
				if got := record[key]; got != want {
					t.Errorf("access record %s = %v, want %v", key, got, want)
				}
			}
			gotID, present := record["upstream_request_id"]
			if present != tc.wantCorrelation || (present && gotID != tc.upstreamRequestID) {
				t.Errorf("access record upstream_request_id = %v (present %t), want %q (present %t)", gotID, present, tc.upstreamRequestID, tc.wantCorrelation)
			}
		})
	}
}

func TestProxyLogsUpstreamRequestIDFromSuccessfulHandshake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "upstream-handshake-123")
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = connection.CloseNow() }()
		_, _, _ = connection.Read(r.Context())
	}))
	t.Cleanup(upstream.Close)

	var logOutput bytes.Buffer
	logger, err := logging.NewWithWriter(&logOutput, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token-secret",
	}, true)
	proxy := newPreupgradeTestProxy(provider, http.DefaultClient, time.Second, logger)
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "downstream-request-456")
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(downstream.Close)

	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
	connection, response, err := websocket.Dial(context.Background(), clientURL, nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	_ = connection.Close(websocket.StatusNormalClosure, "done")
	shutdownPreupgradeTestProxy(t, proxy)

	output := logOutput.String()
	for _, want := range []string{
		`msg="websocket established"`,
		"request_id=downstream-request-456",
		"upstream_request_id=upstream-handshake-123",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("log output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "copilot-token-secret") {
		t.Errorf("Copilot token leaked in logs:\n%s", output)
	}
}

// serveUpgradeAgainst serves one valid upgrade through a proxy whose dial client
// reaches baseURL, and returns the downstream response and accept outcomes.
func serveUpgradeAgainst(t *testing.T, baseURL string, client *http.Client, dialTimeout time.Duration) (*httptest.ResponseRecorder, []AcceptOutcome) {
	t.Helper()
	provider := identity.NewStatic(identity.Credential{BaseURL: baseURL, Token: "copilot-token"}, true)
	observed := &recordingWsMetrics{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(newTestCaller(provider, logger), client, dialTimeout, time.Second, 1<<20, nil, logger, logger, 0,
		WsMetrics{Accept: observed, SessionTerminal: observed})
	t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

	recorder := httptest.NewRecorder()
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, validUpgradeRequest())
	accepts, terminals := observed.snapshot()
	if len(terminals) != 0 {
		t.Errorf("pre-upgrade terminal observations = %v, want none", terminals)
	}
	return recorder, accepts
}

// assertRelayedWithoutBody checks a relayed rejection whose body was omitted:
// Copilot's status, no body, and a Content-Length describing that empty body
// rather than Copilot's declared length.
func assertRelayedWithoutBody(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Errorf("status = %d, want relayed %d", recorder.Code, status)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Errorf("body bytes = %d, want omitted body: %q", got, recorder.Body.String())
	}
	if got := recorder.Header().Values("Content-Length"); len(got) != 1 || got[0] != "0" {
		t.Errorf("Content-Length = %v, want [0] for the omitted body", got)
	}
}

// stalledRejectionClient answers the handshake with a final 429 whose body
// read signals bodyEntered, then blocks until the dial request's context ends.
func stalledRejectionClient(bodyEntered chan struct{}) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusTooManyRequests,
			Header:        http.Header{"Retry-After": {"7"}, "Content-Length": {"100"}},
			ContentLength: 100,
			Body:          &stalledBody{ctx: request.Context(), entered: bodyEntered},
			Request:       request,
		}, nil
	})}
}

type stalledBody struct {
	ctx     context.Context
	entered chan struct{}
	once    sync.Once
}

func (b *stalledBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, io.ErrUnexpectedEOF
}

func (*stalledBody) Close() error { return nil }

// rawHandshakeUpstream answers every handshake with raw bytes, then closes the
// connection, so a test controls framing that net/http would otherwise own.
func rawHandshakeUpstream(t *testing.T, raw string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, readWriter, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack upstream handshake: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = readWriter.WriteString(raw)
		_ = readWriter.Flush()
	}))
	t.Cleanup(server.Close)
	return server
}

func newPreupgradeTestProxy(provider identity.Provider, client *http.Client, dialTimeout time.Duration, logger *slog.Logger) *Proxy {
	return New(newTestCaller(provider, logger), client, dialTimeout, time.Second, 1<<20, nil, logger, logger, 0, WsMetrics{})
}

func shutdownPreupgradeTestProxy(t *testing.T, proxy *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Errorf("shutdown proxy: %v", err)
	}
}

func validUpgradeRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return request
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
