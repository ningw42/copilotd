package wsforward

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/upstream"
)

func TestRawNetworkConnUnwrapsNestedTransports(t *testing.T) {
	raw, peer := net.Pipe()
	t.Cleanup(func() { _ = raw.Close() })
	t.Cleanup(func() { _ = peer.Close() })

	wrapped := testNetConnWrapper{Conn: testNetConnWrapper{Conn: raw}}
	if got := rawNetworkConn(wrapped); got != raw {
		t.Fatalf("raw network connection = %T, want %T", got, raw)
	}
}

func TestProxyClientCancelDuringUpstreamHandshakeWritesNothingAndBooksNoMetric(t *testing.T) {
	requestEntered := make(chan struct{})
	dialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(requestEntered)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	observed := &recordingWsMetrics{}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		dialClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{Accept: observed},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	request := validUpgradeRequest().WithContext(requestCtx)
	recorder := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, request)
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
		t.Fatal("handler did not stop after client cancellation")
	}

	if got := recorder.Body.Len(); got != 0 {
		t.Errorf("response bytes = %d, want 0: %q", got, recorder.Body.String())
	}
	accepts, terminals := observed.snapshot()
	if len(accepts) != 0 {
		t.Errorf("accept observations = %v, want none", accepts)
	}
	if len(terminals) != 0 {
		t.Errorf("session terminal observations = %v, want none", terminals)
	}
}

func TestProxyForwardsClientHeaderAndStripsWebSocketOffers(t *testing.T) {
	requests := make(chan http.Header, 1)
	dialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests <- request.Header.Clone()
		return nil, errors.New("stop after capturing upstream handshake")
	})}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		dialClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	request := validUpgradeRequest()
	request.Header.Set("X-Client-Feature", "forward-me")
	request.Header.Set("Sec-WebSocket-Protocol", "client-offer")
	request.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(httptest.NewRecorder(), request)

	header := <-requests
	if got := header.Get("X-Client-Feature"); got != "forward-me" {
		t.Errorf("upstream X-Client-Feature = %q, want forward-me", got)
	}
	for _, name := range []string{"Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		if got := header.Values(name); len(got) != 0 {
			t.Errorf("upstream %s = %v, want absent", name, got)
		}
	}
}

func TestProxyCredentialResolutionUsesPhaseContextWithoutDialDeadline(t *testing.T) {
	provider := &contextRecordingProvider{credential: identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}}
	dialClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("stop after credential resolution")
	})}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		dialClient,
		time.Nanosecond,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(httptest.NewRecorder(), validUpgradeRequest())

	if provider.hadDeadline {
		t.Error("credential resolution context inherited the dial deadline")
	}
}

func TestProxyPreservesBareQuestionMarkInUpstreamHandshake(t *testing.T) {
	type capturedURL struct {
		value      string
		forceQuery bool
	}
	requests := make(chan capturedURL, 1)
	dialClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests <- capturedURL{value: request.URL.String(), forceQuery: request.URL.ForceQuery}
		return nil, errors.New("stop after capturing upstream URL")
	})}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: "http://upstream.invalid",
		Token:   "copilot-token",
	}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		dialClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	})

	request := validUpgradeRequest()
	request.URL.ForceQuery = true
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(httptest.NewRecorder(), request)

	got := <-requests
	if want := "http://upstream.invalid/responses?"; got.value != want {
		t.Errorf("upstream URL = %q, want %q", got.value, want)
	}
	if !got.forceQuery {
		t.Error("upstream ForceQuery = false, want true")
	}
}

type contextRecordingProvider struct {
	credential  identity.Credential
	hadDeadline bool
}

func (p *contextRecordingProvider) Current(ctx context.Context) (identity.Credential, error) {
	_, p.hadDeadline = ctx.Deadline()
	return p.credential, nil
}

func (*contextRecordingProvider) Ready() bool { return true }

type testNetConnWrapper struct {
	net.Conn
}

func (c testNetConnWrapper) NetConn() net.Conn { return c.Conn }

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
	proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestProxyShutdownDrainsActiveSessionWithGoingAway(t *testing.T) {
	upstreamClosed := make(chan websocket.StatusCode, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, err = conn.Read(context.Background())
		upstreamClosed <- websocket.CloseStatus(err)
	}))
	t.Cleanup(upstreamServer.Close)

	proxy := newTestProxy(identity.NewStatic(identity.Credential{
		BaseURL: upstreamServer.URL,
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
	testProxyShutdownForceClosesSessionThatOverrunsDeadline(t, false)
}

func TestProxyShutdownForceClosesTLSSessionThatOverrunsDeadline(t *testing.T) {
	testProxyShutdownForceClosesSessionThatOverrunsDeadline(t, true)
}

func testProxyShutdownForceClosesSessionThatOverrunsDeadline(t *testing.T, tlsUpstream bool) {
	t.Helper()
	upstreamAccepted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		close(upstreamAccepted)
		<-releaseUpstream
	})
	var upstreamServer *httptest.Server
	if tlsUpstream {
		upstreamServer = httptest.NewTLSServer(upstreamHandler)
	} else {
		upstreamServer = httptest.NewServer(upstreamHandler)
	}
	t.Cleanup(func() {
		close(releaseUpstream)
		upstreamServer.Close()
	})

	dialClient := &http.Client{Transport: http.DefaultTransport}
	if tlsUpstream {
		dialClient = upstreamServer.Client()
	}
	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstreamServer.URL,
		Token:   "copilot-token",
	}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		dialClient,
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
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
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(
		newTestCaller(provider, logger),
		&http.Client{Transport: http.DefaultTransport},
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
}

func newTestCaller(provider identity.Provider, logger *slog.Logger) *upstream.Caller {
	return upstream.New(provider, http.DefaultClient, time.Second, 1<<20, logger)
}

func dialProxy(t *testing.T, proxy *Proxy) (*websocket.Conn, <-chan struct{}, *httptest.Server) {
	t.Helper()
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r)
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
		path     string
		rawQuery string
		header   http.Header
	}
	handshakes := make(chan handshake, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes <- handshake{path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone()}
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
	t.Cleanup(upstreamServer.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstreamServer.URL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(
		newTestCaller(provider, logger),
		&http.Client{Transport: http.DefaultTransport},
		time.Second,
		time.Second,
		1<<20,
		nil,
		logger,
		logger,
		0,
		WsMetrics{},
	)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "request-123")
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r.WithContext(ctx))
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
	if got, want := gotHandshake.path, string(endpoint.OpenAIResponsesWS().Upstream()); got != want {
		t.Errorf("upstream path = %q, want contract route %q", got, want)
	}
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
	upstreamServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(upstreamServer.Close)

	roots := x509.NewCertPool()
	roots.AddCert(upstreamServer.Certificate())
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
	provider := identity.NewStatic(identity.Credential{BaseURL: upstreamServer.URL, Token: "copilot-token"}, true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := New(newTestCaller(provider, logger), dialClient, time.Second, time.Second, 1<<20, nil, logger, logger, 0, WsMetrics{})
	downstream := httptest.NewServer(proxy.Handler(endpoint.OpenAIResponsesWS()))
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
