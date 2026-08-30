package wsforward

import (
	"context"
	"sync"
)

// SessionResult is the bounded terminal summary of an established WebSocket
// session.
type SessionResult struct {
	Terminal  SessionTerminal
	CloseCode int
	MsgsC2U   int64
	MsgsU2C   int64
	BytesC2U  int64
	BytesU2C  int64
}

type sessionResultHolder struct {
	mu     sync.RWMutex
	result SessionResult
	set    bool
}

type sessionResultHolderKey struct{}

// WithSessionResultHolder installs an empty per-request result handoff.
func WithSessionResultHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionResultHolderKey{}, &sessionResultHolder{})
}

// StoreSessionResult publishes one session result when a holder is present.
func StoreSessionResult(ctx context.Context, result SessionResult) {
	holder, ok := ctx.Value(sessionResultHolderKey{}).(*sessionResultHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	if !holder.set {
		holder.result = result
		holder.set = true
	}
	holder.mu.Unlock()
}

// SessionResultFromContext returns the published session result.
func SessionResultFromContext(ctx context.Context) (SessionResult, bool) {
	holder, ok := ctx.Value(sessionResultHolderKey{}).(*sessionResultHolder)
	if !ok || holder == nil {
		return SessionResult{}, false
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.result, holder.set
}
