package forward

import (
	"context"
	"sync"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/sse"
)

// StreamResult is the request-level summary of one streamed response. Surface
// is the canonical low-cardinality inbound surface name.
type StreamResult struct {
	Surface string
	Outcome sse.Outcome
	Frames  int
}

// streamResultHolder carries a stream result across the forward/access-log
// boundary without changing the handler signature. Its mutability stays hidden
// behind the context inject/store/load functions below.
type streamResultHolder struct {
	mu     sync.RWMutex
	result StreamResult
	set    bool
}

type streamResultHolderKey struct{}

// WithStreamResultHolder injects an empty per-request result holder into ctx.
func WithStreamResultHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamResultHolderKey{}, &streamResultHolder{})
}

// StreamResultFromContext loads the stored stream result. ok is false for
// non-stream requests and for requests that ended before the SSE pump ran.
func StreamResultFromContext(ctx context.Context) (result StreamResult, ok bool) {
	h, ok := ctx.Value(streamResultHolderKey{}).(*streamResultHolder)
	if !ok || h == nil {
		return StreamResult{}, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.result, h.set
}

// StoreStreamResult records result in the holder installed on ctx. It is a
// no-op when no access-log holder is present.
func StoreStreamResult(ctx context.Context, result StreamResult) {
	holder, ok := ctx.Value(streamResultHolderKey{}).(*streamResultHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	holder.result = result
	holder.set = true
	holder.mu.Unlock()
}

func streamSurface(surface apierror.Surface) string {
	switch surface {
	case apierror.Anthropic:
		return "anthropic"
	case apierror.OpenAI:
		return "openai"
	default:
		panic("forward: unknown stream surface")
	}
}
