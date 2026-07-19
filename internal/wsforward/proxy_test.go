package wsforward

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

func TestProxyShutdownRefusesNewUpgradesWith503(t *testing.T) {
	proxy := newTestProxy(identity.NewStatic(identity.Credential{}, true))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown proxy: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Version", "13")
	proxy.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestProxyShutdownDrainsActiveSessionWithGoingAway(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	}))
	t.Cleanup(upstream.Close)

	proxy := newTestProxy(identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
	}, true))
	client, handlerDone, downstream := dialProxy(t, proxy)
	t.Cleanup(downstream.Close)
	t.Cleanup(func() { _ = client.CloseNow() })

	clientClosed := make(chan websocket.StatusCode, 1)
	go func() {
		_, _, err := client.Read(context.Background())
		clientClosed <- websocket.CloseStatus(err)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown proxy: %v", err)
	}

	assertCloseStatus(t, clientClosed, websocket.StatusGoingAway)
	assertCloseStatus(t, upstreamClosed, websocket.StatusGoingAway)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after graceful WebSocket drain")
	}
}

func TestProxyShutdownForceClosesSessionThatOverrunsDeadline(t *testing.T) {
	upstreamAccepted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		close(upstreamAccepted)
		<-releaseUpstream
	}))
	t.Cleanup(func() {
		close(releaseUpstream)
		upstream.Close()
	})

	proxy := newTestProxy(identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
	}, true))
	client, handlerDone, downstream := dialProxy(t, proxy)
	t.Cleanup(downstream.Close)
	t.Cleanup(func() { _ = client.CloseNow() })
	select {
	case <-upstreamAccepted:
	case <-time.After(time.Second):
		t.Fatal("upstream WebSocket was not accepted")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := proxy.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown returned after %v, want caller deadline to bound it", elapsed)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("force-close did not release the straggling handler")
	}
}

func TestProxyShutdownForceCancelsHandlerStillResolvingCredential(t *testing.T) {
	provider := &blockingProvider{
		entered: make(chan struct{}),
	}
	proxy := newTestProxy(provider)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler().ServeHTTP(
			httptest.NewRecorder(),
			validUpgradeRequest(),
		)
	}()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not reach credential resolution")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := proxy.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded while handler is mid-accept", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("deadline force-cancel did not release the mid-accept handler")
	}
}

type blockingProvider struct {
	entered chan struct{}
}

func (p *blockingProvider) Current(ctx context.Context) (identity.Credential, error) {
	close(p.entered)
	<-ctx.Done()
	return identity.Credential{}, errors.New("credential unavailable")
}

func (p *blockingProvider) Ready() bool { return true }

func newTestProxy(provider identity.Provider) *Proxy {
	return New(
		provider,
		&http.Client{Transport: http.DefaultTransport},
		time.Second,
		time.Second,
		1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
}

func dialProxy(t *testing.T, proxy *Proxy) (*websocket.Conn, <-chan struct{}, *httptest.Server) {
	t.Helper()
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler().ServeHTTP(w, r)
		close(handlerDone)
	}))
	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
	client, response, err := websocket.Dial(context.Background(), clientURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		downstream.Close()
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	return client, handlerDone, downstream
}

func assertCloseStatus(t *testing.T, statuses <-chan websocket.StatusCode, want websocket.StatusCode) {
	t.Helper()
	select {
	case got := <-statuses:
		if got != want {
			t.Errorf("close status = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("peer did not receive close status %v", want)
	}
}

func TestProxyForwardsMessagesAndBuildsUpstreamHandshake(t *testing.T) {
	type handshake struct {
		rawQuery string
		header   http.Header
	}
	handshakes := make(chan handshake, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes <- handshake{rawQuery: r.URL.RawQuery, header: r.Header.Clone()}
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

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	proxy := New(
		provider,
		&http.Client{Transport: http.DefaultTransport},
		time.Second,
		time.Second,
		1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "request-123")
		proxy.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(downstream.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	clientHeaders := http.Header{
		"Authorization": {"Bearer local-api-key"},
		"X-Api-Key":     {"local-api-key"},
	}
	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") +
		"/openai/v1/responses?beta=two%2Bwords&alpha=1"
	client, response, err := websocket.Dial(context.Background(), clientURL, &websocket.DialOptions{
		HTTPHeader: clientHeaders,
	})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()

	for _, message := range []struct {
		messageType websocket.MessageType
		payload     []byte
	}{
		{messageType: websocket.MessageText, payload: []byte(`{"type":"response.create","turn":1}`)},
		{messageType: websocket.MessageBinary, payload: []byte{0x00, 0x01, 0xfe, 0xff}},
		{messageType: websocket.MessageText, payload: []byte(`{"type":"response.create","turn":2}`)},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := client.Write(ctx, message.messageType, message.payload); err != nil {
			cancel()
			t.Fatalf("write message: %v", err)
		}
		gotType, gotPayload, err := client.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read echoed message: %v", err)
		}
		if gotType != message.messageType {
			t.Errorf("message type = %v, want %v", gotType, message.messageType)
		}
		if string(gotPayload) != string(message.payload) {
			t.Errorf("payload = %x, want %x", gotPayload, message.payload)
		}
	}

	gotHandshake := <-handshakes
	if gotHandshake.rawQuery != "beta=two%2Bwords&alpha=1" {
		t.Errorf("upstream raw query = %q", gotHandshake.rawQuery)
	}
	for name, want := range map[string]string{
		"Authorization":          "Bearer copilot-token",
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Version":         "vscode/1.104.1",
		"X-Request-Id":           "request-123",
	} {
		if got := gotHandshake.header.Get(name); got != want {
			t.Errorf("upstream %s = %q, want %q", name, got, want)
		}
	}
	if got := gotHandshake.header.Get("X-Api-Key"); got != "" {
		t.Errorf("local API key leaked upstream: %q", got)
	}
}

func TestProxyUsesConfiguredProxyAndVerifiedTLSForWSSDial(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept TLS upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		messageType, payload, err := conn.Read(r.Context())
		if err == nil {
			_ = conn.Write(r.Context(), messageType, payload)
		}
	}))
	t.Cleanup(upstream.Close)

	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	proxyRequests := make(chan string, 1)
	dialClient := &http.Client{Transport: &http.Transport{
		Proxy: func(request *http.Request) (*url.URL, error) {
			proxyRequests <- request.URL.Scheme
			return nil, nil
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
	}}
	provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true)
	proxy := New(provider, dialClient, time.Second, time.Second, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)), WsMetrics{})
	downstream := httptest.NewServer(proxy.Handler())
	t.Cleanup(downstream.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	client, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(downstream.URL, "http"), nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, []byte("verified TLS")); err != nil {
		t.Fatalf("write through WSS upstream: %v", err)
	}
	if _, payload, err := client.Read(ctx); err != nil {
		t.Fatalf("read through WSS upstream: %v", err)
	} else if string(payload) != "verified TLS" {
		t.Errorf("payload = %q, want verified TLS", payload)
	}
	select {
	case scheme := <-proxyRequests:
		if scheme != "https" {
			t.Errorf("proxy callback scheme = %q, want https for wss", scheme)
		}
	case <-ctx.Done():
		t.Fatal("configured proxy callback was not used")
	}
	if dialClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Error("upstream TLS verification is disabled")
	}
}
