# Concentrate the served-endpoint contract in `internal/endpoint`

**Status:** proposed
**Date:** 2026-07-20

## Summary

Today copilotd reconstructs each served route's contract — its Surface, its
upstream path, its streaming semantics — across six packages. This design
concentrates that contract into one dependency-light package, `internal/endpoint`,
where each served operation is a single typed value. Registration, forwarding,
catalog filtering, error rendering, and metrics all consume the same contract
instead of re-deriving it. The work also closes a standing gap: the Anthropic
Count Tokens route has no way to declare "never SSE," so streaming semantics leak
from an upstream `Content-Type` header rather than from the route's contract.

The change is internally structural. The seven served routes, their wire
behavior, and their external surface are unchanged, except that Count Tokens can
no longer be pushed into the SSE path by a mislabeled upstream response.

## Motivation

### The contract is reconstructed, not declared

A "served route" has a small set of facts: which inbound method+path serves it,
which inbound API dialect (Surface) it speaks, which upstream dependency it uses,
and what protocol rules apply (may it stream? what ends a stream?). Those facts
are currently spread across, and partially duplicated between, several packages:

- **`internal/server`** restates the raw upstream path and the Surface at every
  registration (`fwd.Handler("/v1/messages", apierror.Anthropic)`), and repeats
  the Surface again in the per-route `guard(...)` wrapper.
- **`internal/apierror`** owns the `Surface` type, even though Surface is a
  served-route identity, not an error-rendering concept.
- **`internal/shim`** defines its own `Route` string type.
- **`internal/catalog`** defines a *second* `Route` string type plus
  `AnthropicMessagesRoute`/`OpenAIResponsesRoute` constants that duplicate the
  forward path strings.
- **metrics** convert a Surface to a string (`forward.streamSurface`) and then
  re-parse that string back into an index (`server/metrics.go`).

Because nothing ties a Surface to its valid routes, invalid combinations such as
OpenAI + `/v1/messages` compile without complaint. Adding a route means touching
several packages and keeping their restatements in sync by hand.

### Streaming semantics leak from `Content-Type`

`forward.forward` decides whether to enter the SSE pump purely from the upstream
`Content-Type`:

```go
eventStream := isEventStream(resp.Header.Get("Content-Type"))
```

and `streamPolicy` receives only the Surface. There is therefore no way for the
Anthropic Count Tokens route — a plain JSON endpoint that must never stream — to
declare "never SSE." If Copilot ever returned `text/event-stream` for
`count_tokens`, the response would be pumped as a stream. ADR-0003 already states
that the route contract, not `Content-Type` alone, selects SSE semantics; the
code does not yet enforce that for the forward path.

## Goals

- One typed contract per served operation, in one dependency-light package.
- Eliminate the duplicated `Route` types, the duplicated route constants, and the
  Surface→string→index round-trip.
- Make invalid `(Surface, upstream)` combinations unconstructable rather than
  merely discouraged.
- Enforce ADR-0003 for the forward path: the contract, not `Content-Type` alone,
  decides whether a response may stream.
- Keep route registration explicit and greppable — one visible line per served
  operation.

## Non-goals

- No generic handler registry or type-dispatch router. Registration stays a flat,
  explicit list.
- No handlers, HTTP clients, authentication, logging, or rendering logic inside
  the endpoint package. It holds declarative facts only.
- No merging of the OpenAI Responses HTTP and WebSocket transports; they remain
  two separate contracts.
- No new "disable WebSocket" configuration flag. WebSocket forwarding stays
  always-served (see [WebSocket forwarding is not optional](#websocket-forwarding-is-not-optional)).

## The concept

An **Endpoint** is *how copilotd serves one operation* — a typed served contract
that binds inbound pattern(s) to a Surface, an upstream dependency, and
declarative protocol facts. There are four **contract kinds** and seven
**instances**:

| Instance | Kind | Inbound pattern(s) | Upstream / protocol facts |
|---|---|---|---|
| `AnthropicMessages` | HTTP forward | `POST /anthropic/v1/messages` | → `/v1/messages`; JSON or SSE |
| `AnthropicCountTokens` | HTTP forward | `POST /anthropic/v1/messages/count_tokens` | → `/v1/messages/count_tokens`; **never SSE** |
| `OpenAIResponsesHTTP` | HTTP forward | `POST /openai/v1/responses` | → `/responses`; JSON or SSE |
| `OpenAIResponsesWS` | WebSocket forward | `GET /openai/v1/responses` | → `ws:/responses`; opaque |
| `Models` | raw passthrough | `GET /models`, `HEAD /models` | → `/models`; raw, never SSE |
| `AnthropicCatalog` | Catalog | `GET /anthropic/v1/models`, `HEAD /anthropic/v1/models` | source Copilot `/models`, required route `/v1/messages`, Anthropic render |
| `OpenAICatalog` | Catalog | `GET /openai/v1/models`, `HEAD /openai/v1/models` | source Copilot `/models`, required route `/responses`, OpenAI or conditional Codex render |

Health (`/healthz`) and readiness (`/readyz`) are local operational routes. They
have no Endpoint contract and stay registered directly.

## The `internal/endpoint` package

A leaf package that imports only the standard library. It owns the identity types
and the contract data — no handlers, clients, auth, logging, or rendering.

```go
package endpoint

// Surface identifies the inbound API dialect copilotd speaks on a route.
type Surface int

const (
	Anthropic Surface = iota
	OpenAI
	GitHubCopilot
)

// Metric is the canonical low-cardinality name for bounded metric labels.
func (s Surface) Metric() string {
	switch s {
	case Anthropic:
		return "anthropic"
	case OpenAI:
		return "openai"
	case GitHubCopilot:
		return "github-copilot"
	default:
		return "unknown"
	}
}

// Route is one exact upstream path. Route values are never normalized because
// each Surface has an exact forwarding contract.
type Route string

const (
	RouteAnthropicMessages    Route = "/v1/messages"
	RouteAnthropicCountTokens Route = "/v1/messages/count_tokens"
	RouteOpenAIResponses      Route = "/responses"
	RouteModels               Route = "/models"
)

// SSEMode declares whether an HTTP-forward endpoint may serve an SSE response.
type SSEMode int

const (
	NeverSSE  SSEMode = iota // JSON only; a text/event-stream upstream is buffered, not pumped
	JSONorSSE                // may stream when the upstream response is text/event-stream
)

// binding is the shared inbound identity embedded by every contract kind. Fields
// are unexported so the package-level instances are immutable to consumers.
type binding struct {
	surface Surface
	methods []string // e.g. {"POST"} or {"GET", "HEAD"}
	path    string   // inbound path, e.g. "/anthropic/v1/messages"
}

func (b binding) Surface() Surface { return b.surface }

// Patterns returns one merged "METHOD /path" string per served method, ready to
// pass straight to http.ServeMux.Handle.
func (b binding) Patterns() []string {
	patterns := make([]string, len(b.methods))
	for i, method := range b.methods {
		patterns[i] = method + " " + b.path
	}
	return patterns
}

// HTTPForward forwards one inbound request to a single upstream path.
type HTTPForward struct {
	binding
	upstream Route
	sse      SSEMode
}

func (h HTTPForward) Upstream() Route { return h.upstream }
func (h HTTPForward) AllowsSSE() bool { return h.sse == JSONorSSE }

// WSForward forwards a WebSocket transport opaquely to an upstream path.
type WSForward struct {
	binding
	upstream Route
}

func (w WSForward) Upstream() Route { return w.upstream }

// Passthrough streams a raw request/response to an upstream path without shims,
// body caps, request peeking, or SSE classification.
type Passthrough struct {
	binding
	upstream Route
}

func (p Passthrough) Upstream() Route { return p.upstream }

// Catalog serves a provider-shaped model list. Its upstream dependency is fixed
// (Copilot /models); requiredRoute is the supported_endpoints value a model must
// advertise to belong in this catalog — a membership predicate, not a forward
// target.
type Catalog struct {
	binding
	requiredRoute Route
}

func (c Catalog) RequiredRoute() Route { return c.requiredRoute }

// Endpoint is the minimal shape the server's register/guard helpers consume.
type Endpoint interface {
	Surface() Surface
	Patterns() []string
}
```

The seven instances are package-level values, constructed once:

```go
var (
	AnthropicMessages = HTTPForward{
		binding:  binding{surface: Anthropic, methods: []string{"POST"}, path: "/anthropic/v1/messages"},
		upstream: RouteAnthropicMessages,
		sse:      JSONorSSE,
	}
	AnthropicCountTokens = HTTPForward{
		binding:  binding{surface: Anthropic, methods: []string{"POST"}, path: "/anthropic/v1/messages/count_tokens"},
		upstream: RouteAnthropicCountTokens,
		sse:      NeverSSE,
	}
	OpenAIResponsesHTTP = HTTPForward{
		binding:  binding{surface: OpenAI, methods: []string{"POST"}, path: "/openai/v1/responses"},
		upstream: RouteOpenAIResponses,
		sse:      JSONorSSE,
	}
	OpenAIResponsesWS = WSForward{
		binding:  binding{surface: OpenAI, methods: []string{"GET"}, path: "/openai/v1/responses"},
		upstream: RouteOpenAIResponses,
	}
	Models = Passthrough{
		binding:  binding{surface: GitHubCopilot, methods: []string{"GET", "HEAD"}, path: "/models"},
		upstream: RouteModels,
	}
	AnthropicCatalog = Catalog{
		binding:       binding{surface: Anthropic, methods: []string{"GET", "HEAD"}, path: "/anthropic/v1/models"},
		requiredRoute: RouteAnthropicMessages,
	}
	OpenAICatalog = Catalog{
		binding:       binding{surface: OpenAI, methods: []string{"GET", "HEAD"}, path: "/openai/v1/models"},
		requiredRoute: RouteOpenAIResponses,
	}
)
```

Two consequences worth naming:

- **`Route` is defined once.** `RouteAnthropicMessages` is used both as
  `AnthropicMessages`'s upstream path *and* as `AnthropicCatalog`'s required
  route. The forward path and the catalog membership route are the same fact and
  are now the same constant.
- **Invalid pairs are unconstructable.** The instances are the only contracts in
  existence and are built in-package. There is no `HTTPForward{OpenAI, "/v1/messages"}`
  to pass anywhere, because no such value is defined. The "invalid combination
  compiles" hazard is removed by construction, not by convention.

## Consumer changes and dependency direction

`internal/endpoint` is a leaf; every other package depends on it and nothing flows
back. This inverts today's direction, in which the served-route identity type
(`Surface`) lives in the error-rendering package.

| Package | Change | Depends on `endpoint` after |
|---|---|---|
| `apierror` | Delete `apierror.Surface`. `Write`, `WriteStreamError`, and the middleware helpers take `endpoint.Surface`. Keeps `Kind`, `Error`, `Reject`, `StreamReason`, the render table, and the render functions. | yes |
| `shim` | Delete `shim.Route` → `endpoint.Route`. `Registration.New` and `NewChain` take `endpoint.Surface, endpoint.Route`. Drops its `apierror` import. | yes |
| `catalog` | Delete `catalog.Route` and the two route constants → `endpoint.Route`. `Model.SupportedRoutes` becomes `[]endpoint.Route`; `Filter` takes `endpoint.Route`. `Handler` takes the contract plus a rendering bundle (see below). | yes |
| `forward` | `Handler` and `PassthroughHandler` take contracts; read `Surface()`, `Upstream()`, `AllowsSSE()` off them. Delete `streamSurface`; stamp `Surface.Metric()`. | yes |
| `wsforward` | `Handler` takes the contract; replace the hardcoded `apierror.OpenAI` values with `ep.Surface()`. | yes |
| `server` | `handler.go` uses the instances plus `register`/`guard`. `metrics.go` is unchanged; its string labels are now sourced from `Surface.Metric()`. | yes |

The `Surface → string → index` round-trip collapses. `forward` stamps
`StreamResult.Surface = ep.Surface().Metric()`; `server/metrics.go` keeps mapping
that string to a bounded index. One conversion, one source of truth, and the
`streamSurface` panic-switch is deleted.

## Registration

`server/handler.go` replaces inline reconstruction with two small helpers and one
explicit line per endpoint:

```go
guard := func(surface endpoint.Surface, h http.Handler) http.Handler {
	return authMW(apikey, surface, readinessMW(provider, surface, h))
}
register := func(ep endpoint.Endpoint, h http.Handler) {
	for _, pattern := range ep.Patterns() { // "POST /anthropic/v1/messages", ...
		mux.Handle(pattern, guard(ep.Surface(), h))
	}
}

register(endpoint.AnthropicMessages,    fwd.Handler(endpoint.AnthropicMessages))
register(endpoint.AnthropicCountTokens, fwd.Handler(endpoint.AnthropicCountTokens))
register(endpoint.OpenAIResponsesHTTP,  fwd.Handler(endpoint.OpenAIResponsesHTTP))
register(endpoint.OpenAIResponsesWS,    wsProxy.Handler(endpoint.OpenAIResponsesWS))
register(endpoint.Models,               fwd.PassthroughHandler(endpoint.Models))
register(endpoint.AnthropicCatalog, catalog.Handler(endpoint.AnthropicCatalog,
	catalog.Rendering{Render: catalog.RenderAnthropic}, fwd))
register(endpoint.OpenAICatalog, catalog.Handler(endpoint.OpenAICatalog,
	catalog.Rendering{Render: catalog.RenderOpenAI, Codex: codexDesc, Logger: logger}, fwd))
```

Every served operation is still one greppable line naming its handler factory —
not a generic dispatch table. Health and readiness stay plain `mux.HandleFunc`
lines with no contract. The contract instance appears twice per line (once for
`register`, once for the typed factory); that is the price of a type-checked
factory signature, and any mismatch is visible on a single line.

## The SSE gate

`forward.forward` gains the contract as a parameter and changes one condition:

```go
// before: Content-Type alone decides
eventStream := isEventStream(resp.Header.Get("Content-Type"))
// after: the contract gates, Content-Type confirms
eventStream := ep.AllowsSSE() && isEventStream(resp.Header.Get("Content-Type"))
```

`AnthropicCountTokens` (`NeverSSE`) can now never enter the SSE pump, even if
Copilot mislabels a `count_tokens` response `text/event-stream`; it falls through
to the buffered/verbatim path and no terminal is synthesized. `streamPolicy`
continues to select *terminal-event semantics* from `Surface` (Anthropic
`message_stop` vs OpenAI `response.completed`) — a legitimate dialect fact,
distinct from the "may this endpoint stream at all" gate the contract now owns.
This is ADR-0003's stated contract enforced on the forward path.

## Catalog rendering and the Codex shape

The Catalog contract carries only the facts that are identical across every
rendering of that catalog: Surface, inbound patterns, and required route. The
rendering itself stays in the `catalog` package, supplied by the server at
registration through a small bundle:

```go
// package catalog
type Rendering struct {
	Render func([]Model) ([]byte, error)
	Codex  CodexDescriptor
	Logger *slog.Logger
}

func Handler(ep endpoint.Catalog, r Rendering, fetcher Fetcher) http.HandlerFunc
```

The **Codex-shaped catalog is a request-conditional rendering of the single
`OpenAICatalog` endpoint, not a separate endpoint.** It shares the entire served
binding with the provider-shaped OpenAI catalog — same inbound patterns, same
Copilot `/models` source, same `/responses` required route (the filtered model
set is exactly what the Codex renderer intersects with its vendored snapshot).
Only three things differ, and all three are rendering or configuration, which the
endpoint package deliberately excludes:

- which schema to emit (Codex `ModelInfo` vs the OpenAI list),
- whether Codex output is enabled at all (config),
- the reviewer/limit mutations (config).

The request-time selection (`client_version` present, Codex enabled, reviewer or
limit override configured) stays in `catalog.Handler`. The `openai`/`codex`
access-log shape value keeps flowing through the existing per-request holder; it
is a rendering output, not an endpoint identity.

## WebSocket forwarding is not optional

`cmd/copilotd/main.go` constructs the WebSocket proxy unconditionally, and
`wsforward.New` is cheap and side-effect-free (two cancel contexts and a struct;
no goroutine, dial, or bind). There is no configuration flag that omits it. The
current `wsProxy != nil` checks in `server.New`, `newHandler`, and shutdown exist
only so a test can pass `nil`; that test convenience has leaked into the
production signature and made one of the seven endpoints appear optional.

This design makes `wsProxy` a required dependency:

- `server.New`/`newHandler` require a non-nil `*wsforward.Proxy`.
- The WebSocket endpoint registers unconditionally, like the other six.
- `server.New` drops the `var ws websocketDrainer; if wsProxy != nil` wiring, and
  `shutdown` drops its two `if s.ws != nil` guards.
- Tests that passed `nil` pass a real proxy via a one-line helper.

## The passthrough handler collapses to one

`Models` is served today by two handler instances, `PassthroughHandler(GET, ...)`
and `PassthroughHandler(HEAD, ...)`, because the upstream method is baked in per
registration. With the contract carrying both methods, `PassthroughHandler(ep
endpoint.Passthrough)` becomes one handler that reads `r.Method` for both the
outbound method and the HEAD-no-body rule. The `register` loop mounts
`GET /models` and `HEAD /models` to that single handler. Behavior is preserved —
a HEAD request forwards as HEAD upstream — and is guarded by a new test.

## Testing

- **`endpoint`**: a golden table test enumerating every instance and asserting its
  `Patterns()`, `Surface()`, `Upstream()`/`RequiredRoute()`, and SSE mode. This
  is the single readable assertion of the whole served set — previously scattered
  across six packages — and the guard against silent drift.
- **`forward` SSE gate**: `AnthropicCountTokens` with a `text/event-stream`
  upstream response goes buffered/verbatim and synthesizes no terminal;
  `AnthropicMessages` and `OpenAIResponsesHTTP` with `text/event-stream` pump;
  raw `Models` never pumps.
- **`forward` passthrough**: `HEAD /models` forwards as HEAD upstream; `GET /models`
  forwards as GET upstream.
- **`server` integration**: the existing suites exercise all seven endpoints
  end-to-end and should pass unchanged; WebSocket test call-sites receive a real
  proxy instead of `nil`.

## Migration order

Each step compiles on its own. Large mechanical renames are isolated behind
temporary type aliases so review lands in slices rather than one flag-day commit.

1. Create `internal/endpoint`: `Surface` (with `Metric`), `Route` (with
   constants), `SSEMode`, `binding`, the four contract structs, the `Endpoint`
   interface, and the seven instances.
2. Bridge `apierror` with `type Surface = endpoint.Surface` and re-exported
   constants; repoint callers package by package; then delete the alias and the
   original `apierror.Surface`.
3. Repoint `shim.Route` and `catalog.Route` → `endpoint.Route`; delete the
   duplicate catalog route constants; change `Model.SupportedRoutes` to
   `[]endpoint.Route`.
4. Change the handler factories (`forward.Handler`/`PassthroughHandler`,
   `wsforward.Handler`, `catalog.Handler`) to take contracts; make `wsProxy`
   required.
5. Rewrite `server` registration with `register`/`guard`; drop the WebSocket nil
   guards; stamp `Surface.Metric()` in `forward`.
6. Gate SSE on `ep.AllowsSSE()`; add the new tests.
7. Update the `CONTEXT.md` glossary (exact wording below).

## CONTEXT.md changes

Replace the **Endpoint** entry:

> **Endpoint**:
> How copilotd serves one operation — a typed served *contract* (one of: HTTP
> forward, WebSocket forward, raw passthrough, or Catalog) that binds inbound
> pattern(s) to a Surface, an upstream dependency, and declarative protocol facts
> (may it stream; what ends a stream). Lives in `internal/endpoint` as the
> concentrated served set; rendering, handlers, authentication, and clients are
> deliberately kept out. Replaces the earlier `(Surface, Route)`-pair sense.
> _Avoid_: "valid Endpoint identities", "valid (Surface, Route) pair"

Amend the **Surface** entry by appending a sentence noting its home:

> The `Surface` type lives in `internal/endpoint`; error rendering (`apierror`)
> and the other consumers depend on it, not the reverse.

Amend the **Route** entry by appending a sentence noting the unified type:

> Modeled as the single `endpoint.Route` type, shared by HTTP forwarding, catalog
> required-route membership, and shim dispatch; the earlier separate `shim.Route`
> and `catalog.Route` types are removed.

## Alternatives considered

**Contract shape — one struct with a `Kind` enum and optional fields.** Rejected:
`SSE` is meaningless on a catalog and `RequiredRoute` is meaningless on a forward,
so the struct would carry fields that are valid only for some kinds — exactly the
optional-field bag this design avoids. Distinct typed structs keep each kind
carrying only its own facts and let the golden test read as a flat list.

**Contract shape — an interface per kind with concrete types unexported.** More
indirection than the problem needs. The contracts are plain data; structs read
better and make the enumerate-the-whole-set test trivial.

**Binding ownership — the server keeps both inbound and upstream paths as string
literals.** Rejected: it leaves the raw upstream-path duplication that motivated
the work. The chosen split — contract owns Surface, upstream, and protocol facts
and exposes a merged inbound pattern; server keeps an explicit `register` line per
endpoint — removes the duplication while keeping registration greppable.

**Binding ownership — the contract owns everything and the server mounts by
iterating an `All` slice.** Rejected: it pushes toward a generic dispatch registry
and makes registration less greppable, trading the explicit per-endpoint line for
a loop that hides which handler serves what.

## Related decisions

- ADR-0003 — the route contract, not `Content-Type` alone, selects SSE. This
  design enforces that on the forward path.
- ADR-0006 — the OpenAI Responses WebSocket transport is a separate, payload-opaque
  path. `OpenAIResponsesWS` and `OpenAIResponsesHTTP` stay distinct contracts.
- ADR-0007 (companion to this design) — model served endpoints as typed contracts
  with a facts-only boundary.
