package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/wsforward"
)

type synchronizedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func websocketTelemetryLogger(t *testing.T) (*slog.Logger, *synchronizedLogBuffer) {
	t.Helper()
	buffer := &synchronizedLogBuffer{}
	logger, err := logging.NewWithWriter(buffer, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	return logger, buffer
}

func TestWebSocketTelemetryEmitsEstablishmentSessionAndAccessRecords(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			messageType, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), messageType, payload); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	provider := readyStub(upstream.URL)
	logger, logs := websocketTelemetryLogger(t)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	accepts := NewWsAcceptCounter()
	terminals := NewWsSessionTerminalCounter()
	proxy := wsforward.New(provider, http.DefaultClient, time.Second, time.Second, 1<<20, logger, wsforward.WsMetrics{
		Accept:          accepts,
		SessionTerminal: terminals,
	})
	base := startServer(t, New(testConfig(), logger, provider, forwarder, proxy, NewStreamOutcomeCounter()))

	clientURL := "ws" + strings.TrimPrefix(base, "http") + "/openai/v1/responses"
	client, response, err := websocket.Dial(context.Background(), clientURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + testAPIKey},
			"X-Request-Id":  {"ws-telemetry-request"},
		},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()
	waitForWsLog(t, logs, `msg="websocket established"`)
	established := logs.String()
	for _, want := range []string{
		`msg="websocket established"`,
		"method=GET",
		`route="GET /openai/v1/responses"`,
		"status=101",
		"bytes=0",
		"ws=true",
		"duration=",
		"request_id=ws-telemetry-request",
	} {
		if !strings.Contains(established, want) {
			t.Errorf("established telemetry missing %q before close:\n%s", want, established)
		}
	}
	for _, closeTimeRecord := range []string{"msg=access", `msg="websocket session"`} {
		if strings.Contains(established, closeTimeRecord) {
			t.Errorf("close-time record %q emitted while WebSocket remains open:\n%s", closeTimeRecord, established)
		}
	}
	payload := []byte("private-session-payload")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := client.Write(ctx, websocket.MessageText, payload); err != nil {
		cancel()
		t.Fatalf("write downstream message: %v", err)
	}
	if _, got, err := client.Read(ctx); err != nil {
		cancel()
		t.Fatalf("read downstream message: %v", err)
	} else if string(got) != string(payload) {
		t.Errorf("echoed payload = %q, want %q", got, payload)
	}
	cancel()
	if err := client.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close downstream client: %v", err)
	}
	waitForWsCount(t, func() uint64 { return terminals.Count(wsforward.SessionClientClosed) }, 1)

	if got := accepts.Count(wsforward.AcceptEstablished); got != 1 {
		t.Errorf("established count = %d, want 1", got)
	}
	if got := terminals.Count(wsforward.SessionClientClosed); got != 1 {
		t.Errorf("client_closed count = %d, want 1", got)
	}
	out := logs.String()
	if got := strings.Count(out, "msg=access"); got != 1 {
		t.Fatalf("access lines = %d, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, `msg="websocket session"`); got != 1 {
		t.Fatalf("session lines = %d, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, `msg="websocket established"`); got != 1 {
		t.Fatalf("established lines = %d, want 1:\n%s", got, out)
	}
	for _, want := range []string{
		`route="GET /openai/v1/responses"`,
		"status=101",
		"bytes=0",
		"ws=true",
		"request_id=ws-telemetry-request",
		"msgs_c2u=1",
		"msgs_u2c=1",
		"bytes_c2u=23",
		"bytes_u2c=23",
		"close_code=1000",
		"terminal_reason=client_closed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("correlated telemetry missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "request_id=ws-telemetry-request"); got != 3 {
		t.Errorf("correlated request_id occurrences = %d, want 3:\n%s", got, out)
	}
	for _, secret := range []string{"private-session-payload", "copilot-token", testAPIKey} {
		if strings.Contains(out, secret) {
			t.Errorf("telemetry leaked %q:\n%s", secret, out)
		}
	}
}

func TestWebSocketPreUpgradeFailureEmitsOnlyAccessRecord(t *testing.T) {
	provider := readyStub("http://unused.invalid")
	logger, logs := websocketTelemetryLogger(t)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	accepts := NewWsAcceptCounter()
	terminals := NewWsSessionTerminalCounter()
	proxy := wsforward.New(provider, http.DefaultClient, time.Second, time.Second, 1<<20, logger, wsforward.WsMetrics{
		Accept:          accepts,
		SessionTerminal: terminals,
	})
	base := startServer(t, New(testConfig(), logger, provider, forwarder, proxy, NewStreamOutcomeCounter()))

	request, err := http.NewRequest(http.MethodGet, base+"/openai/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	request.Header.Set("X-Request-Id", "ws-rejected-request")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request non-upgrade WebSocket route: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.ReadAll(response.Body)

	if response.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("status = %d, want 426", response.StatusCode)
	}
	if got := accepts.Count(wsforward.AcceptRejected); got != 1 {
		t.Errorf("rejected count = %d, want 1", got)
	}
	for _, terminal := range []wsforward.SessionTerminal{
		wsforward.SessionClientClosed,
		wsforward.SessionUpstreamClosed,
		wsforward.SessionError,
	} {
		if got := terminals.Count(terminal); got != 0 {
			t.Errorf("terminal %q count = %d, want 0", terminal, got)
		}
	}
	out := logs.String()
	if got := strings.Count(out, "msg=access"); got != 1 {
		t.Fatalf("access lines = %d, want 1:\n%s", got, out)
	}
	for _, establishedOnlyRecord := range []string{`msg="websocket established"`, `msg="websocket session"`} {
		if strings.Contains(out, establishedOnlyRecord) {
			t.Fatalf("pre-upgrade failure emitted %q:\n%s", establishedOnlyRecord, out)
		}
	}
	for _, want := range []string{
		`route="GET /openai/v1/responses"`,
		"status=426",
		"ws=true",
		"request_id=ws-rejected-request",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handshake telemetry missing %q:\n%s", want, out)
		}
	}
}

type panicOnEstablished struct{}

func (panicOnEstablished) ObserveAccept(outcome wsforward.AcceptOutcome) {
	if outcome == wsforward.AcceptEstablished {
		panic("injected post-upgrade observer panic")
	}
}

func TestAssembledServerRecoversPostUpgradeObserverPanicAndClosesBothSockets(t *testing.T) {
	upstreamClosed := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			upstreamClosed <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(context.Background())
		upstreamClosed <- err
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "private-copilot-token",
	}, true)
	logger, logs := websocketTelemetryLogger(t)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	proxy := wsforward.New(provider, http.DefaultClient, time.Second, time.Second, 1<<20, logger, wsforward.WsMetrics{
		Accept: panicOnEstablished{},
	})
	base := startServer(t, New(testConfig(), logger, provider, forwarder, proxy, NewStreamOutcomeCounter()))

	clientURL := "ws" + strings.TrimPrefix(base, "http") + "/openai/v1/responses"
	client, response, err := websocket.Dial(context.Background(), clientURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + testAPIKey},
			"X-Request-Id":  {"ws-panic-request"},
		},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("downstream socket remained open after observer panic")
	}
	select {
	case err := <-upstreamClosed:
		if err == nil {
			t.Fatal("upstream socket remained open after observer panic")
		}
	case <-ctx.Done():
		t.Fatal("upstream socket was not closed after observer panic")
	}

	deadline := time.Now().Add(time.Second)
	for (!strings.Contains(logs.String(), "panic recovered") || !strings.Contains(logs.String(), "status=101")) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="panic recovered"`,
		"injected post-upgrade observer panic",
		"request_id=ws-panic-request",
		"status=101",
		"ws=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("recovered panic telemetry missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "private-copilot-token") {
		t.Errorf("panic telemetry leaked Copilot token:\n%s", out)
	}
}

func waitForWsCount(t *testing.T, count func() uint64, want uint64) {
	t.Helper()
	waitForWsCondition(t, func() bool { return count() == want })
}

func waitForWsLog(t *testing.T, logs *synchronizedLogBuffer, want string) {
	t.Helper()
	waitForWsCondition(t, func() bool { return strings.Contains(logs.String(), want) })
}

func waitForWsCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}
