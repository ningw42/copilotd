package shim

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/sse"
)

type benchmarkPostCommitHooks struct{}

func (benchmarkPostCommitHooks) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	return []sse.Frame{frame}
}

func (benchmarkPostCommitHooks) Finalize(context.Context) []sse.Frame { return nil }

func (benchmarkPostCommitHooks) TransformClientMessage(context.Context, *Message) bool { return true }

func (benchmarkPostCommitHooks) TransformServerMessage(context.Context, *Message) bool { return true }

var (
	benchmarkFrames []sse.Frame
	benchmarkEmit   bool
)

func BenchmarkHookMonitoring(b *testing.B) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := Registry{{
		Name:    "benchmark",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return benchmarkPostCommitHooks{}
		},
	}}

	for _, monitoring := range []struct {
		name      string
		threshold time.Duration
	}{
		{name: "unmonitored", threshold: 0},
		{name: "enabled", threshold: time.Hour},
	} {
		b.Run("sse_event_transform/"+monitoring.name, func(b *testing.B) {
			chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
			adapter := chain.StreamAdapter(NewMonitor(logger, monitoring.threshold), ctx)
			frame := sse.Frame{Type: "response.output_text.delta", Raw: []byte("event: response.output_text.delta\ndata: {}\n\n")}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkFrames = adapter.Transform(ctx, frame)
			}
		})

		b.Run("websocket_client_message/"+monitoring.name, func(b *testing.B) {
			chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
			adapter := chain.WSClientAdapter(NewMonitor(logger, monitoring.threshold), ctx)
			message := Message{Kind: MessageText, Data: []byte(`{"type":"response.create"}`)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkEmit = adapter(ctx, &message)
			}
		})

		b.Run("websocket_server_message/"+monitoring.name, func(b *testing.B) {
			chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
			adapter := chain.WSServerAdapter(NewMonitor(logger, monitoring.threshold), ctx)
			message := Message{Kind: MessageText, Data: []byte(`{"type":"response.output_text.delta"}`)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkEmit = adapter(ctx, &message)
			}
		})
	}
}
