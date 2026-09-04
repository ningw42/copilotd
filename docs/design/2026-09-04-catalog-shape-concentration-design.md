# Concentrate the Catalog shape decision where the Catalog is rendered — Design

Status: proposed design (Candidate 1 from `2026-09-04` architecture review), pending implementation
Date: 2026-09-04
Review reference: `/tmp/architecture-review-20260904-223557.html` · Candidate 1 · Top recommendation

## 1. Goal & outcome

Give the "what does the OpenAI `/models` response look like right now?" question a single home. Today the
answering logic is spread across five files in one package plus one cross-package enum in a logging-side
module. This change moves the shape *tag* and its *discriminator* into the `internal/catalog` package next to
the render path that chooses them, exposes one render entry point that returns `(bytes, shape)`, and stops
`internal/catalog` from importing `internal/requestsummary` to name its own product.

**Outcome:** every file that decides what the `/models` response is becomes reachable from one module entry,
and the `catalog` module no longer depends on the logging/aggregation module for its own output shape.

## 2. Scope

**In scope:**

- Move the `CatalogShape` enum (currently `internal/requestsummary/summary.go`) into `internal/catalog` as a
  new `Shape` type, keeping the string values `"openai"` and `"codex"`.
- Add one catalog render entry point, `catalog.RenderOne`, that renders and returns `([]byte, Shape, error)`;
  the caller no longer re-derives the shape from request context.
- Refactor `catalog.Handler` so the render/shape decision lives in `RenderOne`. The handler consumes the
  returned shape and delegates *recording* to a caller-supplied callback, so `catalog` keeps no
  `requestsummary` import.
- Narrow `internal/requestsummary`: `RecordCatalogShape` accepts a plain `string` (an already-known shape),
  its `CatalogShape` type is removed, and `Finish` continues to emit the bounded `catalog_shape` attr.
- Wire the single recording call site in `internal/server`.

**Out of scope (unchanged):**

- ADR-0004 (provider-shaped catalogs) and ADR-0005 (Codex catalog re-emits pinned modelinfo) stay in force.
- No change to the two shapes themselves, the Codex `messages` fidelity contract, or any `/models` response body.
- The `deletion`-profile candidates (endpoint accessors, Codex source seam, `logging.New`) are not touched.

## 3. Direction of the fix

The failing direction is `catalog → requestsummary`. `requestsummary` is the terminal-summary aggregator; it
must not own the shape vocabulary that a domain renderer produces. After this change:

```
catalog (owns Shape, decides the shape, renders)   →  returns (bytes, shape)  →  server (records)
                                                      (no catalog->requestsummary import)
```

`requestsummary` stays an aggregator: it records a *known* string shape and reports it in the access record.

## 4. Interface changes

### 4.1 New file `internal/catalog/shape.go`

```go
// Shape identifies a successfully rendered OpenAI Catalog shape.
type Shape string

const (
    // ShapeOpenAI is the provider-shaped OpenAI Catalog.
    ShapeOpenAI Shape = "openai"
    // ShapeCodex is the client-shaped Codex catalog.
    ShapeCodex Shape = "codex"
)
```

Move the shape *discriminator* here as a method on `CodexDescriptor` (it already lives in `catalog`; this
relocates it beside the `Shape` type so the narrowing decision and its tag colocate):

```go
// servesCodexShape reports whether this descriptor's opt-in gates are open for the
// OpenAI Surface and the given request.
func (d CodexDescriptor) servesCodexShape(ep endpoint.Catalog, r *http.Request) bool
```

Body is unchanged from the current `servesCodexShape`:

```go
func (d CodexDescriptor) servesCodexShape(ep endpoint.Catalog, r *http.Request) bool {
    return ep.Surface() == endpoint.OpenAI &&
        r.URL.Query().Has("client_version") &&
        d.Enabled &&
        d.RenderConfig.mutates()
}
```

### 4.2 `internal/catalog/handler.go`

- `Rendering` gains one field (the recording callback injected by the composition root):

```go
type Rendering struct {
    Render func([]Model) ([]byte, error)
    Codex  CodexDescriptor
    // RecordShape, when non-nil, reports the catalog Shape that was served for an
    // OpenAI Surface render. It is a narrow hook so catalog never imports requestsummary.
    RecordShape func(context.Context, Shape)
}
```

- `Handler` keeps its existing signature `Handler(logger, ep, rendering, source)`. Its body changes so the
  decision + rendering move into `RenderOne`, and the shape is recorded only after a successful render:

```go
representation, shape, err := RenderOne(ep, rendering, r, filtered, responseCtx, logger)
if err != nil {
    apierror.Write(w, ep.Surface(), apierror.BadGateway, "could not render the models catalog")
    return
}
if rendering.RecordShape != nil {
    rendering.RecordShape(r.Context(), shape)
}
```

  The existing `servesCodexShape` function in `handler.go` is removed.

- New render entry point (in `handler.go`, near `Rendering`):

```go
// RenderOne renders the OpenAI Catalog for ep and returns the response bytes along
// with the Shape that was actually served. It owns the shape decision: whether the
// Codex client shape or the OpenAI provider shape wins.
func RenderOne(ep endpoint.Catalog, rendering Rendering, r *http.Request, models []Model, responseCtx context.Context, logger *slog.Logger) ([]byte, Shape, error)
```

  It relocates the current `Handler` shape-fork: when `rendering.Codex.servesCodexShape(...)` it renders the
  Codex shape (and emits the alias/reviewer warning logs, which move here with the render), otherwise it
  renders the OpenAI shape. It never records the shape.

### 4.3 `internal/requestsummary/summary.go`

- Delete the `CatalogShape` type and its `CatalogShapeOpenAI`/`CatalogShapeCodex` constants.
- Change the `Summary` field type to a plain `string`.
- `RecordCatalogShape` becomes:

```go
// RecordCatalogShape records a successfully rendered OpenAI Catalog shape, replacing an
// earlier valid shape. The shape is a known bounded token produced by internal/catalog.
// It does nothing without a summary, ignores non-token shapes, and ignores calls after Finish.
func RecordCatalogShape(ctx context.Context, shape string)
```

  Valid values are the literal `"openai"` and `"codex"`; anything else is ignored, preserving today's
  "invalid shape omitted" behavior. Keep two unexported string constants for these tokens.

- `Finish` continues to emit `slog.String(logging.CatalogShapeKey, s.catalogShape)`; behavior is unchanged.

### 4.4 `internal/server/handler.go`

- Populate the new `RecordShape` hook for the OpenAI catalog registration:

```go
registerCatalog(endpoint.OpenAICatalog(), catalog.Rendering{
    Render: catalog.RenderOpenAI,
    Codex:  catalogs.Codex,
    RecordShape: func(ctx context.Context, shape catalog.Shape) {
        requestsummary.RecordCatalogShape(ctx, string(shape))
    },
})
```

  The Anthropic catalog registration passes `RecordShape: nil` (unchanged — Anthropic never records a shape).

## 5. Dependency result

- `internal/catalog` no longer imports `internal/requestsummary` (production code). The only such import today
  is `internal/catalog/handler.go`, which is removed.
- `requestsummary` still knows nothing about `catalog`; it records a plain string token.

## 6. Tests

- `internal/requestsummary/summary_test.go`: replace `requestsummary.CatalogShape*` with the plain string tokens
  (`"openai"`, `"codex"`, `"invalid"`). The public shape type no longer exists here.
- `internal/catalog/handler_test.go`: provide `RecordShape` capturing closures where a shape assertion is
  needed, or assert through the attribute as today. The existing 14 `Handler(...)` call sites keep their
  signature; add `RecordShape: ...` only where the test checks the published shape.
- `internal/server/server_test.go` (`TestAccessLogRecordsOnlyTheBoundedCatalogShape`): call
  `requestsummary.RecordCatalogShape(r.Context(), "codex")` (string token).
- Integration tests assert `catalog_shape=codex` / `catalog_shape=openai` in logs — these values are unchanged.

## 7. Verification

- `nix develop -c go test ./internal/catalog/... ./internal/requestsummary/... ./internal/server/...`
- `nix develop -c go test -race ./... -count=1` (full suite)
- `nix fmt` (format the tree)

## 8. What "done" looks like

- `catalog.RenderOne` returns `(bytes, Shape, error)`; the shape decision + render live together.
- `internal/catalog` does not import `internal/requestsummary`.
- `requestsummary.RecordCatalogShape` takes a `string`; its `CatalogShape` enum is gone.
- The `/models` response body is byte-identical to today for both shapes; access records still emit
  `catalog_shape=openai|codex`; no catalog metadata is leaked into any response header.
