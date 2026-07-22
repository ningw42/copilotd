package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
)

type stubFetcher struct {
	status int
	body   []byte
	err    error
	fetch  func(context.Context, endpoint.Route) (int, []byte, error)
}

type routeRecordingFetcher struct {
	upstream endpoint.Route
}

func (f *routeRecordingFetcher) FetchModels(_ context.Context, upstream endpoint.Route) (int, []byte, error) {
	f.upstream = upstream
	return http.StatusOK, []byte(`{"data":[]}`), nil
}

func TestHandlerFetchesTheCatalogContractsUpstreamRoute(t *testing.T) {
	fetcher := &routeRecordingFetcher{}
	handler := Handler(endpoint.OpenAICatalog(), Rendering{Render: RenderOpenAI}, fetcher)
	recorder := httptest.NewRecorder()

	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got, want := fetcher.upstream, endpoint.OpenAICatalog().Upstream(); got != want {
		t.Errorf("fetched upstream route = %q, want contract route %q", got, want)
	}
}

func TestHandlerNegotiatesCodexShapeOnlyWhenEveryGateIsOpen(t *testing.T) {
	upstreamBody := []byte(`{"data":[{"id":"gpt-5.4","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	models, err := Decode(upstreamBody)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	wantOpenAI, err := RenderOpenAI(Filter(models, endpoint.RouteOpenAIResponses))
	if err != nil {
		t.Fatalf("render expected OpenAI catalog: %v", err)
	}

	tests := []struct {
		name          string
		rawQuery      string
		enabled       bool
		reviewer      string
		overrideLimit bool
		wantCodex     bool
	}{
		{name: "client key absent", enabled: true, reviewer: "gpt-5.4"},
		{name: "catalog disabled", rawQuery: "client_version=0.144.5", reviewer: "gpt-5.4"},
		{name: "nothing to inject", rawQuery: "client_version=0.144.5", enabled: true},
		{name: "empty client value is present with reviewer", rawQuery: "client_version=", enabled: true, reviewer: "gpt-5.4", wantCodex: true},
		{name: "valueless client key is present with limits", rawQuery: "client_version", enabled: true, overrideLimit: true, wantCodex: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rendering := Rendering{
				Render: RenderOpenAI,
				Codex: CodexDescriptor{
					Enabled: tc.enabled,
					RenderConfig: CodexRenderConfig{
						AutoReviewModel: tc.reviewer,
						OverrideLimits:  tc.overrideLimit,
					},
				},
			}
			handler := Handler(endpoint.OpenAICatalog(), rendering, stubFetcher{status: http.StatusOK, body: upstreamBody})
			target := "/openai/v1/models"
			if tc.rawQuery != "" {
				target += "?" + tc.rawQuery
			}
			recorder := httptest.NewRecorder()

			handler(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			if got, want := recorder.Header().Get("Content-Length"), strconv.Itoa(recorder.Body.Len()); got != want {
				t.Errorf("Content-Length = %q, want %q", got, want)
			}
			if tc.wantCodex {
				if got := recorder.Body.String(); len(got) < len(`{"models":`) || got[:len(`{"models":`)] != `{"models":` {
					t.Errorf("body = %s, want Codex catalog shape", got)
				}
				return
			}
			if got := recorder.Body.Bytes(); string(got) != string(wantOpenAI) {
				t.Errorf("OpenAI fallback body changed:\n got %s\nwant %s", got, wantOpenAI)
			}
		})
	}
}

func TestHandlerCodexHEADMatchesGETHeadersAndSuppressesBody(t *testing.T) {
	upstreamBody := []byte(`{"data":[{"id":"gpt-5.4","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	rendering := Rendering{
		Render: RenderOpenAI,
		Codex: CodexDescriptor{
			Enabled: true,
			RenderConfig: CodexRenderConfig{
				AutoReviewModel: "gpt-5.4",
			},
		},
	}
	handler := Handler(endpoint.OpenAICatalog(), rendering, stubFetcher{status: http.StatusOK, body: upstreamBody})

	getRecorder := httptest.NewRecorder()
	handler(getRecorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=secret-query-value", nil))
	headRecorder := httptest.NewRecorder()
	handler(headRecorder, httptest.NewRequest(http.MethodHead, "/openai/v1/models?client_version=", nil))

	if getRecorder.Code != http.StatusOK || headRecorder.Code != http.StatusOK {
		t.Fatalf("GET/HEAD status = %d/%d, want 200/200", getRecorder.Code, headRecorder.Code)
	}
	for _, header := range []string{"Content-Type", "Content-Length"} {
		if got, want := headRecorder.Header().Get(header), getRecorder.Header().Get(header); got != want {
			t.Errorf("HEAD %s = %q, want GET value %q", header, got, want)
		}
	}
	if got := headRecorder.Body.Len(); got != 0 {
		t.Errorf("HEAD body length = %d, want 0", got)
	}
	if got, want := getRecorder.Header().Get("Content-Length"), strconv.Itoa(getRecorder.Body.Len()); got != want {
		t.Errorf("GET Content-Length = %q, want %q", got, want)
	}
	if got := getRecorder.Header().Get("X-Catalog-Shape"); got != "" {
		t.Errorf("internal catalog shape header leaked as %q", got)
	}
}

func TestHandlerRendersCodexFromCurrentCachedBytes(t *testing.T) {
	fresh := validCodexModelsBytes(t, "fresh-model", "release prompt")
	registry := cache.NewRegistry()
	modelsValue := cache.New(cache.Cacheable[[]byte]{
		Fallback:        embeddedCodexModels,
		FallbackVersion: embeddedCodexModelsVersion,
		TTL:             time.Hour,
		Fetch: func(context.Context) ([]byte, string, error) {
			return fresh, "rust-v0.145.0", nil
		},
		Hash: hashModels,
		Validate: func(currentBytes []byte) error {
			_, err := decodeCodexModels(currentBytes)
			return err
		},
	})
	registry.Register(modelsValue)
	registry.Prime(context.Background())

	upstreamBody := []byte(`{"data":[{"id":"fresh-model","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	handler := Handler(endpoint.OpenAICatalog(), Rendering{
		Render: RenderOpenAI,
		Codex: CodexDescriptor{
			Enabled: true,
			Models:  modelsValue,
			RenderConfig: CodexRenderConfig{
				OverrideLimits: true,
			},
		},
	}, stubFetcher{status: http.StatusOK, body: upstreamBody})
	recorder := httptest.NewRecorder()

	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=0.145.0", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	entries := decodeRenderedCodex(t, recorder.Body.Bytes())
	if got := renderedSlugs(t, entries); len(got) != 1 || got[0] != "fresh-model" {
		t.Fatalf("rendered slugs = %q, want current fetched model", got)
	}
}

func (f stubFetcher) FetchModels(ctx context.Context, upstream endpoint.Route) (int, []byte, error) {
	if f.fetch != nil {
		return f.fetch(ctx, upstream)
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
		name string
		ep   endpoint.Catalog
		body func(string) string
	}{
		{
			name: "Anthropic",
			ep:   endpoint.AnthropicCatalog(),
			body: func(message string) string {
				return `{"type":"error","error":{"type":"api_error","message":"` + message + `"}}`
			},
		},
		{
			name: "OpenAI",
			ep:   endpoint.OpenAICatalog(),
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
				handler := Handler(surface.ep, Rendering{Render: render}, tc.fetcher)
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
	fetcher := stubFetcher{fetch: func(ctx context.Context, _ endpoint.Route) (int, []byte, error) {
		close(started)
		<-ctx.Done()
		return 0, nil, fmt.Errorf("%w: %v", ErrUpstreamRead, ctx.Err())
	}}
	handler := Handler(endpoint.AnthropicCatalog(), Rendering{Render: RenderAnthropic}, fetcher)
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
