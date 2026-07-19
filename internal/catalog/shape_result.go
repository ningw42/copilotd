package catalog

import (
	"context"
	"sync"
)

// CatalogShape is the bounded access-log value for a successfully rendered
// OpenAI catalog. Query values and response contents never cross this seam.
type CatalogShape string

const (
	CatalogShapeOpenAI CatalogShape = "openai"
	CatalogShapeCodex  CatalogShape = "codex"
)

type shapeResultHolder struct {
	mu    sync.RWMutex
	shape CatalogShape
	set   bool
}

type shapeResultHolderKey struct{}

// WithShapeResultHolder injects an empty per-request result holder into ctx.
func WithShapeResultHolder(ctx context.Context) context.Context {
	return context.WithValue(ctx, shapeResultHolderKey{}, &shapeResultHolder{})
}

// ShapeResultFromContext loads the catalog shape recorded by Handler.
func ShapeResultFromContext(ctx context.Context) (CatalogShape, bool) {
	holder, ok := ctx.Value(shapeResultHolderKey{}).(*shapeResultHolder)
	if !ok || holder == nil {
		return "", false
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return holder.shape, holder.set
}

// StoreCatalogShape records shape in the holder installed on ctx. It is a
// no-op when the handler is used without the server access-log middleware.
func StoreCatalogShape(ctx context.Context, shape CatalogShape) {
	if shape != CatalogShapeOpenAI && shape != CatalogShapeCodex {
		return
	}
	holder, ok := ctx.Value(shapeResultHolderKey{}).(*shapeResultHolder)
	if !ok || holder == nil {
		return
	}
	holder.mu.Lock()
	holder.shape = shape
	holder.set = true
	holder.mu.Unlock()
}
