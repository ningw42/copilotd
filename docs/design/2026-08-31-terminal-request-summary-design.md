# Concentrate the terminal request summary behind one typed contract

**Status:** proposed
**Date:** 2026-08-31
**Issue:** [#161](https://github.com/ningw42/copilotd/issues/161)
**Source review:** `architecture-review-20260831-162223.html`, Candidate 02 (`#terminal-summary`) — rated **Strong**, selected as the **top pick**, and named the top recommendation among four candidates

## Summary

`accessLog` emits copilotd's sole terminal request summary, but today it installs
and understands five independent mutable handoffs owned by four producer
packages and `internal/server` itself:

1. matched binding scope and probe classification;
2. a differing upstream request-id context;
3. SSE completion facts;
4. the successfully rendered Catalog shape; and
5. established WebSocket session completion facts.

This design replaces those handoffs with one `internal/requestsummary` module.
The access middleware creates one `Summary` per request and places only its
handle in the Go context. Registration, upstream, forwarding, Catalog, and
WebSocket code publish typed, bounded completion facts through package
functions. The outer middleware retains the same `Summary`, finalizes it after
the handler returns, and makes the only `logger.LogAttrs(..., "access", ...)`
call.

The module is deliberately not a logger and not an attribute bag. It has no
arbitrary key/value or `slog.Attr` insertion interface. It concentrates
synchronization, handler-return bridging, log-context precedence, level policy,
stream metric observation, and final attribute materialization. Ordinary
`log/slog` emission remains in `internal/server`, preserving ADR-0015's
Component ownership and one-terminal-record rule.

The intended change is architectural, not observable. Existing access keys,
field conditions, levels, correlation, metrics, confidentiality, and
WebSocket-establishment behavior remain unchanged.

## Motivation

### The current seam is five seams

`internal/server/middleware.go` currently constructs the terminal-summary graph
one holder at a time:

```go
ctx := forward.WithStreamResultHolder(r.Context())
ctx = catalog.WithShapeResultHolder(ctx)
ctx = withMatchedScopeHolder(ctx)
ctx = wsforward.WithSessionResultHolder(ctx)
ctx = upstream.WithCorrelationHolder(ctx)
```

After `next.ServeHTTP` returns, the same function knows how to read each holder,
which package type it contains, which fields become access attributes, which
facts affect level, which context takes precedence, and which stream metric to
observe. The resulting imports make the coupling visible:

- `internal/catalog`
- `internal/forward`
- `internal/sse`
- `internal/upstream`
- `internal/wsforward`

Each individual holder is narrow and safe. The locality problem is their
multiplication: every new terminal fact requires another private context key,
mutex, install function, store function, load function, middleware installation,
after-handler branch, and parallel test setup.

### Context propagation does not solve return-path aggregation by itself

A Go context is an immutable tree. Inner code can derive a child context, but
outer middleware does not receive that child when the handler returns. That is
why the current implementation places mutable pointers in the context before
entering the handler.

The new design keeps this valid use of context. It changes what is carried:
one opaque `Summary` handle instead of five holder handles. Completion values
live inside the `Summary`; they are not independent `context.WithValue` entries.

### Request scope and completion facts are different

Request scope is an immutable context describing facts true for a stretch of
work—request id, matched `inbound`, Surface, WebSocket classification, and a
later differing upstream request id. It is available to every context-aware
record emitted beneath that context.

A completion fact is bounded state learned while handling the request and used
only when constructing the terminal access summary—for example, SSE frame count
or WebSocket close code. It must cross the handler-return seam but must not
silently decorate intermediate records. `requestsummary` carries the handle that
bridges those facts back; `internal/logging` continues to carry immutable scope.
The design does not merge those responsibilities.

### The access site knows producer implementation details

The current access site does more than consume completion facts. It knows that
stream completion comes from `forward.StreamResultFromContext`, Catalog shape
from `catalog.ShapeResultFromContext`, and WebSocket completion from
`wsforward.SessionResultFromContext`. Tests reproduce those same holder
interfaces.

The desired seam is one level higher: producers report bounded facts, and the
terminal-summary module decides how those facts contribute to the one access
record. Producer packages no longer expose mutable handoff implementations.

### Why now

The holder pattern was independently reinvented three times. Phase 2 SSE added
the stream-result holder in `a1f6623`; Codex Catalog routing added the shape
holder in `cd1f966`; and the ADR-0015 conversion added matched-scope, upstream-
correlation, and WebSocket-session holders in `88761da`. That conversion adopted
the two existing holders into one access path and standardized five narrow
handoffs; it did not introduce all five. The repeated independent arrival of the
same mechanism is stronger evidence for a shared module than a single planned
introduction would have been.

The deletion test supports a module here. Deleting `requestsummary` would not
remove complexity: synchronization, context-return bridging, precedence, metric
observation, and bounded fact projection would spread back across all five
producers and `accessLog`; distributing terminal records into transports would
violate ADR-0015; or terminal facts would be dropped because a child context
does not flow back to outer middleware.

## Goals

- One per-request module interface for every fact used by the terminal access
  summary.
- One context-carried handle, created and retained by the access middleware.
- Typed fact publication with no arbitrary attribute insertion.
- Producer modules report what happened without owning access formatting,
  level, or stream metric observation.
- The terminal-summary module owns synchronization, publication precedence,
  finalization, and publication-plan materialization.
- `internal/server` remains the owner and emitter of the `access` record.
- Existing access behavior and telemetry remain byte-for-byte equivalent where
  native slog formatting is deterministic.
- Production and tests cross the same summary interface.
- A future transport can join the terminal summary without adding another
  context-holder implementation or another producer import to access
  middleware.

## Non-goals

- No generic request metadata, telemetry, or log-attribute bag.
- No copilotd logger type, logging method, custom handler semantics, or runtime
  key registry beyond ADR-0015.
- No change to access keys, message text, levels, Component, formats, or field
  conditions.
- No change to request-id resolution or upstream correlation semantics.
- No new record, metric, exporter, trace, request watchdog, or timeout signal.
- No change to Endpoint registration, Surface behavior, SSE pumping, Catalog
  rendering, WebSocket forwarding, or wire output.
- No removal or merger of the distinct `websocket established` milestone.
- No generic plugin interface for third-party fact producers.
- No broad relocation of producer domain types merely to avoid an explicit
  projection at this seam.

## Governing constraints

### ADR-0015 remains controlling

[ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md)
requires:

- ordinary `log/slog` emission through an explicit `*slog.Logger`;
- access as the sole terminal request summary;
- `component=internal/server` for access even when facts originate elsewhere;
- request scope carried through immutable logging contexts;
- level selected by operational consequence; and
- top-level keys from the central registry.

`requestsummary` may create the publication plan, but it must not call a logger.
The `LogAttrs` call remains physically in `internal/server`; therefore the
record's Component still names the package that owns its emission.

### This is not the rejected custom logging interface

ADR-0015 rejected a copilotd logger type and typed attribute vocabulary. This
design does not wrap `slog`, replace its methods, or accept application log
records. It models one existing domain event: completion of a returning inbound
request.

The interface speaks in bounded completion facts such as stream outcome and
WebSocket terminal reason. Only finalization projects those facts onto the
already-governed access keys. All other copilotd records continue to use
ordinary `slog` directly.

### Review wording resolved under ADR precedence

The source review's Solution says to concentrate bounded facts “and
publication,” while its After diagram names bounded facts, scope precedence,
outcome policy, and metric observation rather than emission. This design reads
“publication” as preparing the one publication plan, not calling `LogAttrs`.
ADR-0015 is authoritative: the call remains in `internal/server` so Component
ownership stays truthful.

The review also puts outcome policy in the new module while ADR-0015 says level
belongs at the site that knows operational consequence. There is no conflict
after concentration: `requestsummary` becomes the one site holding status,
probe classification, stream outcome, and WebSocket terminal reason, so it is
the site that can apply the accepted level rule. It returns that level to the
server-owned emission site.

### “Terminal” is qualified

The glossary's **Terminal event** is an SSE event that legitimately ends a
stream. A **terminal request summary** is the whole-request `access` record from
ADR-0015. The implementation and comments must use the qualified phrase where
the distinction matters; `requestsummary` avoids naming the package simply
`terminal`.

## Proposed module

### Placement and dependency direction

Add `internal/requestsummary` as the module containing the per-request summary,
its context carrier, bounded projection types, finalization policy, and
publication plan.

The dependency direction is:

```text
server access middleware ─┐
Endpoint scope wrapper ───┤
upstream call ─────────────┤
HTTP forwarder ────────────┼──> requestsummary
Catalog handler ───────────┤
WebSocket forwarder ───────┘
```

ADR-0013 narrowly authorizes the direct
`internal/upstream → internal/requestsummary` edge so `Caller.Correlate` remains
the owner of differing upstream request-id publication. The direction is
one-way: `requestsummary` must not import `internal/server`, `internal/forward`,
`internal/catalog`, `internal/upstream`, or `internal/wsforward`. That rule keeps
producer implementations behind the seam and prevents import cycles.

`requestsummary` may use stable leaf vocabulary such as `sse.Outcome`,
standard-library types, and the central logging key registry. In particular,
`requestsummary.StreamResult.Outcome` remains typed as `sse.Outcome`; therefore
the authorized direct edge gives `internal/upstream` the accepted transitive
`internal/requestsummary → internal/sse` dependency. Layering remains sound and
no cycle results because neither `requestsummary` nor `sse` imports `upstream`.

Each producer explicitly projects its package-owned result into a
summary-owned fact. The projection is intentional: producer result types may
evolve without becoming the terminal-summary interface.

### Interface sketch

The interface shape and names below are fixed by this design:

```go
package requestsummary

type Summary struct { /* private synchronized state */ }

type StreamOutcomeObserver interface {
    ObserveStreamOutcome(surface string, outcome sse.Outcome)
}

// Begin creates one summary, stores its handle in a child context, and returns
// both. The caller retains Summary for after-handler finalization. streams is a
// required dependency; tests pass a spy or explicit no-op implementation.
func Begin(ctx context.Context, streams StreamOutcomeObserver) (context.Context, *Summary)

// Producer publication. Each is a no-op when ctx carries no Summary.
func RecordBinding(ctx context.Context, binding Binding)
func RecordCorrelation(ctx, correlated context.Context)
func RecordStream(ctx context.Context, result StreamResult)
func RecordCatalogShape(ctx context.Context, shape CatalogShape)
func RecordWebSocket(ctx context.Context, result WebSocketResult)

// MatchedContext returns registration-owned scope for panic recovery. It does
// not substitute late upstream correlation for that scope.
func MatchedContext(ctx context.Context) (context.Context, bool)

// Finish closes publication, observes an existing stream result once, and
// returns a stable publication plan. Repeated calls return the first plan and
// produce no second metric effect.
func (s *Summary) Finish(response ResponseResult) Publication
```

The summary-owned fact types are closed structs with no embedded map,
`[]slog.Attr`, `any`, callback, or extension field:

```go
type Binding struct {
    Context context.Context
    Probe   bool
}

type StreamResult struct {
    Surface   string
    Outcome   sse.Outcome
    Frames    int
    Fallbacks int
}

type CatalogShape string

const (
    CatalogShapeOpenAI CatalogShape = "openai"
    CatalogShapeCodex  CatalogShape = "codex"
)

type WebSocketTerminal string

const (
    WebSocketClientClosed   WebSocketTerminal = "client_closed"
    WebSocketUpstreamClosed WebSocketTerminal = "upstream_closed"
    WebSocketError          WebSocketTerminal = "error"
)

type WebSocketResult struct {
    Terminal  WebSocketTerminal
    CloseCode int
    MsgsC2U   int64
    MsgsU2C   int64
    BytesC2U  int64
    BytesU2C  int64
}

type ResponseResult struct {
    Method   string
    Status   int
    Bytes    int64
    Duration time.Duration
}

type Publication struct {
    Context context.Context
    Level   slog.Level
    Attrs   []slog.Attr
}
```

Go string types are not closed enums: callers can explicitly construct unknown
values. The contract therefore prevents arbitrary fields and keys, not every
possible invalid scalar. Existing producer modules remain responsible for
supplying their canonical bounded values, as ADR-0015 already requires. The
summary adds no general runtime vocabulary validator. The existing two-value
Catalog shape check moves unchanged into `RecordCatalogShape`; preserving that
one handoff-specific check does not turn the module into a general validator.

`Publication.Attrs` is a fresh slice owned by the caller. Repeated `Finish`
calls return a fresh copy of the cached attributes so one caller cannot mutate
a later result. The contexts and scalar values inside the summary remain
private.

### Why package functions publish facts

A caller has only a `context.Context`; it should not need to retrieve or type
assert the summary handle. Package functions keep the private context key,
missing-summary no-op behavior, locking, and closed-state checks inside the
module:

```go
requestsummary.RecordStream(r.Context(), requestsummary.StreamResult{
    Surface:   ep.Surface().String(),
    Outcome:   result.Outcome,
    Frames:    result.Frames,
    Fallbacks: result.Fallbacks,
})
```

This also avoids handing every producer the full mutable `Summary`. A producer
can invoke only the explicit operation it imports; it cannot read another
producer's facts or finalize the request.

### No generic setter or getter

The module must not expose any equivalent of:

```go
Set(key string, value any)
SetAttr(slog.Attr)
Values() map[string]any
```

Adding a fact category is an interface change reviewed alongside its access
field, cardinality, confidentiality, level consequence, and tests. That friction
is intentional.

There is likewise no generic value getter. `Finish` is the terminal consumer,
and `MatchedContext` is the one narrow pre-finalization read required by panic
recovery.

## Lifecycle

### Begin

For every request entering `accessLog`:

1. Capture start time and install the existing response writer.
2. Call `requestsummary.Begin` with the request context and the existing
   `StreamOutcomeObserver`.
3. Invoke the wrapped handler with the returned context.
4. After it returns, call `Summary.Finish` with method, status, downstream byte
   count, and elapsed duration.
5. Emit one server-owned `access` record from the returned publication plan.

In outline:

```go
ctx, summary := requestsummary.Begin(r.Context(), streamOutcomes)
next.ServeHTTP(sw, r.WithContext(ctx))

publication := summary.Finish(requestsummary.ResponseResult{
    Method:   r.Method,
    Status:   sw.status,
    Bytes:    sw.bytes,
    Duration: time.Since(start),
})
logger.LogAttrs(
    publication.Context,
    publication.Level,
    "access",
    publication.Attrs...,
)
```

The context transports the handle. The directly retained `summary` reference is
the return-path bridge.

### Matched binding

`scoped` continues deriving an immutable logging context from registration-owned
attributes before auth, readiness, or handler execution. It records one named
`Binding` containing that child context and `probe` classification through
`RecordBinding`, then invokes the next handler with the same child context.

A binding is accepted once. Absence retains its existing meaning: the mux itself
answered, so no registered handler ran. A wrong method, 404, and mux redirect
therefore continue to omit `inbound`, Surface, and WebSocket scope.

`recoverMW` calls `MatchedContext` when handling a panic. It uses registration
scope, not a later upstream correlation context, preserving current panic-log
behavior. The recovered 500 still reaches access finalization and becomes Warn
for a non-probe request; a matched probe remains Debug under level rule 1.

### Differing upstream request-id correlation

`upstream.Caller.Correlate` continues to derive and return a later immutable
logging context only when Copilot's request id exists and differs from
copilotd's resolved request id. Under the narrowly amended ADR-0013 boundary,
`Caller.Correlate` remains the owner of publication and records that context
through `RecordCorrelation` before returning it.

The method's signature, unchanged-context and derived-context returns, all four
existing consumer call sites, and standalone Debug correlation record remain
unchanged.
Response-path records continue using the returned child context directly. Final
access publication prefers the recorded correlated context because it descends
from matched scope and therefore already includes `inbound`, Surface, and
WebSocket attributes.

Correlation is accepted once. Later attempts do not replace the first differing
upstream id, preserving the existing holder's first-publication behavior.

### SSE completion

After `sse.Pump` returns, the HTTP forwarder projects the canonical Surface,
outcome, frame count, and fallback count into `requestsummary.StreamResult`.
No body, frame contents, error, request header, or response header crosses the
seam.

A completed stream contributes:

- `outcome`, `frames`, and `fallbacks` to access;
- one observation to the existing per-Surface/outcome counter; and
- Warn when the outcome is synthesized, stall, upstream error, or shim error,
  unless the request is a probe.

Actual production executes one pump per request. To preserve the current
handoff exactly, a later pre-finalization stream publication replaces an earlier
one. Publication after finalization is ignored. Duplicate publication is not a
supported producer behavior; the deterministic overwrite rule exists to keep
this refactor behavior-neutral.

### Catalog shape

After an OpenAI Catalog representation is successfully rendered, the Catalog
handler records `openai` or `codex`. It records nothing on rendering failure,
for the Anthropic Catalog, or for a request that never reaches successful
representation materialization.

Only the bounded shape crosses the seam. Query values, `client_version`, model
ids, reviewer configuration, representations, and response bodies do not.
Unknown shapes continue to be ignored. As with the current holder, a later valid
pre-finalization shape replaces an earlier one; production publishes once.

The access record adds `catalog_shape` only when a shape was recorded. Catalog
shape does not alter level.

### Established WebSocket completion

After an established WebSocket session ends, the WebSocket forwarder projects
its canonical terminal reason, close code, directional message counts, and
directional byte counts into `requestsummary.WebSocketResult`.

The summary adds the same six attributes to access. A terminal reason of error
makes a non-probe access record Warn. The existing WebSocket terminal metric
remains observed by the WebSocket forwarder; this design does not move or
duplicate it. Only the SSE outcome metric moves behind summary finalization
because it is currently observed by `accessLog` from the SSE result handoff.

WebSocket completion is accepted once, preserving the existing holder's
first-publication behavior. Pre-upgrade failures publish no session result and
continue to emit only access, with no terminal session fields.

The distinct `websocket established` Info milestone remains where it is and is
not part of this summary.

### Finish

`Finish` is idempotent and closes the summary. The first call:

1. prevents later fact publication from affecting the request;
2. snapshots the selected log context and all fact slots;
3. computes level;
4. creates record-local attributes in the existing order;
5. marks the stream metric, when present, for exactly one observation; and
6. caches the publication plan.

Later calls discard their `ResponseResult` argument and return the first call's
context, level, and attributes, with a fresh copy of the cached attribute slice.
They do not repeat metric observation.

Production calls `Finish` once. Idempotence is a deliberate, narrow safety
property—not an invitation to multiple finalizers—because stream observation is
an external effect and must remain exactly once if finalization is accidentally
retried. It also makes the closed lifecycle directly testable without
authorizing multiple access emissions; `internal/server` still owns the one
`LogAttrs` call.

A fact racing with finalization is included only if its record operation acquires
the summary lock before finalization closes the summary. The handler-return
contract requires all legitimate producer publication before return, so a
post-return loser indicates escaped work and is correctly absent from the
terminal summary.

## State and concurrency

`Summary` contains one mutex, a closed flag, a cached publication, and optional
slots for binding, correlation, stream, Catalog, and WebSocket facts. No slot or
mutex is exported.

All record operations follow the same shape:

1. find the `Summary` under the private context key;
2. return if absent;
3. lock;
4. return if closed;
5. apply that fact's documented first-write or replacement rule; and
6. unlock.

Finalization snapshots under the same lock and performs external metric
observation after unlocking. The observer must never run while the summary
mutex is held: an observer is an injected dependency and may itself synchronize.

The publication plan is constructed from copied values. No caller can mutate
stored summary state after finalization.

The per-request allocation changes from five holder objects plus their context
nodes to one summary object plus one context node. Registration and upstream
logging still derive immutable child contexts as today. This is a locality win;
allocation reduction is incidental and not a performance claim.

## Publication policy

### Log-context precedence

Final access uses, in order:

1. the differing upstream request-id context, if recorded;
2. the matched binding context, if recorded; or
3. the request context supplied to `Begin`.

A correlated context is derived on the response path from a descendant of the
matched context, so preferring it retains request id, `inbound`, Surface, and
WebSocket scope. A cancelled correlated context remains selected, exactly as
under the current implementation.

### Level precedence

The summary starts at Info and applies these existing rules:

1. A matched probe is Debug, including a not-ready 503.
2. Otherwise, an SSE outcome of synthesized, stall, upstream error, or shim
   error is Warn.
3. Otherwise, a WebSocket terminal reason of error is Warn.
4. Otherwise, an HTTP status of 500 or greater is Warn.
5. Otherwise, the record is Info.

The implementation may set Warn for every matching abnormal condition rather
than use mutually exclusive branches; the result is the same. Access never
emits Error. A recovered panic has its separate Error record and, for a non-
probe request, a terminal Warn access record for the resulting 500; a matched
probe remains Debug under rule 1.

### Record-local attributes

The publication contains the existing record-local fields in this order:

1. `method`, `status`, `bytes`, `duration` — always;
2. `outcome`, `frames`, `fallbacks` — when an SSE pump completed;
3. `catalog_shape` — when the OpenAI Catalog rendered successfully; and
4. `terminal_reason`, `close_code`, `msgs_c2u`, `msgs_u2c`, `bytes_c2u`,
   `bytes_u2c` — when a WebSocket session was established and ended.

`request_id`, `inbound`, `surface`, `ws`, and `upstream_request_id` remain
context-carried scope injected by `internal/logging`; they are not duplicated as
record-local summary fields.

All top-level attribute constructors continue using the key registry from
`internal/logging`, so the existing AST gate covers the new module.

### Metric effects

Only a present SSE result causes summary finalization to call
`ObserveStreamOutcome`. The exact Surface string and `sse.Outcome` reach the
existing bounded counter. Unknown labels remain ignored by that counter.

The call occurs once even if `Finish` is called repeatedly. It occurs for probe
scope if a stream result somehow exists, matching the current ordering where
metric observation is independent of access level.

Observation of WebSocket accept and terminal metrics stays in
`internal/wsforward`, which owns the observer interfaces and call sites. The
concrete `WsAcceptCounter` and `WsSessionTerminalCounter` remain server-owned in
`internal/server/metrics.go`, exactly as the stream counter does. Neither
WebSocket metric is an access-owned effect today, so both are outside this
concentration.

## Error and absence behavior

- Calling any `Record…` function without an installed summary is a no-op.
- Passing a nil context is unsupported under ordinary Go context conventions;
  callers must supply a non-nil context.
- A stream observer is a required `Begin` dependency, preserving the current
  access path's unconditional observer call when an SSE result exists. Focused
  tests pass a spy or explicit no-op observer rather than nil.
- Missing binding, correlation, stream, Catalog, or WebSocket facts are ordinary
  optional states and omit only their corresponding scope or attributes.
- Negative counts and unknown producer values are not newly normalized or
  validated by this refactor. Producers remain responsible for canonical
  values, and existing behavior tests guard production paths.
- Recording after finalization is a no-op. It never panics or mutates a published
  record.
- Repeated finalization does not emit. It only returns the cached plan; the
  caller owns emission.

## Changes by module

### `internal/requestsummary`

Add the new deep module: private context key, synchronized `Summary`, fact
projections, producer publication functions, matched-context lookup,
finalization policy, publication plan, and focused tests.

### `internal/server`

`accessLog` replaces five holder installations and reads with `Begin`, one
`Finish`, and one unchanged server-owned `LogAttrs` emission. The Catalog,
forward, SSE, and WebSocket result/finalization imports leave middleware.
`internal/upstream` remains imported there because request-ID handling continues
to use `upstream.RequestIDHeader`.

`scoped` records registration-owned scope through `RecordBinding`.
`recoverMW` resolves matched scope through `MatchedContext`. The server-owned
matched-scope holder is deleted.

`requestsummary` becomes the canonical home of `StreamOutcomeObserver` because
it owns the observation call. `internal/server` retains
`type StreamOutcomeObserver = requestsummary.StreamOutcomeObserver` as a source-
compatible exported alias, so `server.New`, `newHandler`, test helpers, and the
composition root keep their current parameter spelling while sharing one type.
The concrete counter remains server-owned until a process-wide metrics decision
says otherwise.

### `internal/upstream`

Under ADR-0013's narrow direct-import amendment, `Caller.Correlate` remains the
owner of `RecordCorrelation` and records its derived correlated context through
the shared contract. Its signature, unchanged- and derived-context return
behavior, Debug record, and callers remain unchanged. The correlation holder is
deleted.

### `internal/forward`

The SSE path records `requestsummary.StreamResult` after `sse.Pump` returns. The
package-owned `StreamResult` holder and its exported install/store/load
functions are deleted. Focused forwarding tests use the production summary
interface when they need to assert pump completion projection.

### `internal/catalog`

The successful OpenAI Catalog path records the summary-owned bounded
`CatalogShape`. The package-owned shape holder is deleted. Its existing two-
value validation moves unchanged into `RecordCatalogShape`. Current server tests
provide positive-case prior art for a recorded Codex shape, but no current
Catalog test proves omission on failed rendering, the Anthropic Catalog, or a
pre-render failure; this change must add those negative integration cases.

### `internal/wsforward`

The established-session path converts its `SessionTerminal` and counters into
`requestsummary.WebSocketResult`. The package-owned session-result holder is
deleted. `SessionTerminal`, session execution, establishment logging, and the
WebSocket observer interfaces and observation call sites remain owned by
`wsforward`; the concrete accept and terminal counters remain server-owned.

## Testing strategy

### Primary seam: complete server behavior

The highest test seam is the complete server handler through HTTP, plus the real
WebSocket integration path. These tests are authoritative because they exercise
production Endpoint registration, middleware order, producer publication,
summary finalization, ordinary slog handling, and metrics together.

Existing tests provide prior art for most required behavior:

- one Debug access record for each probe, including a not-ready readiness probe;
- no binding scope for 404, 405, or a mux redirect;
- registration-owned scope for auth-rejected HTTP and WebSocket Endpoints;
- zero downstream bytes for HEAD;
- SSE terminal fields, every canonical outcome metric, and consequence-based
  level;
- the bounded stream counter ignoring unknown Surface and outcome labels;
- Warn for non-probe server failures while matched probes remain Debug;
- partial non-stream omission coverage for `outcome` and `frames`;
- absence of stream bodies, request bodies, API keys, Catalog bodies, and query
  values;
- the positive bounded Catalog-shape case;
- matched panic scope and the recovered non-probe 500 access result;
- one WebSocket establishment milestone and one delayed terminal access record;
- WebSocket error severity and pre-upgrade omission of session-only facts; and
- differing upstream request-id correlation on access.

These tests should retain assertions on emitted behavior rather than mention the
new module. The refactor must add or strengthen whole-server cases for:

- every non-stream request omitting all stream-only fields, including
  `fallbacks`; and
- Catalog shape omission on failed rendering, the Anthropic Catalog, and a
  pre-render failure.

### Summary interface tests

Focused `requestsummary` tests cover behavior that is difficult to isolate at
the whole-server seam:

- `Begin` returns a child context and retains one shared summary;
- every publication function is a no-op without a summary;
- producers cannot retrieve mutable summary state;
- binding, correlation, and WebSocket first-publication behavior;
- stream and valid Catalog replacement behavior before finalization;
- invalid Catalog shape omission;
- correlation → binding → base context precedence;
- probe and abnormal-outcome level precedence;
- exact optional attribute groups and stable order;
- stream observation exactly once;
- idempotent `Finish` with copied attribute slices;
- publication after `Finish` ignored; and
- concurrent record/finalize operations pass under the race detector.

These tests cross only the public module interface. They do not assert mutexes,
private key identity, field layout, or helper function decomposition.

### Producer tests

Tests that currently construct package-owned holders should install the
production summary and inspect `Finish` output. If the same behavior is already
proven by a server integration test, remove the lower duplicate rather than
creating a test-only getter.

Important producer-local assertions remain:

- the HTTP forwarder projects the actual `sse.Pump` result;
- new Catalog integration coverage proves shape publication after successful
  OpenAI rendering and omission on failed rendering, the Anthropic Catalog, and
  pre-render failure;
- the WebSocket forwarder publishes one result after an established session;
- upstream correlation both returns and publishes the same derived logging
  context.

### Structural logging gate

`internal/logging`'s existing AST test continues to ban package-level/default
logger emission and unregistered top-level keys. `requestsummary` emits no
record and needs no Component logger. Any `slog.Attr` constructors it adds must
use registry constants.

### Verification command

The implementation completes only when the repository's race-enabled full Go
test command passes. Documentation-only drafting cannot run it in environments
without the repository's Go toolchain; implementation must use the project's
normal development environment.

## Migration plan

Land the refactor atomically. The implementation order inside the one change is:

1. Add `requestsummary` and focused interface tests without connecting it to
   production.
2. Add or strengthen whole-server regression tests for the complete existing
   record matrix, especially the currently missing Catalog omission cases.
3. Perform one coordinated production cutover: change access middleware to
   `Begin` and `Finish`; move all five publication sites and panic scope lookup;
   and delete all five old holders in the same commit state.
4. Replace holder-oriented producer tests with the production summary seam,
   deleting lower duplicates when whole-server coverage is authoritative.
5. Run formatting, the race-enabled full suite, and the logging structure gate.
6. Verify the final access record matrix against ADR-0015 before merge.

The production cutover has no adapter phase and no dual source. No commit
intended for merge may install both an old holder and the new summary for the
same fact, or route one fact through an old holder into the new summary.

## Alternatives considered

### Keep the five narrow holders

This is safe and already works, but every future fact repeats lifecycle and test
plumbing. It leaves `accessLog` coupled to every producer implementation and
fails the locality goal that motivated the review.

### Put arbitrary values directly in `context.Context`

Rejected. Child context values do not flow back to outer middleware, so this
does not solve after-handler aggregation without a mutable object. Exported
string keys and `any` values also lose discovery, type checking, confidentiality
review, and bounded vocabulary.

### Put one `map[string]any` or `[]slog.Attr` in context

Rejected. This solves return-path visibility but creates an unrestricted
logging side channel. Any package could invent keys, leak secrets, add
high-cardinality values, or pre-decide formatting. It recreates the custom
logging vocabulary ADR-0015 rejected.

### Pass `*Summary` explicitly through every handler and call

Rejected for this cross-cutting request-scoped concern. `net/http` fixes the
handler signature, and explicit propagation would force unrelated auth,
readiness, forwarding, shim, and upstream interfaces to carry a value they do
not use. Context is the conventional Go carrier here; the typed summary remains
the contract.

### Expose the whole mutable `Summary` to producers

Rejected. It would let one producer inspect or alter another producer's facts,
finalize early, or depend on storage layout. Package publication functions give
each caller the narrow operation it needs.

### Inject producer-specific observers through constructors

Rejected. Each producer would gain another process-lifetime dependency whose
only job is to rediscover a request-lifetime summary in context. Runtime wiring
would grow while the after-handler lifecycle remained distributed.

### Move the `access` emission into `requestsummary`

Rejected by ADR-0015 ownership. A `LogAttrs` call in `requestsummary` labelled
`component=internal/server` would falsely name the emitter; relabelling it would
change the accepted record contract. Returning a publication plan keeps policy
local while emission remains honestly server-owned.

### Return a domain snapshot and let server rebuild attributes and level

Rejected because it leaves the hard policy in `accessLog`: producer precedence,
severity, metric observation, and attribute projection would remain spread
across the caller. A publication plan is the smaller, deeper interface.

### Introduce a generic typed-slot registry

Rejected. Generic slots appear flexible but still require dynamic registration,
slot identity, and a final consumer that understands every slot. The repository
has a closed set of first-party transports; explicit fact operations are easier
to discover, review, and test.

## Consequences

### Positive

- Access middleware learns one module interface instead of five holder
  implementations.
- Context allocation and synchronization concentrate in one per-request object.
- Producers publish facts but do not own access policy.
- Future terminal facts have an explicit review point for cardinality,
  confidentiality, severity, and tests.
- Tests cross the production seam rather than reconstructing private holders.
- Exactly-once stream metric observation becomes part of finalization.
- Ordinary slog ownership and all accepted external behavior remain intact.

### Costs

- `requestsummary` becomes a deliberate cross-cutting module imported by every
  terminal-fact producer.
- Producer results require small explicit projections into summary-owned facts.
- The module's interface changes whenever a genuinely new access fact category
  is added; this is intentional but not free.
- Finalization returns `slog` concepts (`Level` and `Attr`) while not emitting;
  the module is therefore specific to the access record, not a reusable generic
  request-state facility.
- A single mutex serializes the handful of per-request publications. Their
  frequency is bounded and terminal, so simplicity wins over independent locks.

## Naming decision

The non-emitting final value is named `Publication`. The name describes the
complete plan prepared for the one access publication without implying that the
module emits it. `requestsummary` prepares the `Publication`; `internal/server`
emits it.

SC1 is resolved as Option 1: `Caller.Correlate` remains the owner of
`RecordCorrelation` under the narrow ADR-0013 direct-import amendment;
`requestsummary.StreamResult.Outcome` remains typed as `sse.Outcome`, and the
resulting transitive SSE dependency is accepted.

## CONTEXT.md changes

None. **Terminal request summary** is existing ADR-0015 logging language for the
`access` record, not a new project domain entity. **Completion fact** and
**publication plan** are design-local interface descriptions; they do not need a
shared ubiquitous meaning outside this module. The glossary's existing
**Terminal event** entry remains authoritative for the SSE concept, and this
design qualifies “terminal request summary” wherever confusion is possible.

If implementation causes either design-local phrase to appear across unrelated
modules or operator documentation, that would be new evidence for a glossary
change; this design does not require one.

## Related decisions

- Architecture review `architecture-review-20260831-162223.html`, Candidate 02
  (`#terminal-summary`) — the **Strong**, top-ranked recommendation this design
  realizes. Its phrase
  “completion facts and publication” is reconciled under ADR-0015 in
  [Review wording resolved under ADR precedence](#review-wording-resolved-under-adr-precedence).
- [ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md)
  — access remains the sole server-owned terminal request summary emitted with
  ordinary slog; its scope, level, key, and Component rules are unchanged.
- [ADR-0013](../adr/0013-govern-authenticated-upstream-calls-in-internal-upstream.md)
  — narrowly amended to authorize the direct
  `internal/upstream → internal/requestsummary` edge while
  `Caller.Correlate` retains publication ownership. The amendment also records
  the accepted transitive `internal/requestsummary → internal/sse` consequence;
  the rest of the upstream-call boundary is unchanged.
- [ADR-0007](../adr/0007-served-endpoints-as-typed-contracts.md) — the typed
  Endpoint owns Surface, and its approved `Surface.String()` projection supplies
  the stream metric label without re-derivation.
- [Log-record structure conversion](2026-08-30-log-record-structure-conversion-design.md)
  — standardized the five handoffs in one access path, explicitly left the
  pre-existing stream and Catalog holders outside that atomic sweep, and named
  neutral concentration as the clean follow-up when repetition justified it.
