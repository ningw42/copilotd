package catalog

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestVendoredCodexCatalogRoundTripFidelity(t *testing.T) {
	release := embeddedCodexRelease
	wantFallbackBytes := bytes.Clone(embeddedCodexModels)
	if got := len(embeddedCodexModels); got != release.Models.Size {
		t.Fatalf("%s vendored snapshot size = %d, want pinned %d", release.Release.Tag, got, release.Models.Size)
	}
	if got := hashModels(embeddedCodexModels); got != release.Models.SHA256 {
		t.Fatalf("%s vendored snapshot hash = %s, want pinned %s", release.Release.Tag, got, release.Models.SHA256)
	}
	if got := gitBlobObjectID(embeddedCodexModels); got != release.Models.GitBlob {
		t.Fatalf("%s vendored snapshot Git blob = %s, want upstream %s", release.Release.Tag, got, release.Models.GitBlob)
	}
	if _, err := validateCodexModels(embeddedCodexModels); err != nil {
		t.Fatalf("decode %s vendored snapshot at %s: %v", release.Release.Tag, release.Release.PeeledCommit, err)
	}
	vendoredModels := rawCodexModelsBySlug(t, embeddedCodexModels)
	defaultSlug := release.Models.AuditedBundledDefault
	defaultModel, present := vendoredModels[defaultSlug]
	if !present {
		t.Fatalf("vendored catalog has no audited bundled default %q", defaultSlug)
	}
	if _, present := defaultModel["base_instructions"]; present {
		t.Errorf("audited bundled default %q unexpectedly has legacy base_instructions", defaultSlug)
	}
	var defaultMessages map[string]json.RawMessage
	if err := json.Unmarshal(defaultModel["model_messages"], &defaultMessages); err != nil {
		t.Fatalf("decode audited bundled default %q model_messages: %v", defaultSlug, err)
	}
	if template := decodeStringField(t, defaultMessages, "instructions_template"); template == "" {
		t.Errorf("audited bundled default %q has empty canonical instructions_template", defaultSlug)
	}
	if got := bytes.TrimSpace(defaultModel["requires_sandboxed_review"]); !bytes.Equal(got, []byte("false")) {
		t.Errorf("%s.requires_sandboxed_review = %s, want preserved false", defaultSlug, got)
	}

	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("GitHub request method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("credential-isolated GitHub request carried Authorization %q", got)
		}
		switch r.URL.Path {
		case latestCodexReleasePath:
			if r.URL.RawQuery != "" {
				t.Errorf("latest-release query = %q, want empty", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"tag_name":"`+release.Release.Tag+`","target_commitish":"main","prerelease":false,"draft":false}`)
		case codexReleaseCommitPath + release.Release.Tag:
			if got := r.Header.Get("Accept"); got != githubSHA1MediaType {
				t.Errorf("commit Accept = %q, want %q", got, githubSHA1MediaType)
			}
			_, _ = io.WriteString(w, release.Release.PeeledCommit)
		case codexModelsContentPath:
			if got := r.URL.Query().Get("ref"); got != release.Release.PeeledCommit {
				t.Errorf("models ref = %q, want peeled commit %q", got, release.Release.PeeledCommit)
			}
			if got := r.Header.Get("Accept"); got != githubRawMediaType {
				t.Errorf("models Accept = %q, want %q", got, githubRawMediaType)
			}
			_, _ = w.Write(embeddedCodexModels)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	registry := cache.NewRegistry()
	modelsValue := NewModelsCache(ModelsCacheConfig{RefreshInterval: time.Hour}, ModelsEdge{
		BaseURL: github.URL,
		Client:  github.Client(),
	}, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Prime(context.Background())

	currentBytes, status := modelsValue.Current()
	if !bytes.Equal(currentBytes, wantFallbackBytes) {
		t.Fatalf("cache retained %d bytes, want exact %d-byte %s catalog", len(currentBytes), len(wantFallbackBytes), release.Release.Tag)
	}
	if status.Source != "fallback" || status.Version != release.Release.Tag || status.LastSuccess != nil {
		t.Fatalf("cache status = %#v, want unchanged vendored fallback %s", status, release.Release.Tag)
	}

	copilotBytes, err := os.ReadFile("testdata/copilot-models-2026-07-18.json")
	if err != nil {
		t.Fatalf("read raw Copilot /models fixture: %v", err)
	}
	const (
		overlayModel         = "gpt-5.6-luna"
		overlayPromptLimit   = 123456
		overlayContextWindow = 234567
	)
	copilotBytes = withCompatibilityLimits(t, copilotBytes, overlayModel, overlayPromptLimit, overlayContextWindow)
	const reviewer = "gpt-5.6-luna"
	handler := Handler(discardHandlerLogger(), endpoint.OpenAICatalog(), Rendering{
		Render: RenderOpenAI,
		Codex: CodexDescriptor{
			Enabled: true,
			Models:  modelsValue,
			RenderConfig: CodexRenderConfig{
				AutoReviewModel: reviewer,
				OverrideLimits:  true,
			},
		},
	}, stubSource{status: http.StatusOK, body: copilotBytes})
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=1.2.3", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	entries := decodeRenderedCodex(t, recorder.Body.Bytes())
	wantSlugs := []string{
		"gpt-5.4-mini", "gpt-5.4", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Fatalf("rendered slugs = %q, want vendored/Copilot intersection %q", got, wantSlugs)
	}

	copilotModels, err := Decode(copilotBytes)
	if err != nil {
		t.Fatalf("decode raw Copilot /models fixture: %v", err)
	}
	forwardable := make(map[string]Model)
	for _, model := range Filter(copilotModels, endpoint.RouteOpenAIResponses) {
		forwardable[model.ID] = model
	}
	mutatedFields := map[string]struct{}{
		"auto_review_model_override": {},
		"context_window":             {},
		"max_context_window":         {},
	}
	seenReviewer := false
	seenLimitOverlay := false
	for _, entry := range entries {
		slug := decodeStringField(t, entry, "slug")
		source := vendoredModels[slug]
		for field, want := range source {
			if _, mutated := mutatedFields[field]; mutated {
				continue
			}
			got, present := entry[field]
			if !present {
				t.Errorf("%s lost upstream field %q", slug, field)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s.%s changed:\n got: %s\nwant: %s", slug, field, got, want)
			}
		}
		for field := range entry {
			if _, sourceField := source[field]; !sourceField {
				if _, governed := mutatedFields[field]; !governed {
					t.Errorf("%s fabricated ungoverned field %q", slug, field)
				}
			}
		}
		if got := decodeStringField(t, entry, "auto_review_model_override"); got != reviewer {
			t.Errorf("%s reviewer = %q, want %q", slug, got, reviewer)
		}
		if slug == reviewer {
			seenReviewer = true
		}
		model := forwardable[slug]
		assertOptionalOverlay(t, slug, entry, "context_window", model.Capabilities.Limits.MaxPromptTokens, source["context_window"])
		assertOptionalOverlay(t, slug, entry, "max_context_window", model.Capabilities.Limits.MaxContextWindowTokens, source["max_context_window"])
		if slug == overlayModel {
			assertJSONInt(t, entry, "context_window", overlayPromptLimit)
			assertJSONInt(t, entry, "max_context_window", overlayContextWindow)
			seenLimitOverlay = true
		}
	}
	if !seenReviewer {
		t.Fatalf("reviewer %q was injected but not emitted", reviewer)
	}
	if !seenLimitOverlay {
		t.Fatalf("limit overlay model %q was not emitted", overlayModel)
	}
}

func BenchmarkValidateVendoredCodexModels(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := validateCodexModels(embeddedCodexModels); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseVendoredCodexModels(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseCodexModels(embeddedCodexModels); err != nil {
			b.Fatal(err)
		}
	}
}

func gitBlobObjectID(body []byte) string {
	hasher := sha1.New() // Git's object identity format requires SHA-1.
	_, _ = fmt.Fprintf(hasher, "blob %d%c", len(body), byte(0))
	_, _ = hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}

func withCompatibilityLimits(t *testing.T, body []byte, slug string, promptLimit, contextWindow int) []byte {
	t.Helper()
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode raw Copilot /models fixture: %v", err)
	}
	for _, model := range envelope.Data {
		if model["id"] != slug {
			continue
		}
		model["capabilities"] = map[string]any{"limits": map[string]any{
			"max_prompt_tokens":         promptLimit,
			"max_context_window_tokens": contextWindow,
		}}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("encode raw Copilot /models fixture with limits: %v", err)
		}
		return encoded
	}
	t.Fatalf("raw Copilot /models fixture has no %q", slug)
	return nil
}

func rawCodexModelsBySlug(t *testing.T, body []byte) map[string]map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode raw vendored Codex snapshot: %v", err)
	}
	models := make(map[string]map[string]json.RawMessage, len(envelope.Models))
	for i, entry := range envelope.Models {
		var slug string
		if err := json.Unmarshal(entry["slug"], &slug); err != nil {
			t.Fatalf("decode raw models[%d] slug: %v", i, err)
		}
		models[slug] = entry
	}
	return models
}

func assertOptionalOverlay(t *testing.T, slug string, entry map[string]json.RawMessage, field string, limit *int, fallback json.RawMessage) {
	t.Helper()
	if limit == nil {
		if !bytes.Equal(entry[field], fallback) {
			t.Errorf("%s.%s = %s, want upstream fallback %s", slug, field, entry[field], fallback)
		}
		return
	}
	var got int
	if err := json.Unmarshal(entry[field], &got); err != nil {
		t.Fatalf("decode %s.%s: %v", slug, field, err)
	}
	if got != *limit {
		t.Errorf("%s.%s = %d, want Copilot limit %d", slug, field, got, *limit)
	}
}
