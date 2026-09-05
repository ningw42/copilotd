// Package usage defines Surface-native token usage observations and the prompt,
// non-blocking sink contract consumed by the Usage meter Shim.
package usage

import "time"

// Sink receives one observed successful completion. Record must be safe for
// concurrent callers and must not block, do I/O, or emit synchronous logs: it
// is called from hooks inside the SSE pump and WebSocket server pump. A call
// attempts enqueueing; it does not acknowledge persistence. The supplied Turn,
// including pointed-to optional values, is an immutable snapshot.
type Sink interface {
	Record(Turn)
}

// Transport names the path that served a Turn.
type Transport string

const (
	TransportBuffered  Transport = "buffered"
	TransportSSE       Transport = "sse"
	TransportWebSocket Transport = "websocket"
)

// Turn is the Surface-independent completion-observation envelope. Usage
// carries the verbatim Surface-native token fields and selects the destination
// table.
type Turn struct {
	At         time.Time
	RequestID  string // inbound HTTP correlation; empty if unavailable
	ResponseID string // upstream message.id / response.id, not an HTTP request ID
	Model      string // as reported upstream, never the client's requested name
	Transport  Transport
	TurnIndex  int // submission-attempt ordinal within the Shim instance
	Usage      Usage
}

// Usage is a closed sum: only the two Surface-native records satisfy it.
type Usage interface {
	isUsage()
}

// AnthropicUsage mirrors Messages API usage verbatim.
//
// InputTokens is the UNCACHED REMAINDER. Real input is InputTokens plus
// CacheCreationInputTokens plus CacheReadInputTokens. The Ephemeral fields are
// TTL subsets inside CacheCreationInputTokens. OutputTokens is the COMPLETE
// count; ThinkingTokens is a re-tokenized subset already inside it, including
// delimiters, and subtraction only approximates non-reasoning output. A nil
// pointer means upstream did not report a numeric value; a pointer to zero means
// upstream reported zero.
type AnthropicUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
	Ephemeral5mInputTokens   *int64 // usage.cache_creation.ephemeral_5m_input_tokens
	Ephemeral1hInputTokens   *int64 // usage.cache_creation.ephemeral_1h_input_tokens
	ThinkingTokens           *int64 // usage.output_tokens_details.thinking_tokens; subset of OutputTokens
}

// OpenAIUsage mirrors Responses API usage verbatim.
//
// InputTokens is the COMPLETE count; CachedTokens and CacheWriteTokens are
// subsets already inside it. OutputTokens is the COMPLETE count;
// ReasoningTokens is a subset already inside it. TotalTokens is stored only as
// reported, never recalculated. A nil pointer means no numeric report; a pointer
// to zero means upstream reported zero.
type OpenAIUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CachedTokens     *int64 // input_tokens_details.cached_tokens
	CacheWriteTokens *int64 // input_tokens_details.cache_write_tokens
	ReasoningTokens  *int64 // output_tokens_details.reasoning_tokens
	TotalTokens      *int64
}

func (AnthropicUsage) isUsage() {}
func (OpenAIUsage) isUsage()    {}
