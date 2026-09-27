package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestServeRelaysWebSocketHandshakeRedirectWithoutFollowingIt proves, through
// the production serve wiring, that a 3xx handshake answer is Copilot's final
// non-101 response: it is relayed before any downstream 101 and never followed.
func TestServeRelaysWebSocketHandshakeRedirectWithoutFollowingIt(t *testing.T) {
	const redirectBody = "copilot handshake redirect body"
	var targetRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(target.Close)
	// Same hostname on another port: net/http would even re-send the Copilot
	// token to it while following.
	location := target.URL + "/redirect-target-must-not-be-followed"

	var handshakes atomic.Int64
	copilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.URL.Path != "/responses" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("upstream request = %s %s (Upgrade %q), want the /responses WebSocket handshake", r.Method, r.URL.Path, r.Header.Get("Upgrade"))
		}
		w.Header().Set("Location", location)
		w.Header().Set("Content-Length", strconv.Itoa(len(redirectBody)))
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, redirectBody)
	}))
	t.Cleanup(copilot.Close)

	cfg := lifecycleConfig("gho-websocket-redirect")
	cfg.WebSocketHandshakeTimeout = 5 * time.Second
	exchange := newGitHubExchangeStub(t, "copilot-websocket-redirect-token", copilot.URL)
	base := startServedLifecycle(t, discardLogger(t), serveInput{Config: cfg, Edges: exchangeServeEdges(exchange.server)})

	// The client must not follow the relayed redirect itself, or its second
	// request would obscure the response under test.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/openai/v1/responses", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + testAPIKey}},
	})
	if err == nil {
		_ = conn.CloseNow()
		t.Fatalf("WebSocket dial was upgraded (status %d), want the relayed handshake redirect", response.StatusCode)
	}
	if response == nil {
		t.Fatalf("WebSocket dial failed without a response: %v", err)
	}
	if response.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want Copilot's first handshake answer 302", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != location {
		t.Errorf("Location = %q, want Copilot's %q", got, location)
	}
	if body, _ := io.ReadAll(response.Body); string(body) != redirectBody {
		t.Errorf("body = %q, want Copilot's complete body %q", body, redirectBody)
	}
	if got := handshakes.Load(); got != 1 {
		t.Errorf("Copilot handshakes = %d, want exactly one", got)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Errorf("redirect target requests = %d, want none", got)
	}
}
