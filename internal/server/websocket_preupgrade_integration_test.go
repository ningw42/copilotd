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

func TestOpenAIWebSocketAuthAndReadinessRejectBeforeUpgrade(t *testing.T) {
	tests := []struct {
		name       string
		ready      bool
		apiKey     string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "wrong local API key",
			ready:      true,
			apiKey:     "wrong-api-key",
			wantStatus: http.StatusUnauthorized,
			wantBody:   `{"error":{"message":"missing or invalid API key","type":"invalid_request_error","code":"invalid_api_key","param":null}}`,
		},
		{
			name:       "not ready",
			ready:      false,
			apiKey:     testAPIKey,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":{"message":"service not ready","type":"api_error","code":null,"param":null}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := identity.NewStatic(identity.Credential{
				BaseURL: "http://unused.invalid",
				Token:   "copilot-token",
			}, test.ready)
			logger := discardLogger(t)
			forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
			proxy := wsforward.New(provider, http.DefaultClient, time.Second, time.Second, 1<<20, logger, wsforward.WsMetrics{})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := proxy.Shutdown(ctx); err != nil {
					t.Errorf("shutdown WebSocket proxy: %v", err)
				}
			})

			handler := newHandler(testAPIKey, provider, forwarder, logger, NewStreamOutcomeCounter(), config.CodexConfig{}, proxy)
			downstream := httptest.NewServer(handler)
			t.Cleanup(downstream.Close)

			clientURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/openai/v1/responses"
			connection, response, err := websocket.Dial(context.Background(), clientURL, &websocket.DialOptions{
				HTTPHeader: http.Header{"Authorization": {"Bearer " + test.apiKey}},
			})
			if connection != nil {
				_ = connection.CloseNow()
			}
			if err == nil {
				t.Fatal("WebSocket dial unexpectedly succeeded")
			}
			if response == nil {
				t.Fatal("WebSocket rejection did not return an HTTP response")
			}
			defer func() { _ = response.Body.Close() }()
			body, readErr := io.ReadAll(response.Body)
			if readErr != nil {
				t.Fatalf("read rejection body: %v", readErr)
			}
			if response.StatusCode != test.wantStatus {
				t.Errorf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if got := string(body); got != test.wantBody {
				t.Errorf("body = %q, want %q", got, test.wantBody)
			}
			if got := response.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}
