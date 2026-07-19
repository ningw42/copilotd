package wsforward

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/identity"
)

func TestProxyClosesOversizeMessageWith1009OnBothSides(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, _, cleanup := startSession(t, 4, time.Second, func(conn *websocket.Conn) {
		_, _, err := conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte("12345")); err != nil {
		t.Fatalf("write oversize message: %v", err)
	}
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("client close status = %v, want %v (err: %v)", got, websocket.StatusMessageTooBig, err)
	}

	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusMessageTooBig {
			t.Errorf("upstream close status = %v, want %v", got, websocket.StatusMessageTooBig)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not receive propagated 1009 close")
	}
}

func TestProxyClosesOversizeUpstreamMessageWith1009OnBothSides(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, _, cleanup := startSession(t, 4, time.Second, func(conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, []byte("12345")); err != nil {
			upstreamClosed <- websocket.CloseStatus(err)
			return
		}
		_, _, err := conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("client close status = %v, want %v (err: %v)", got, websocket.StatusMessageTooBig, err)
	}

	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusMessageTooBig {
			t.Errorf("upstream close status = %v, want %v", got, websocket.StatusMessageTooBig)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not receive 1009 for its oversize message")
	}
}

func TestProxyPropagatesClientCloseCodeUpstream(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, sessionDone, cleanup := startSession(t, 1<<20, time.Second, func(conn *websocket.Conn) {
		_, _, err := conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	})
	defer cleanup()

	const code websocket.StatusCode = 4001
	if err := client.Close(code, "client done"); err != nil {
		t.Fatalf("close client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case got := <-upstreamClosed:
		if got != code {
			t.Errorf("upstream close status = %v, want %v", got, code)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not receive propagated client close")
	}
	select {
	case <-sessionDone:
	case <-ctx.Done():
		t.Fatal("session did not stop after client close")
	}
}

func TestProxyPropagatesUpstreamCloseCodeToClient(t *testing.T) {
	const code websocket.StatusCode = 4002
	client, sessionDone, cleanup := startSession(t, 1<<20, time.Second, func(conn *websocket.Conn) {
		_ = conn.Close(code, "upstream done")
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != code {
		t.Fatalf("client close status = %v, want %v (err: %v)", got, code, err)
	}
	select {
	case <-sessionDone:
	case <-ctx.Done():
		t.Fatal("session did not stop after upstream close")
	}
}

func TestProxyUses1011ForUpstreamFailureWithoutCloseCode(t *testing.T) {
	client, sessionDone, cleanup := startSession(t, 1<<20, time.Second, func(conn *websocket.Conn) {
		_ = conn.CloseNow()
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("client close status = %v, want %v (err: %v)", got, websocket.StatusInternalError, err)
	}
	select {
	case <-sessionDone:
	case <-ctx.Done():
		t.Fatal("session did not stop after abrupt upstream failure")
	}
}

func TestProxyUses1011ForUpstreamCloseWithoutStatus(t *testing.T) {
	client, _, cleanup := startSession(t, 1<<20, time.Second, func(conn *websocket.Conn) {
		_ = conn.Close(websocket.StatusNoStatusRcvd, "")
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("client close status = %v, want %v (err: %v)", got, websocket.StatusInternalError, err)
	}
}

func TestProxyDoesNotApplyWriteTimeoutToWholeSession(t *testing.T) {
	client, _, cleanup := startSession(t, 1<<20, 25*time.Millisecond, func(conn *websocket.Conn) {
		messageType, payload, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		_ = conn.Write(context.Background(), messageType, payload)
	})
	defer cleanup()

	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want := []byte("still alive")
	if err := client.Write(ctx, websocket.MessageText, want); err != nil {
		t.Fatalf("write after silent interval: %v", err)
	}
	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read after silent interval: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

func TestProxyWriteTimeoutTearsDownSlowReaderSession(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	client, sessionDone, cleanup := startSession(t, 32<<20, 25*time.Millisecond, func(conn *websocket.Conn) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 16<<20)); err != nil {
			upstreamClosed <- websocket.CloseStatus(err)
			return
		}
		_, _, err := conn.Read(ctx)
		upstreamClosed <- websocket.CloseStatus(err)
	})
	defer cleanup()
	// Deliberately do not read from client: the proxy's downstream write must
	// hit its per-write deadline and cancel the sibling upstream read.
	_ = client

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case got := <-upstreamClosed:
		if got != websocket.StatusInternalError {
			t.Errorf("upstream close status = %v, want %v", got, websocket.StatusInternalError)
		}
	case <-ctx.Done():
		t.Fatal("upstream did not close after downstream write timeout")
	}
	select {
	case <-sessionDone:
	case <-ctx.Done():
		t.Fatal("session did not stop after downstream write timeout")
	}
}

func startSession(t *testing.T, maxMessageBytes int64, writeTimeout time.Duration, serveUpstream func(*websocket.Conn)) (*websocket.Conn, <-chan struct{}, func()) {
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
		Token:   "copilot-token",
	}, true)
	proxy := New(
		provider,
		&http.Client{Transport: http.DefaultTransport},
		time.Second,
		writeTimeout,
		maxMessageBytes,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
	sessionDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler().ServeHTTP(w, r)
		close(sessionDone)
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
	return client, sessionDone, cleanup
}
