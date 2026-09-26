package usage_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
)

func pointer(value int64) *int64 { return &value }

type declaredCount struct {
	name, path string
	required   bool
}

// declaredByName deliberately ignores declaration order, which no consumer may
// depend on.
func declaredByName[U usage.Usage](projection usage.Projection[U]) map[string]declaredCount {
	counts := map[string]declaredCount{}
	for _, count := range projection.All() {
		counts[count.Name()] = declaredCount{name: count.Name(), path: count.Path(), required: count.Required()}
	}
	return counts
}

// vectorFromNames builds a vector through the declaration, pointing each
// reported count at the matching element of storage.
func vectorFromNames[U usage.Usage](projection usage.Projection[U], storage []int64, values map[string]int64) []*int64 {
	vector := make([]*int64, projection.Len())
	for index, count := range projection.All() {
		if value, ok := values[count.Name()]; ok {
			storage[index] = value
			vector[index] = &storage[index]
		}
	}
	return vector
}

// usageFromNames builds a vector through the declaration from values keyed by
// name; a name missing from values is unreported.
func usageFromNames[U usage.Usage](t *testing.T, projection usage.Projection[U], values map[string]int64) U {
	t.Helper()
	native, err := projection.Usage(vectorFromNames(projection, make([]int64, projection.Len()), values))
	if err != nil {
		t.Fatalf("Usage(%v) error = %v", values, err)
	}
	return native
}

// The expectations below are written independently of the declaration, so a
// row that binds a name or path to the wrong field of the same type fails.
func TestAnthropicProjectionBindsEachNameAndPathToItsDocumentedField(t *testing.T) {
	want := map[string]declaredCount{
		"input_tokens":                {name: "input_tokens", path: "input_tokens", required: true},
		"output_tokens":               {name: "output_tokens", path: "output_tokens", required: true},
		"cache_creation_input_tokens": {name: "cache_creation_input_tokens", path: "cache_creation_input_tokens"},
		"cache_read_input_tokens":     {name: "cache_read_input_tokens", path: "cache_read_input_tokens"},
		"ephemeral_5m_input_tokens":   {name: "ephemeral_5m_input_tokens", path: "cache_creation.ephemeral_5m_input_tokens"},
		"ephemeral_1h_input_tokens":   {name: "ephemeral_1h_input_tokens", path: "cache_creation.ephemeral_1h_input_tokens"},
		"thinking_tokens":             {name: "thinking_tokens", path: "output_tokens_details.thinking_tokens"},
	}
	if got := declaredByName(usage.AnthropicProjection()); !reflect.DeepEqual(got, want) {
		t.Errorf("Anthropic declaration = %+v, want %+v", got, want)
	}
	got := usageFromNames(t, usage.AnthropicProjection(), map[string]int64{
		"input_tokens": 11, "output_tokens": 12, "cache_creation_input_tokens": 13, "cache_read_input_tokens": 14,
		"ephemeral_5m_input_tokens": 15, "ephemeral_1h_input_tokens": 16, "thinking_tokens": 17,
	})
	wantUsage := usage.AnthropicUsage{
		InputTokens: 11, OutputTokens: 12, CacheCreationInputTokens: pointer(13), CacheReadInputTokens: pointer(14),
		Ephemeral5mInputTokens: pointer(15), Ephemeral1hInputTokens: pointer(16), ThinkingTokens: pointer(17),
	}
	if !reflect.DeepEqual(got, wantUsage) {
		t.Errorf("Anthropic usage = %+v, want %+v", got, wantUsage)
	}
}

func TestOpenAIProjectionBindsEachNameAndPathToItsDocumentedField(t *testing.T) {
	want := map[string]declaredCount{
		"input_tokens":       {name: "input_tokens", path: "input_tokens", required: true},
		"output_tokens":      {name: "output_tokens", path: "output_tokens", required: true},
		"cached_tokens":      {name: "cached_tokens", path: "input_tokens_details.cached_tokens"},
		"cache_write_tokens": {name: "cache_write_tokens", path: "input_tokens_details.cache_write_tokens"},
		"reasoning_tokens":   {name: "reasoning_tokens", path: "output_tokens_details.reasoning_tokens"},
		"total_tokens":       {name: "total_tokens", path: "total_tokens"},
	}
	if got := declaredByName(usage.OpenAIProjection()); !reflect.DeepEqual(got, want) {
		t.Errorf("OpenAI declaration = %+v, want %+v", got, want)
	}
	got := usageFromNames(t, usage.OpenAIProjection(), map[string]int64{
		"input_tokens": 21, "output_tokens": 22, "cached_tokens": 23, "cache_write_tokens": 24,
		"reasoning_tokens": 25, "total_tokens": 26,
	})
	wantUsage := usage.OpenAIUsage{
		InputTokens: 21, OutputTokens: 22, CachedTokens: pointer(23), CacheWriteTokens: pointer(24),
		ReasoningTokens: pointer(25), TotalTokens: pointer(26),
	}
	if !reflect.DeepEqual(got, wantUsage) {
		t.Errorf("OpenAI usage = %+v, want %+v", got, wantUsage)
	}
}

func TestProjectionNamesAndPathsAreUniqueWellFormedAndReadable(t *testing.T) {
	t.Run("Anthropic", func(t *testing.T) { assertWellFormedProjection(t, usage.AnthropicProjection()) })
	t.Run("OpenAI", func(t *testing.T) { assertWellFormedProjection(t, usage.OpenAIProjection()) })
}

// assertWellFormedProjection requires unique non-empty names, unique paths
// without empty segments, and a Value accessor that reads what Usage stored.
func assertWellFormedProjection[U usage.Usage](t *testing.T, projection usage.Projection[U]) {
	t.Helper()
	native := distinctUsage(t, projection)
	names, paths := map[string]bool{}, map[string]bool{}
	for index, count := range projection.All() {
		name, path := count.Name(), count.Path()
		if name == "" || names[name] {
			t.Errorf("count %d name %q is empty or duplicated", index, name)
		}
		if paths[path] {
			t.Errorf("count %d path %q is duplicated", index, path)
		}
		for segment := range strings.SplitSeq(path, ".") {
			if segment == "" {
				t.Errorf("count %d path %q has an empty segment", index, path)
			}
		}
		names[name], paths[path] = true, true
		if got, ok := count.Value(native); !ok || got != int64(100+index) {
			t.Errorf("count %s Value = %d, %t; want %d, true", name, got, ok, 100+index)
		}
	}
}

// distinctUsage reports 100 plus each count's declaration position.
func distinctUsage[U usage.Usage](t *testing.T, projection usage.Projection[U]) U {
	t.Helper()
	vector := make([]*int64, projection.Len())
	for index := range projection.All() {
		vector[index] = pointer(int64(100 + index))
	}
	native, err := projection.Usage(vector)
	if err != nil {
		t.Fatal(err)
	}
	return native
}

func TestProjectionUsageRejectsMissingRequiredCounts(t *testing.T) {
	for _, name := range []string{"input_tokens", "output_tokens"} {
		t.Run("Anthropic "+name, func(t *testing.T) {
			assertMissingRequired(t, usage.AnthropicProjection(), name)
		})
		t.Run("OpenAI "+name, func(t *testing.T) {
			assertMissingRequired(t, usage.OpenAIProjection(), name)
		})
	}
}

func assertMissingRequired[U usage.Usage](t *testing.T, projection usage.Projection[U], missing string) {
	t.Helper()
	vector := make([]*int64, projection.Len())
	for index, count := range projection.All() {
		if count.Name() != missing {
			vector[index] = pointer(1)
		}
	}
	native, err := projection.Usage(vector)
	if err == nil {
		t.Fatalf("Usage without %s = %+v, want error", missing, native)
	}
	var zero U
	if !reflect.DeepEqual(native, zero) {
		t.Errorf("failed Usage = %+v, want zero value", native)
	}
}

func TestProjectionUsageRejectsWrongVectorLength(t *testing.T) {
	for _, length := range []int{0, 5, 6, 8} {
		vector := make([]*int64, length)
		for index := range vector {
			vector[index] = pointer(1)
		}
		if length != usage.AnthropicProjection().Len() {
			if native, err := usage.AnthropicProjection().Usage(vector); err == nil {
				t.Errorf("Anthropic Usage(len %d) = %+v, want error", length, native)
			}
		}
		if length != usage.OpenAIProjection().Len() {
			if native, err := usage.OpenAIProjection().Usage(vector); err == nil {
				t.Errorf("OpenAI Usage(len %d) = %+v, want error", length, native)
			}
		}
	}
	if got := usage.AnthropicProjection().Len(); got != 7 {
		t.Errorf("Anthropic projection length = %d, want 7", got)
	}
	if got := usage.OpenAIProjection().Len(); got != 6 {
		t.Errorf("OpenAI projection length = %d, want 6", got)
	}
}

func TestProjectionUsageOwnsItsSnapshotAfterTheCallerReusesItsVector(t *testing.T) {
	projection := usage.AnthropicProjection()
	storage := make([]int64, projection.Len())
	vector := vectorFromNames(projection, storage, map[string]int64{
		"input_tokens": 1, "output_tokens": 2, "cache_creation_input_tokens": 3, "cache_read_input_tokens": 4,
		"ephemeral_1h_input_tokens": 6, "thinking_tokens": 7,
	})
	native, err := projection.Usage(vector)
	if err != nil {
		t.Fatal(err)
	}
	want := usage.AnthropicUsage{
		InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: pointer(3), CacheReadInputTokens: pointer(4),
		Ephemeral1hInputTokens: pointer(6), ThinkingTokens: pointer(7),
	}
	if !reflect.DeepEqual(native, want) {
		t.Fatalf("Usage = %+v, want %+v", native, want)
	}
	for _, field := range []*int64{native.CacheCreationInputTokens, native.CacheReadInputTokens, native.Ephemeral1hInputTokens, native.ThinkingTokens} {
		for index := range storage {
			if field == &storage[index] {
				t.Fatalf("optional count aliases caller storage index %d", index)
			}
		}
	}

	// Reuse both the scan storage and the vector for a later row.
	for index := range storage {
		storage[index] = 0
	}
	extra := int64(99)
	for index := range vector {
		vector[index] = &extra
	}
	if !reflect.DeepEqual(native, want) || native.Ephemeral5mInputTokens != nil {
		t.Errorf("Usage snapshot changed after caller reuse: %+v, want %+v", native, want)
	}
}

func TestProjectionUsageKeepsUnreportedOptionalNilAndReportedZero(t *testing.T) {
	projection := usage.OpenAIProjection()
	storage := make([]int64, projection.Len())
	vector := vectorFromNames(projection, storage, map[string]int64{"input_tokens": 0, "output_tokens": 0, "cached_tokens": 0, "reasoning_tokens": 0})
	native, err := projection.Usage(vector)
	if err != nil {
		t.Fatal(err)
	}
	want := usage.OpenAIUsage{CachedTokens: pointer(0), ReasoningTokens: pointer(0)}
	if !reflect.DeepEqual(native, want) || native.CacheWriteTokens != nil || native.TotalTokens != nil {
		t.Errorf("Usage = %+v, want %+v", native, want)
	}
	if native.CachedTokens == native.ReasoningTokens {
		t.Error("reported zeros share storage")
	}
	for index := range storage {
		if native.CachedTokens == &storage[index] || native.ReasoningTokens == &storage[index] {
			t.Errorf("reported zero aliases caller storage index %d", index)
		}
	}
}
