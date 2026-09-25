package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

func TestHTTPHandlersWriteNothingWhenClientLeavesDuringOnDemandMint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(*Forwarder) http.HandlerFunc
		method  string
		path    string
	}{
		{
			name:    "forward",
			handler: func(f *Forwarder) http.HandlerFunc { return f.Handler(endpoint.AnthropicMessages()) },
			method:  http.MethodPost,
			path:    "/anthropic/v1/messages",
		},
		{
			name:    "passthrough",
			handler: func(f *Forwarder) http.HandlerFunc { return f.PassthroughHandler(endpoint.Models()) },
			method:  http.MethodGet,
			path:    "/models",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exchange, arrived := newBlockingExchange(t)
			manager := newMintTestManager(exchange, 0)
			var callerLogs bytes.Buffer
			callerLogger, err := logging.NewWithWriter(&callerLogs, config.ServeConfig{LogLevel: "debug", LogFormat: "text"})
			if err != nil {
				t.Fatalf("build logger: %v", err)
			}
			f := newTestForwarderWithLogger(manager, unreachableUpstream(t), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, callerLogger, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)).WithContext(ctx)
			writer := &failingResponseWriter{header: make(http.Header)}
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.handler(f)(writer, request)
			}()
			select {
			case <-arrived:
			case <-time.After(time.Second):
				t.Fatal("On-demand mint did not reach the exchange")
			}

			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not return after the client left mid-mint")
			}
			if writer.status != 0 || len(writer.written) != 0 || len(writer.header) != 0 {
				t.Errorf("response = status %d headers %v body %q, want no write", writer.status, writer.header, writer.written)
			}
			if strings.Contains(callerLogs.String(), "level=WARN") {
				t.Errorf("Caller logged a warning for a departed client:\n%s", callerLogs.String())
			}
		})
	}
}

func TestForwardRendersNotReadyWhenOnDemandMintTimesOutForConnectedClient(t *testing.T) {
	exchange, _ := newBlockingExchange(t)
	manager := newMintTestManager(exchange, 20*time.Millisecond)
	f := newTestForwarderWithLogger(manager, unreachableUpstream(t), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	recorder := httptest.NewRecorder()

	f.Handler(endpoint.AnthropicMessages())(recorder, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(`{}`)))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", recorder.Code)
	}
	const wantBody = `{"type":"error","error":{"type":"api_error","message":"no upstream credential available"}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("body = %q, want Surface-shaped credential failure %q", got, wantBody)
	}
}

// newBlockingExchange serves a Copilot token exchange that never answers
// before the test ends, keeping every On-demand mint in flight. arrived
// receives once per exchange request.
func newBlockingExchange(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(exchange.Close)
	t.Cleanup(func() { close(release) }) // runs first, so Close can finish
	return exchange, arrived
}

func newMintTestManager(exchange *httptest.Server, exchangeTimeout time.Duration) *identity.Manager {
	return identity.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), identity.ManagerConfig{
		OAuthToken:      "gho-mint-test",
		GitHubBaseURL:   exchange.URL,
		HTTPClient:      exchange.Client(),
		ExchangeTimeout: exchangeTimeout,
	})
}

func unreachableUpstream(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("upstream call executed without a credential")
		return nil, errors.New("upstream must not be called")
	})}
}
