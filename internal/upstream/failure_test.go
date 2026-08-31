package upstream

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
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/sse"
)

func TestCallerClassifyMapsExecutionFailures(t *testing.T) {
	genericCause := errors.New("dial failed at https://secret.example")
	tests := []struct {
		name           string
		context        func(t *testing.T) context.Context
		err            error
		wantKind       apierror.Kind
		wantMessage    string
		wantClientGone bool
	}{
		{
			name:        "execution error",
			context:     func(*testing.T) context.Context { return context.Background() },
			err:         genericCause,
			wantKind:    apierror.BadGateway,
			wantMessage: "could not reach the upstream",
		},
		{
			name:        "execution error wraps deadline",
			context:     func(*testing.T) context.Context { return context.Background() },
			err:         fmt.Errorf("request failed: %w", context.DeadlineExceeded),
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
		{
			name: "context deadline",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
				t.Cleanup(cancel)
				return ctx
			},
			err:         genericCause,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
		{
			name: "client cancellation",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				t.Cleanup(cancel)
				return ctx
			},
			err:            genericCause,
			wantClientGone: true,
		},
		{
			name: "deadline error takes precedence over cancelled context",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				t.Cleanup(cancel)
				return ctx
			},
			err:         fmt.Errorf("dial: %w", context.DeadlineExceeded),
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
		{
			name: "deadline cause takes precedence over cancelled context error",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(context.DeadlineExceeded)
				t.Cleanup(func() { cancel(context.Canceled) })
				return ctx
			},
			err:         genericCause,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			caller := &Caller{logger: slog.New(slog.NewTextHandler(&logs, nil))}

			failure := caller.Classify(tc.context(t), tc.err)

			if failure.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", failure.Kind, tc.wantKind)
			}
			if failure.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", failure.Message, tc.wantMessage)
			}
			if failure.ClientGone != tc.wantClientGone {
				t.Errorf("ClientGone = %v, want %v", failure.ClientGone, tc.wantClientGone)
			}
			if failure.Err != tc.err {
				t.Errorf("Err = %v, want original error %v", failure.Err, tc.err)
			}

			logOutput := logs.String()
			if got := strings.Count(logOutput, "\n"); got != 1 {
				t.Errorf("classified failure log records = %d, want 1: %q", got, logOutput)
			}
			if got := strings.Count(logOutput, tc.err.Error()); got != 1 {
				t.Errorf("underlying cause occurrences in log = %d, want 1: %q", got, logOutput)
			}

			response := httptest.NewRecorder()
			failure.RespondTo(response, endpoint.OpenAI)
			if strings.Contains(response.Body.String(), tc.err.Error()) {
				t.Errorf("rendered body leaked underlying cause: %q", response.Body.String())
			}
		})
	}
}

type noOpStreamObserver struct{}

func (noOpStreamObserver) ObserveStreamOutcome(string, sse.Outcome) {}

func TestCallerCorrelateLogsOnlyDifferentResolvedRequestIDs(t *testing.T) {
	tests := []struct {
		name              string
		ctx               context.Context
		upstreamRequestID string
		wantLog           bool
	}{
		{
			name:              "no resolved request id",
			ctx:               context.Background(),
			upstreamRequestID: "upstream-123",
		},
		{
			name: "no upstream request id",
			ctx:  logging.WithRequestID(context.Background(), "copilotd-123"),
		},
		{
			name:              "matching request ids",
			ctx:               logging.WithRequestID(context.Background(), "copilotd-123"),
			upstreamRequestID: "copilotd-123",
		},
		{
			name:              "different request ids",
			ctx:               logging.WithRequestID(context.Background(), "copilotd-123"),
			upstreamRequestID: "upstream-456",
			wantLog:           true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger, err := logging.NewWithWriter(&logs, config.ServeConfig{
				LogLevel:  "debug",
				LogFormat: "text",
			})
			if err != nil {
				t.Fatalf("new logger: %v", err)
			}
			caller := &Caller{logger: logger}
			input, summary := requestsummary.Begin(tc.ctx, noOpStreamObserver{})
			header := make(http.Header)
			if tc.upstreamRequestID != "" {
				header.Set("X-Request-Id", tc.upstreamRequestID)
			}

			got := caller.Correlate(input, header)
			publication := summary.Finish(requestsummary.ResponseResult{})

			logOutput := logs.String()
			if !tc.wantLog {
				if got != input {
					t.Error("absent or equal upstream id derived a new context")
				}
				if publication.Context != tc.ctx {
					t.Error("absent or equal upstream id changed the summary publication context")
				}
				if logOutput != "" {
					t.Errorf("correlation log = %q, want none", logOutput)
				}
				return
			}
			if got == input {
				t.Error("differing upstream id returned the input context")
			}
			if publication.Context != got {
				t.Error("differing upstream id did not publish the returned context")
			}
			for _, want := range []string{
				"level=DEBUG",
				`msg="upstream response correlation"`,
				"request_id=copilotd-123",
				"upstream_request_id=upstream-456",
			} {
				if !strings.Contains(logOutput, want) {
					t.Errorf("correlation log = %q, want %q", logOutput, want)
				}
			}
			logs.Reset()
			logger.InfoContext(got, "later response path")
			for _, want := range []string{"request_id=copilotd-123", "upstream_request_id=upstream-456"} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("later response-path record = %q, want %q", logs.String(), want)
				}
			}
		})
	}
}

func TestCallerCorrelatePublishesTheFirstDifferingContext(t *testing.T) {
	base := logging.WithRequestID(context.Background(), "copilotd-123")
	ctx, summary := requestsummary.Begin(base, noOpStreamObserver{})
	caller := &Caller{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	firstHeader := http.Header{RequestIDHeader: {"upstream-first"}}
	secondHeader := http.Header{RequestIDHeader: {"upstream-second"}}

	first := caller.Correlate(ctx, firstHeader)
	second := caller.Correlate(ctx, secondHeader)
	publication := summary.Finish(requestsummary.ResponseResult{})

	if first == ctx || second == ctx || first == second {
		t.Fatal("differing upstream ids did not derive distinct response contexts")
	}
	if publication.Context != first {
		t.Error("later differing upstream id replaced the first published context")
	}
}

func TestRequestIDHeaderMatchesTheWireName(t *testing.T) {
	if RequestIDHeader != "X-Request-Id" {
		t.Errorf("RequestIDHeader = %q, want X-Request-Id", RequestIDHeader)
	}
}

func TestFailureRespondToRendersEachSurfaceDialect(t *testing.T) {
	tests := []struct {
		name       string
		surface    endpoint.Surface
		kind       apierror.Kind
		wantStatus int
		wantBody   string
	}{
		{
			name:       "Anthropic unavailable",
			surface:    endpoint.Anthropic,
			kind:       apierror.NotReady,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
		{
			name:       "Anthropic bad gateway",
			surface:    endpoint.Anthropic,
			kind:       apierror.BadGateway,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
		{
			name:       "Anthropic timeout",
			surface:    endpoint.Anthropic,
			kind:       apierror.GatewayTimeout,
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
		{
			name:       "OpenAI unavailable",
			surface:    endpoint.OpenAI,
			kind:       apierror.NotReady,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"error":{"message":"classified message","type":"api_error","code":null,"param":null}}`,
		},
		{
			name:       "OpenAI bad gateway",
			surface:    endpoint.OpenAI,
			kind:       apierror.BadGateway,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"error":{"message":"classified message","type":"api_error","code":null,"param":null}}`,
		},
		{
			name:       "OpenAI timeout",
			surface:    endpoint.OpenAI,
			kind:       apierror.GatewayTimeout,
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   `{"error":{"message":"classified message","type":"api_error","code":null,"param":null}}`,
		},
		{
			name:       "GitHub Copilot unavailable",
			surface:    endpoint.GitHubCopilot,
			kind:       apierror.NotReady,
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
		{
			name:       "GitHub Copilot bad gateway",
			surface:    endpoint.GitHubCopilot,
			kind:       apierror.BadGateway,
			wantStatus: http.StatusBadGateway,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
		{
			name:       "GitHub Copilot timeout",
			surface:    endpoint.GitHubCopilot,
			kind:       apierror.GatewayTimeout,
			wantStatus: http.StatusGatewayTimeout,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"classified message"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failure := &Failure{Kind: tc.kind, Message: "classified message"}
			response := httptest.NewRecorder()

			if wrote := failure.RespondTo(response, tc.surface); !wrote {
				t.Fatal("RespondTo returned false, want true")
			}
			if response.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, tc.wantStatus)
			}
			if got := response.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestFailureRespondToStaysSilentWhenClientIsGone(t *testing.T) {
	failure := &Failure{
		Kind:       apierror.Unauthorized,
		Message:    "must not be rendered",
		ClientGone: true,
	}
	response := httptest.NewRecorder()
	response.Code = 0 // distinguish untouched from an explicit WriteHeader(200)

	if wrote := failure.RespondTo(response, endpoint.OpenAI); wrote {
		t.Fatal("RespondTo returned true, want false")
	}
	if response.Code != 0 {
		t.Errorf("status = %d, want no status written", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Errorf("body = %q, want no bytes", response.Body.String())
	}
	if len(response.Header()) != 0 {
		t.Errorf("headers = %v, want none", response.Header())
	}
}
