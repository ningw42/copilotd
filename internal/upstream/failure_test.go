package upstream

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/ningw42/copilotd/internal/identity"
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
			caller := &Caller{logger: debugJSONLogger(t, &logs)}

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

			assertCallerFailureLogged(t, logs.Bytes(), tc.wantClientGone, tc.err)

			response := httptest.NewRecorder()
			failure.RespondTo(response, endpoint.OpenAI)
			if strings.Contains(response.Body.String(), tc.err.Error()) {
				t.Errorf("rendered body leaked underlying cause: %q", response.Body.String())
			}
		})
	}
}

func TestCallerPrepareClassifiesDepartureDuringCredentialAcquisitionAsClientGone(t *testing.T) {
	provider := &departingProvider{entered: make(chan struct{})}
	caller := executionCaller(provider, nil, time.Second, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type prepared struct {
		request *http.Request
		failure *Failure
	}
	result := make(chan prepared, 1)
	go func() {
		request, failure := caller.Prepare(ctx, executionCall())
		result <- prepared{request: request, failure: failure}
	}()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("Prepare did not reach credential acquisition")
	}

	cancel()

	var got prepared
	select {
	case got = <-result:
	case <-time.After(time.Second):
		t.Fatal("Prepare did not return after the caller left")
	}
	if got.request != nil {
		t.Errorf("Prepare() request = %v, want nil", got.request)
	}
	if got.failure == nil || !got.failure.ClientGone {
		t.Fatalf("Prepare() failure = %#v, want ClientGone", got.failure)
	}
	if !errors.Is(got.failure.Err, context.Canceled) {
		t.Errorf("failure.Err = %v, want the provider's context.Canceled", got.failure.Err)
	}
	recorder := httptest.NewRecorder()
	recorder.Code = 0 // distinguish untouched from an explicit WriteHeader(200)
	if wrote := got.failure.RespondTo(recorder, endpoint.OpenAI); wrote || recorder.Code != 0 || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
		t.Errorf("RespondTo() = wrote %v status %d headers %v body %q, want no write", wrote, recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestCallerPrepareClassifiesCredentialFailureByCallerContextAlone(t *testing.T) {
	live := func(*testing.T) context.Context { return context.Background() }
	tests := []struct {
		name           string
		context        func(t *testing.T) context.Context
		err            error
		wantKind       apierror.Kind
		wantMessage    string
		wantClientGone bool
	}{
		{
			name:        "ordinary provider error, caller live",
			context:     live,
			err:         errors.New("copilot token exchange: status 502"),
			wantKind:    apierror.NotReady,
			wantMessage: "no upstream credential available",
		},
		{
			// The mint runs on its own bounded context; its timeout is still a
			// Request-scoped mint failure for a connected caller.
			name:        "provider error wraps deadline, caller live",
			context:     live,
			err:         fmt.Errorf("copilot token exchange: %w", context.DeadlineExceeded),
			wantKind:    apierror.NotReady,
			wantMessage: "no upstream credential available",
		},
		{
			name:        "provider error wraps cancellation, caller live",
			context:     live,
			err:         fmt.Errorf("copilot token exchange: %w", context.Canceled),
			wantKind:    apierror.NotReady,
			wantMessage: "no upstream credential available",
		},
		{
			name: "caller cancelled",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				t.Cleanup(cancel)
				return ctx
			},
			err:            context.Canceled,
			wantClientGone: true,
		},
		{
			name: "provider error wraps deadline, caller cancelled",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				t.Cleanup(cancel)
				return ctx
			},
			err:            fmt.Errorf("copilot token exchange: %w", context.DeadlineExceeded),
			wantClientGone: true,
		},
		{
			name: "caller deadline expired",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
				t.Cleanup(cancel)
				return ctx
			},
			err:         context.DeadlineExceeded,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
		{
			name: "caller deadline cause takes precedence over cancellation",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(context.DeadlineExceeded)
				t.Cleanup(func() { cancel(context.Canceled) })
				return ctx
			},
			err:         context.Canceled,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := readyExecutionProvider("https://upstream.invalid")
			provider.SetError(tc.err)
			var logs bytes.Buffer
			caller := executionCaller(provider, nil, time.Second, 1<<20, debugJSONLogger(t, &logs))

			request, failure := caller.Prepare(tc.context(t), executionCall())

			if request != nil {
				t.Errorf("Prepare() request = %v, want nil", request)
			}
			if failure == nil {
				t.Fatal("Prepare() failure = nil, want classified credential failure")
			}
			if failure.ClientGone != tc.wantClientGone {
				t.Errorf("ClientGone = %v, want %v", failure.ClientGone, tc.wantClientGone)
			}
			if !tc.wantClientGone && (failure.Kind != tc.wantKind || failure.Message != tc.wantMessage) {
				t.Errorf("failure = (%v, %q), want (%v, %q)", failure.Kind, failure.Message, tc.wantKind, tc.wantMessage)
			}
			if failure.Err != tc.err {
				t.Errorf("Err = %v, want provider error %v", failure.Err, tc.err)
			}
			assertCallerFailureLogged(t, logs.Bytes(), tc.wantClientGone, tc.err)
		})
	}
}

// assertCallerFailureLogged checks that the Caller logged cause exactly once,
// on one failure record at Debug for a departed client and at Warn otherwise.
func assertCallerFailureLogged(t *testing.T, logs []byte, clientGone bool, cause error) {
	t.Helper()
	wantLevel := slog.LevelWarn
	if clientGone {
		wantLevel = slog.LevelDebug
	}
	record := callerFailureRecord(t, logs, wantLevel)
	if got := record[logging.ErrorKey]; got != cause.Error() {
		t.Errorf("failure record %s = %v, want %q", logging.ErrorKey, got, cause.Error())
	}
	quoted, err := json.Marshal(cause.Error())
	if err != nil {
		t.Fatalf("encode cause: %v", err)
	}
	if got := bytes.Count(logs, quoted); got != 1 {
		t.Errorf("underlying cause occurrences in log = %d, want 1: %s", got, logs)
	}
}

// callerFailureRecord returns the single Caller failure record in JSON logs.
// It selects the record by the error key, so Debug records such as upstream
// response correlation may appear alongside it. The failure record must be at
// wantLevel, and no other record may be at Warn.
func callerFailureRecord(t *testing.T, logs []byte, wantLevel slog.Level) map[string]any {
	t.Helper()
	var failures []map[string]any
	warnings := 0
	for _, line := range bytes.Split(bytes.TrimSpace(logs), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode log record: %v: %s", err, line)
		}
		if record["level"] == slog.LevelWarn.String() {
			warnings++
		}
		if _, ok := record[logging.ErrorKey]; ok {
			failures = append(failures, record)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("Caller failure records = %d, want 1: %s", len(failures), logs)
	}
	if got := failures[0]["level"]; got != wantLevel.String() {
		t.Errorf("failure record level = %v, want %s", got, wantLevel)
	}
	wantWarnings := 0
	if wantLevel == slog.LevelWarn {
		wantWarnings = 1
	}
	if warnings != wantWarnings {
		t.Errorf("Warn records = %d, want %d: %s", warnings, wantWarnings, logs)
	}
	return failures[0]
}

func debugJSONLogger(t *testing.T, w io.Writer) *slog.Logger {
	t.Helper()
	logger, err := logging.NewWithWriter(w, config.ServeConfig{LogLevel: "debug", LogFormat: "json"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	return logger
}

// departingProvider behaves like identity.Manager when its caller leaves an
// On-demand mint: it waits until the caller's context ends, then returns that
// context's error.
type departingProvider struct {
	entered chan struct{}
}

func (p *departingProvider) Current(ctx context.Context) (identity.Credential, error) {
	close(p.entered)
	<-ctx.Done()
	return identity.Credential{}, ctx.Err()
}

func (*departingProvider) Ready() bool { return true }

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
