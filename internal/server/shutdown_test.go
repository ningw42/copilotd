package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/wsforward"
)

// fixtureIOTimeout is the mixed-transport fixture's own watchdog over its
// synchronous SSE HTTP I/O (headers, frames, EOF). It is deliberately a fixed
// local bound, independent of the shutdown grace or stream idle timeouts under
// test, and comfortably beyond every drain observation window used here, so a
// broken forwarding or termination path fails the test instead of hanging it.
const fixtureIOTimeout = 15 * time.Second

// mixedTransportFixture runs a real copilotd Server on an ephemeral listener
// in front of an upstream that serves both a Responses WebSocket and a
// Responses SSE stream which stays open until releaseSSE closes.
type mixedTransportFixture struct {
	runErr     chan error
	cancel     context.CancelFunc
	releaseSSE func()
	ws         *websocket.Conn
	sseBody    io.ReadCloser
	sseReader  *bufio.Reader
	// sseWatchdog expires after fixtureIOTimeout, cancelling the SSE request
	// and closing its body so blocked reads return.
	sseWatchdog context.Context
}

// requireServerOriginatedSSEEnd fails if the SSE stream ended because the
// fixture's own watchdog cancelled it rather than because the server closed it.
func (f *mixedTransportFixture) requireServerOriginatedSSEEnd(t *testing.T, what string, err error) {
	t.Helper()
	if watchdogErr := f.sseWatchdog.Err(); watchdogErr != nil {
		t.Fatalf("%s ended by the fixture's %v SSE I/O watchdog (%v, read error %v), not by the server", what, fixtureIOTimeout, watchdogErr, err)
	}
}

func startMixedTransportFixture(t *testing.T, shutdownTimeout time.Duration) *mixedTransportFixture {
	t.Helper()
	releaseSSE := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSSE) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept upstream WebSocket: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for {
				messageType, payload, err := conn.Read(context.Background())
				if err != nil {
					return
				}
				if err := conn.Write(context.Background(), messageType, payload); err != nil {
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-releaseSSE:
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		release()
		upstream.Close()
	})

	provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true)
	logger := discardLogger(t)
	forwarder := newTestForwarder(provider, forward.NewClient(5*time.Second), 5*time.Second, 5*time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	wsProxy := wsforward.New(newTestWSCaller(provider, logger), http.DefaultClient, 5*time.Second, 5*time.Second, 1<<20, nil, logger, logger, 0, wsforward.WsMetrics{})
	cfg := testConfig()
	cfg.ShutdownTimeout = shutdownTimeout
	srv := newTestServerFromBase(cfg, logger, provider, newTestReadyObservers(), forwarder, newTestCatalogSource(provider), wsProxy, NewStreamOutcomeCounter(), catalog.RenderDescriptors{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &mixedTransportFixture{runErr: make(chan error, 1), cancel: cancel, releaseSSE: release}
	go func() { fixture.runErr <- srv.Run(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-fixture.runErr:
		case <-time.After(shutdownTimeout + 5*time.Second):
			t.Error("Run did not return during cleanup")
		}
	})

	base := ln.Addr().String()
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	ws, response, err := websocket.Dial(dialCtx, "ws://"+base+"/openai/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testAPIKey}},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial WebSocket: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	// Established traffic proves the session is pumping before shutdown.
	if err := ws.Write(dialCtx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write WebSocket message: %v", err)
	}
	if _, payload, err := ws.Read(dialCtx); err != nil || string(payload) != `{"type":"response.create"}` {
		t.Fatalf("read WebSocket echo = %q, %v", payload, err)
	}
	fixture.ws = ws

	sseWatchdog, cancelSSE := context.WithTimeout(context.Background(), fixtureIOTimeout)
	t.Cleanup(cancelSSE)
	fixture.sseWatchdog = sseWatchdog
	request, err := http.NewRequestWithContext(sseWatchdog, http.MethodPost, "http://"+base+"/openai/v1/responses", strings.NewReader(`{"model":"gpt","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	request.Header.Set("Content-Type", "application/json")
	sseResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST SSE: %v (watchdog: %v)", err, sseWatchdog.Err())
	}
	t.Cleanup(func() { _ = sseResponse.Body.Close() })
	// Request cancellation already aborts body reads; closing the body as well
	// keeps the bound independent of transport behaviour.
	stopWatchdogClose := context.AfterFunc(sseWatchdog, func() { _ = sseResponse.Body.Close() })
	t.Cleanup(func() { stopWatchdogClose() })
	if sseResponse.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", sseResponse.StatusCode)
	}
	fixture.sseBody = sseResponse.Body
	fixture.sseReader = bufio.NewReader(sseResponse.Body)
	first, err := readSSEFrame(fixture.sseReader)
	if err != nil || !strings.Contains(first, "response.created") {
		t.Fatalf("first SSE frame = %q, %v (watchdog: %v)", first, err, sseWatchdog.Err())
	}
	return fixture
}

func readSSEFrame(reader *bufio.Reader) (string, error) {
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		frame.WriteString(line)
		if err != nil {
			return frame.String(), err
		}
		if line == "\n" {
			return frame.String(), nil
		}
	}
}

// watchWebSocketClose reports the close status the cooperating client
// observes; reading lets coder/websocket complete the close handshake.
func watchWebSocketClose(ws *websocket.Conn) <-chan websocket.StatusCode {
	closed := make(chan websocket.StatusCode, 1)
	go func() {
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	return closed
}

// watchSSEEnd drains the remaining SSE body and reports how it ended.
func watchSSEEnd(body io.Reader) <-chan error {
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		ended <- err
	}()
	return ended
}

func TestShutdownSendsWebSocketGoingAwayWhileSSEOverrunsGracePeriod(t *testing.T) {
	const shutdownTimeout = 1500 * time.Millisecond
	fixture := startMixedTransportFixture(t, shutdownTimeout)
	wsClosed := watchWebSocketClose(fixture.ws)
	sseEnded := watchSSEEnd(fixture.sseReader)

	cancelledAt := time.Now()
	fixture.cancel()

	select {
	case status := <-wsClosed:
		if status != websocket.StatusGoingAway {
			t.Fatalf("WebSocket close status = %v, want 1001 going away", status)
		}
	case <-time.After(shutdownTimeout / 2):
		t.Fatal("WebSocket 1001 did not arrive well before the shutdown deadline")
	}
	select {
	case err := <-sseEnded:
		t.Fatalf("SSE ended (%v) before the shutdown deadline; want it still open when 1001 arrives", err)
	default:
	}

	select {
	case err := <-sseEnded:
		fixture.requireServerOriginatedSSEEnd(t, "SSE force-close", err)
		if elapsed := time.Since(cancelledAt); elapsed < shutdownTimeout-100*time.Millisecond {
			t.Errorf("SSE ended after %v, want force-close at the %v deadline", elapsed, shutdownTimeout)
		}
	case <-time.After(fixtureIOTimeout + time.Second):
		t.Fatal("SSE was not force-closed after the shutdown deadline")
	}
	select {
	case err := <-fixture.runErr:
		if !errors.Is(err, ErrForcedDrain) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run error = %v, want ErrForcedDrain wrapping context.DeadlineExceeded", err)
		}
		fixture.runErr <- err // let cleanup observe completion
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the forced drain")
	}
}

func TestShutdownCompletesCleanlyWhenSSEFinishesAfterWebSocketDrain(t *testing.T) {
	const shutdownTimeout = 5 * time.Second
	fixture := startMixedTransportFixture(t, shutdownTimeout)
	wsClosed := watchWebSocketClose(fixture.ws)

	fixture.cancel()
	select {
	case status := <-wsClosed:
		if status != websocket.StatusGoingAway {
			t.Fatalf("WebSocket close status = %v, want 1001 going away", status)
		}
	case <-time.After(shutdownTimeout / 2):
		t.Fatal("WebSocket 1001 did not arrive while SSE was still open")
	}
	fixture.releaseSSE()

	frame, err := readSSEFrame(fixture.sseReader)
	if err != nil || !strings.Contains(frame, "response.completed") {
		fixture.requireServerOriginatedSSEEnd(t, "final SSE frame read", err)
		t.Fatalf("final SSE frame = %q, %v", frame, err)
	}
	if rest, err := io.ReadAll(fixture.sseReader); err != nil {
		fixture.requireServerOriginatedSSEEnd(t, "SSE EOF read", err)
		t.Fatalf("SSE body ended with %v after %q, want clean completion", err, rest)
	}
	fixture.requireServerOriginatedSSEEnd(t, "clean SSE completion", nil)
	select {
	case err := <-fixture.runErr:
		if err != nil {
			t.Fatalf("Run error = %v, want clean drain", err)
		}
		fixture.runErr <- err
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("Run did not return after both drains completed")
	}
}

// drainFake is a goroutine-safe stand-in for either drain. Its Shutdown
// records the deadline it received and then behaves as configured: return
// err immediately, or (when waitForDeadline) block until ctx ends and return
// ctx.Err().
type drainFake struct {
	err             error
	waitForDeadline bool

	mu       sync.Mutex
	deadline time.Time
	started  atomic.Bool
	closed   atomic.Int32
}

func (f *drainFake) shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.deadline, _ = ctx.Deadline()
	f.mu.Unlock()
	if f.waitForDeadline {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *drainFake) observedDeadline() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deadline
}

type fakeHTTPLifecycle struct{ *drainFake }

func (fakeHTTPLifecycle) Serve(net.Listener) error { return http.ErrServerClosed }
func (f fakeHTTPLifecycle) Shutdown(ctx context.Context) error {
	return f.shutdown(ctx)
}
func (f fakeHTTPLifecycle) Close() error {
	f.closed.Add(1)
	return nil
}

type fakeWebSocketDrainer struct{ *drainFake }

func (f fakeWebSocketDrainer) StartDrain() { f.started.Store(true) }
func (f fakeWebSocketDrainer) Shutdown(ctx context.Context) error {
	if !f.started.Load() {
		return errors.New("WebSocket Shutdown called before StartDrain")
	}
	return f.shutdown(ctx)
}

func TestShutdownClassifiesDrainResults(t *testing.T) {
	httpFailure := errors.New("HTTP listener close failed")
	wsFailure := errors.New("WebSocket drain failed")
	type result struct {
		err             error
		waitForDeadline bool
	}
	clean := result{}
	timeout := result{waitForDeadline: true}
	earlyDeadline := result{err: context.DeadlineExceeded}
	tests := []struct {
		name            string
		shutdownTimeout time.Duration
		http, ws        result
		wantNil         bool
		wantForced      bool
		wantIs          []error
	}{
		{name: "both clean", shutdownTimeout: time.Second, http: clean, ws: clean, wantNil: true},
		{name: "HTTP deadline", shutdownTimeout: 30 * time.Millisecond, http: timeout, ws: clean, wantForced: true, wantIs: []error{context.DeadlineExceeded}},
		{name: "WebSocket deadline", shutdownTimeout: 30 * time.Millisecond, http: clean, ws: timeout, wantForced: true, wantIs: []error{context.DeadlineExceeded}},
		{name: "both deadlines", shutdownTimeout: 30 * time.Millisecond, http: timeout, ws: timeout, wantForced: true, wantIs: []error{context.DeadlineExceeded}},
		{name: "genuine HTTP error", shutdownTimeout: time.Second, http: result{err: httpFailure}, ws: clean, wantIs: []error{httpFailure}},
		{name: "genuine WebSocket error", shutdownTimeout: time.Second, http: clean, ws: result{err: wsFailure}, wantIs: []error{wsFailure}},
		{name: "genuine HTTP error with WebSocket deadline", shutdownTimeout: 30 * time.Millisecond, http: result{err: httpFailure}, ws: timeout, wantIs: []error{httpFailure, context.DeadlineExceeded}},
		{name: "HTTP deadline with genuine WebSocket error", shutdownTimeout: 30 * time.Millisecond, http: timeout, ws: result{err: wsFailure}, wantIs: []error{wsFailure, context.DeadlineExceeded}},
		{name: "deadline returned before grace expired", shutdownTimeout: time.Minute, http: earlyDeadline, ws: clean, wantIs: []error{context.DeadlineExceeded}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpFake := &drainFake{err: tt.http.err, waitForDeadline: tt.http.waitForDeadline}
			wsFake := &drainFake{err: tt.ws.err, waitForDeadline: tt.ws.waitForDeadline}
			cfg := testConfig()
			cfg.ShutdownTimeout = tt.shutdownTimeout
			srv := &Server{cfg: cfg, logger: discardLogger(t), http: fakeHTTPLifecycle{httpFake}, ws: fakeWebSocketDrainer{wsFake}}

			done := make(chan error, 1)
			go func() { done <- srv.shutdown() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(tt.shutdownTimeout + 5*time.Second):
				t.Fatal("shutdown did not return")
			}

			if tt.wantNil {
				if err != nil {
					t.Fatalf("shutdown error = %v, want nil", err)
				}
				if got := httpFake.closed.Load(); got != 0 {
					t.Errorf("hard close calls = %d, want 0 after a clean drain", got)
				}
			} else {
				if err == nil {
					t.Fatal("shutdown error = nil, want a drain failure")
				}
				if got := httpFake.closed.Load(); got != 1 {
					t.Errorf("hard close calls = %d, want 1 after a failed drain", got)
				}
			}
			if got := errors.Is(err, ErrForcedDrain); got != tt.wantForced {
				t.Errorf("errors.Is(%v, ErrForcedDrain) = %v, want %v", err, got, tt.wantForced)
			}
			for _, want := range tt.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("shutdown error = %v, want it to wrap %v", err, want)
				}
			}
			if a, b := httpFake.observedDeadline(), wsFake.observedDeadline(); a.IsZero() || !a.Equal(b) {
				t.Errorf("drain deadlines HTTP=%v WebSocket=%v, want one shared grace deadline", a, b)
			}
		})
	}
}
