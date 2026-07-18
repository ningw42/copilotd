package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ningw42/copilotd/internal/apierror"
)

type stubFetcher struct {
	status int
	body   []byte
	err    error
	fetch  func(context.Context) (int, []byte, error)
}

func (f stubFetcher) FetchModels(ctx context.Context) (int, []byte, error) {
	if f.fetch != nil {
		return f.fetch(ctx)
	}
	return f.status, f.body, f.err
}

func TestHandlerMapsEveryFailureInTheSelectedSurfaceDialect(t *testing.T) {
	tests := []struct {
		name        string
		fetcher     stubFetcher
		render      func([]Model) ([]byte, error)
		wantStatus  int
		wantMessage string
	}{
		{name: "missing credential", fetcher: stubFetcher{err: fmt.Errorf("%w: credential-secret", ErrNoCredential)}, wantStatus: 503, wantMessage: "no upstream credential available"},
		{name: "request construction", fetcher: stubFetcher{err: fmt.Errorf("%w: url-secret", ErrBuildUpstream)}, wantStatus: 502, wantMessage: "could not fetch the upstream models catalog"},
		{name: "unreachable", fetcher: stubFetcher{err: fmt.Errorf("%w: network-secret", ErrUpstreamUnreachable)}, wantStatus: 502, wantMessage: "could not fetch the upstream models catalog"},
		{name: "response read", fetcher: stubFetcher{err: fmt.Errorf("%w: response-secret", ErrUpstreamRead)}, wantStatus: 502, wantMessage: "could not fetch the upstream models catalog"},
		{name: "timeout", fetcher: stubFetcher{err: fmt.Errorf("%w: timeout-secret", ErrUpstreamTimeout)}, wantStatus: 504, wantMessage: "the upstream request timed out"},
		{name: "unknown fetch error", fetcher: stubFetcher{err: errors.New("unknown-secret")}, wantStatus: 502, wantMessage: "could not fetch the upstream models catalog"},
		{name: "upstream status", fetcher: stubFetcher{status: 429, body: []byte(`{"copilot":"body-secret"}`)}, wantStatus: 502, wantMessage: "upstream models request failed"},
		{name: "malformed catalog", fetcher: stubFetcher{status: 200, body: []byte(`<body-secret>`)}, wantStatus: 502, wantMessage: "upstream models response was invalid"},
		{
			name:        "render failure",
			fetcher:     stubFetcher{status: 200, body: []byte(`{"data":[]}`)},
			render:      func([]Model) ([]byte, error) { return nil, errors.New("render-secret") },
			wantStatus:  502,
			wantMessage: "could not render the models catalog",
		},
	}
	surfaces := []struct {
		name    string
		surface apierror.Surface
		body    func(string) string
	}{
		{
			name:    "Anthropic",
			surface: apierror.Anthropic,
			body: func(message string) string {
				return `{"type":"error","error":{"type":"api_error","message":"` + message + `"}}`
			},
		},
		{
			name:    "OpenAI",
			surface: apierror.OpenAI,
			body: func(message string) string {
				return `{"error":{"message":"` + message + `","type":"api_error","code":null,"param":null}}`
			},
		},
	}

	for _, surface := range surfaces {
		for _, tc := range tests {
			t.Run(surface.name+"/"+tc.name, func(t *testing.T) {
				render := tc.render
				if render == nil {
					render = RenderOpenAI
				}
				handler := Handler(Descriptor{Surface: surface.surface, RequiredRoute: OpenAIResponsesRoute, Render: render}, tc.fetcher)
				recorder := httptest.NewRecorder()

				handler(recorder, httptest.NewRequest(http.MethodGet, "/models", nil))

				if recorder.Code != tc.wantStatus {
					t.Errorf("status = %d, want %d", recorder.Code, tc.wantStatus)
				}
				if got, want := recorder.Body.String(), surface.body(tc.wantMessage); got != want {
					t.Errorf("body = %s, want exact Surface envelope %s", got, want)
				}
			})
		}
	}
}

type writeSpy struct {
	header      http.Header
	writeHeader int
	writes      int
}

func (w *writeSpy) Header() http.Header { return w.header }
func (w *writeSpy) WriteHeader(int)     { w.writeHeader++ }
func (w *writeSpy) Write(body []byte) (int, error) {
	w.writes++
	return len(body), nil
}

func TestHandlerPropagatesCancellationWithoutWritingAReplacementError(t *testing.T) {
	started := make(chan struct{})
	fetcher := stubFetcher{fetch: func(ctx context.Context) (int, []byte, error) {
		close(started)
		<-ctx.Done()
		return 0, nil, fmt.Errorf("%w: %v", ErrUpstreamRead, ctx.Err())
	}}
	handler := Handler(Descriptor{Surface: apierror.Anthropic, RequiredRoute: AnthropicMessagesRoute, Render: RenderAnthropic}, fetcher)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil).WithContext(ctx)
	writer := &writeSpy{header: make(http.Header)}
	done := make(chan struct{})
	go func() {
		handler(writer, request)
		close(done)
	}()
	<-started
	cancel()
	<-done

	if writer.writeHeader != 0 || writer.writes != 0 || len(writer.header) != 0 {
		t.Errorf("cancelled handler wrote replacement response: headers=%v WriteHeader=%d Write=%d", writer.header, writer.writeHeader, writer.writes)
	}
}
