package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/upstream"
)

type stubSource struct {
	status   int
	body     []byte
	failure  *upstream.Failure
	buffered func(context.Context, upstream.Call) (int, []byte, context.Context, *upstream.Failure)
}

type routeRecordingSource struct {
	call upstream.Call
}

func (s *routeRecordingSource) Buffered(ctx context.Context, call upstream.Call) (int, []byte, context.Context, *upstream.Failure) {
	s.call = call
	return http.StatusOK, []byte(`{"data":[]}`), ctx, nil
}

func TestHandlerCallsTheCatalogContractsUpstreamRoute(t *testing.T) {
	source := &routeRecordingSource{}
	handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), RenderDescriptors{}, source, nil)
	recorder := httptest.NewRecorder()

	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got, want := source.call.Route, endpoint.OpenAICatalog().Upstream(); got != want {
		t.Errorf("upstream call route = %q, want contract route %q", got, want)
	}
	if source.call.Method != http.MethodGet {
		t.Errorf("upstream call method = %q, want GET", source.call.Method)
	}
	if !source.call.AcceptIdentityEncoding {
		t.Error("upstream call did not request identity encoding")
	}
}

// shapeRecorder is a shape hook that counts its calls and keeps the last Shape
// and context it received.
type shapeRecorder struct {
	calls int
	shape Shape
	ctx   context.Context
}

func (r *shapeRecorder) record(ctx context.Context, shape Shape) {
	r.calls++
	r.shape = shape
	r.ctx = ctx
}

// unrenderableCatalogBody is a picker-visible model advertising both Surfaces'
// required Routes but no id: it passes Decode and Filter, then fails both
// provider renderers.
const unrenderableCatalogBody = `{"data":[{"name":"Nameless","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/v1/messages","/responses"]}]}`

func TestHandlerRecordsTheServedShapeOnceAfterEachSuccessfulRender(t *testing.T) {
	type responseKey struct{}
	upstreamBody := []byte(`{"data":[{"id":"gpt-5.4","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	// The source answers with a derived response-path context; the hook must
	// still receive the request's own context.
	source := stubSource{buffered: func(ctx context.Context, _ upstream.Call) (int, []byte, context.Context, *upstream.Failure) {
		return http.StatusOK, upstreamBody, context.WithValue(ctx, responseKey{}, true), nil
	}}
	descriptors := RenderDescriptors{Codex: CodexDescriptor{
		Models:       pinnedCodexModels(t, "gpt-5.4"),
		RenderConfig: CodexRenderConfig{AutoReviewModel: "gpt-5.4"},
	}}
	tests := []struct {
		name       string
		target     string
		wantShape  Shape
		wantPrefix string
	}{
		{name: "provider-shaped OpenAI catalog", target: "/openai/v1/models", wantShape: ShapeOpenAI, wantPrefix: `{"object":"list","data":[`},
		{name: "client-shaped Codex catalog", target: "/openai/v1/models?client_version=fixture", wantShape: ShapeCodex, wantPrefix: `{"models":[`},
	}

	for _, tc := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				var recorded shapeRecorder
				handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), descriptors, source, recorded.record)
				request := httptest.NewRequest(method, tc.target, nil)
				recorder := httptest.NewRecorder()

				handler(recorder, request)

				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
				}
				if method == http.MethodGet && !strings.HasPrefix(recorder.Body.String(), tc.wantPrefix) {
					t.Errorf("representation = %s, want prefix %s", recorder.Body.String(), tc.wantPrefix)
				}
				if recorded.calls != 1 || recorded.shape != tc.wantShape {
					t.Errorf("shape hook calls/shape = %d/%q, want 1/%q", recorded.calls, recorded.shape, tc.wantShape)
				}
				if recorded.ctx != request.Context() {
					t.Error("shape hook did not receive the request context")
				}

				nilHookRecorder := httptest.NewRecorder()
				Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), descriptors, source, nil)(nilHookRecorder, httptest.NewRequest(method, tc.target, nil))
				if nilHookRecorder.Code != http.StatusOK || nilHookRecorder.Body.String() != recorder.Body.String() {
					t.Errorf("nil shape hook served %d %s, want the recorded response", nilHookRecorder.Code, nilHookRecorder.Body.String())
				}
			})
		}
	}
}

func TestHandlerRendersTheAnthropicCatalogWithItsConfig(t *testing.T) {
	upstreamBody := []byte(`{"data":[{"id":"claude-sonnet-4.5","name":"Claude Sonnet 4.5","vendor":"Anthropic","model_picker_enabled":true,"supported_endpoints":["/v1/messages"]}]}`)
	handler := Handler(discardHandlerLogger(), endpoint.AnthropicCatalog(), RenderDescriptors{
		Anthropic: AnthropicRenderConfig{ModelIDNormalizationEnabled: true},
	}, stubSource{status: http.StatusOK, body: upstreamBody}, nil)
	recorder := httptest.NewRecorder()

	handler(recorder, httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); !strings.HasPrefix(got, `{"data":[{"id":"claude-sonnet-4-5","type":"model"`) {
		t.Errorf("body = %s, want the normalized Anthropic catalog", got)
	}
}

func TestHandlerNegotiatesCodexShapeOnlyWhenEveryGateIsOpen(t *testing.T) {
	upstreamBody := []byte(`{"data":[{"id":"gpt-5.4","name":"GPT-5.4","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses","/v1/messages"]}]}`)
	models, err := Decode(upstreamBody)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	wantOpenAI, err := RenderOpenAI(Filter(models, endpoint.RouteOpenAIResponses))
	if err != nil {
		t.Fatalf("render expected OpenAI catalog: %v", err)
	}
	wantAnthropic, err := RenderAnthropic(Filter(models, endpoint.RouteAnthropicMessages))
	if err != nil {
		t.Fatalf("render expected Anthropic catalog: %v", err)
	}

	tests := []struct {
		name              string
		anthropicCatalog  bool
		rawQuery          string
		codexSource       bool
		reviewer          string
		aliases           map[string]string
		reviewerOverrides map[string]string
		overrideLimit     bool
		wantCodex         bool
	}{
		{name: "client key absent", codexSource: true, reviewer: "gpt-5.4"},
		{name: "Codex source absent", rawQuery: "client_version=fixture", reviewer: "gpt-5.4"},
		{name: "nothing to inject", rawQuery: "client_version=fixture", codexSource: true},
		{
			name:              "Anthropic Catalog with every add-on",
			anthropicCatalog:  true,
			rawQuery:          "client_version=fixture",
			codexSource:       true,
			reviewer:          "gpt-5.4",
			aliases:           map[string]string{"gpt-example-alias": "gpt-5.4"},
			reviewerOverrides: map[string]string{"gpt-5.4": "gpt-5.4"},
			overrideLimit:     true,
		},
		{name: "aliases are enough to inject", rawQuery: "client_version=fixture", codexSource: true, aliases: map[string]string{"gpt-example-alias": "gpt-5.4"}, wantCodex: true},
		{name: "empty client value is present with reviewer", rawQuery: "client_version=", codexSource: true, reviewer: "gpt-5.4", wantCodex: true},
		{name: "valueless client key is present with limits", rawQuery: "client_version", codexSource: true, overrideLimit: true, wantCodex: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			codex := CodexDescriptor{RenderConfig: CodexRenderConfig{
				ModelAliases:             tc.aliases,
				AutoReviewModel:          tc.reviewer,
				AutoReviewModelOverrides: tc.reviewerOverrides,
				OverrideLimits:           tc.overrideLimit,
			}}
			if tc.codexSource {
				codex.Models = pinnedCodexModels(t, "gpt-5.4")
			}
			ep, target := endpoint.OpenAICatalog(), "/openai/v1/models"
			if tc.anthropicCatalog {
				ep, target = endpoint.AnthropicCatalog(), "/anthropic/v1/models"
			}
			if tc.rawQuery != "" {
				target += "?" + tc.rawQuery
			}
			var recorded shapeRecorder
			handler := Handler(discardHandlerLogger(), ep, RenderDescriptors{Codex: codex}, stubSource{status: http.StatusOK, body: upstreamBody}, recorded.record)
			recorder := httptest.NewRecorder()

			handler(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			if got, want := recorder.Header().Get("Content-Length"), strconv.Itoa(recorder.Body.Len()); got != want {
				t.Errorf("Content-Length = %q, want %q", got, want)
			}
			switch {
			case tc.anthropicCatalog:
				if got := recorder.Body.Bytes(); string(got) != string(wantAnthropic) {
					t.Errorf("Anthropic Catalog body changed:\n got %s\nwant %s", got, wantAnthropic)
				}
				if recorded.calls != 0 {
					t.Errorf("Anthropic Catalog called the shape hook %d times with %q, want none", recorded.calls, recorded.shape)
				}
			case tc.wantCodex:
				if got := recorder.Body.String(); !strings.HasPrefix(got, `{"models":`) {
					t.Errorf("body = %s, want Codex catalog shape", got)
				}
				if recorded.calls != 1 || recorded.shape != ShapeCodex {
					t.Errorf("shape hook calls/shape = %d/%q, want 1/%q", recorded.calls, recorded.shape, ShapeCodex)
				}
			default:
				if got := recorder.Body.Bytes(); string(got) != string(wantOpenAI) {
					t.Errorf("OpenAI fallback body changed:\n got %s\nwant %s", got, wantOpenAI)
				}
				if recorded.calls != 1 || recorded.shape != ShapeOpenAI {
					t.Errorf("shape hook calls/shape = %d/%q, want 1/%q", recorded.calls, recorded.shape, ShapeOpenAI)
				}
			}
		})
	}
}

func TestHandlerCodexHEADMatchesGETHeadersAndSuppressesBody(t *testing.T) {
	const alias = "gpt-example-alias"
	upstreamBody := []byte(`{"data":[{"id":"` + alias + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	descriptors := RenderDescriptors{
		Codex: CodexDescriptor{
			Models: pinnedCodexModels(t, "gpt-5.4"),
			RenderConfig: CodexRenderConfig{
				ModelAliases: map[string]string{alias: "gpt-5.4"},
			},
		},
	}
	handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), descriptors, stubSource{status: http.StatusOK, body: upstreamBody}, nil)

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

func TestHandlerLogsEverySkippedCodexReviewer(t *testing.T) {
	var logs bytes.Buffer
	logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody := []byte(`{"data":[{"id":"gpt-5.4","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	source := stubSource{buffered: func(ctx context.Context, _ upstream.Call) (int, []byte, context.Context, *upstream.Failure) {
		responseCtx := logging.With(ctx, slog.String(logging.UpstreamRequestIDKey, "catalog-upstream-id"))
		return http.StatusOK, upstreamBody, responseCtx, nil
	}}
	handler := Handler(logger, endpoint.OpenAICatalog(), RenderDescriptors{
		Codex: CodexDescriptor{
			Models: pinnedCodexModels(t, "gpt-5.4"),
			RenderConfig: CodexRenderConfig{
				AutoReviewModel: "missing-reviewer",
			},
		},
	}, source, nil)
	recorder := httptest.NewRecorder()
	ctx := logging.WithRequestID(context.Background(), "catalog-request-id")
	request := httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=fixture", nil).WithContext(ctx)

	handler(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	out := logs.String()
	for _, want := range []string{`msg="Codex catalog reviewer was skipped"`, "model=gpt-5.4", "reviewer=missing-reviewer", "request_id=catalog-request-id", "upstream_request_id=catalog-upstream-id"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "skip_reason=") {
		t.Errorf("reviewer warning gained an alias skip reason: %s", out)
	}
}

func TestHandlerLogsEveryUnappliedCodexAliasOnEveryRequest(t *testing.T) {
	const (
		notForwarded  = "a-not-forwarded"
		missingSource = "b-missing-source"
		unconfigured  = "c-unconfigured-copilot-only"
		shadowed      = "gpt-5.4"
	)
	var logs bytes.Buffer
	logger, err := logging.NewWithWriter(&logs, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody := []byte(`{"data":[` +
		`{"id":"` + shadowed + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]},` +
		`{"id":"` + missingSource + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]},` +
		`{"id":"` + unconfigured + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}` +
		`]}`)
	handler := Handler(logger, endpoint.OpenAICatalog(), RenderDescriptors{
		Codex: CodexDescriptor{
			Models: pinnedCodexModels(t, shadowed, "gpt-5.5", "gpt-5.6-sol"),
			RenderConfig: CodexRenderConfig{ModelAliases: map[string]string{
				notForwarded:  "gpt-5.6-sol",
				missingSource: "gpt-no-such-source",
				shadowed:      "gpt-5.5",
			}},
		},
	}, stubSource{status: http.StatusOK, body: upstreamBody}, nil)

	for requestNumber := 0; requestNumber < 2; requestNumber++ {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=fixture", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200: %s", requestNumber+1, recorder.Code, recorder.Body.String())
		}
		if got := renderedSlugs(t, decodeRenderedCodex(t, recorder.Body.Bytes())); !reflect.DeepEqual(got, []string{shadowed}) {
			t.Errorf("request %d rendered slugs = %q, want shadowed official entry", requestNumber+1, got)
		}
	}

	output := logs.String()
	var warningLines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, `msg="Codex catalog alias mapping was not applied"`) {
			warningLines = append(warningLines, line)
		}
	}
	if len(warningLines) != 6 {
		t.Fatalf("unapplied alias warnings = %d, want 6 (three per request):\n%s", len(warningLines), output)
	}
	wantOrder := []string{notForwarded, missingSource, shadowed, notForwarded, missingSource, shadowed}
	for i, wantAlias := range wantOrder {
		if !strings.Contains(warningLines[i], "model="+wantAlias) {
			t.Errorf("warning[%d] = %s, want alias-sorted model %s", i, warningLines[i], wantAlias)
		}
	}
	for _, want := range []string{
		"level=WARN",
		"model=" + notForwarded, "metadata_source=gpt-5.6-sol", "skip_reason=alias_not_forwardable",
		"model=" + missingSource, "metadata_source=gpt-no-such-source", "skip_reason=metadata_source_missing",
		"model=" + shadowed, "metadata_source=gpt-5.5", "skip_reason=shadowed_by_official",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("warning output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "model="+unconfigured) {
		t.Errorf("unconfigured Copilot-only model produced a warning:\n%s", output)
	}
	if strings.Contains(output, "failure_class=") {
		t.Errorf("unapplied alias warning used failure_class:\n%s", output)
	}
}

func discardHandlerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandlerRendersCodexFromCurrentCachedBytes(t *testing.T) {
	fallback := validCodexModelsBytes(t, "gpt-fallback", "fallback prompt")
	fresh := codexModelsBytesWithoutField(t, "base_instructions")
	modelsValue := testCodexModelsValue(t, fallback, fresh, nil)

	upstreamBody := []byte(`{"data":[{"id":"gpt-test","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), RenderDescriptors{
		Codex: CodexDescriptor{
			Models: modelsValue,
			RenderConfig: CodexRenderConfig{
				OverrideLimits: true,
			},
		},
	}, stubSource{status: http.StatusOK, body: upstreamBody}, nil)
	recorder := httptest.NewRecorder()

	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=fixture", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	entries := decodeRenderedCodex(t, recorder.Body.Bytes())
	if got := renderedSlugs(t, entries); len(got) != 1 || got[0] != "gpt-test" {
		t.Fatalf("rendered slugs = %q, want current Catalog model", got)
	}
}

func TestHandlerRendersAliasFromCurrentAndFallbackCodexModels(t *testing.T) {
	const alias = "gpt-alias"
	upstreamBody := []byte(`{"data":[{"id":"` + alias + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	fallback := codexModelsBytes(t, completeCodexEntry("gpt-fallback-source", map[string]any{"display_name": "Fallback model"}))

	tests := []struct {
		name        string
		models      *cache.Value[[]byte]
		source      string
		displayName string
	}{
		{
			name:        "accepted current bytes",
			models:      testCodexModelsValue(t, fallback, validCodexModelsBytes(t, "gpt-source", "fresh prompt"), nil),
			source:      "gpt-source",
			displayName: "Fresh model",
		},
		{
			name:        "fallback after refresh failure",
			models:      testCodexModelsValue(t, fallback, nil, errors.New("refresh failed")),
			source:      "gpt-fallback-source",
			displayName: "Fallback model",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), RenderDescriptors{
				Codex: CodexDescriptor{
					Models: tc.models,
					RenderConfig: CodexRenderConfig{
						ModelAliases: map[string]string{alias: tc.source},
					},
				},
			}, stubSource{status: http.StatusOK, body: upstreamBody}, nil)
			recorder := httptest.NewRecorder()
			handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=fixture", nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
			}
			entries := decodeRenderedCodex(t, recorder.Body.Bytes())
			if len(entries) != 1 || decodeStringField(t, entries[0], "slug") != alias {
				t.Fatalf("rendered entries = %s, want sole alias", recorder.Body.Bytes())
			}
			if got := decodeStringField(t, entries[0], "display_name"); got != tc.displayName {
				t.Errorf("alias display_name = %q, want source value %q", got, tc.displayName)
			}
		})
	}
}

func TestHandlerAliasFailuresRemainOpenAIBadGateway(t *testing.T) {
	const alias = "gpt-error-alias"
	validUpstream := []byte(`{"data":[{"id":"` + alias + `","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	invalidCurrent := cache.New(discardHandlerLogger(), cache.Cacheable[[]byte]{
		Fallback:        []byte(`{"models":[`),
		FallbackVersion: "invalid",
		TTL:             0,
		Fetch: func(context.Context) ([]byte, string, error) {
			return nil, "", errors.New("unused")
		},
		Hash: hashModels,
	})

	// These are the two alias-configured 502 causes reachable through Handler.
	// parseCodexModels rejects invalid cached JSON before RawMessages reach the
	// renderer, while the renderer's json.Marshal inputs are strings and cannot
	// fail. TestRenderCodexRejectsInvalidRawMetadataSourceField covers the pure
	// renderer's defensive invalid-RawMessage branch.
	tests := []struct {
		name         string
		upstreamBody []byte
		models       *cache.Value[[]byte]
		wantMessage  string
	}{
		{name: "invalid live Copilot JSON", upstreamBody: []byte(`{"data":[`), models: pinnedCodexModels(t, "gpt-5.4"), wantMessage: "upstream models response was invalid"},
		{name: "invalid current Codex bytes", upstreamBody: validUpstream, models: invalidCurrent, wantMessage: "could not render the models catalog"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recorded shapeRecorder
			handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), RenderDescriptors{
				Codex: CodexDescriptor{
					Models: tc.models,
					RenderConfig: CodexRenderConfig{
						ModelAliases: map[string]string{alias: "gpt-5.4"},
					},
				},
			}, stubSource{status: http.StatusOK, body: tc.upstreamBody}, recorded.record)
			recorder := httptest.NewRecorder()
			handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=fixture", nil))

			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), `"type":"api_error"`) || !strings.Contains(recorder.Body.String(), tc.wantMessage) {
				t.Errorf("body = %s, want OpenAI api_error containing %q", recorder.Body.String(), tc.wantMessage)
			}
			if recorded.calls != 0 {
				t.Errorf("failed Codex render recorded catalog shape %q", recorded.shape)
			}
		})
	}
}

// testCodexModelsValue returns a Codex Models source that starts from synthetic
// fallback bytes and has already made one refresh attempt for current.
func testCodexModelsValue(t *testing.T, fallback, current []byte, fetchErr error) *cache.Value[[]byte] {
	t.Helper()
	modelsValue := cache.New(discardHandlerLogger(), cache.Cacheable[[]byte]{
		Fallback:        fallback,
		FallbackVersion: "synthetic-fallback",
		TTL:             time.Hour,
		Fetch: func(context.Context) ([]byte, string, error) {
			return current, "rust-v1.2.3", fetchErr
		},
		Hash: hashModels,
		Validate: func(currentBytes []byte) error {
			_, err := validateCodexModels(currentBytes)
			return err
		},
	})
	registry := cache.NewRegistry()
	registry.Register(modelsValue)
	registry.Prime(context.Background())
	return modelsValue
}

// pinnedCodexModels returns a Codex Models source whose current bytes hold one
// complete synthetic entry per slug and never refresh.
func pinnedCodexModels(t *testing.T, slugs ...string) *cache.Value[[]byte] {
	t.Helper()
	return cache.New(discardHandlerLogger(), cache.Cacheable[[]byte]{
		Fallback:        codexModelsBytes(t, completeCodexEntries(slugs...)...),
		FallbackVersion: "synthetic-pinned",
		Fetch: func(context.Context) ([]byte, string, error) {
			return nil, "", errors.New("pinned synthetic Codex models are never fetched")
		},
		Hash: hashModels,
	})
}

func (s stubSource) Buffered(ctx context.Context, call upstream.Call) (int, []byte, context.Context, *upstream.Failure) {
	if s.buffered != nil {
		return s.buffered(ctx, call)
	}
	return s.status, s.body, ctx, s.failure
}

func TestHandlerMapsEveryFailureInTheSelectedSurfaceDialect(t *testing.T) {
	tests := []struct {
		name        string
		source      stubSource
		wantStatus  int
		wantMessage string
	}{
		{name: "missing credential", source: stubSource{failure: &upstream.Failure{Kind: apierror.NotReady, Message: "no upstream credential available", Err: errors.New("credential-secret")}}, wantStatus: 503, wantMessage: "no upstream credential available"},
		{name: "request construction", source: stubSource{failure: &upstream.Failure{Kind: apierror.BadGateway, Message: "could not build the upstream request", Err: errors.New("url-secret")}}, wantStatus: 502, wantMessage: "could not build the upstream request"},
		{name: "unreachable", source: stubSource{failure: &upstream.Failure{Kind: apierror.BadGateway, Message: "could not reach the upstream", Err: errors.New("network-secret")}}, wantStatus: 502, wantMessage: "could not reach the upstream"},
		{name: "response read", source: stubSource{failure: &upstream.Failure{Kind: apierror.BadGateway, Message: "could not read the upstream response", Err: errors.New("response-secret")}}, wantStatus: 502, wantMessage: "could not read the upstream response"},
		{name: "response over cap", source: stubSource{failure: &upstream.Failure{Kind: apierror.BadGateway, Message: "upstream response body exceeds the maximum allowed size", Err: errors.New("over-cap-secret")}}, wantStatus: 502, wantMessage: "upstream response body exceeds the maximum allowed size"},
		{name: "timeout", source: stubSource{failure: &upstream.Failure{Kind: apierror.GatewayTimeout, Message: "the upstream request timed out", Err: errors.New("timeout-secret")}}, wantStatus: 504, wantMessage: "the upstream request timed out"},
		{name: "upstream status", source: stubSource{status: 429, body: []byte(`{"copilot":"body-secret"}`)}, wantStatus: 502, wantMessage: "upstream models request failed"},
		{name: "malformed catalog", source: stubSource{status: 200, body: []byte(`<body-secret>`)}, wantStatus: 502, wantMessage: "upstream models response was invalid"},
		{name: "render failure", source: stubSource{status: 200, body: []byte(unrenderableCatalogBody)}, wantStatus: 502, wantMessage: "could not render the models catalog"},
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
				var recorded shapeRecorder
				handler := Handler(discardHandlerLogger(), surface.ep, RenderDescriptors{}, tc.source, recorded.record)
				recorder := httptest.NewRecorder()

				handler(recorder, httptest.NewRequest(http.MethodGet, "/models", nil))

				if recorder.Code != tc.wantStatus {
					t.Errorf("status = %d, want %d", recorder.Code, tc.wantStatus)
				}
				if got, want := recorder.Body.String(), surface.body(tc.wantMessage); got != want {
					t.Errorf("body = %s, want exact Surface envelope %s", got, want)
				}
				if recorded.calls != 0 {
					t.Errorf("failed request recorded catalog shape %q", recorded.shape)
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
	source := stubSource{buffered: func(ctx context.Context, _ upstream.Call) (int, []byte, context.Context, *upstream.Failure) {
		close(started)
		<-ctx.Done()
		return 0, nil, ctx, &upstream.Failure{ClientGone: true, Err: ctx.Err()}
	}}
	handler := Handler(discardHandlerLogger(), endpoint.AnthropicCatalog(), RenderDescriptors{}, source, nil)
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
