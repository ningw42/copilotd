package wsforward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestProxyCorrelatesRejectedHandshakeWarningAndSummary(t *testing.T) {
	const requestID = "copilotd-handshake-rejected"
	for _, tc := range []struct {
		name              string
		upstreamRequestID string
		wantCorrelation   bool
	}{
		{name: "different", upstreamRequestID: "upstream-handshake-rejected", wantCorrelation: true},
		{name: "identical", upstreamRequestID: requestID},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.upstreamRequestID != "" {
					w.Header().Set(upstream.RequestIDHeader, tc.upstreamRequestID)
				}
				http.Error(w, "upstream handshake rejected", http.StatusForbidden)
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
			proxy := New(newTestCaller(provider, logging.ForComponent(base, "internal/upstream")),
				upstreamServer.Client(), time.Second, time.Second, 1<<20, nil,
				logging.ForComponent(base, "internal/wsforward"), logging.ForComponent(base, "internal/shim"), 0, WsMetrics{})
			t.Cleanup(func() { shutdownPreupgradeTestProxy(t, proxy) })

			request := validUpgradeRequest()
			ctx, summary := requestsummary.Begin(logging.WithRequestID(request.Context(), requestID), telemetryStreamObserver{})
			recorder := httptest.NewRecorder()
			proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, request.WithContext(ctx))
			if recorder.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want 502 before any downstream 101", recorder.Code)
			}
			publication := summary.Finish(requestsummary.ResponseResult{Method: request.Method, Status: recorder.Code})
			logging.ForComponent(base, "internal/server").LogAttrs(publication.Context, publication.Level, "access", publication.Attrs...)

			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(lines) != 2 {
				t.Fatalf("log records = %d, want one failure warning and one summary: %s", len(lines), logs.String())
			}
			for i, component := range []string{"internal/upstream", "internal/server"} {
				var record map[string]any
				if err := json.Unmarshal(lines[i], &record); err != nil {
					t.Fatalf("decode %s record: %v: %s", component, err, lines[i])
				}
				for key, want := range map[string]string{
					"level":      "WARN",
					"component":  component,
					"request_id": requestID,
				} {
					if got := record[key]; got != want {
						t.Errorf("%s record %s = %v, want %q", component, key, got, want)
					}
				}
				gotID, present := record["upstream_request_id"]
				if present != tc.wantCorrelation || (present && gotID != tc.upstreamRequestID) {
					t.Errorf("%s record upstream_request_id = %v (present %t), want %q (present %t)", component, gotID, present, tc.upstreamRequestID, tc.wantCorrelation)
				}
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
