package wsforward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
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
				nil,
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
			proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, test.request)

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

func TestProxyDroppedClientMessageIsNotForwardedOrCounted(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	registry := shim.Registry{{
		Name:    "drop-client-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				return string(message.Data) != "drop", nil
			})
		},
	}}
	received := make(chan string, 1)
	client, handlerDone, cleanup := startTelemetrySessionWithRegistry(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		1<<20,
		registry,
		func(conn *websocket.Conn) {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			received <- string(data)
			_ = conn.Write(context.Background(), kind, []byte("ack"))
			_, _, _ = conn.Read(context.Background())
		},
	)
	defer cleanup()

	for _, data := range []string{"drop", "keep"} {
		if err := client.Write(context.Background(), websocket.MessageText, []byte(data)); err != nil {
			t.Fatalf("write %q: %v", data, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := client.Read(ctx); err != nil {
		t.Fatalf("read upstream acknowledgment: %v", err)
	}
	select {
	case got := <-received:
		if got != "keep" {
			t.Errorf("upstream message = %q, want keep", got)
		}
	case <-ctx.Done():
		t.Fatal("kept message was not forwarded")
	}
	if err := client.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close client: %v", err)
	}
	waitForHandler(t, handlerDone)

	for _, want := range []string{"msgs_c2u=1", "bytes_c2u=4"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("session log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestProxyDroppedServerMessageIsNotForwardedOrCounted(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	registry := shim.Registry{{
		Name:    "drop-server-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				return string(message.Data) != "drop", nil
			})
		},
	}}
	client, handlerDone, cleanup := startTelemetrySessionWithRegistry(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		1<<20,
		registry,
		func(conn *websocket.Conn) {
			_ = conn.Write(context.Background(), websocket.MessageText, []byte("drop"))
			_ = conn.Write(context.Background(), websocket.MessageText, []byte("keep"))
			_, _, _ = conn.Read(context.Background())
		},
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read kept server message: %v", err)
	}
	if string(data) != "keep" {
		t.Errorf("server message = %q, want keep", data)
	}
	if err := client.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close client: %v", err)
	}
	waitForHandler(t, handlerDone)

	for _, want := range []string{"msgs_u2c=1", "bytes_u2c=4"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("session log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestProxyClientTransformErrorClosesBothPeersWith1011AndRecordsErrorTerminal(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	registry := shim.Registry{{
		Name:    "failing-client-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				return false, errors.New("transform failed")
			})
		},
	}}
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, handlerDone, cleanup := startTelemetrySessionWithRegistry(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		1<<20,
		registry,
		func(conn *websocket.Conn) {
			_, _, err := conn.Read(context.Background())
			upstreamClosed <- websocket.CloseStatus(err)
		},
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte("trigger")); err != nil {
		t.Fatalf("write trigger message: %v", err)
	}
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Errorf("client close status = %v, want 1011 (err: %v)", got, err)
		_ = client.CloseNow()
	}
	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusInternalError {
			t.Errorf("upstream close status = %v, want 1011", got)
		}
	case <-time.After(time.Second):
		t.Error("sibling upstream pump was not torn down")
	}
	waitForHandler(t, handlerDone)

	_, terminals := observed.snapshot()
	if len(terminals) != 1 || terminals[0] != SessionError {
		t.Errorf("terminal observations = %v, want [error]", terminals)
	}
	for _, want := range []string{"level=WARN", "msgs_c2u=0", "close_code=1011", "terminal_reason=error"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("session log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestProxyServerTransformErrorClosesBothPeersWith1011AndRecordsErrorTerminal(t *testing.T) {
	observed := &recordingWsMetrics{}
	var logs bytes.Buffer
	registry := shim.Registry{{
		Name:    "failing-server-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				return false, errors.New("transform failed")
			})
		},
	}}
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, handlerDone, cleanup := startTelemetrySessionWithRegistry(
		t,
		slog.New(slog.NewTextHandler(&logs, nil)),
		observed,
		1<<20,
		registry,
		func(conn *websocket.Conn) {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte("trigger")); err != nil {
				upstreamClosed <- websocket.CloseStatus(err)
				return
			}
			_, _, err := conn.Read(context.Background())
			upstreamClosed <- websocket.CloseStatus(err)
		},
	)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Errorf("client close status = %v, want 1011 (err: %v)", got, err)
		_ = client.CloseNow()
	}
	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusInternalError {
			t.Errorf("upstream close status = %v, want 1011", got)
		}
	case <-time.After(time.Second):
		t.Error("upstream peer did not receive the fatal transform close")
	}
	waitForHandler(t, handlerDone)

	_, terminals := observed.snapshot()
	if len(terminals) != 1 || terminals[0] != SessionError {
		t.Errorf("terminal observations = %v, want [error]", terminals)
	}
	for _, want := range []string{"level=WARN", "msgs_u2c=0", "bytes_u2c=0", "close_code=1011", "terminal_reason=error"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("session log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestProxyClientDisconnectRacingTransformErrorUsesValidTerminalAndTearsDown(t *testing.T) {
	observed := &recordingWsMetrics{}
	transformEntered := make(chan struct{})
	raceStart := make(chan struct{})
	registry := shim.Registry{{
		Name:    "racing-client-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				close(transformEntered)
				<-raceStart
				return false, errors.New("transform failed during disconnect")
			})
		},
	}}
	upstreamDone := make(chan struct{})
	client, handlerDone, cleanup := startTelemetrySessionWithRegistry(
		t,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		observed,
		1<<20,
		registry,
		func(conn *websocket.Conn) {
			defer close(upstreamDone)
			<-raceStart
			_ = conn.Write(context.Background(), websocket.MessageText, []byte("concurrent-server-message"))
			_, _, _ = conn.Read(context.Background())
		},
	)
	defer cleanup()

	if err := client.Write(context.Background(), websocket.MessageText, []byte("trigger")); err != nil {
		t.Fatalf("write trigger message: %v", err)
	}
	select {
	case <-transformEntered:
	case <-time.After(time.Second):
		t.Fatal("transform did not start")
	}
	if err := client.CloseNow(); err != nil {
		t.Fatalf("disconnect client: %v", err)
	}
	close(raceStart)
	waitForHandler(t, handlerDone)
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Error("upstream peer did not unwind after race")
	}

	_, terminals := observed.snapshot()
	if len(terminals) != 1 || (terminals[0] != SessionClientClosed && terminals[0] != SessionError) {
		t.Errorf("terminal observations = %v, want [client_closed] or [error]", terminals)
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
	return startTelemetrySessionWithRegistry(t, logger, observed, maxMessageBytes, nil, serveUpstream)
}

func startTelemetrySessionWithRegistry(t *testing.T, logger *slog.Logger, observed *recordingWsMetrics, maxMessageBytes int64, registry shim.Registry, serveUpstream func(*websocket.Conn)) (*websocket.Conn, <-chan struct{}, func()) {
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
		registry,
		logger,
		WsMetrics{Accept: observed, SessionTerminal: observed},
	)
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "request-telemetry")
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r.WithContext(ctx))
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
