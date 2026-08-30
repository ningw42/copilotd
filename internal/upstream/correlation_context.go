package upstream

import (
	"context"
	"sync"
)

type correlationHolder struct {
	mu  sync.RWMutex
	ctx context.Context
	set bool
}

type correlationHolderKey struct{}

// WithCorrelationHolder installs the per-request handoff used to publish a
// response-path context to after-handler observers.
func WithCorrelationHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, correlationHolderKey{}, &correlationHolder{})
}

// CorrelatedContextFromContext returns the response-path context published for
// a differing upstream request id.
func CorrelatedContextFromContext(ctx context.Context) (context.Context, bool) {
	holder, ok := ctx.Value(correlationHolderKey{}).(*correlationHolder)
	if !ok || holder == nil {
		return nil, false
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.ctx, holder.set
}

func publishCorrelatedContext(ctx, correlated context.Context) {
	holder, ok := ctx.Value(correlationHolderKey{}).(*correlationHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	if !holder.set {
		holder.ctx = correlated
		holder.set = true
	}
	holder.mu.Unlock()
}
