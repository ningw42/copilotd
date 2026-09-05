package forward

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/usage"
)

type forwardUsageSink struct {
	mu    sync.Mutex
	turns []usage.Turn
}

func (s *forwardUsageSink) Record(turn usage.Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, turn)
}

func (s *forwardUsageSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}

func (s *forwardUsageSink) snapshot() []usage.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usage.Turn(nil), s.turns...)
}

func enabledUsageRegistry(sink usage.Sink) shim.Registry {
	registry := shim.CanonicalRegistry(sink)
	registry[len(registry)-1].Enabled = true
	return registry
}

const (
	qualifyingBufferedOpenAIResponse    = `{"id":"resp-forward","model":"reported-model","status":"completed","usage":{"input_tokens":12,"output_tokens":6}}`
	qualifyingBufferedAnthropicResponse = `{"id":"msg-forward","type":"message","model":"reported-model","stop_reason":"future-reason","usage":{"input_tokens":12,"output_tokens":6}}`
)

var bufferedUsageSurfaces = []struct {
	name     string
	endpoint endpoint.HTTPForward
	path     string
	response string
}{
	{name: "OpenAI Responses", endpoint: endpoint.OpenAIResponsesHTTP(), path: "/openai/v1/responses", response: qualifyingBufferedOpenAIResponse},
	{name: "Anthropic Messages", endpoint: endpoint.AnthropicMessages(), path: "/anthropic/v1/messages", response: qualifyingBufferedAnthropicResponse},
}

func TestForwardOpenAIUsageMeterSelectsTransportByEndpointAndUpstreamContentType(t *testing.T) {
	const completedEvent = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sse\",\"model\":\"reported-model\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":6}}}\n\n"
	tests := []struct {
		name        string
		requestBody string
		contentType string
		response    string
		want        usage.Transport
	}{
		{name: "SSE without inbound stream flag", requestBody: `{"stream":false}`, contentType: "text/event-stream", response: completedEvent, want: usage.TransportSSE},
		{name: "buffered despite inbound stream flag", requestBody: `{"stream":true}`, contentType: "application/json", response: qualifyingBufferedOpenAIResponse, want: usage.TransportBuffered},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {test.contentType}}, Body: io.NopCloser(strings.NewReader(test.response)), Request: r}, nil
			})}
			sink := &forwardUsageSink{}
			forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
			recorder := newDeadlineRecorder()

			forwarder.Handler(endpoint.OpenAIResponsesHTTP())(recorder, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(test.requestBody)))

			if recorder.Code != http.StatusOK || recorder.Body.String() != test.response {
				t.Errorf("response = %d %q, want unchanged %d %q", recorder.Code, recorder.Body.String(), http.StatusOK, test.response)
			}
			turns := sink.snapshot()
			if len(turns) != 1 || turns[0].Transport != test.want {
				t.Errorf("usage Turns = %+v, want one %q observation", turns, test.want)
			}
		})
	}
}

func TestForwardOpenAIUsageMeterRejectsUnsupportedSSEEncodingBeforeObservation(t *testing.T) {
	const completedEvent = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-sse\",\"model\":\"reported-model\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":6}}}\n\n"
	upstreamBody := &observedReadCloser{reader: strings.NewReader(completedEvent)}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}},
			Body:       upstreamBody,
			Request:    r,
		}, nil
	})}
	sink := &forwardUsageSink{}
	forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
	recorder := newDeadlineRecorder()

	forwarder.Handler(endpoint.OpenAIResponsesHTTP())(recorder, httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"stream":true}`)))

	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "unsupported Content-Encoding") {
		t.Errorf("response = %d %q, want pre-hook 502", recorder.Code, recorder.Body.String())
	}
	if upstreamBody.reads != 0 || sink.count() != 0 {
		t.Errorf("upstream reads/usage observations = %d/%d, want 0/0", upstreamBody.reads, sink.count())
	}
}

func TestForwardUsageMeterBuffersEveryNonSSEIdentityResponseByPayload(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		for _, tc := range []struct {
			name        string
			status      int
			contentType string
			wantRecord  bool
		}{
			{name: "successful JSON", status: http.StatusOK, contentType: "application/json", wantRecord: true},
			{name: "error status text content type", status: http.StatusTeapot, contentType: "text/plain", wantRecord: true},
			{name: "non JSON body is still buffered", status: http.StatusBadRequest, contentType: "text/plain"},
		} {
			t.Run(surface.name+"/"+tc.name, func(t *testing.T) {
				bodyText := surface.response
				if tc.name == "non JSON body is still buffered" {
					bodyText = "not json"
				}
				upstreamBody := &observedReadCloser{reader: strings.NewReader(bodyText)}
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}, Body: upstreamBody, Request: r}, nil
				})}
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				recorder := &commitObservingRecorder{deadlineRecorder: newDeadlineRecorder(), body: upstreamBody}

				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))

				if recorder.Code != tc.status || recorder.Body.String() != bodyText {
					t.Errorf("response = %d %q, want unchanged %d %q", recorder.Code, recorder.Body.String(), tc.status, bodyText)
				}
				if recorder.readsAtCommit == 0 {
					t.Error("metered response committed before the required whole-body read")
				}
				want := 0
				if tc.wantRecord {
					want = 1
				}
				if got := sink.count(); got != want {
					t.Errorf("usage records = %d, want %d", got, want)
				}
			})
		}
	}
}

func TestForwardUsageMeterIdentityEncodingPredicateAndOpaqueBypass(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		for _, tc := range []struct {
			name       string
			encodings  []string
			wantRecord bool
		}{
			{name: "absent", wantRecord: true},
			{name: "trimmed case insensitive identity", encodings: []string{"  IdEnTiTy\t"}, wantRecord: true},
			{name: "explicit empty", encodings: []string{""}},
			{name: "repeated identity", encodings: []string{"identity", "identity"}},
			{name: "list", encodings: []string{"identity, gzip"}},
			{name: "unsupported", encodings: []string{"gzip"}},
		} {
			t.Run(surface.name+"/"+tc.name, func(t *testing.T) {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					header := http.Header{"Content-Type": {"application/json"}, "Content-Encoding": append([]string(nil), tc.encodings...)}
					return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(surface.response)), Request: r}, nil
				})}
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				recorder := newDeadlineRecorder()

				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))

				if recorder.Code != http.StatusOK || recorder.Body.String() != surface.response {
					t.Errorf("response = %d %q", recorder.Code, recorder.Body.String())
				}
				want := 0
				if tc.wantRecord {
					want = 1
				}
				if sink.count() != want {
					t.Errorf("records = %d, want %d", sink.count(), want)
				}
			})
		}
	}
}

func TestForwardUsageMeterClassifiesBufferedReadFailureTimeoutAndCancellation(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			t.Run("read failure is 502", func(t *testing.T) {
				body := &cancelAwareBody{terminal: errors.New("read failed")}
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), bodyClient(body, http.Header{"Content-Type": {"application/json"}}), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				recorder := newDeadlineRecorder()
				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
				if recorder.Code != http.StatusBadGateway || sink.count() != 0 {
					t.Errorf("read failure = status %d records %d, want 502/0", recorder.Code, sink.count())
				}
			})

			t.Run("timeout is 504", func(t *testing.T) {
				body := &cancelAwareBody{blockAfterChunks: true}
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), bodyClient(body, http.Header{"Content-Type": {"application/json"}}), 20*time.Millisecond, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				recorder := newDeadlineRecorder()
				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
				if recorder.Code != http.StatusGatewayTimeout || sink.count() != 0 {
					t.Errorf("timeout = status %d records %d, want 504/0", recorder.Code, sink.count())
				}
			})

			t.Run("client cancellation is silent", func(t *testing.T) {
				started := make(chan struct{})
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: &contextReadCloser{ctx: r.Context(), started: started}, Request: r}, nil
				})}
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				ctx, cancel := context.WithCancel(context.Background())
				request := httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)).WithContext(ctx)
				writer := &failingResponseWriter{header: make(http.Header)}
				done := make(chan struct{})
				go func() {
					forwarder.Handler(surface.endpoint)(writer, request)
					close(done)
				}()
				<-started
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("handler did not return after client cancellation")
				}
				if writer.status != 0 || len(writer.written) != 0 || sink.count() != 0 {
					t.Errorf("cancellation wrote status/body/records = %d/%q/%d, want none", writer.status, writer.written, sink.count())
				}
			})
		})
	}
}

func TestForwardUsageMeterAloneActivatesBoundedRead(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			run := func(enabled bool) (int, string, int) {
				t.Helper()
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(surface.response)), Request: r}, nil
				})}
				sink := &forwardUsageSink{}
				var registry shim.Registry
				if enabled {
					registry = enabledUsageRegistry(sink)
				} else {
					registry = shim.CanonicalRegistry(nil)
				}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 8, registry)
				recorder := newDeadlineRecorder()
				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
				return recorder.Code, recorder.Body.String(), sink.count()
			}

			offStatus, offBody, offRecords := run(false)
			if offStatus != http.StatusAccepted || offBody != surface.response || offRecords != 0 {
				t.Errorf("meter off = %d %q records %d, want upstream passthrough", offStatus, offBody, offRecords)
			}
			onStatus, onBody, onRecords := run(true)
			if onStatus != http.StatusBadGateway || !strings.Contains(onBody, "exceeds the maximum allowed size") || onRecords != 0 {
				t.Errorf("meter on = %d %q records %d, want precommit 502 and no record", onStatus, onBody, onRecords)
			}
		})
	}
}

type rejectingOuterBufferedShim struct{}

func (*rejectingOuterBufferedShim) TransformBuffered(context.Context, *shim.Body) error {
	return errors.New("outer rejection")
}

func TestForwardUsageObservationPrecedesDownstreamWriteAndOuterShimFailure(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(surface.response)), Request: r}, nil
			})}

			t.Run("downstream write failure does not withdraw", func(t *testing.T) {
				sink := &forwardUsageSink{}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
				writer := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("client gone")}
				forwarder.Handler(surface.endpoint)(writer, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
				if sink.count() != 1 {
					t.Errorf("records = %d, want retained observation", sink.count())
				}
			})

			t.Run("innermost meter observes before outer rejection", func(t *testing.T) {
				sink := &forwardUsageSink{}
				registry := enabledUsageRegistry(sink)
				registry = append(shim.Registry{{Name: "outer-reject", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return &rejectingOuterBufferedShim{} }}}, registry...)
				if got := registry[len(registry)-1].Name; got != "usage-meter" {
					t.Fatalf("last registration = %q, want usage-meter innermost", got)
				}
				forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, registry)
				recorder := newDeadlineRecorder()
				forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
				if recorder.Code != http.StatusInternalServerError || sink.count() != 1 {
					t.Errorf("outer rejection status/records = %d/%d, want 500/1", recorder.Code, sink.count())
				}
			})
		})
	}
}

func TestForwardUsageMeterLeavesRecomputedContentLengthAndExactPayload(t *testing.T) {
	for _, surface := range bufferedUsageSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {"1"}}, Body: io.NopCloser(strings.NewReader(surface.response)), Request: r}, nil
			})}
			sink := &forwardUsageSink{}
			forwarder := newTestForwarder(readyStub("https://upstream.invalid"), client, time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, enabledUsageRegistry(sink))
			recorder := newDeadlineRecorder()
			forwarder.Handler(surface.endpoint)(recorder, httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(`{}`)))
			if recorder.Body.String() != surface.response || recorder.Header().Get("Content-Length") != strconv.Itoa(len(surface.response)) {
				t.Errorf("body/length = %q/%q", recorder.Body.String(), recorder.Header().Get("Content-Length"))
			}
			if sink.count() != 1 {
				t.Errorf("records = %d", sink.count())
			}
		})
	}
}

func TestEnabledUsageRegistryIsStable(t *testing.T) {
	// A small independent oracle keeps future registry additions from silently
	// moving the Usage meter away from its last/innermost position.
	registry := enabledUsageRegistry(&forwardUsageSink{})
	var names []string
	for _, registration := range registry {
		names = append(names, registration.Name)
	}
	if !reflect.DeepEqual(names, []string{"nop", "responses-item-id-stabilizer", "usage-meter"}) {
		t.Errorf("registration order = %v", names)
	}
}
