package wsforward

import (
	"bytes"
	"context"
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
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
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
			proxy.Handler().ServeHTTP(recorder, test.request())

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
	proxy.Handler().ServeHTTP(recorder, request)

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
	proxy.Handler().ServeHTTP(recorder, validUpgradeRequest())

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 before any downstream 101", recorder.Code)
	}
	const wantBody = `{"error":{"message":"could not reach the upstream WebSocket","type":"api_error","code":null,"param":null}}`
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
	proxy.Handler().ServeHTTP(recorder, validUpgradeRequest())

	if recorder.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504 before any downstream 101", recorder.Code)
	}
	const wantBody = `{"error":{"message":"the upstream WebSocket handshake timed out","type":"api_error","code":null,"param":null}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
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
		proxy.Handler().ServeHTTP(w, r.WithContext(ctx))
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
		`msg="upstream response correlation"`,
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
	return New(provider, client, dialTimeout, time.Second, 1<<20, logger, WsMetrics{})
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
