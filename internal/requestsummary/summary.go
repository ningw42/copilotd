package requestsummary

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
)

type summaryKey struct{}

// Summary accumulates the bounded completion facts for one terminal request
// summary. Its state is private; only the pointer returned by Begin can finalize
// the publication.
type Summary struct {
	mu sync.Mutex

	closed      bool
	publication Publication

	base    context.Context
	streams StreamOutcomeObserver

	binding    Binding
	hasBinding bool

	correlation    context.Context
	hasCorrelation bool

	stream    StreamResult
	hasStream bool

	catalogShape    CatalogShape
	hasCatalogShape bool

	webSocket    WebSocketResult
	hasWebSocket bool

	hookOverrunsApplicable bool
	hookOverruns           int
}

// StreamOutcomeObserver observes the completed SSE outcome for one Surface.
type StreamOutcomeObserver interface {
	ObserveStreamOutcome(surface string, outcome sse.Outcome)
}

// Binding is registration-owned request scope and probe classification.
type Binding struct {
	Context context.Context
	Probe   bool
}

// StreamResult contains the bounded completion facts from one SSE pump.
type StreamResult struct {
	Surface   string
	Outcome   sse.Outcome
	Frames    int
	Fallbacks int
}

// CatalogShape identifies a successfully rendered OpenAI Catalog shape.
type CatalogShape string

const (
	// CatalogShapeOpenAI is the provider-shaped OpenAI Catalog.
	CatalogShapeOpenAI CatalogShape = "openai"
	// CatalogShapeCodex is the client-shaped Codex catalog.
	CatalogShapeCodex CatalogShape = "codex"
)

// WebSocketTerminal identifies how an established WebSocket session ended.
type WebSocketTerminal string

const (
	// WebSocketClientClosed means the downstream client ended the session.
	WebSocketClientClosed WebSocketTerminal = "client_closed"
	// WebSocketUpstreamClosed means the session ended from the upstream side or
	// copilotd's own drain, rather than by the client or abnormally.
	WebSocketUpstreamClosed WebSocketTerminal = "upstream_closed"
	// WebSocketError means the session ended abnormally.
	WebSocketError WebSocketTerminal = "error"
)

// WebSocketResult contains bounded completion facts for an established
// WebSocket session.
type WebSocketResult struct {
	Terminal  WebSocketTerminal
	CloseCode int
	MsgsC2U   int64
	MsgsU2C   int64
	BytesC2U  int64
	BytesU2C  int64
}

// ResponseResult contains the downstream response facts known after a handler
// returns.
type ResponseResult struct {
	Method   string
	Status   int
	Bytes    int64
	Duration time.Duration
}

// Publication is the non-emitting plan for the server-owned access record.
type Publication struct {
	Context context.Context
	Level   slog.Level
	Attrs   []slog.Attr
}

// Begin creates a Summary, stores its opaque handle in a child context, and
// returns both. ctx and streams are required and must be non-nil.
func Begin(ctx context.Context, streams StreamOutcomeObserver) (context.Context, *Summary) {
	if streams == nil {
		panic("requestsummary: nil stream observer")
	}
	summary := &Summary{base: ctx, streams: streams}
	return context.WithValue(ctx, summaryKey{}, summary), summary
}

// RecordBinding records the first supplied registration-owned context and probe
// classification. It does nothing without a summary, ignores duplicate
// bindings, and ignores calls after Finish.
func RecordBinding(ctx context.Context, binding Binding) {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed || summary.hasBinding {
		return
	}
	summary.binding = binding
	summary.hasBinding = true
}

// RecordCorrelation records the first supplied context for a differing upstream
// request id. It does nothing without a summary, ignores duplicate contexts, and
// ignores calls after Finish.
func RecordCorrelation(ctx, correlated context.Context) {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed || summary.hasCorrelation {
		return
	}
	summary.correlation = correlated
	summary.hasCorrelation = true
}

// RecordStream records a supplied SSE completion projection without normalizing
// its scalar values, replacing an earlier stream result. It does nothing without
// a summary and ignores calls after Finish.
func RecordStream(ctx context.Context, result StreamResult) {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed {
		return
	}
	summary.stream = result
	summary.hasStream = true
}

// RecordCatalogShape records a valid successfully rendered Catalog shape,
// replacing an earlier valid shape. It does nothing without a summary, ignores
// invalid shapes, and ignores calls after Finish.
func RecordCatalogShape(ctx context.Context, shape CatalogShape) {
	if shape != CatalogShapeOpenAI && shape != CatalogShapeCodex {
		return
	}
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed {
		return
	}
	summary.catalogShape = shape
	summary.hasCatalogShape = true
}

// RecordWebSocket records the first supplied established-session completion
// without normalizing its scalar values. It does nothing without a summary,
// ignores duplicate results, and ignores calls after Finish.
func RecordWebSocket(ctx context.Context, result WebSocketResult) {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed || summary.hasWebSocket {
		return
	}
	summary.webSocket = result
	summary.hasWebSocket = true
}

// HookOverrunRecorder counts individual post-commit shim hook invocations that
// cross the configured threshold for one request. Its methods are safe for
// concurrent use by both WebSocket directions.
type HookOverrunRecorder struct {
	summary *Summary
}

// NewHookOverrunRecorder marks Hook overrun counts as applicable to the request
// carrying ctx and returns its recorder. Calling it without an installed
// Summary returns a no-op recorder. Calls after Finish cannot reopen the summary.
func NewHookOverrunRecorder(ctx context.Context) *HookOverrunRecorder {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return &HookOverrunRecorder{}
	}
	summary.mu.Lock()
	if !summary.closed {
		summary.hookOverrunsApplicable = true
	}
	summary.mu.Unlock()
	return &HookOverrunRecorder{summary: summary}
}

// Increment records one threshold crossing. Increments after Finish and calls
// on a recorder created without a Summary are ignored.
func (r *HookOverrunRecorder) Increment() {
	if r == nil || r.summary == nil {
		return
	}
	r.summary.mu.Lock()
	defer r.summary.mu.Unlock()
	if r.summary.closed || !r.summary.hookOverrunsApplicable {
		return
	}
	r.summary.hookOverruns++
}

// MatchedContext returns the registration-owned binding context before Finish,
// if recorded. It never substitutes a later correlation context; after Finish it
// returns no context.
func MatchedContext(ctx context.Context) (context.Context, bool) {
	summary, ok := summaryFromContext(ctx)
	if !ok {
		return nil, false
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	if summary.closed || !summary.hasBinding {
		return nil, false
	}
	return summary.binding.Context, true
}

func summaryFromContext(ctx context.Context) (*Summary, bool) {
	summary, ok := ctx.Value(summaryKey{}).(*Summary)
	return summary, ok
}

// Finish closes publication, observes a recorded stream after releasing the
// summary's synchronization, and returns a publication plan. The first response
// wins; later calls repeat no observation and return fresh caller-owned Attrs.
func (s *Summary) Finish(response ResponseResult) Publication {
	s.mu.Lock()
	if s.closed {
		publication := clonePublication(s.publication)
		s.mu.Unlock()
		return publication
	}
	s.closed = true

	ctx := s.base
	probe := false
	if s.hasBinding {
		ctx = s.binding.Context
		probe = s.binding.Probe
	}
	if s.hasCorrelation {
		ctx = s.correlation
	}
	level := slog.LevelInfo
	if response.Status >= 500 {
		level = slog.LevelWarn
	}
	if s.hasWebSocket && s.webSocket.Terminal == WebSocketError {
		level = slog.LevelWarn
	}
	if s.hasStream {
		switch s.stream.Outcome {
		case sse.OutcomeSynthesized, sse.OutcomeStall, sse.OutcomeUpstreamError, sse.OutcomeShimError:
			level = slog.LevelWarn
		}
	}
	if probe {
		level = slog.LevelDebug
	}
	attrs := []slog.Attr{
		slog.String(logging.MethodKey, response.Method),
		slog.Int(logging.StatusKey, response.Status),
		slog.Int64(logging.BytesKey, response.Bytes),
		slog.Duration(logging.DurationKey, response.Duration),
	}
	stream, hasStream := s.stream, s.hasStream
	if hasStream {
		attrs = append(attrs,
			slog.String(logging.OutcomeKey, string(stream.Outcome)),
			slog.Int(logging.FramesKey, stream.Frames),
			slog.Int(logging.FallbacksKey, stream.Fallbacks),
		)
	}
	if s.hasCatalogShape {
		attrs = append(attrs, slog.String(logging.CatalogShapeKey, string(s.catalogShape)))
	}
	if s.hasWebSocket {
		attrs = append(attrs,
			slog.String(logging.TerminalReasonKey, string(s.webSocket.Terminal)),
			slog.Int(logging.CloseCodeKey, s.webSocket.CloseCode),
			slog.Int64(logging.MsgsC2UKey, s.webSocket.MsgsC2U),
			slog.Int64(logging.MsgsU2CKey, s.webSocket.MsgsU2C),
			slog.Int64(logging.BytesC2UKey, s.webSocket.BytesC2U),
			slog.Int64(logging.BytesU2CKey, s.webSocket.BytesU2C),
		)
	}
	if s.hookOverrunsApplicable {
		attrs = append(attrs, slog.Int(logging.HookOverrunsKey, s.hookOverruns))
	}
	s.publication = Publication{Context: ctx, Level: level, Attrs: attrs}
	publication := clonePublication(s.publication)
	s.mu.Unlock()

	if hasStream {
		s.streams.ObserveStreamOutcome(stream.Surface, stream.Outcome)
	}
	return publication
}

func clonePublication(publication Publication) Publication {
	publication.Attrs = append([]slog.Attr(nil), publication.Attrs...)
	return publication
}
