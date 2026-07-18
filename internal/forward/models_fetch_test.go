package forward

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
)

func TestFetchModelsReturnsOneCredentialedCatalogResponse(t *testing.T) {
	const responseBody = `{"data":[{"id":"model-one"}]}`
	var calls int
	var gotRequest *http.Request
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotRequest = r.Clone(r.Context())
		gotRequest.Header = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if len(body) != 0 {
			t.Errorf("upstream request body = %q, want empty", body)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     make(http.Header),
			Body:       io.NopCloser(io.LimitReader(&repeatingReader{value: responseBody}, int64(len(responseBody)))),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)
	ctx := logging.WithRequestID(context.Background(), "catalog-request-id")

	status, body, err := f.FetchModels(ctx)

	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if status != http.StatusAccepted {
		t.Errorf("status = %d, want %d", status, http.StatusAccepted)
	}
	if got := string(body); got != responseBody {
		t.Errorf("body = %q, want %q", got, responseBody)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", calls)
	}
	if gotRequest.Method != http.MethodGet || gotRequest.URL.Path != "/models" || gotRequest.URL.RawQuery != "" {
		t.Errorf("upstream request = %s %q, want GET /models without query", gotRequest.Method, gotRequest.URL.RequestURI())
	}
	for name, want := range (http.Header{
		"Authorization":          {"Bearer copilot-token"},
		"Accept-Encoding":        {"identity"},
		"Copilot-Integration-Id": {"vscode-chat"},
		"Editor-Version":         {"vscode/1.104.1"},
		"X-Request-Id":           {"catalog-request-id"},
	}) {
		if got := gotRequest.Header.Values(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestFetchModelsClassifiesMissingCredentialWithoutCallingUpstream(t *testing.T) {
	provider := readyStub("https://upstream.invalid")
	provider.SetError(errors.New("credential unavailable"))
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})}
	f := New(provider, client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrNoCredential) {
		t.Fatalf("FetchModels() error = %v, want ErrNoCredential", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if calls != 0 {
		t.Errorf("upstream calls = %d, want 0", calls)
	}
}

func TestFetchModelsClassifiesRequestConstructionFailure(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not be called")
	})}
	f := New(readyStub(":"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrBuildUpstream) {
		t.Fatalf("FetchModels() error = %v, want ErrBuildUpstream", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if calls != 0 {
		t.Errorf("upstream calls = %d, want 0", calls)
	}
}

func TestFetchModelsClassifiesUnreachableUpstream(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("dial failed")
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamUnreachable) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamUnreachable", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want exactly 1", calls)
	}
}

func TestFetchModelsClassifiesUpstreamTimeout(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamTimeout) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamTimeout", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
}

func TestFetchModelsUsesConfiguredResponseHeaderTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer upstream.Close()
	f := New(readyStub(upstream.URL), NewClient(10*time.Millisecond), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamTimeout) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamTimeout", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
}

func TestFetchModelsClassifiesOutboundBodyTimeout(t *testing.T) {
	body := &cancelAwareBody{blockAfterChunks: true}
	f := New(readyStub("https://upstream.invalid"), bodyClient(body, make(http.Header)), 10*time.Millisecond, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, responseBody, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamTimeout) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamTimeout", err)
	}
	if errors.Is(err, catalog.ErrUpstreamRead) {
		t.Errorf("FetchModels() error = %v, must not classify local timeout as ErrUpstreamRead", err)
	}
	if status != 0 || responseBody != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, responseBody)
	}
	assertBodyCleanup(t, body, true)
}

func TestFetchModelsClassifiesResponseReadFailureWithoutPartialBody(t *testing.T) {
	readFailure := errors.New("response stream failed")
	upstreamBody := &observedReadCloser{reader: io.MultiReader(
		strings.NewReader(`{"data":[`),
		terminalErrorReader{err: readFailure},
	)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       upstreamBody,
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamRead) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamRead", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestFetchModelsPropagatesCallerCancellationDuringResponseRead(t *testing.T) {
	prefixRead := make(chan struct{})
	upstreamBody := &cancelAwareBody{
		chunks:           [][]byte{[]byte(`{"data":[`)},
		blockAfterChunks: true,
		firstRead:        prefixRead,
	}
	f := New(readyStub("https://upstream.invalid"), bodyClient(upstreamBody, make(http.Header)), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-prefixRead
		cancel()
	}()

	status, body, err := f.FetchModels(ctx)

	if !errors.Is(err, catalog.ErrUpstreamRead) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamRead", err)
	}
	if errors.Is(err, catalog.ErrUpstreamTimeout) {
		t.Errorf("FetchModels() error = %v, caller cancellation must not be classified as timeout", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	assertBodyCleanup(t, upstreamBody, true)
}

func TestFetchModelsRejectsOversizedResponseWithoutReturningTruncatedBody(t *testing.T) {
	upstreamBody := &byteCountingReadCloser{reader: strings.NewReader("0123456789abcdef")}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       upstreamBody,
			Request:    r,
		}, nil
	})}
	const responseLimit = 8
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, responseLimit, nil)

	status, body, err := f.FetchModels(context.Background())

	if !errors.Is(err, catalog.ErrUpstreamRead) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamRead", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if upstreamBody.bytesRead != responseLimit+1 {
		t.Errorf("upstream bytes read = %d, want bounded probe of %d", upstreamBody.bytesRead, responseLimit+1)
	}
	if !upstreamBody.closed {
		t.Error("upstream response body remains open")
	}
}

func TestFetchModelsAcceptsResponseAtSizeBoundary(t *testing.T) {
	const responseBody = "12345678"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, int64(len(responseBody)), nil)

	status, body, err := f.FetchModels(context.Background())

	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if status != http.StatusOK || string(body) != responseBody {
		t.Errorf("result = (%d, %q), want (%d, %q)", status, body, http.StatusOK, responseBody)
	}
}

func TestFetchModelsLeavesSSEAndShimsOutOfCatalogFetch(t *testing.T) {
	const responseBody = "event: model\ndata: opaque-catalog-bytes\n\n"
	var shimCalls int
	registry := shim.Registry{{
		Name:    "must-not-run",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			shimCalls++
			return struct{}{}
		},
	}}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    r,
		}, nil
	})}
	f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, registry)

	status, body, err := f.FetchModels(context.Background())

	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if status != http.StatusOK || string(body) != responseBody {
		t.Errorf("result = (%d, %q), want raw SSE-looking response (%d, %q)", status, body, http.StatusOK, responseBody)
	}
	if shimCalls != 0 {
		t.Errorf("shim construction calls = %d, want 0", shimCalls)
	}
}

type terminalErrorReader struct {
	err error
}

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

type byteCountingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (r *byteCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *byteCountingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestFetchModelsConsecutiveCallsFetchIndependentResponses(t *testing.T) {
	provider := &countingProvider{cred: identity.Credential{
		BaseURL: "https://upstream.invalid",
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}}
	responses := []string{`{"data":[{"id":"first"}]}`, `{"data":[{"id":"second"}]}`}
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := responses[calls]
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(&repeatingReader{value: body}),
			Request:    r,
		}, nil
	})}
	f := New(provider, client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	_, first, firstErr := f.FetchModels(context.Background())
	_, second, secondErr := f.FetchModels(context.Background())

	if firstErr != nil || secondErr != nil {
		t.Fatalf("FetchModels() errors = %v, %v", firstErr, secondErr)
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

func TestFetchModelsDoesNotReplayAfterUpstreamResponseFailure(t *testing.T) {
	var transportAttempts atomic.Int32
	var modelsCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		modelsCalls.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()

	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		transportAttempts.Add(1)
	}}
	ctx := httptrace.WithClientTrace(context.Background(), trace)
	f := New(readyStub(upstream.URL), NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)

	status, body, err := f.FetchModels(ctx)

	if !errors.Is(err, catalog.ErrUpstreamUnreachable) {
		t.Fatalf("FetchModels() error = %v, want ErrUpstreamUnreachable", err)
	}
	if status != 0 || body != nil {
		t.Errorf("failure result = (%d, %q), want zero status and nil body", status, body)
	}
	if got := transportAttempts.Load(); got != 1 {
		t.Errorf("transport attempts = %d, want exactly 1 with no transparent replay", got)
	}
	if got := modelsCalls.Load(); got != 1 {
		t.Errorf("upstream /models calls = %d, want exactly 1", got)
	}
}

func TestFetchModelsLogsOnlyDifferentUpstreamRequestID(t *testing.T) {
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
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				header := make(http.Header)
				if tc.upstreamRequestID != "" {
					header.Set("X-Request-Id", tc.upstreamRequestID)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: http.NoBody, Request: r}, nil
			})}
			f := New(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil, WithLogger(logger))
			ctx := logging.WithRequestID(context.Background(), requestID)

			if _, _, err := f.FetchModels(ctx); err != nil {
				t.Fatalf("FetchModels() error = %v", err)
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

type repeatingReader struct {
	value  string
	offset int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.value) {
		return 0, io.EOF
	}
	n := copy(p, r.value[r.offset:])
	r.offset += n
	return n, nil
}
