package wsforward

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

type recordingWsMetrics struct {
	mu        sync.Mutex
	accepts   []AcceptOutcome
	terminals []SessionTerminal
}

func (m *recordingWsMetrics) ObserveAccept(outcome AcceptOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accepts = append(m.accepts, outcome)
}

func (m *recordingWsMetrics) ObserveSessionTerminal(terminal SessionTerminal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.terminals = append(m.terminals, terminal)
}

func (m *recordingWsMetrics) snapshot() ([]AcceptOutcome, []SessionTerminal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AcceptOutcome(nil), m.accepts...), append([]SessionTerminal(nil), m.terminals...)
}

func TestProxyObservesOnePreUpgradeAcceptOutcome(t *testing.T) {
	unavailable := identity.NewStatic(identity.Credential{
		BaseURL: "http://unused.invalid",
		Token:   "private-copilot-token",
	}, true)
	unavailable.SetError(context.Canceled)
	tests := []struct {
		name        string
		provider    identity.Provider
		client      *http.Client
		dialTimeout time.Duration
		request     *http.Request
		wantStatus  int
		want        AcceptOutcome
	}{
		{
			name: "invalid upgrade is rejected",
			provider: identity.NewStatic(identity.Credential{
				BaseURL: "http://unused.invalid",
				Token:   "private-copilot-token",
			}, true),
			request:    httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil),
			wantStatus: http.StatusUpgradeRequired,
			want:       AcceptRejected,
		},
		{
			name:       "credential failure is rejected",
			provider:   unavailable,
			request:    validUpgradeRequest(),
			wantStatus: http.StatusServiceUnavailable,
			want:       AcceptRejected,
		},
		{
			name: "invalid upstream URL is a dial failure",
			provider: identity.NewStatic(identity.Credential{
				BaseURL: "://invalid",
				Token:   "private-copilot-token",
			}, true),
			request:    validUpgradeRequest(),
			wantStatus: http.StatusBadGateway,
			want:       AcceptDialFailed,
		},
		{
			name: "upstream timeout is a dial failure",
			provider: identity.NewStatic(identity.Credential{
				BaseURL: "http://upstream.invalid",
				Token:   "private-copilot-token",
			}, true),
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			})},
			dialTimeout: 5 * time.Millisecond,
			request:     validUpgradeRequest(),
			wantStatus:  http.StatusGatewayTimeout,
			want:        AcceptDialFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := test.client
			if client == nil {
				client = http.DefaultClient
			}
			dialTimeout := test.dialTimeout
			if dialTimeout == 0 {
				dialTimeout = time.Second
			}
			observed := &recordingWsMetrics{}
			proxy := New(
				test.provider,
				client,
				dialTimeout,
				time.Second,
				1<<20,
				slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
				WsMetrics{Accept: observed, SessionTerminal: observed},
			)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := proxy.Shutdown(ctx); err != nil {
					t.Errorf("shutdown proxy: %v", err)
				}
			})

			recorder := httptest.NewRecorder()
			proxy.Handler().ServeHTTP(recorder, test.request)

			if recorder.Code != test.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			accepts, terminals := observed.snapshot()
			if len(accepts) != 1 || accepts[0] != test.want {
				t.Errorf("accept observations = %v, want [%s]", accepts, test.want)
			}
			if len(terminals) != 0 {
				t.Errorf("pre-upgrade terminal observations = %v, want none", terminals)
			}
		})
	}
}

func TestProxyLogsClientClosedSessionWithDirectionalCounts(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client, handlerDone, cleanup := startTelemetrySession(t, logger, observed, 1<<20, func(conn *websocket.Conn) {
		for {
			messageType, payload, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if err := conn.Write(context.Background(), messageType, payload); err != nil {
				return
			}
		}
	})
	defer cleanup()

	messages := [][]byte{[]byte("private-payload"), {0x00, 0xff}}
	for index, payload := range messages {
		messageType := websocket.MessageText
		if index == 1 {
			messageType = websocket.MessageBinary
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := client.Write(ctx, messageType, payload); err != nil {
			cancel()
			t.Fatalf("write message: %v", err)
		}
		gotType, gotPayload, err := client.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read echoed message: %v", err)
		}
		if gotType != messageType || !bytes.Equal(gotPayload, payload) {
			t.Errorf("echo = (%v, %x), want (%v, %x)", gotType, gotPayload, messageType, payload)
		}
	}
	if err := client.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close client: %v", err)
	}
	waitForHandler(t, handlerDone)

	accepts, terminals := observed.snapshot()
	if len(accepts) != 1 || accepts[0] != AcceptEstablished {
		t.Errorf("accept observations = %v, want [established]", accepts)
	}
	if len(terminals) != 1 || terminals[0] != SessionClientClosed {
		t.Errorf("terminal observations = %v, want [client_closed]", terminals)
	}

	totalBytes := len(messages[0]) + len(messages[1])
	out := logs.String()
	for _, want := range []string{
		`msg="websocket session"`,
		"level=INFO",
		"request_id=request-telemetry",
		"msgs_c2u=2",
		"msgs_u2c=2",
		fmt.Sprintf("bytes_c2u=%d", totalBytes),
		fmt.Sprintf("bytes_u2c=%d", totalBytes),
		"close_code=1000",
		"terminal_reason=client_closed",
		"duration=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("session log missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"private-payload", "private-copilot-token"} {
		if strings.Contains(out, secret) {
			t.Errorf("session log leaked %q:\n%s", secret, out)
		}
	}
}

func TestProxyTreatsAbruptClientDisconnectAsCleanClientClosure(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, handlerDone, cleanup := startTelemetrySession(t, logger, observed, 1<<20, func(conn *websocket.Conn) {
		_, _, err := conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	})
	defer cleanup()

	if err := client.CloseNow(); err != nil {
		t.Fatalf("abruptly close client: %v", err)
	}
	waitForHandler(t, handlerDone)

	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusGoingAway {
			t.Errorf("upstream close status = %v, want 1001", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive client-disconnect close")
	}
	_, terminals := observed.snapshot()
	if len(terminals) != 1 || terminals[0] != SessionClientClosed {
		t.Errorf("terminal observations = %v, want [client_closed]", terminals)
	}
	for _, want := range []string{"level=INFO", "close_code=1001", "terminal_reason=client_closed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("session log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestProxyLogsUpstreamCloseAsCleanTerminal(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	client, handlerDone, cleanup := startTelemetrySession(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		1<<20,
		func(conn *websocket.Conn) { _ = conn.Close(4002, "upstream done") },
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != 4002 {
		t.Fatalf("client close status = %v, want 4002 (err: %v)", got, err)
	}
	waitForHandler(t, handlerDone)

	_, terminals := observed.snapshot()
	if len(terminals) != 1 || terminals[0] != SessionUpstreamClosed {
		t.Errorf("terminal observations = %v, want [upstream_closed]", terminals)
	}
	out := logs.String()
	for _, want := range []string{"level=INFO", "close_code=4002", "terminal_reason=upstream_closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("session log missing %q:\n%s", want, out)
		}
	}
}

func TestProxyLogsOversizeAsAbnormalTerminal(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	client, handlerDone, cleanup := startTelemetrySession(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		4,
		func(conn *websocket.Conn) { _, _, _ = conn.Read(context.Background()) },
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte("12345")); err != nil {
		t.Fatalf("write oversize message: %v", err)
	}
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("client close status = %v, want 1009 (err: %v)", got, err)
	}
	waitForHandler(t, handlerDone)

	_, terminals := observed.snapshot()
	if len(terminals) != 1 || terminals[0] != SessionError {
		t.Errorf("terminal observations = %v, want [error]", terminals)
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "close_code=1009", "terminal_reason=error"} {
		if !strings.Contains(out, want) {
			t.Errorf("session log missing %q:\n%s", want, out)
		}
	}
}

func startTelemetrySession(t *testing.T, logger *slog.Logger, observed *recordingWsMetrics, maxMessageBytes int64, serveUpstream func(*websocket.Conn)) (*websocket.Conn, <-chan struct{}, func()) {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		serveUpstream(conn)
	}))
	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "private-copilot-token",
	}, true)
	proxy := New(
		provider,
		http.DefaultClient,
		time.Second,
		time.Second,
		maxMessageBytes,
		logger,
		WsMetrics{Accept: observed, SessionTerminal: observed},
	)
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "request-telemetry")
		proxy.Handler().ServeHTTP(w, r.WithContext(ctx))
		close(handlerDone)
	}))
	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
	client, response, err := websocket.Dial(context.Background(), clientURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		downstream.Close()
		upstream.Close()
		t.Fatalf("dial downstream WebSocket: %v", err)
	}

	cleanup := func() {
		_ = client.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
		downstream.Close()
		upstream.Close()
	}
	return client, handlerDone, cleanup
}

func waitForHandler(t *testing.T, handlerDone <-chan struct{}) {
	t.Helper()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("WebSocket handler did not return")
	}
}
