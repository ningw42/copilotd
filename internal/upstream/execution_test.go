package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

func TestCallerDoUsesCallerContextVerbatimAndArmsNoTimer(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "caller-owned")
	const outboundTimeout = 5 * time.Millisecond
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Context() != ctx {
			t.Errorf("request context is derived, want caller context verbatim")
		}
		select {
		case <-request.Context().Done():
			return nil, request.Context().Err()
		case <-time.After(4 * outboundTimeout):
			return executionResponse(request, http.StatusNoContent, http.NoBody), nil
		}
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, outboundTimeout, 1<<20, slog.Default())

	response, failure := caller.Do(ctx, executionCall())

	if failure != nil {
		t.Fatalf("Do() failure = %#v, want success", failure)
	}
	if response == nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("Do() response = %#v, want 204 response", response)
	}
}

func TestCallerDoPropagatesCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Hour, 1<<20, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan *Failure, 1)
	go func() {
		_, failure := caller.Do(ctx, executionCall())
		result <- failure
	}()
	<-started
	cancel()

	select {
	case failure := <-result:
		if failure == nil || !failure.ClientGone {
			t.Fatalf("Do() failure = %#v, want ClientGone", failure)
		}
		if !errors.Is(failure.Err, context.Canceled) {
			t.Errorf("failure.Err = %v, want context.Canceled", failure.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("Do() did not return after caller cancellation")
	}
}

func TestCallerDoClassifiesExecutionFailure(t *testing.T) {
	executionFailure := errors.New("dial failed")
	client := &http.Client{Transport: executionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, executionFailure
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, slog.Default())

	response, failure := caller.Do(context.Background(), executionCall())

	if response != nil {
		t.Errorf("Do() response = %#v, want nil", response)
	}
	if failure == nil {
		t.Fatal("Do() failure = nil, want classified failure")
	}
	if failure.Kind != apierror.BadGateway || failure.Message != "could not reach the upstream" {
		t.Errorf("Do() failure = (%v, %q), want BadGateway execution failure", failure.Kind, failure.Message)
	}
	if !errors.Is(failure.Err, executionFailure) {
		t.Errorf("Do() failure.Err = %v, want wrapping %v", failure.Err, executionFailure)
	}
}

func TestCallerDoCorrelatesSuccessfulResponse(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := executionResponse(request, http.StatusOK, http.NoBody)
		response.Header.Set(RequestIDHeader, "upstream-request-id")
		return response, nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, logger)
	ctx := logging.WithRequestID(context.Background(), "copilotd-request-id")

	response, failure := caller.Do(ctx, executionCall())

	if failure != nil || response == nil {
		t.Fatalf("Do() = (%#v, %#v), want response and no failure", response, failure)
	}
	for _, want := range []string{
		"request_id=copilotd-request-id",
		"upstream_request_id=upstream-request-id",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("correlation log = %q, want %q", logs.String(), want)
		}
	}
}

func TestCallerWithIdentityManagerLogsOnlySanitizedCredentialFailureCause(t *testing.T) {
	const (
		oauthSecret   = "gho-raw-secret"
		rawMintDetail = "raw-mint-body-must-not-leak"
	)
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, rawMintDetail)
	}))
	defer exchange.Close()

	var logs bytes.Buffer
	logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	manager := identity.NewManager(identity.ManagerConfig{
		OAuthToken:    oauthSecret,
		GitHubBaseURL: exchange.URL,
		HTTPClient:    exchange.Client(),
		Logger:        logger,
	})
	caller := executionCaller(manager, exchange.Client(), time.Second, 1<<20, logger)

	response, failure := caller.Do(context.Background(), executionCall())

	if response != nil {
		t.Errorf("Do() response = %#v, want nil", response)
	}
	if failure == nil || failure.Kind != apierror.NotReady || failure.Message != "no upstream credential available" {
		t.Fatalf("Do() failure = %#v, want NotReady credential failure", failure)
	}
	const sanitizedCause = "copilot token exchange: status 502"
	if failure.Err == nil || failure.Err.Error() != sanitizedCause {
		t.Errorf("failure.Err = %v, want sanitized provider cause %q", failure.Err, sanitizedCause)
	}
	logOutput := logs.String()
	if got := strings.Count(logOutput, sanitizedCause); got != 1 {
		t.Errorf("sanitized provider cause occurrences = %d, want 1: %q", got, logOutput)
	}
	for _, secret := range []string{oauthSecret, rawMintDetail} {
		if strings.Contains(logOutput, secret) {
			t.Errorf("log output leaked raw mint detail %q: %q", secret, logOutput)
		}
	}
}

func TestReadBoundedAndCallerFormAgree(t *testing.T) {
	readFailure := errors.New("response stream failed")
	tests := []struct {
		name        string
		max         int64
		reader      func() io.Reader
		wantBody    string
		wantKind    apierror.Kind
		wantMessage string
		wantErr     error
	}{
		{
			name:     "under cap",
			max:      8,
			reader:   func() io.Reader { return strings.NewReader("1234567") },
			wantBody: "1234567",
		},
		{
			name:     "at cap",
			max:      8,
			reader:   func() io.Reader { return strings.NewReader("12345678") },
			wantBody: "12345678",
		},
		{
			name:        "over cap",
			max:         8,
			reader:      func() io.Reader { return strings.NewReader("123456789") },
			wantKind:    apierror.BadGateway,
			wantMessage: "upstream response body exceeds the maximum allowed size",
		},
		{
			name: "read error",
			max:  8,
			reader: func() io.Reader {
				return io.MultiReader(strings.NewReader("partial"), executionTerminalErrorReader{err: readFailure})
			},
			wantKind:    apierror.BadGateway,
			wantMessage: "could not read the upstream response",
			wantErr:     readFailure,
		},
		{
			name:     "maximum cap does not overflow",
			max:      math.MaxInt64,
			reader:   func() io.Reader { return strings.NewReader("unbounded") },
			wantBody: "unbounded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := executionCaller(nil, nil, 0, tc.max, slog.Default())
			freeBody, freeFailure := ReadBounded(tc.reader(), tc.max)
			callerBody, callerFailure := caller.ReadBounded(tc.reader())

			assertBoundedResult(t, "free ReadBounded", freeBody, freeFailure, tc.wantBody, tc.wantKind, tc.wantMessage, tc.wantErr)
			assertBoundedResult(t, "Caller.ReadBounded", callerBody, callerFailure, tc.wantBody, tc.wantKind, tc.wantMessage, tc.wantErr)
			if string(freeBody) != string(callerBody) {
				t.Errorf("bounded forms disagree on body: free=%q caller=%q", freeBody, callerBody)
			}
			if !sameFailure(freeFailure, callerFailure) {
				t.Errorf("bounded forms disagree on failure: free=%#v caller=%#v", freeFailure, callerFailure)
			}
		})
	}
}

func TestCallerReadBoundedClassifiesTheBoundUpstreamCallContext(t *testing.T) {
	tests := []struct {
		name           string
		cause          error
		wantKind       apierror.Kind
		wantMessage    string
		wantClientGone bool
		wantStatus     int
	}{
		{
			name:           "inbound cancellation is ClientGone",
			cause:          context.Canceled,
			wantClientGone: true,
		},
		{
			name:        "caller-owned deadline cause is GatewayTimeout",
			cause:       context.DeadlineExceeded,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
			wantStatus:  http.StatusGatewayTimeout,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
			if err != nil {
				t.Fatalf("build logger: %v", err)
			}
			upstreamBody := &executionCancelAwareBody{blockAfterChunks: true}
			client := executionBodyClient(upstreamBody, make(http.Header))
			caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Hour, 1<<20, logger)
			ctx, cancel := context.WithCancelCause(context.Background())
			response, failure := caller.Do(ctx, executionCall())
			if failure != nil {
				t.Fatalf("Do() failure = %#v, want response", failure)
			}
			cancel(tc.cause)

			body, failure := caller.ReadBounded(response.Body)
			_ = response.Body.Close()

			if body != nil {
				t.Errorf("ReadBounded() body = %q, want nil", body)
			}
			if failure == nil {
				t.Fatal("ReadBounded() failure = nil, want classified failure")
			}
			if failure.Kind != tc.wantKind || failure.Message != tc.wantMessage || failure.ClientGone != tc.wantClientGone {
				t.Errorf("ReadBounded() failure = (%v, %q, ClientGone=%v), want (%v, %q, ClientGone=%v)", failure.Kind, failure.Message, failure.ClientGone, tc.wantKind, tc.wantMessage, tc.wantClientGone)
			}
			if !errors.Is(failure.Err, context.Canceled) {
				t.Errorf("failure.Err = %v, want interrupted-read context cancellation", failure.Err)
			}
			if got := strings.Count(logs.String(), "upstream call failed"); got != 1 {
				t.Errorf("failure log records = %d, want 1: %q", got, logs.String())
			}

			recorder := httptest.NewRecorder()
			recorder.Code = 0
			wrote := failure.RespondTo(recorder, endpoint.OpenAI)
			if tc.wantClientGone {
				if wrote || recorder.Code != 0 || recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
					t.Errorf("ClientGone response = wrote %v status %d headers %v body %q, want no write", wrote, recorder.Code, recorder.Header(), recorder.Body.String())
				}
			} else if !wrote || recorder.Code != tc.wantStatus {
				t.Errorf("timeout response = wrote %v status %d, want wrote true status %d", wrote, recorder.Code, tc.wantStatus)
			}
		})
	}
}

func TestCallerReadBoundedKeepsOverCapClassificationAcrossBoundContextCauses(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{name: "inbound cancellation", cause: context.Canceled},
		{name: "caller-owned deadline", cause: context.DeadlineExceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.cause)
			body := &contextBoundResponseBody{
				ReadCloser: io.NopCloser(strings.NewReader("123456789")),
				ctx:        ctx,
			}
			caller := executionCaller(nil, nil, 0, 8, slog.Default())

			contents, failure := caller.ReadBounded(body)

			if contents != nil {
				t.Errorf("ReadBounded() body = %q, want nil", contents)
			}
			if failure == nil {
				t.Fatal("ReadBounded() failure = nil, want over-cap failure")
			}
			if failure.Kind != apierror.BadGateway || failure.Message != "upstream response body exceeds the maximum allowed size" || failure.ClientGone {
				t.Errorf("ReadBounded() failure = (%v, %q, ClientGone=%v), want (BadGateway, over-cap message, false)", failure.Kind, failure.Message, failure.ClientGone)
			}

			recorder := httptest.NewRecorder()
			if wrote := failure.RespondTo(recorder, endpoint.OpenAI); !wrote || recorder.Code != http.StatusBadGateway {
				t.Errorf("RespondTo() = wrote %v status %d, want wrote true status 502", wrote, recorder.Code)
			}
		})
	}
}

func TestReadBoundedProbesOnlyOneByteBeyondCap(t *testing.T) {
	reader := &executionByteCountingReader{reader: strings.NewReader("0123456789abcdef")}

	body, failure := ReadBounded(reader, 8)

	if body != nil {
		t.Errorf("ReadBounded() body = %q, want nil", body)
	}
	if failure == nil || failure.Message != "upstream response body exceeds the maximum allowed size" {
		t.Fatalf("ReadBounded() failure = %#v, want over-cap failure", failure)
	}
	if reader.bytesRead != 9 {
		t.Errorf("bytes read = %d, want cap+1 probe of 9", reader.bytesRead)
	}
}

func TestCallerBufferedReturnsOneCredentialedResponse(t *testing.T) {
	const responseBody = `{"data":[{"id":"model-one"}]}`
	const route endpoint.Route = "/contract-models"
	var calls int
	var gotRequest *http.Request
	upstreamBody := &executionObservedReadCloser{reader: strings.NewReader(responseBody)}
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		gotRequest = request.Clone(request.Context())
		gotRequest.Header = request.Header.Clone()
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("upstream request body = %q, want empty", body)
		}
		return executionResponse(request, http.StatusAccepted, upstreamBody), nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, slog.Default())
	ctx := logging.WithRequestID(context.Background(), "catalog-request-id")

	status, body, failure := caller.Buffered(ctx, Call{
		Route:                  route,
		Method:                 http.MethodGet,
		AcceptIdentityEncoding: true,
	})

	if failure != nil {
		t.Fatalf("Buffered() failure = %#v", failure)
	}
	if status != http.StatusAccepted || string(body) != responseBody {
		t.Errorf("Buffered() = (%d, %q), want (%d, %q)", status, body, http.StatusAccepted, responseBody)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", calls)
	}
	if gotRequest.Method != http.MethodGet || gotRequest.URL.Path != string(route) || gotRequest.URL.RawQuery != "" {
		t.Errorf("upstream request = %s %q, want GET %s without query", gotRequest.Method, gotRequest.URL.RequestURI(), route)
	}
	for name, want := range (http.Header{
		"Authorization":          {"Bearer copilot-token"},
		"Accept-Encoding":        {"identity"},
		"Copilot-Integration-Id": {"vscode-chat"},
		"Editor-Version":         {"vscode/1.104.1"},
		RequestIDHeader:          {"catalog-request-id"},
	}) {
		if got := gotRequest.Header.Values(name); !equalStrings(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestCallerBufferedClassifiesSetupAndExecutionFailures(t *testing.T) {
	credentialProvider := readyExecutionProvider("https://upstream.invalid")
	credentialCause := errors.New("credential unavailable")
	credentialProvider.SetError(credentialCause)
	executionCause := errors.New("dial failed")
	tests := []struct {
		name        string
		provider    identity.Provider
		execute     func(*http.Request) (*http.Response, error)
		wantCalls   int
		wantKind    apierror.Kind
		wantMessage string
		wantErr     error
	}{
		{
			name:        "missing credential",
			provider:    credentialProvider,
			wantKind:    apierror.NotReady,
			wantMessage: "no upstream credential available",
			wantErr:     credentialCause,
		},
		{
			name:        "request construction failure",
			provider:    readyExecutionProvider(":"),
			wantKind:    apierror.BadGateway,
			wantMessage: "could not build the upstream request",
		},
		{
			name:     "unreachable upstream",
			provider: readyExecutionProvider("https://upstream.invalid"),
			execute: func(*http.Request) (*http.Response, error) {
				return nil, executionCause
			},
			wantCalls:   1,
			wantKind:    apierror.BadGateway,
			wantMessage: "could not reach the upstream",
			wantErr:     executionCause,
		},
		{
			name:     "upstream timeout",
			provider: readyExecutionProvider("https://upstream.invalid"),
			execute: func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			},
			wantCalls:   1,
			wantKind:    apierror.GatewayTimeout,
			wantMessage: "the upstream request timed out",
			wantErr:     context.DeadlineExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if tc.execute == nil {
					return nil, errors.New("must not be called")
				}
				return tc.execute(request)
			})}
			caller := executionCaller(tc.provider, client, time.Second, 1<<20, slog.Default())

			status, body, failure := caller.Buffered(context.Background(), executionCall())

			if status != 0 || body != nil {
				t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
			}
			if failure == nil {
				t.Fatal("failure = nil, want classified failure")
			}
			if failure.Kind != tc.wantKind || failure.Message != tc.wantMessage {
				t.Errorf("failure = (%v, %q), want (%v, %q)", failure.Kind, failure.Message, tc.wantKind, tc.wantMessage)
			}
			if tc.wantErr != nil && !errors.Is(failure.Err, tc.wantErr) {
				t.Errorf("failure.Err = %v, want wrapping %v", failure.Err, tc.wantErr)
			}
			if failure.Err == nil {
				t.Error("failure.Err = nil, want underlying cause")
			}
			if calls != tc.wantCalls {
				t.Errorf("upstream calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestCallerBufferedUsesConfiguredResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Millisecond}}
	caller := executionCaller(readyExecutionProvider(server.URL), client, time.Second, 1<<20, slog.Default())

	status, body, failure := caller.Buffered(context.Background(), executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || failure.Kind != apierror.GatewayTimeout || failure.Message != "the upstream request timed out" {
		t.Fatalf("Buffered() failure = %#v, want GatewayTimeout", failure)
	}
}

func TestCallerBufferedArmsTimeoutAfterResponseAndInterruptsStalledRead(t *testing.T) {
	const outboundTimeout = 10 * time.Millisecond
	upstreamBody := &executionCancelAwareBody{blockAfterChunks: true}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Context() == parent {
			t.Error("Buffered request uses parent context directly, want pre-Do derived context")
		}
		time.Sleep(4 * outboundTimeout)
		if err := request.Context().Err(); err != nil {
			return nil, errors.New("outbound timer armed before response")
		}
		upstreamBody.bind(request.Context())
		return executionResponse(request, http.StatusOK, upstreamBody), nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, outboundTimeout, 1<<20, slog.Default())
	started := time.Now()

	status, body, failure := caller.Buffered(parent, executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || failure.Kind != apierror.GatewayTimeout || failure.Message != "the upstream request timed out" || failure.ClientGone {
		t.Fatalf("Buffered() failure = %#v, want GatewayTimeout", failure)
	}
	if !errors.Is(failure.Err, context.Canceled) {
		t.Errorf("failure.Err = %v, want interrupted-read context cancellation", failure.Err)
	}
	if elapsed := time.Since(started); elapsed < 4*outboundTimeout || elapsed >= time.Second {
		t.Errorf("Buffered() elapsed = %v, want response delay plus post-response timeout before parent deadline", elapsed)
	}
	assertExecutionBodyCleanup(t, upstreamBody, true)
}

func TestCallerBufferedClassifiesParentCancellationDuringReadAsClientGone(t *testing.T) {
	firstRead := make(chan struct{})
	upstreamBody := &executionCancelAwareBody{
		chunks:           [][]byte{[]byte(`{"data":[`)},
		blockAfterChunks: true,
		firstRead:        firstRead,
	}
	client := executionBodyClient(upstreamBody, make(http.Header))
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Hour, 1<<20, slog.Default())
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstRead
		cancel()
	}()

	status, body, failure := caller.Buffered(parent, executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || !failure.ClientGone {
		t.Fatalf("Buffered() failure = %#v, want ClientGone", failure)
	}
	if failure.Message != "" {
		t.Errorf("ClientGone message = %q, want empty", failure.Message)
	}
	if !errors.Is(failure.Err, context.Canceled) {
		t.Errorf("failure.Err = %v, want context.Canceled", failure.Err)
	}
	assertExecutionBodyCleanup(t, upstreamBody, true)
}

func TestCallerBufferedClassifiesPlainReadFailureWithoutPartialBody(t *testing.T) {
	readFailure := errors.New("response stream failed")
	upstreamBody := &executionObservedReadCloser{reader: io.MultiReader(
		strings.NewReader(`{"data":[`),
		executionTerminalErrorReader{err: readFailure},
	)}
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return executionResponse(request, http.StatusOK, upstreamBody), nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, slog.Default())

	status, body, failure := caller.Buffered(context.Background(), executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || failure.Kind != apierror.BadGateway || failure.Message != "could not read the upstream response" {
		t.Fatalf("Buffered() failure = %#v, want read failure", failure)
	}
	if failure.Err != readFailure {
		t.Errorf("failure.Err = %v, want exact read error %v", failure.Err, readFailure)
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestCallerBufferedRejectsOversizedResponseWithoutReturningTruncatedBody(t *testing.T) {
	upstreamBody := &executionByteCountingReadCloser{reader: strings.NewReader("0123456789abcdef")}
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return executionResponse(request, http.StatusOK, upstreamBody), nil
	})}
	const responseLimit = 8
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, responseLimit, slog.Default())

	status, body, failure := caller.Buffered(context.Background(), executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || failure.Kind != apierror.BadGateway || failure.Message != "upstream response body exceeds the maximum allowed size" {
		t.Fatalf("Buffered() failure = %#v, want over-cap BadGateway", failure)
	}
	if upstreamBody.bytesRead != responseLimit+1 {
		t.Errorf("upstream bytes read = %d, want bounded probe of %d", upstreamBody.bytesRead, responseLimit+1)
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestCallerBufferedAcceptsResponseAtSizeBoundary(t *testing.T) {
	const responseBody = "12345678"
	upstreamBody := &executionObservedReadCloser{reader: strings.NewReader(responseBody)}
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return executionResponse(request, http.StatusOK, upstreamBody), nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, int64(len(responseBody)), slog.Default())

	status, body, failure := caller.Buffered(context.Background(), executionCall())

	if failure != nil {
		t.Fatalf("Buffered() failure = %#v", failure)
	}
	if status != http.StatusOK || string(body) != responseBody {
		t.Errorf("result = (%d, %q), want (%d, %q)", status, body, http.StatusOK, responseBody)
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestCallerBufferedLeavesSSELookingBodyOpaque(t *testing.T) {
	const responseBody = "event: model\ndata: opaque-catalog-bytes\n\n"
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := executionResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(responseBody)))
		response.Header.Set("Content-Type", "text/event-stream")
		return response, nil
	})}
	caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, slog.Default())

	status, body, failure := caller.Buffered(context.Background(), executionCall())

	if failure != nil {
		t.Fatalf("Buffered() failure = %#v", failure)
	}
	if status != http.StatusOK || string(body) != responseBody {
		t.Errorf("result = (%d, %q), want raw SSE-looking response (%d, %q)", status, body, http.StatusOK, responseBody)
	}
}

func TestCallerBufferedMakesIndependentUpstreamCalls(t *testing.T) {
	provider := &executionCountingProvider{credential: identity.Credential{
		BaseURL: "https://upstream.invalid",
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}}
	responses := []string{`{"data":[{"id":"first"}]}`, `{"data":[{"id":"second"}]}`}
	var calls int
	client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := responses[calls]
		calls++
		return executionResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(body))), nil
	})}
	caller := executionCaller(provider, client, time.Second, 1<<20, slog.Default())

	_, first, firstFailure := caller.Buffered(context.Background(), executionCall())
	_, second, secondFailure := caller.Buffered(context.Background(), executionCall())

	if firstFailure != nil || secondFailure != nil {
		t.Fatalf("Buffered() failures = %#v, %#v", firstFailure, secondFailure)
	}
	if got := string(first); got != responses[0] {
		t.Errorf("first body = %q, want %q", got, responses[0])
	}
	if got := string(second); got != responses[1] {
		t.Errorf("second body = %q, want %q", got, responses[1])
	}
	if calls != 2 || provider.calls != 2 {
		t.Errorf("upstream/provider calls = %d/%d, want 2/2", calls, provider.calls)
	}
}

func TestCallerBufferedDoesNotReplayAfterUpstreamResponseFailure(t *testing.T) {
	var transportAttempts atomic.Int32
	var modelsCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		modelsCalls.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		transportAttempts.Add(1)
	}}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	caller := executionCaller(readyExecutionProvider(server.URL), &http.Client{}, time.Second, 1<<20, slog.Default())

	status, body, failure := caller.Buffered(ctx, executionCall())

	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if failure == nil || failure.Kind != apierror.BadGateway || failure.Message != "could not reach the upstream" {
		t.Fatalf("Buffered() failure = %#v, want unreachable failure", failure)
	}
	if got := transportAttempts.Load(); got != 1 {
		t.Errorf("transport attempts = %d, want exactly 1 with no transparent replay", got)
	}
	if got := modelsCalls.Load(); got != 1 {
		t.Errorf("upstream /models calls = %d, want exactly 1", got)
	}
}

func TestCallerBufferedLogsOnlyDifferentUpstreamRequestID(t *testing.T) {
	const requestID = "resolved-catalog-request-id"
	tests := []struct {
		name              string
		upstreamRequestID string
		wantCorrelation   bool
	}{
		{name: "different", upstreamRequestID: "upstream-catalog-request-id", wantCorrelation: true},
		{name: "identical", upstreamRequestID: requestID},
		{name: "absent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
			if err != nil {
				t.Fatalf("build logger: %v", err)
			}
			client := &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := executionResponse(request, http.StatusOK, http.NoBody)
				if tc.upstreamRequestID != "" {
					response.Header.Set(RequestIDHeader, tc.upstreamRequestID)
				}
				return response, nil
			})}
			caller := executionCaller(readyExecutionProvider("https://upstream.invalid"), client, time.Second, 1<<20, logger)
			ctx := logging.WithRequestID(context.Background(), requestID)

			if _, _, failure := caller.Buffered(ctx, executionCall()); failure != nil {
				t.Fatalf("Buffered() failure = %#v", failure)
			}

			logOutput := logs.String()
			if tc.wantCorrelation {
				if !strings.Contains(logOutput, "upstream_request_id="+tc.upstreamRequestID) || !strings.Contains(logOutput, "request_id="+requestID) {
					t.Errorf("correlation log = %q, want upstream and resolved request IDs", logOutput)
				}
			} else if strings.Contains(logOutput, "upstream_request_id=") {
				t.Errorf("absent or identical upstream ID produced a correlation log: %q", logOutput)
			}
		})
	}
}

func assertBoundedResult(t *testing.T, form string, body []byte, failure *Failure, wantBody string, wantKind apierror.Kind, wantMessage string, wantErr error) {
	t.Helper()
	if wantMessage == "" {
		if failure != nil {
			t.Errorf("%s failure = %#v, want nil", form, failure)
		}
		if got := string(body); got != wantBody {
			t.Errorf("%s body = %q, want %q", form, got, wantBody)
		}
		return
	}
	if body != nil {
		t.Errorf("%s body = %q, want nil", form, body)
	}
	if failure == nil {
		t.Errorf("%s failure = nil, want classified failure", form)
		return
	}
	if failure.Kind != wantKind || failure.Message != wantMessage || failure.ClientGone {
		t.Errorf("%s failure = (%v, %q, ClientGone=%v), want (%v, %q, false)", form, failure.Kind, failure.Message, failure.ClientGone, wantKind, wantMessage)
	}
	if wantErr != nil && !errors.Is(failure.Err, wantErr) {
		t.Errorf("%s failure.Err = %v, want wrapping %v", form, failure.Err, wantErr)
	}
	if failure.Err == nil {
		t.Errorf("%s failure.Err = nil, want underlying cause", form)
	}
}

func sameFailure(left, right *Failure) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Kind == right.Kind &&
		left.Message == right.Message &&
		left.ClientGone == right.ClientGone &&
		left.Err.Error() == right.Err.Error()
}

func executionCall() Call {
	return Call{
		Route:                  endpoint.RouteModels,
		Method:                 http.MethodGet,
		AcceptIdentityEncoding: true,
	}
}

func executionCaller(provider identity.Provider, client *http.Client, outboundTimeout time.Duration, maxBufferedBytes int64, logger *slog.Logger) *Caller {
	return &Caller{
		provider:         provider,
		client:           client,
		logger:           logger,
		outboundTimeout:  outboundTimeout,
		maxBufferedBytes: maxBufferedBytes,
	}
}

func readyExecutionProvider(baseURL string) *identity.Static {
	return identity.NewStatic(identity.Credential{
		BaseURL: baseURL,
		Token:   "copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
}

type executionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f executionRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func executionResponse(request *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       body,
		Request:    request,
	}
}

type executionTerminalErrorReader struct {
	err error
}

func (r executionTerminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

type executionByteCountingReader struct {
	reader    io.Reader
	bytesRead int
}

func (r *executionByteCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

type executionObservedReadCloser struct {
	reader io.Reader
	closed bool
}

func (r *executionObservedReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *executionObservedReadCloser) Close() error {
	r.closed = true
	return nil
}

type executionByteCountingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (r *executionByteCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *executionByteCountingReadCloser) Close() error {
	r.closed = true
	return nil
}

type executionCancelAwareBody struct {
	mu               sync.Mutex
	ctx              context.Context
	chunks           [][]byte
	terminal         error
	blockAfterChunks bool
	firstRead        chan struct{}
	firstReadOnce    sync.Once
	closed           bool
	canceledAtClose  bool
}

func (b *executionCancelAwareBody) bind(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctx = ctx
}

func (b *executionCancelAwareBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.chunks) > 0 {
		chunk := b.chunks[0]
		b.chunks = b.chunks[1:]
		b.mu.Unlock()
		n := copy(p, chunk)
		if n < len(chunk) {
			b.mu.Lock()
			b.chunks = append([][]byte{append([]byte(nil), chunk[n:]...)}, b.chunks...)
			b.mu.Unlock()
		}
		if b.firstRead != nil {
			b.firstReadOnce.Do(func() { close(b.firstRead) })
		}
		return n, nil
	}
	ctx := b.ctx
	block := b.blockAfterChunks
	terminal := b.terminal
	b.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if terminal != nil {
		return 0, terminal
	}
	return 0, io.EOF
}

func (b *executionCancelAwareBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.canceledAtClose = b.ctx != nil && b.ctx.Err() != nil
	return nil
}

func executionBodyClient(body *executionCancelAwareBody, header http.Header) *http.Client {
	return &http.Client{Transport: executionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body.bind(request.Context())
		response := executionResponse(request, http.StatusOK, body)
		response.Header = header
		return response, nil
	})}
}

func assertExecutionBodyCleanup(t *testing.T, body *executionCancelAwareBody, wantCanceledAtClose bool) {
	t.Helper()
	body.mu.Lock()
	defer body.mu.Unlock()
	if !body.closed {
		t.Error("upstream response body remains open")
	}
	if body.canceledAtClose != wantCanceledAtClose {
		t.Errorf("outbound context canceled at body close = %t, want %t", body.canceledAtClose, wantCanceledAtClose)
	}
}

type executionCountingProvider struct {
	credential identity.Credential
	calls      int
}

func (p *executionCountingProvider) Current(context.Context) (identity.Credential, error) {
	p.calls++
	return p.credential, nil
}

func (*executionCountingProvider) Ready() bool { return true }

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
