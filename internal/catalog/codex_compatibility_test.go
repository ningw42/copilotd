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
	"sort"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
)

// Tests in this file read the exact vendored snapshot. Every expectation is
// computed from its bytes, the captured Copilot /models fixture, or
// release.json, so a routine floor bump needs no test edit.

func TestEmbeddedCodexModelsLoadAtStartup(t *testing.T) {
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(embeddedCodexModels, &envelope); err != nil {
		t.Fatalf("decode vendored Codex envelope: %v", err)
	}
	wantSlugs := make([]string, len(envelope.Models))
	for i, entry := range envelope.Models {
		if err := json.Unmarshal(entry["slug"], &wantSlugs[i]); err != nil {
			t.Fatalf("decode vendored models[%d] slug: %v", i, err)
		}
	}
	sort.Strings(wantSlugs)
	if len(wantSlugs) == 0 {
		t.Fatal("vendored Codex snapshot has no entries")
	}

	loaded := mustDecodeCodexModels(embeddedCodexModels)
	gotSlugs := make([]string, 0, len(loaded))
	for slug, fields := range loaded {
		gotSlugs = append(gotSlugs, slug)

		var embeddedSlug string
		if err := json.Unmarshal(fields["slug"], &embeddedSlug); err != nil {
			t.Errorf("decode slug field for %q: %v", slug, err)
		} else if embeddedSlug != slug {
			t.Errorf("entry keyed by %q carries slug %q", slug, embeddedSlug)
		}
		if len(fields) <= 1 {
			t.Errorf("entry %q did not retain its non-slug fields", slug)
		}
		for field, raw := range fields {
			if !json.Valid(raw) {
				t.Errorf("entry %q field %q is not valid raw JSON", slug, field)
			}
		}
	}
	sort.Strings(gotSlugs)
	if !reflect.DeepEqual(gotSlugs, wantSlugs) {
		t.Errorf("embedded Codex slugs = %q, want every vendored entry %q", gotSlugs, wantSlugs)
	}
}

func TestVendoredCodexCatalogRoundTripFidelity(t *testing.T) {
	release := embeddedCodexRelease
	assertCodexReleaseRecordComplete(t, release)
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
	if _, present := vendoredModels[defaultSlug]; !present {
		t.Fatalf("vendored catalog has no audited bundled default %q", defaultSlug)
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
	// The audited bundled default is always vendored, so it anchors the reviewer
	// and the limits overlay however the rest of the catalog moves.
	const (
		overlayPromptLimit   = 123456
		overlayContextWindow = 234567
	)
	overlayModel, reviewer := defaultSlug, defaultSlug
	copilotBytes = withCompatibilityLimits(t, copilotBytes, overlayModel, overlayPromptLimit, overlayContextWindow)
	copilotModels, err := Decode(copilotBytes)
	if err != nil {
		t.Fatalf("decode raw Copilot /models fixture: %v", err)
	}
	forwardable := make(map[string]Model)
	var wantSlugs []string
	for _, model := range Filter(copilotModels, endpoint.RouteOpenAIResponses) {
		forwardable[model.ID] = model
		if _, vendored := vendoredModels[model.ID]; vendored {
			wantSlugs = append(wantSlugs, model.ID)
		}
	}

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
	if got := renderedSlugs(t, entries); !reflect.DeepEqual(got, wantSlugs) {
		t.Fatalf("rendered slugs = %q, want vendored/Copilot intersection %q", got, wantSlugs)
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

func assertCodexReleaseRecordComplete(t *testing.T, record codexReleaseRecord) {
	t.Helper()
	requiredStrings := []struct {
		name  string
		value string
	}{
		{name: "release.repository", value: record.Release.Repository},
		{name: "release.tag", value: record.Release.Tag},
		{name: "release.peeled_commit", value: record.Release.PeeledCommit},
		{name: "release.audit_date", value: record.Release.AuditDate},
		{name: "release.published_at", value: record.Release.PublishedAt},
		{name: "release.tag_object", value: record.Release.TagObject},
		{name: "models.source_path", value: record.Models.SourcePath},
		{name: "models.git_blob", value: record.Models.GitBlob},
		{name: "models.sha256", value: record.Models.SHA256},
		{name: "models.audited_bundled_default", value: record.Models.AuditedBundledDefault},
		{name: "manifest.asset_name", value: record.Manifest.AssetName},
		{name: "manifest.sha256", value: record.Manifest.SHA256},
		{name: "executable_audit.asset_name", value: record.ExecutableAudit.AssetName},
		{name: "executable_audit.archive_sha256", value: record.ExecutableAudit.ArchiveSHA256},
		{name: "executable_audit.executable_sha256", value: record.ExecutableAudit.ExecutableSHA256},
	}
	for _, field := range requiredStrings {
		if field.value == "" {
			t.Errorf("embedded Codex release record field %s is empty", field.name)
		}
	}
	if !isStableCodexReleaseTag(record.Release.Tag) {
		t.Errorf("embedded Codex release tag %q is not stable", record.Release.Tag)
	}
	if !isGitCommitSHA(record.Release.PeeledCommit) {
		t.Errorf("embedded Codex peeled commit %q is invalid", record.Release.PeeledCommit)
	}
	if record.Release.GitHubReleaseID <= 0 {
		t.Error("embedded Codex release record has no positive GitHub release ID")
	}
	if record.Manifest.GitHubAssetID <= 0 {
		t.Error("embedded Codex release record has no positive manifest asset ID")
	}
	if record.Models.Size <= 0 {
		t.Error("embedded Codex release record has no positive models size")
	}
}

func gitBlobObjectID(body []byte) string {
	hasher := sha1.New() // Git's object identity format requires SHA-1.
	_, _ = fmt.Fprintf(hasher, "blob %d%c", len(body), byte(0))
	_, _ = hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}

// withCompatibilityLimits gives slug live Copilot limits, first adding it as a
// Responses-forwardable model when the captured fixture lacks it.
func withCompatibilityLimits(t *testing.T, body []byte, slug string, promptLimit, contextWindow int) []byte {
	t.Helper()
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode raw Copilot /models fixture: %v", err)
	}
	var target map[string]any
	for _, model := range envelope.Data {
		if model["id"] == slug {
			target = model
			break
		}
	}
	if target == nil {
		target = map[string]any{
			"id":                   slug,
			"name":                 slug,
			"vendor":               "OpenAI",
			"model_picker_enabled": true,
			"supported_endpoints":  []string{string(endpoint.RouteOpenAIResponses)},
		}
		envelope.Data = append(envelope.Data, target)
	}
	target["capabilities"] = map[string]any{"limits": map[string]any{
		"max_prompt_tokens":         promptLimit,
		"max_context_window_tokens": contextWindow,
	}}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode raw Copilot /models fixture with limits: %v", err)
	}
	return encoded
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
