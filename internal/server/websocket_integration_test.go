package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/wsforward"
)

func TestOpenAIResponsesHTTPAndWebSocketTransportsCoexist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept upstream WebSocket: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			messageType, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			_ = conn.Write(r.Context(), messageType, payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"transport":"http"}`)
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   "copilot-token",
	}, true)
	logger := discardLogger(t)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	wsProxy := wsforward.New(provider, http.DefaultClient, time.Second, time.Second, 1<<20, nil, logger, wsforward.WsMetrics{})
	handler := newHandler(testAPIKey, provider, newTestReadyObservers(), forwarder, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, wsProxy)
	downstream := httptest.NewServer(handler)
	t.Cleanup(downstream.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := wsProxy.Shutdown(ctx); err != nil {
			t.Errorf("shutdown WebSocket proxy: %v", err)
		}
	})

	webSocketURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
	conn, response, err := websocket.Dial(context.Background(), webSocketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testAPIKey}},
	})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial WebSocket transport: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write WebSocket message: %v", err)
	}
	if _, payload, err := conn.Read(ctx); err != nil {
		t.Fatalf("read WebSocket message: %v", err)
	} else if string(payload) != `{"type":"response.create"}` {
		t.Errorf("WebSocket payload = %q", payload)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	request, err := http.NewRequest(http.MethodPost, downstream.URL+"/openai/v1/responses", strings.NewReader(`{"model":"gpt"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	request.Header.Set("Content-Type", "application/json")
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST Responses transport: %v", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	body, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatalf("read POST response: %v", err)
	}
	if httpResponse.StatusCode != http.StatusOK || string(body) != `{"transport":"http"}` {
		t.Errorf("POST response = %d %q", httpResponse.StatusCode, body)
	}

	_, rejected, err := websocket.Dial(context.Background(), webSocketURL, nil)
	if err == nil {
		t.Fatal("WebSocket dial without API key unexpectedly succeeded")
	}
	if rejected == nil || rejected.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %v, want 401", rejected)
	}
	_ = rejected.Body.Close()
}
