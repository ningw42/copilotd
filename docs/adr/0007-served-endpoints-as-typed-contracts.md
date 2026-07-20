# Model served endpoints as typed contracts with a facts-only boundary

**Status:** proposed

copilotd models each served operation as a single typed **Endpoint contract** in
the dependency-light `internal/endpoint` package. A contract is one of four kinds
— HTTP forward, WebSocket forward, raw passthrough, or Catalog — and holds only
declarative served facts: the inbound pattern(s) it answers, the Surface (inbound
API dialect) it speaks, its upstream dependency, and its protocol rules (whether
it may stream, what ends a stream). The `Surface` and `Route` identity types live
here too. Rendering, request handlers, HTTP clients, authentication, and logging
are deliberately kept out: consumers (`server`, `forward`, `wsforward`,
`catalog`, `apierror`, metrics) depend on the contract, combine it with their own
implementation, and never re-derive the facts it already states. The endpoint
package is a leaf — it imports only the standard library, and nothing flows back
into it.

The boundary is the point of this decision. `internal/endpoint` answers *what is
served and with what rules*; it never answers *how the bytes are produced*. A new
served operation adds one contract instance and wires it to an existing handler
factory; it does not scatter Surface, path, and streaming facts across packages,
and it does not add behavior to the endpoint package.

## Considered options

- **Reconstruct the contract at each call site** (status quo): rejected — the
  Surface, upstream path, and streaming semantics were restated across `server`,
  `forward`, `shim`, `catalog`, `apierror`, and metrics, with duplicated `Route`
  types and route constants kept in sync by hand. Invalid `(Surface, upstream)`
  pairs compiled, and streaming semantics leaked from an upstream `Content-Type`
  header rather than from the route's contract.
- **One contract struct with a `Kind` enum and optional fields**: rejected — it
  reintroduces meaningless fields per kind (an SSE mode on a catalog, a required
  route on a forward) and invites the same "set the wrong field" errors the typed
  kinds prevent.
- **A generic handler registry that dispatches by contract**: rejected — it hides
  which handler serves which route behind a table and makes registration
  non-greppable. Registration stays an explicit line per endpoint.
- **Put rendering or handlers inside the endpoint package**: rejected — it would
  make the leaf package depend on the implementation packages it exists to serve,
  and would pull request-time and configuration concerns (e.g. the Codex catalog
  shape) into a package meant to hold only static facts.
- **Distinct typed contracts in a dependency-light package, facts only**
  (chosen): each kind carries only its own facts; consumers supply implementation
  at registration.

## Consequences

- Invalid `(Surface, upstream)` combinations are unconstructable: the contract
  instances are the only ones in existence and are built in-package.
- The duplicated `Route` types collapse to one `endpoint.Route`, and one route
  constant serves both a forward's upstream path and a catalog's required route.
- The `Surface → string → index` metric round-trip collapses to a single
  `Surface.Metric()` conversion.
- Streaming eligibility is a contract fact (`AllowsSSE`), so a JSON-only route
  such as Anthropic Count Tokens can never be pumped as a stream by a mislabeled
  upstream response — the forward-path realization of ADR-0003.
- The boundary binds future work: a parity feature, a new render shape, or a new
  transport adds its behavior in a consumer and, at most, one contract instance —
  never handlers, clients, auth, or rendering inside `internal/endpoint`.
- Request-conditional variants of one served operation (the Codex catalog shape
  on the OpenAI catalog) remain rendering branches of a single contract, not
  separate endpoints.

See `docs/design/2026-07-20-endpoint-contract-concentration-design.md`.
