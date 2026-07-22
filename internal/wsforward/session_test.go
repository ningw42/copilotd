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
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
)

func TestProxyAppliesClientMessageShimAndPreservesKinds(t *testing.T) {
	received := make(chan struct {
		kind websocket.MessageType
		data []byte
	}, 2)
	registry := shim.Registry{{
		Name:    "prefix-client-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				message.Data = append([]byte("shim:"), message.Data...)
				return true, nil
			})
		},
	}}
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		for range 2 {
			kind, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			received <- struct {
				kind websocket.MessageType
				data []byte
			}{kind: kind, data: data}
		}
	})
	defer cleanup()

	inputs := []struct {
		kind websocket.MessageType
		data []byte
	}{
		{kind: websocket.MessageText, data: []byte("text")},
		{kind: websocket.MessageBinary, data: []byte{0x00, 0xff}},
	}
	for _, input := range inputs {
		if err := client.Write(context.Background(), input.kind, input.data); err != nil {
			t.Fatalf("write client message: %v", err)
		}
	}
	for i, input := range inputs {
		select {
		case got := <-received:
			wantData := append([]byte("shim:"), input.data...)
			if got.kind != input.kind || !bytes.Equal(got.data, wantData) {
				t.Errorf("message %d = (%v, %x), want (%v, %x)", i, got.kind, got.data, input.kind, wantData)
			}
		case <-time.After(time.Second):
			t.Fatalf("message %d was not forwarded", i)
		}
	}
}

func TestProxyAppliesServerMessageShimWhileClientMessagesStayVerbatim(t *testing.T) {
	upstreamReceived := make(chan []byte, 1)
	registry := shim.Registry{{
		Name:    "prefix-server-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				message.Data = append([]byte("shim:"), message.Data...)
				return true, nil
			})
		},
	}}
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		kind, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		upstreamReceived <- data
		_ = conn.Write(context.Background(), kind, []byte("server-origin"))
	})
	defer cleanup()

	if err := client.Write(context.Background(), websocket.MessageText, []byte("client-origin")); err != nil {
		t.Fatalf("write client message: %v", err)
	}
	select {
	case got := <-upstreamReceived:
		if !bytes.Equal(got, []byte("client-origin")) {
			t.Errorf("upstream message = %q, want verbatim client-origin", got)
		}
	case <-time.After(time.Second):
		t.Fatal("client message was not forwarded upstream")
	}
	kind, data, err := client.Read(context.Background())
	if err != nil {
		t.Fatalf("read server message: %v", err)
	}
	if kind != websocket.MessageText || !bytes.Equal(data, []byte("shim:server-origin")) {
		t.Errorf("server message = (%v, %q), want (text, shim:server-origin)", kind, data)
	}
}

func TestProxyHonorsClientMessageKindMutationsWithInvalidKindFallingBackToText(t *testing.T) {
	received := make(chan websocket.MessageType, 3)
	registry := shim.Registry{{
		Name:    "mutate-client-message-kind",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				switch string(message.Data) {
				case "to-binary":
					message.Kind = shim.MessageBinary
				case "to-text":
					message.Kind = shim.MessageText
				case "invalid":
					message.Kind = shim.MessageKind(99)
				}
				return true, nil
			})
		},
	}}
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		for range 3 {
			kind, _, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			received <- kind
		}
	})
	defer cleanup()

	inputs := []struct {
		kind websocket.MessageType
		data string
		want websocket.MessageType
	}{
		{kind: websocket.MessageText, data: "to-binary", want: websocket.MessageBinary},
		{kind: websocket.MessageBinary, data: "to-text", want: websocket.MessageText},
		{kind: websocket.MessageText, data: "invalid", want: websocket.MessageText},
	}
	for _, input := range inputs {
		if err := client.Write(context.Background(), input.kind, []byte(input.data)); err != nil {
			t.Fatalf("write %q: %v", input.data, err)
		}
	}
	for _, input := range inputs {
		select {
		case got := <-received:
			if got != input.want {
				t.Errorf("message %q kind = %v, want %v", input.data, got, input.want)
			}
		case <-time.After(time.Second):
			t.Fatalf("message %q was not forwarded", input.data)
		}
	}
}

func TestProxyHonorsServerMessageKindMutations(t *testing.T) {
	registry := shim.Registry{{
		Name:    "mutate-server-message-kind",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
				switch string(message.Data) {
				case "to-binary":
					message.Kind = shim.MessageBinary
				case "to-text":
					message.Kind = shim.MessageText
				}
				return true, nil
			})
		},
	}}
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		_ = conn.Write(context.Background(), websocket.MessageText, []byte("to-binary"))
		_ = conn.Write(context.Background(), websocket.MessageBinary, []byte("to-text"))
	})
	defer cleanup()

	for _, want := range []websocket.MessageType{websocket.MessageBinary, websocket.MessageText} {
		kind, _, err := client.Read(context.Background())
		if err != nil {
			t.Fatalf("read server message: %v", err)
		}
		if kind != want {
			t.Errorf("server message kind = %v, want %v", kind, want)
		}
	}
}

func TestProxyComposesTwoBidirectionalShimsAndShortCircuitsDrops(t *testing.T) {
	registry := shim.Registry{
		{
			Name:    "outer",
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return bidirectionalMessageTransformFuncs{
					client: func(_ context.Context, message *shim.Message) (bool, error) {
						if string(message.Data) == "drop-client" {
							return false, nil
						}
						message.Data = []byte("outer(" + string(message.Data) + ")")
						return true, nil
					},
					server: func(_ context.Context, message *shim.Message) (bool, error) {
						if string(message.Data) == "drop-server" {
							return false, errors.New("server drop did not short-circuit")
						}
						message.Data = []byte("outer(" + string(message.Data) + ")")
						return true, nil
					},
				}
			},
		},
		{
			Name:    "inner",
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return bidirectionalMessageTransformFuncs{
					client: func(_ context.Context, message *shim.Message) (bool, error) {
						if string(message.Data) == "drop-client" {
							return false, errors.New("client drop did not short-circuit")
						}
						message.Data = []byte("inner(" + string(message.Data) + ")")
						return true, nil
					},
					server: func(_ context.Context, message *shim.Message) (bool, error) {
						if string(message.Data) == "drop-server" {
							return false, nil
						}
						message.Data = []byte("inner(" + string(message.Data) + ")")
						return true, nil
					},
				}
			},
		},
	}
	upstreamMessages := make(chan []string, 1)
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		var got []string
		for range 2 {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			got = append(got, string(data))
		}
		upstreamMessages <- got
		for _, data := range []string{"seed", "drop-server", "after-drop"} {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(data)); err != nil {
				return
			}
		}
	})
	defer cleanup()

	for _, data := range []string{"seed", "drop-client", "after-drop"} {
		if err := client.Write(context.Background(), websocket.MessageText, []byte(data)); err != nil {
			t.Fatalf("write client message %q: %v", data, err)
		}
	}
	select {
	case got := <-upstreamMessages:
		want := []string{"inner(outer(seed))", "inner(outer(after-drop))"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("upstream messages = %v, want %v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("composed client messages were not forwarded")
	}
	for _, want := range []string{"outer(inner(seed))", "outer(inner(after-drop))"} {
		_, data, err := client.Read(context.Background())
		if err != nil {
			t.Fatalf("read server message: %v", err)
		}
		if string(data) != want {
			t.Errorf("client message = %q, want %q", data, want)
		}
	}
}

func TestProxySharesSynchronizedShimStateAcrossConcurrentDirectionsAndTurns(t *testing.T) {
	registry := shim.Registry{{
		Name:    "session-state",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return newSynchronizedSessionStateShim()
		},
	}}
	upstreamMessages := make(chan string, 2)
	client, _, cleanup := startSessionWithRegistry(t, 1<<20, time.Second, registry, func(conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, []byte("server-one")); err != nil {
			return
		}
		for turn := 1; turn <= 2; turn++ {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			upstreamMessages <- string(data)
			if turn == 1 {
				if err := conn.Write(context.Background(), websocket.MessageText, []byte("server-two")); err != nil {
					return
				}
			}
		}
	})
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := client.Write(ctx, websocket.MessageText, []byte("client-one")); err != nil {
		t.Fatalf("write first client turn: %v", err)
	}
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read first server turn: %v", err)
	}
	if string(data) != "tag-1:server-one" {
		t.Errorf("first server turn = %q, want tag-1:server-one", data)
	}
	select {
	case got := <-upstreamMessages:
		if got != "tag-1:client-one" {
			t.Errorf("first client turn = %q, want tag-1:client-one", got)
		}
	case <-ctx.Done():
		t.Fatal("first client turn was not forwarded")
	}

	_, data, err = client.Read(ctx)
	if err != nil {
		t.Fatalf("read second server turn: %v", err)
	}
	if string(data) != "tag-2:server-two" {
		t.Errorf("second server turn = %q, want tag-2:server-two", data)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte("client-two")); err != nil {
		t.Fatalf("write second client turn: %v", err)
	}
	select {
	case got := <-upstreamMessages:
		if got != "tag-2:client-two" {
			t.Errorf("second client turn = %q, want tag-2:client-two", got)
		}
	case <-ctx.Done():
		t.Fatal("second client turn was not forwarded")
	}
}

func TestProxySnapshotsRegistryAndBuildsFreshContextualChainPerSession(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := conn.Write(context.Background(), websocket.MessageBinary, []byte("server-origin")); err != nil {
			return
		}
		kind, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		if err := conn.Write(context.Background(), kind, data); err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer upstream.Close()

	var instanceCount atomic.Int64
	registry := shim.Registry{{
		Name:    "session-state",
		Enabled: true,
		New: func(ctx context.Context, surface endpoint.Surface, route endpoint.Route) any {
			instance := instanceCount.Add(1)
			requestID, _ := logging.RequestIDFrom(ctx)
			messageCount := 0
			return clientMessageTransformFunc(func(transformCtx context.Context, message *shim.Message) (bool, error) {
				messageCount++
				transformRequestID := "none"
				if value, ok := logging.RequestIDFrom(transformCtx); ok {
					transformRequestID = value
				}
				message.Data = []byte(fmt.Sprintf(
					"instance=%d message=%d request=%s transform_request=%s surface=%s route=%s",
					instance, messageCount, requestID, transformRequestID, surface, route,
				))
				return true, nil
			})
		},
	}}
	proxy := New(
		identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true),
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		registry,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := proxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown proxy: %v", err)
		}
	}()
	registry[0].New = func(context.Context, endpoint.Surface, endpoint.Route) any {
		return clientMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
			return false, fmt.Errorf("mutated caller registry")
		})
	}

	handlerDone := make(chan struct{}, 2)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithRequestID(r.Context(), "request-99")
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r.WithContext(ctx))
		handlerDone <- struct{}{}
	}))
	defer downstream.Close()
	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"

	for session := int64(1); session <= 2; session++ {
		client, response, err := websocket.Dial(context.Background(), clientURL, nil)
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			t.Fatalf("dial session %d: %v", session, err)
		}
		kind, data, err := client.Read(context.Background())
		if err != nil {
			t.Fatalf("read server-origin message for session %d: %v", session, err)
		}
		if kind != websocket.MessageBinary || !bytes.Equal(data, []byte("server-origin")) {
			t.Errorf("server-origin message for session %d = (%v, %q), want (binary, server-origin)", session, kind, data)
		}
		if err := client.Write(context.Background(), websocket.MessageText, []byte("client-origin")); err != nil {
			t.Fatalf("write session %d client message: %v", session, err)
		}
		kind, data, err = client.Read(context.Background())
		if err != nil {
			t.Fatalf("read session %d echoed message: %v", session, err)
		}
		want := fmt.Sprintf("instance=%d message=1 request=request-99 transform_request=none surface=openai route=/responses", session)
		if kind != websocket.MessageText || string(data) != want {
			t.Errorf("session %d echoed message = (%v, %q), want (text, %q)", session, kind, data, want)
		}
		_, _, _ = client.Read(context.Background())
		_ = client.CloseNow()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Fatalf("session %d handler did not finish", session)
		}
	}
}

func TestProxyShutdownCancelsRunningClientTransformAndSiblingPump(t *testing.T) {
	transformEntered := make(chan struct{})
	transformResult := make(chan error, 1)
	registry := shim.Registry{{
		Name:    "blocking-client-message",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(ctx context.Context, _ *shim.Message) (bool, error) {
				close(transformEntered)
				select {
				case <-ctx.Done():
					transformResult <- ctx.Err()
					return false, ctx.Err()
				case <-time.After(300 * time.Millisecond):
					transformResult <- nil
					return false, nil
				}
			})
		},
	}}
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, _, _ = conn.Read(context.Background())
		close(upstreamClosed)
	}))
	defer upstream.Close()
	proxy := New(
		identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true),
		http.DefaultClient,
		time.Second,
		time.Second,
		1<<20,
		registry,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
	handlerDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r)
		close(handlerDone)
	}))
	defer downstream.Close()
	clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
	client, response, err := websocket.Dial(context.Background(), clientURL, nil)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream WebSocket: %v", err)
	}
	defer func() { _ = client.CloseNow() }()

	if err := client.Write(context.Background(), websocket.MessageText, []byte("block")); err != nil {
		t.Fatalf("write trigger message: %v", err)
	}
	select {
	case <-transformEntered:
	case <-time.After(time.Second):
		t.Fatal("transform did not start")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancelShutdown()
	if err := proxy.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown error = %v, want deadline exceeded at force boundary", err)
	}
	select {
	case err := <-transformResult:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("transform context error = %v, want canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("base-context cancellation did not release transform")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Error("sibling upstream pump did not unwind")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Error("handler did not return after forced shutdown")
	}
}

type clientMessageTransformFunc func(context.Context, *shim.Message) (bool, error)

func (f clientMessageTransformFunc) TransformClientMessage(ctx context.Context, message *shim.Message) (bool, error) {
	return f(ctx, message)
}

type serverMessageTransformFunc func(context.Context, *shim.Message) (bool, error)

func (f serverMessageTransformFunc) TransformServerMessage(ctx context.Context, message *shim.Message) (bool, error) {
	return f(ctx, message)
}

type bidirectionalMessageTransformFuncs struct {
	client shim.MessageTransform
	server shim.MessageTransform
}

func (f bidirectionalMessageTransformFuncs) TransformClientMessage(ctx context.Context, message *shim.Message) (bool, error) {
	return f.client(ctx, message)
}

func (f bidirectionalMessageTransformFuncs) TransformServerMessage(ctx context.Context, message *shim.Message) (bool, error) {
	return f.server(ctx, message)
}

type synchronizedSessionStateShim struct {
	mu                sync.Mutex
	tag               int
	clientEntered     chan struct{}
	serverEntered     chan struct{}
	firstStateReady   chan struct{}
	clientEnteredOnce sync.Once
	serverEnteredOnce sync.Once
	stateReadyOnce    sync.Once
}

func newSynchronizedSessionStateShim() *synchronizedSessionStateShim {
	return &synchronizedSessionStateShim{
		clientEntered:   make(chan struct{}),
		serverEntered:   make(chan struct{}),
		firstStateReady: make(chan struct{}),
	}
}

func (s *synchronizedSessionStateShim) TransformClientMessage(ctx context.Context, message *shim.Message) (bool, error) {
	s.clientEnteredOnce.Do(func() { close(s.clientEntered) })
	if err := waitForTransformSignal(ctx, s.serverEntered); err != nil {
		return false, err
	}
	if err := waitForTransformSignal(ctx, s.firstStateReady); err != nil {
		return false, err
	}
	s.mu.Lock()
	tag := s.tag
	s.mu.Unlock()
	message.Data = []byte(fmt.Sprintf("tag-%d:%s", tag, message.Data))
	return true, nil
}

func (s *synchronizedSessionStateShim) TransformServerMessage(ctx context.Context, message *shim.Message) (bool, error) {
	s.serverEnteredOnce.Do(func() { close(s.serverEntered) })
	if err := waitForTransformSignal(ctx, s.clientEntered); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.tag++
	tag := s.tag
	s.mu.Unlock()
	s.stateReadyOnce.Do(func() { close(s.firstStateReady) })
	message.Data = []byte(fmt.Sprintf("tag-%d:%s", tag, message.Data))
	return true, nil
}

func waitForTransformSignal(ctx context.Context, signal <-chan struct{}) error {
	select {
	case <-signal:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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
	return startSessionWithRegistry(t, maxMessageBytes, writeTimeout, nil, serveUpstream)
}

func startSessionWithRegistry(t *testing.T, maxMessageBytes int64, writeTimeout time.Duration, registry shim.Registry, serveUpstream func(*websocket.Conn)) (*websocket.Conn, <-chan struct{}, func()) {
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
		registry,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WsMetrics{},
	)
	sessionDone := make(chan struct{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(endpoint.OpenAIResponsesWS()).ServeHTTP(w, r)
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
