# Govern authenticated GitHub Copilot calls in `internal/upstream`

**Status:** accepted

Every authenticated request copilotd makes to GitHub Copilot is an **upstream
call** whose shared policy is governed by the dependency-light
`internal/upstream` package. That policy covers credential acquisition, base URL
joining, outbound and inbound header rules, request-id correlation, bounded
response reading, and pre-commit failure classification and rendering. Consumers
provide an `endpoint.Route` from their typed Endpoint contract and retain the
transport- or response-specific tail: pumping an SSE stream, dialing or upgrading
a WebSocket, decoding a Catalog, or copying bytes verbatim.

## Why

The same authenticated-call sequence was implemented separately by the two HTTP
forwarding paths, the Catalog upstream call, and the WebSocket forwarder. Those copies
had drifted in observable ways: the WebSocket path dropped client headers and
missed client cancellation, buffered reads disagreed on cancellation and over-cap
classification, URL joining differed, and request-id correlation and failure
messages were duplicated. The Catalog also introduced a second error vocabulary
that round-tripped back to `internal/apierror` while losing detail.

The shared policy needs one owner below every consumer. `internal/upstream` is a
dependency-light package whose direct imports are limited to
`internal/apierror`, `internal/endpoint`, `internal/identity`,
`internal/logging`, `internal/requestsummary`, and the standard library. The
structural allowlist governs direct imports only. The `internal/requestsummary`
edge exists so `Caller.Correlate` remains the owner of differing upstream
request-id publication. Because `requestsummary.StreamResult.Outcome` retains
its `sse.Outcome` type, that edge has the accepted transitive consequence
`internal/requestsummary → internal/sse`.

The direction remains one-way: `internal/requestsummary` is not a consumer of
`internal/upstream` and does not import it, and neither it nor `internal/sse`
points back to `internal/upstream`. The shared policy therefore remains below
its consumers and the accepted transitive dependency introduces no import
cycle. `internal/upstream` still does not own WebSocket dialing or post-commit
response interpretation, so the package concentrates the common trunk without
imposing one transport abstraction on different response tails.

## Considered options

- **Keep the policy at each call site:** rejected — independent credential,
  header, correlation, and classification branches had already drifted into live
  defects and duplicate declarations.
- **Extract helpers but leave classification and response separate:** rejected —
  callers would still maintain branch sets, preserving the mechanism that lost
  the WebSocket cancellation case.
- **Own HTTP execution and WebSocket dialing behind one transport abstraction:**
  rejected — WebSocket forwarding must retain its raw network connection for
  force-close authority and would pull `coder/websocket` into consumers that do
  not otherwise use it.
- **Govern the common trunk in a dependency-light `internal/upstream` package**
  (chosen): HTTP execution composes there, while named preparation,
  classification, and correlation seams let the WebSocket forwarder keep its
  transport ownership.

## Consequences

- Credential use, URL joining, header policy, request-id correlation, bounded
  reading, and failure classification have one implementation and one test
  surface across the Forwarder, WebSocket forwarder, and Catalog.
- `X-Request-Id` has one declaration, `upstream.RequestIDHeader`. Server
  middleware reads it directly; upstream transports consume it transitively
  through `Caller.Prepare` and `Caller.Correlate`. The duplicated correlation
  helpers and Catalog failure vocabulary are removed.
- Failure classification and response remain inseparable through
  `Failure.RespondTo`, including silent client cancellation. `Caller` logs every
  classified cause exactly once and never renders it to clients. For credential
  acquisition it logs the cause exposed by the provider; `identity.Manager`
  remains governed by ADR-0001, so tokens, raw exchange bodies, underlying
  exchange errors, and raw mint internals are omitted while `Caller` records the
  sanitized cause the Manager returns.
- Consumers keep interpreting successful responses. In particular,
  `internal/upstream` does not pump streams, upgrade WebSockets, decode catalogs,
  or own the raw WebSocket connection; ADR-0003's post-commit stream policy is
  unchanged.
- The direct-import allowlist and the prohibition on `internal/forward`
  importing `internal/catalog` are structural test invariants. The allowlist
  admits only the named direct dependencies above; it does not prohibit the
  accepted transitive `internal/requestsummary → internal/sse` dependency. This
  keeps the shared module below its consumers and preserves ADR-0007's
  facts-only Endpoint boundary.
- The consolidation introduces no new wire-output divergence. It continues to
  render the existing `internal/apierror.Kind` Fabrications, while request-side
  hop-by-hop stripping is ordinary proxy behavior, so the divergence ledger is
  unchanged.

See `docs/design/2026-07-26-upstream-call-concentration-design.md`.
