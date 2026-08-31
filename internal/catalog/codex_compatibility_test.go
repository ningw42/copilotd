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

const (
	latestStableCodexTag    = "rust-v0.151.0"
	latestStableCodexCommit = "78c290807ce710180111df227df3b7a4fe845452"
	latestStableCodexBlob   = "0c4137ad9560e1ac7b9baf1adc95dbc7051e2b6c"
	latestStableCodexSHA256 = "eb0d7b9a5dcaf103895c5f8a14c16b269df46e039b375a55ba97f6238542d2ed"
)

func TestLatestStableCodexCatalogRoundTripFidelity(t *testing.T) {
	latestBytes, err := os.ReadFile("testdata/codex-latest/models.json")
	if err != nil {
		t.Fatalf("read %s catalog fixture at %s: %v", latestStableCodexTag, latestStableCodexCommit, err)
	}
	if got := hashModels(latestBytes); got != latestStableCodexSHA256 {
		t.Fatalf("%s fixture hash = %s, want pinned %s", latestStableCodexTag, got, latestStableCodexSHA256)
	}
	if got := gitBlobObjectID(latestBytes); got != latestStableCodexBlob {
		t.Fatalf("%s fixture Git blob = %s, want upstream %s", latestStableCodexTag, got, latestStableCodexBlob)
	}
	if _, err := validateCodexModels(latestBytes); err != nil {
		t.Fatalf("decode %s catalog at %s: %v", latestStableCodexTag, latestStableCodexCommit, err)
	}
	latestModels := rawCodexModelsBySlug(t, latestBytes)

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
			_, _ = io.WriteString(w, `{"tag_name":"`+latestStableCodexTag+`","target_commitish":"main","prerelease":false,"draft":false}`)
		case codexReleaseCommitPath + latestStableCodexTag:
			if got := r.Header.Get("Accept"); got != githubSHA1MediaType {
				t.Errorf("commit Accept = %q, want %q", got, githubSHA1MediaType)
			}
			_, _ = io.WriteString(w, latestStableCodexCommit)
		case codexModelsContentPath:
			if got := r.URL.Query().Get("ref"); got != latestStableCodexCommit {
				t.Errorf("models ref = %q, want peeled commit %q", got, latestStableCodexCommit)
			}
			if got := r.Header.Get("Accept"); got != githubRawMediaType {
				t.Errorf("models Accept = %q, want %q", got, githubRawMediaType)
			}
			_, _ = w.Write(latestBytes)
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
	if !bytes.Equal(currentBytes, latestBytes) {
		t.Fatalf("cache retained %d bytes, want exact %d-byte %s catalog", len(currentBytes), len(latestBytes), latestStableCodexTag)
	}
	if status.Source != "fetched" || status.Version != latestStableCodexTag || status.LastSuccess == nil {
		t.Fatalf("cache status = %#v, want fetched %s with last success", status, latestStableCodexTag)
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
	handler(recorder, httptest.NewRequest(http.MethodGet, "/openai/v1/models?client_version=0.151.0", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	entries := decodeRenderedCodex(t, recorder.Body.Bytes())
	wantSlugs := []string{
		"gpt-5.4-mini", "gpt-5.4", "gpt-5.5",
		"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
	}
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Fatalf("rendered slugs = %q, want latest-stable/Copilot intersection %q", got, wantSlugs)
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
		source := latestModels[slug]
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

func BenchmarkValidateCodexModelsLatest(b *testing.B) {
	latestBytes, err := os.ReadFile("testdata/codex-latest/models.json")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := validateCodexModels(latestBytes); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseCodexModelsLatest(b *testing.B) {
	latestBytes, err := os.ReadFile("testdata/codex-latest/models.json")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseCodexModels(latestBytes); err != nil {
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
		t.Fatalf("decode raw Codex fixture: %v", err)
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
