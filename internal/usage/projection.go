package usage

import (
	"fmt"
	"iter"
)

// NativeCount declares one Surface-native token count of U. Its name is also
// the storage column, the Usage report metric name and the report wire key.
// Its path locates the count inside the provider's usage object, with a dot
// separating a detail container from the count within it. A required count
// must be reported by every qualifying completion. The semantic meaning of the
// count stays documented beside its Go field; a declaration carries no SQL,
// pricing formula or cross-count validation.
type NativeCount[U Usage] struct {
	name     string
	path     string
	required bool
	get      func(*U) (int64, bool)
	set      func(*U, int64)
}

func (c NativeCount[U]) Name() string   { return c.name }
func (c NativeCount[U]) Path() string   { return c.path }
func (c NativeCount[U]) Required() bool { return c.required }

// Value returns the count carried by native. False means an optional count was
// not reported; a required count is always reported.
func (c NativeCount[U]) Value(native U) (int64, bool) { return c.get(&native) }

// requiredCount binds a count that every qualifying completion reports to its
// int64 field of U.
func requiredCount[U Usage](name, path string, field func(*U) *int64) NativeCount[U] {
	return NativeCount[U]{
		name:     name,
		path:     path,
		required: true,
		get:      func(native *U) (int64, bool) { return *field(native), true },
		set:      func(native *U, value int64) { *field(native) = value },
	}
}

// optionalCount binds a nullable count to its *int64 field of U. Setting it
// stores a pointer to the setter's own copy of the value.
func optionalCount[U Usage](name, path string, field func(*U) **int64) NativeCount[U] {
	return NativeCount[U]{
		name: name,
		path: path,
		get: func(native *U) (int64, bool) {
			if value := *field(native); value != nil {
				return *value, true
			}
			return 0, false
		},
		set: func(native *U, value int64) { *field(native) = &value },
	}
}

// Projection is one Surface's complete native count projection in its fixed
// declaration order. It is the only source of that order: any vector with one
// nullable value per count follows it, and consumers build such vectors only by
// iterating All. A Projection is an immutable value that shares its storage.
type Projection[U Usage] struct {
	counts []NativeCount[U]
}

// Len is the number of declared counts.
func (p Projection[U]) Len() int { return len(p.counts) }

// All yields each declared count with its declaration position.
func (p Projection[U]) All() iter.Seq2[int, NativeCount[U]] {
	return func(yield func(int, NativeCount[U]) bool) {
		for index, count := range p.counts {
			if !yield(index, count) {
				return
			}
		}
	}
}

// Usage converts one nullable value per declared count, in declaration order,
// into typed native usage. A nil value is an unreported count. It fails when
// the vector length differs from the declaration or a required count is
// unreported. The result never aliases values: every reported optional count
// points to its own copy, and every unreported one stays nil.
func (p Projection[U]) Usage(values []*int64) (U, error) {
	var native U
	if len(values) != len(p.counts) {
		return native, fmt.Errorf("got %d native counts, want %d", len(values), len(p.counts))
	}
	for index, count := range p.counts {
		value := values[index]
		if value == nil {
			if count.required {
				var zero U
				return zero, fmt.Errorf("required native count %s is unreported", count.name)
			}
			continue
		}
		count.set(&native, *value)
	}
	return native, nil
}

// AnthropicProjection is the frozen seven-count Messages projection.
func AnthropicProjection() Projection[AnthropicUsage] { return anthropicProjection }

// OpenAIProjection is the frozen six-count Responses projection.
func OpenAIProjection() Projection[OpenAIUsage] { return openAIProjection }

var anthropicProjection = Projection[AnthropicUsage]{counts: []NativeCount[AnthropicUsage]{
	requiredCount("input_tokens", "input_tokens", func(u *AnthropicUsage) *int64 { return &u.InputTokens }),
	requiredCount("output_tokens", "output_tokens", func(u *AnthropicUsage) *int64 { return &u.OutputTokens }),
	optionalCount("cache_creation_input_tokens", "cache_creation_input_tokens", func(u *AnthropicUsage) **int64 { return &u.CacheCreationInputTokens }),
	optionalCount("cache_read_input_tokens", "cache_read_input_tokens", func(u *AnthropicUsage) **int64 { return &u.CacheReadInputTokens }),
	optionalCount("ephemeral_5m_input_tokens", "cache_creation.ephemeral_5m_input_tokens", func(u *AnthropicUsage) **int64 { return &u.Ephemeral5mInputTokens }),
	optionalCount("ephemeral_1h_input_tokens", "cache_creation.ephemeral_1h_input_tokens", func(u *AnthropicUsage) **int64 { return &u.Ephemeral1hInputTokens }),
	optionalCount("thinking_tokens", "output_tokens_details.thinking_tokens", func(u *AnthropicUsage) **int64 { return &u.ThinkingTokens }),
}}

var openAIProjection = Projection[OpenAIUsage]{counts: []NativeCount[OpenAIUsage]{
	requiredCount("input_tokens", "input_tokens", func(u *OpenAIUsage) *int64 { return &u.InputTokens }),
	requiredCount("output_tokens", "output_tokens", func(u *OpenAIUsage) *int64 { return &u.OutputTokens }),
	optionalCount("cached_tokens", "input_tokens_details.cached_tokens", func(u *OpenAIUsage) **int64 { return &u.CachedTokens }),
	optionalCount("cache_write_tokens", "input_tokens_details.cache_write_tokens", func(u *OpenAIUsage) **int64 { return &u.CacheWriteTokens }),
	optionalCount("reasoning_tokens", "output_tokens_details.reasoning_tokens", func(u *OpenAIUsage) **int64 { return &u.ReasoningTokens }),
	optionalCount("total_tokens", "total_tokens", func(u *OpenAIUsage) **int64 { return &u.TotalTokens }),
}}
