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
and what protocol rules apply (whether it may stream; and, through its Surface,
what ends a stream). Those facts are currently spread across, and partially
duplicated between, several packages:

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

An **Endpoint** is *how copilotd serves one operation*: an **inbound binding
paired with an upstream (outbound) dependency**, plus the facts that govern how
the two are served. A route with an inbound side but no outbound dependency —
`/healthz`, `/readyz` — is not an Endpoint; it is answered locally.

**Surface** (the inbound API dialect: `/anthropic`, `/openai`, `/models`) is a
facet of the *inbound* half. An Endpoint *owns* its Surface, so governance runs
`Endpoint → Surface → dialect-derived facts`: a fact that is uniform across a
Surface (the event that ends a stream, the error dialect) is reached through the
owned Surface, while a fact that can differ between two endpoints of the *same*
Surface — such as whether the endpoint may stream at all — sits directly on the
contract. That placement rule (*directly on the Endpoint iff it varies within a
Surface, otherwise on the Surface*) decides where every fact belongs.

There are four **contract kinds** and seven **instances**:

| Instance | Kind | Inbound pattern(s) | Upstream / protocol facts |
|---|---|---|---|
| `AnthropicMessages` | HTTP forward | `POST /anthropic/v1/messages` | → `/v1/messages`; JSON or SSE |
| `AnthropicCountTokens` | HTTP forward | `POST /anthropic/v1/messages/count_tokens` | → `/v1/messages/count_tokens`; **never SSE** |
| `OpenAIResponsesHTTP` | HTTP forward | `POST /openai/v1/responses` | → `/responses`; JSON or SSE |
| `OpenAIResponsesWS` | WebSocket forward | `GET /openai/v1/responses` | → `ws:/responses`; opaque |
| `Models` | raw passthrough | `GET /models`, `HEAD /models` | → `/models`; raw, never SSE |
| `AnthropicCatalog` | Catalog | `GET /anthropic/v1/models`, `HEAD /anthropic/v1/models` | outbound `/models`, required route `/v1/messages`, Anthropic render |
| `OpenAICatalog` | Catalog | `GET /openai/v1/models`, `HEAD /openai/v1/models` | outbound `/models`, required route `/responses`, OpenAI or conditional Codex render |

Health (`/healthz`) and readiness (`/readyz`) have no outbound dependency, so they
are not Endpoints; they stay registered directly.

## The `internal/endpoint` package

A leaf package that imports only the standard library. It owns the identity types
and the contract data — no handlers, clients, auth, logging, or rendering.

```go
// Package endpoint holds copilotd's served-endpoint contracts as dependency-light
// typed facts. It imports only the standard library. Patterns() returns strings
// in net/http ServeMux's "METHOD /path" grammar — the one router-serialization
// concession in this otherwise router-agnostic package.
package endpoint

import "net/http"

// Surface identifies the inbound API dialect copilotd speaks on a route.
type Surface int

const (
	Anthropic Surface = iota
	OpenAI
	GitHubCopilot
)

// String is Surface's canonical lowercase name — used for metric labels, log
// fields, and correlation. It is a pure projection of the identity, not rendering.
func (s Surface) String() string {
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

// Endpoint is the immutable inbound projection shared by every contract kind.
// It carries no upstream fact and is not accepted by behavior factories.
type Endpoint struct {
	id endpointID
}

func (e Endpoint) Surface() Surface
func (e Endpoint) Patterns() []string

// The four complete kinds are opaque concrete values. Their private state can
// only select canonical fact sets defined in this package.
type HTTPForward struct{ id endpointID }
type WSForward struct{}
type Passthrough struct{}
type Catalog struct{ id endpointID }

func (h HTTPForward) Endpoint() Endpoint
func (h HTTPForward) Upstream() Route
func (h HTTPForward) AllowsSSE() bool

func (w WSForward) Endpoint() Endpoint
func (w WSForward) Upstream() Route

func (p Passthrough) Endpoint() Endpoint
func (p Passthrough) Upstream() Route

func (c Catalog) Endpoint() Endpoint
func (c Catalog) Upstream() Route
func (c Catalog) RequiredRoute() Route
```

Every externally constructible zero value is canonical: `HTTPForward{}` is
Anthropic Messages, `WSForward{}` is the OpenAI Responses WebSocket contract,
`Passthrough{}` is Models, and `Catalog{}` is the Anthropic catalog. Unexported
discriminants select the remaining canonical fact sets. Parameterless functions,
not writable package variables, expose all seven values:

```go
func AnthropicMessages() HTTPForward
func AnthropicCountTokens() HTTPForward
func OpenAIResponsesHTTP() HTTPForward
func OpenAIResponsesWS() WSForward
func Models() Passthrough
func AnthropicCatalog() Catalog
func OpenAICatalog() Catalog
```

Factories accept the concrete kind they implement, so embedding a kind in a new
external struct cannot produce a value assignable to that parameter. Registration
projects the inbound half explicitly with `ep.Endpoint()` before mounting it.

Three consequences worth naming:

- **`Route` is defined once.** `RouteAnthropicMessages` is used both as
  `AnthropicMessages()`'s upstream path *and* as `AnthropicCatalog()`'s required
  route. The forward path and the catalog membership route are the same fact and
  are now the same constant.
- **`/models` is one path serving three Endpoints.** `Models()` passes it through
  raw; both catalogs fetch it as their outbound source and render the result.
  Every one of the three returns `RouteModels` from `Upstream()` — the same
  upstream dependency, three different served contracts.
- **Invalid pairs are unconstructable.** Complete kinds are opaque concrete
  values with private state, valid canonical zero semantics, and no mutators or
  arbitrary constructors. The seven named values are parameterless accessors,
  not writable variables. An external wrapper — including one that embeds a kind
  and overrides all its fact methods — is a different concrete type and cannot be
  passed to a behavior factory. The "invalid combination compiles" hazard is
  removed by construction, not by convention or runtime validation.

## Consumer changes and dependency direction

`internal/endpoint` is a leaf; every other package depends on it and nothing flows
back. This inverts today's direction, in which the served-route identity type
(`Surface`) lives in the error-rendering package.

| Package | Change | Depends on `endpoint` after |
|---|---|---|
| `apierror` | Delete `apierror.Surface`. `Write`, `WriteStreamError`, and the middleware helpers take `endpoint.Surface`. Keeps `Kind`, `Error`, `Reject`, `StreamReason`, the render table, and the render functions. | yes |
| `shim` | Delete `shim.Route` → `endpoint.Route`. `Registration.New` and `NewChain` take `endpoint.Surface, endpoint.Route`. Drops its `apierror` import. | yes |
| `catalog` | Delete `catalog.Route` and the two route constants → `endpoint.Route`. `Model.SupportedRoutes` becomes `[]endpoint.Route`; `Filter` takes `endpoint.Route`. `Fetcher.FetchModels` gains an upstream-`Route` parameter. `Handler` takes the contract plus a rendering bundle (see below). | yes |
| `forward` | `Handler` and `PassthroughHandler` take contracts; read `Surface()`, `Upstream()`, `AllowsSSE()` off them. `FetchModels` builds its URL from the passed upstream path, not the literal `"/models"`. Delete `streamSurface`; stamp `Surface.String()`. | yes |
| `wsforward` | `Handler` takes the contract; replace the hardcoded `apierror.OpenAI` values with `ep.Surface()`. | yes |
| `server` | `handler.go` uses the canonical accessors plus the `mount`/`register*` helpers. Each per-kind helper passes `ep.Endpoint()` to `mount`; `metrics.go` is unchanged and its string labels are sourced from `Surface.String()`. | yes |

The `Surface → string → index` round-trip collapses. `forward` stamps
`StreamResult.Surface = ep.Surface().String()`; `server/metrics.go` keeps mapping
that string to a bounded index. One conversion, one source of truth, and the
`streamSurface` panic-switch is deleted.

## Registration

`server/handler.go` replaces inline reconstruction with a `mount` helper, one
per-kind register closure, and one explicit line per endpoint:

```go
guard := func(surface endpoint.Surface, h http.Handler) http.Handler {
	return authMW(apikey, surface, readinessMW(provider, surface, h))
}
mount := func(ep endpoint.Endpoint, h http.Handler) {
	guarded := guard(ep.Surface(), h)
	for _, pattern := range ep.Patterns() { // "POST /anthropic/v1/messages", ...
		mux.Handle(pattern, guarded)
	}
}
registerForward     := func(ep endpoint.HTTPForward) { mount(ep.Endpoint(), fwd.Handler(ep)) }
registerWS          := func(ep endpoint.WSForward)   { mount(ep.Endpoint(), wsProxy.Handler(ep)) }
registerPassthrough := func(ep endpoint.Passthrough) { mount(ep.Endpoint(), fwd.PassthroughHandler(ep)) }
registerCatalog     := func(ep endpoint.Catalog, r catalog.Rendering) { mount(ep.Endpoint(), catalog.Handler(ep, r, fwd)) }

registerForward(endpoint.AnthropicMessages())
registerForward(endpoint.AnthropicCountTokens())
registerForward(endpoint.OpenAIResponsesHTTP())
registerWS(endpoint.OpenAIResponsesWS())
registerPassthrough(endpoint.Models())
registerCatalog(endpoint.AnthropicCatalog(), catalog.Rendering{Render: catalog.RenderAnthropic})
registerCatalog(endpoint.OpenAICatalog(),    catalog.Rendering{Render: catalog.RenderOpenAI, Codex: codexDesc, Logger: logger})
```

Every served operation is one greppable line naming its contract exactly once; the
kind is explicit in the helper name, and the factory is one indirection away.
`mount` is the sole place that applies `guard` and expands `Patterns()`. A
per-kind closure can only be handed a contract of its kind, so the earlier
double-reference mismatch — passing one contract to `register` and a different one
to the factory — can no longer be written. Health and readiness stay plain
`mux.HandleFunc` lines with no contract; this is not a generic dispatch table.

## The SSE gate

`forward.forward` gains the contract as a parameter and changes one condition:

```go
// before: Content-Type alone decides
eventStream := isEventStream(resp.Header.Get("Content-Type"))
// after: the contract gates, Content-Type confirms
eventStream := ep.AllowsSSE() && isEventStream(resp.Header.Get("Content-Type"))
```

`AnthropicCountTokens()` (`NeverSSE`) can now never enter the SSE pump, even if
Copilot mislabels a `count_tokens` response `text/event-stream`; it falls through
to the buffered/verbatim path and no terminal is synthesized. `streamPolicy` is
fed `ep.Surface()` and continues to select *terminal-event semantics* from it
(Anthropic `message_stop` vs OpenAI `response.completed`) — a Surface-level fact
the Endpoint governs through its owned Surface, distinct from the "may this
endpoint stream at all" fact the contract carries directly. This is ADR-0003's
stated contract enforced on the forward path.

## Catalog rendering and the Codex shape

The Catalog contract carries the facts identical across every rendering of that
catalog: Surface, inbound patterns, the outbound source (`/models`), and the
required route. The rendering itself stays in the `catalog` package, supplied by
the server at registration through a small bundle:

```go
// package catalog
type Rendering struct {
	Render func([]Model) ([]byte, error)
	Codex  CodexDescriptor
	Logger *slog.Logger
}

func Handler(ep endpoint.Catalog, r Rendering, fetcher Fetcher) http.HandlerFunc
```

The handler fetches the outbound source with `fetcher.FetchModels(ctx, ep.Upstream())`,
so the `/models` path is the contract's, not a literal buried in the fetcher.

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
outbound method and the HEAD-no-body rule. The `mount` loop maps `GET /models` and
`HEAD /models` to that single handler. Behavior is preserved — a HEAD request
forwards as HEAD upstream — and is guarded by a new test.

## Testing

- **`endpoint`**: a golden table test enumerating every instance and asserting its
  `Patterns()`, `Surface()`, `Upstream()`, `RequiredRoute()` (catalogs), and SSE
  mode (HTTP-forward). This is the single readable assertion of the whole served
  set — previously scattered across six packages — and the guard against silent
  drift. External-package API tests also prove the four kinds are opaque concrete
  values, every constructible zero is canonical, accessors are stable and
  parameterless, no fields or mutators are exported, and embedding cannot forge
  a value accepted by a behavior factory.
- **`forward` SSE gate**: `AnthropicCountTokens` with a `text/event-stream`
  upstream response goes buffered/verbatim and synthesizes no terminal;
  `AnthropicMessages` and `OpenAIResponsesHTTP` with `text/event-stream` pump;
  raw `Models` never pumps.
- **`forward` passthrough**: `HEAD /models` forwards as HEAD upstream; `GET /models`
  forwards as GET upstream.
- **`catalog` fetch**: the handler fetches `ep.Upstream()`, so a catalog contract's
  outbound path drives the fetch URL.
- **`server` integration**: the existing suites exercise all seven endpoints
  end-to-end and should pass unchanged; WebSocket test call-sites receive a real
  proxy instead of `nil`.

## Migration order

Each step compiles on its own. Large mechanical renames are isolated behind
temporary type aliases so review lands in slices rather than one flag-day commit.

1. Create `internal/endpoint`: `Surface` (with `String`), `Route` (with
   constants), `SSEMode`, the concrete inbound `Endpoint` projection, four opaque
   concrete kinds with canonical zero values, and seven parameterless accessors.
2. Bridge `apierror` with `type Surface = endpoint.Surface` and re-exported
   constants; repoint callers package by package; then delete the alias and the
   original `apierror.Surface`.
3. Repoint `shim.Route` and `catalog.Route` → `endpoint.Route`; delete the
   duplicate catalog route constants; change `Model.SupportedRoutes` to
   `[]endpoint.Route`.
4. Change the handler factories (`forward.Handler`/`PassthroughHandler`,
   `wsforward.Handler`, `catalog.Handler`) to take contracts; add the upstream-
   `Route` parameter to `Fetcher.FetchModels`; make `wsProxy` required.
5. Rewrite `server` registration with `mount` and the per-kind register closures;
   drop the WebSocket nil guards; stamp `Surface.String()` in `forward` and delete
   `streamSurface`.
6. Gate SSE on `ep.AllowsSSE()`; add the new tests.
7. Update the `CONTEXT.md` glossary (exact wording below).

## CONTEXT.md changes

Replace the **Endpoint** entry:

> **Endpoint**:
> How copilotd serves one operation — an inbound binding paired with an upstream
> (outbound) dependency, modeled as a typed served *contract* (one of: HTTP
> forward, WebSocket forward, raw passthrough, or Catalog). A route with an
> inbound side but no outbound dependency (`/healthz`, `/readyz`) is not an
> Endpoint. An Endpoint owns its Surface, so Surface-level facts (the terminal
> event, the error dialect) are governed through it; a fact sits directly on the
> Endpoint only when it can differ between two endpoints of the same Surface (for
> example, whether it may stream). Lives in `internal/endpoint`; rendering,
> handlers, authentication, clients, and logging are kept out. Replaces the
> earlier `(Surface, Route)`-pair sense.
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
optional-field bag this design avoids. Four concrete kinds keep each kind exposing
only its own facts and let the golden test read as a flat list.

**Contract shape — sealed interfaces with unexported methods.** Rejected: Go
promotes an embedded interface's private methods. An external struct can embed a
kind, override every exported fact projection, and still satisfy the supposedly
sealed interface accepted by a factory.

**Contract shape — opaque concrete kinds with canonical zero values and
parameterless accessors.** Chosen: unexported fields prevent arbitrary facts;
each externally constructible zero resolves to a valid canonical fact set; named
accessors replace mutable package variables; and an embedding wrapper is a
different concrete type that a factory rejects at compile time.

**Catalog outbound — leave the `/models` source implicit in the fetcher.**
Rejected: naming the source only in the fetcher while the contract states
`RequiredRoute()` would split the catalog's outbound fact across two places. The
Catalog contract carries an explicit `Upstream()` fact (`= /models`) and the
fetch consumes `ep.Upstream()`, so the source has one authoritative home.

**Registration — a single `register(ep, factory(ep))` helper.** Rejected: naming
the contract twice per line lets a copy-paste mismatch compile (both arguments are
the same concrete type). Per-kind register closures name each contract once and
bind its factory by kind, so a mismatch cannot be written.

**`Surface.Metric()` — a metric-specific accessor.** Rejected: the value is the
Surface's canonical name, useful in logs and correlation as well as metrics.
`Surface.String()` serves every consumer as one idiomatic projection.

**Binding ownership — the server keeps both inbound and upstream paths as string
literals.** Rejected: it leaves the raw upstream-path duplication that motivated
the work. The chosen split — contract owns Surface, upstream, and protocol facts
and exposes a merged inbound pattern; server keeps an explicit register line per
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
