package server

import (
	"context"
	"sync"
)

type matchedScope struct {
	ctx   context.Context
	probe bool
}

type matchedScopeHolder struct {
	mu    sync.RWMutex
	scope matchedScope
	set   bool
}

type matchedScopeHolderKey struct{}

func withMatchedScopeHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, matchedScopeHolderKey{}, &matchedScopeHolder{})
}

func publishMatchedScope(ctx context.Context, scope matchedScope) {
	holder, ok := ctx.Value(matchedScopeHolderKey{}).(*matchedScopeHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	if !holder.set {
		holder.scope = scope
		holder.set = true
	}
	holder.mu.Unlock()
}

func matchedScopeFromContext(ctx context.Context) (matchedScope, bool) {
	holder, ok := ctx.Value(matchedScopeHolderKey{}).(*matchedScopeHolder)
	if !ok || holder == nil {
		return matchedScope{}, false
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.scope, holder.set
}
