# Model served endpoints as typed contracts with a facts-only boundary

**Status:** proposed

copilotd models each served operation as a single typed **Endpoint contract** in
the dependency-light `internal/endpoint` package. An Endpoint is an **inbound
binding paired with an upstream (outbound) dependency**: a route with an inbound
side but no outbound dependency (`/healthz`, `/readyz`) is answered locally and is
not an Endpoint. Each contract is one of four kinds — HTTP forward, WebSocket
forward, raw passthrough, or Catalog — and holds only declarative served facts:
the inbound pattern(s) it answers, the **Surface** (inbound API dialect) it
speaks, its upstream dependency, and its protocol rules. The `Surface` and `Route`
identity types live here too.

An Endpoint **owns its Surface**, so governance runs `Endpoint → Surface →
dialect-derived facts`. This gives a single rule for where a fact belongs: a fact
that can differ between two endpoints of the *same* Surface sits **directly on the
Endpoint** (whether an HTTP-forward endpoint may stream at all — `AllowsSSE`; the
upstream path; the inbound patterns); a fact uniform across a Surface is reached
**through the owned Surface** (the event that ends a stream, the error dialect).
*Directly on the Endpoint iff it varies within a Surface, otherwise on the
Surface* — that is how a future contributor decides where a new fact goes.

The boundary is the point of this decision. `internal/endpoint` answers *what is
served and with what facts*; it never answers *how the bytes are produced*. Pure
projections of those facts belong here (`Surface.String()`, `binding.Patterns()`,
`HTTPForward.AllowsSSE()`); request handlers, HTTP clients, authentication,
rendering, and logging do not. Consumers (`server`, `forward`, `wsforward`,
`catalog`, `apierror`, metrics) depend on the contract, combine it with their own
implementation, and never re-derive the facts it already states. The package is a
leaf — standard library only, and nothing flows back into it.

The four kinds are an **open set**. Adding a served operation that fits an existing
kind is a new package-level instance plus a registration line — no decision record
needed. Adding a *new kind* — a genuinely different outbound or protocol
fact-shape — must amend this ADR (or a successor) to record the kind, its distinct
fact-shape, and why no existing kind fits, so the taxonomy's growth stays
auditable in one place.

## Considered options

- **Reconstruct the contract at each call site** (status quo): rejected — the
  Surface, upstream path, and streaming semantics were restated across `server`,
  `forward`, `shim`, `catalog`, `apierror`, and metrics, with two duplicated
  `Route` types and route constants kept in sync by hand. Invalid `(Surface,
  upstream)` pairs compiled, and streaming eligibility leaked from an upstream
  `Content-Type` header rather than from the route's contract.
- **One contract struct with a `Kind` enum and optional fields**: rejected — it
  reintroduces meaningless fields per kind (an SSE mode on a catalog, a required
  route on a forward) and invites the same "set the wrong field" errors the typed
  kinds prevent.
- **A generic handler registry that dispatches by contract**: rejected — it hides
  which handler serves which route behind a table and makes registration
  non-greppable. Registration stays an explicit line per endpoint.
- **Put rendering or handlers inside the endpoint package**: rejected — it would
  make the leaf package depend on the implementation packages it exists to serve,
  and would pull request-time and configuration concerns (for example, the Codex
  catalog shape) into a package meant to hold only static facts.
- **Distinct typed contracts in a dependency-light package, facts only**
  (chosen): each kind carries only its own facts; consumers supply implementation
  at registration.

## Consequences

- Invalid `(Surface, upstream)` combinations are unconstructable: the contract
  instances are the only ones in existence and are built in-package.
- The duplicated `Route` types collapse to one `endpoint.Route`, and one route
  constant serves both a forward's upstream path and a catalog's required route.
  `/models` is one upstream path serving three Endpoints — the raw passthrough and
  the two catalogs — which makes the "same upstream, different served contracts"
  point concrete.
- The `Surface → string → index` metric round-trip collapses to a single
  `Surface.String()` projection, consumed by metrics, logs, and correlation alike.
- Streaming eligibility is a fact on the `HTTPForward` kind (`AllowsSSE`) — the one
  kind where it varies within a Surface — so a JSON-only route such as Anthropic
  Count Tokens can never be pumped as a stream by a mislabeled upstream response.
  This is the forward-path realization of ADR-0003.
- The placement rule binds future work: a new fact goes directly on the Endpoint
  only if it varies between endpoints of the same Surface; otherwise it belongs on
  the Surface (or its consumer).
- The boundary binds future work: a parity feature, a new render shape, or a new
  transport adds its behavior in a consumer and, at most, one contract instance —
  never handlers, clients, auth, or rendering inside `internal/endpoint`.
- The kind taxonomy is open but auditable: a new kind is allowed and must be
  recorded here, while a new instance of an existing kind needs no record.
- Request-conditional variants of one served operation (the Codex catalog shape on
  the OpenAI catalog) remain rendering branches of a single contract, not separate
  endpoints.

See `docs/design/2026-07-20-endpoint-contract-concentration-design.md`.
