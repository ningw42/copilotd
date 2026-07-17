# Phase 3 — Middleware framework (the shim onion) — Design

Status: proposed design (refined via brainstorming + adversarial critique), pending implementation plan
Date: 2026-07-16
Roadmap reference: `ROADMAP.md` §7 "Phase 3 — Middleware framework"
Builds on: `docs/design/2026-07-15-phase-2-sse-streaming-engine-design.md`,
`docs/design/2026-07-16-forwarding-fidelity-and-sse-identity-design.md`

## 1. Goal & outcome

Phase 2 made streaming genuinely usable and deliberately left a seam: the SSE
engine is *frame-aware, payload-opaque* precisely so a per-event transform can
nest inside the pump later. Phase 3 builds that extension mechanism — the onion
contract — and nothing else. It ships **the framework plus a no-op passthrough**;
no parity shim is implemented here (those are Phase 4).

**Outcome:** a place to hang parity features without touching the dumb core. A
request can be transformed on the way in; a response can be transformed on the way
out — as a whole buffered body, or event-by-event on a stream — through a
composable, individually-toggleable stack, proven end to end by a passthrough that
changes nothing.

### 1.1 Terminology decision — `shim`, not "middleware"

`ROADMAP.md` §7 labels this phase "Middleware framework." We retire "middleware"
as the *name* of the mechanism: it is a ratified HTTP term for the **request**
pipeline (`func(http.Handler) http.Handler`), and this mechanism reaches into the
response envelope **and** individual stream events — it is broader than middleware
in the received sense. `CONTEXT.md` already ratifies the right word: a **shim** is
"a composable middleware layer that closes one specific parity gap." So:

- The package is **`internal/shim`**. It defines what a shim *is* (the hook
  interfaces), how shims *compose* (the `Chain`), and the canonical no-op.
- An individual parity layer is a **shim** (`CONTEXT.md`). The mechanism that
  nests them is **the onion** (`ROADMAP.md` principle #2, "The onion is the only
  extension mechanism").
- Every *payload-touching* hook is a **transform** (the four `…Transformer`
  interfaces). The fifth hook, `StreamFinalizer`, is **not** a transform: it flushes
  frames a `Transform` chose to hold, adding no new information.

**ROADMAP follow-ups (not done here):** §7's phase label still reads "Middleware
framework" (rename to "Shim framework" for consistency); and §5's seed catalog
still lists "Self-heal retries" as a shim, but this design relocates it to core
(§2, §14) — annotate or drop that catalog entry.

### 1.2 The two-level onion

```
LEVEL 1 — request granularity
   client ─req─► auth ─► shim1 ─► shim2 ─► shim3 ─►  DUMB FORWARDER  ─► Copilot
                        (TransformRequest, 1→2→3)     +token +imp.
   client ◄─resp─ shim1 ◄ shim2 ◄ shim3 ◄──────────  branch(Content-Type)  ◄─ Copilot
                        (Prelude / Buffered, 3→2→1)     ├ buffered ┤ ├ stream → Level 2 ┤

LEVEL 2 — stream-event granularity (inside the stream path)
   Copilot SSE ─► Reader ─frame─► shim3►shim2►shim1 ─0..n frames─► Writer ─► client
                    ▲ STALL          (TransformEvent, fold inner→outer)      ▲ TERMINAL + KEEPALIVE
                    (input side)                                             (output side)
   at stream end before a terminal reaches the client (EOF or stall): Finalize sweep (3→2→1) —
       each shim's HELD frames re-enter the outer shims' TransformEvent before emission ─►
       then terminal enforced (over the emitted stream; a held-but-complete stream ends `clean`)
```

Request half runs outer→inner (`1→2→3`); every response half runs inner→outer
(`3→2→1`). This is standard onion nesting: `shim1` is outermost (nearest the
client), `shim3` innermost (nearest the forwarder).

## 2. Scope

**In scope (Phase 3):**

- **`internal/shim` (new):** the five hook interfaces, the per-request instance
  model, the `Chain` (ordered composition + type-assert dispatch + per-shim
  toggle), the canonical no-op, and the `sseAdapter` that folds the enabled stream
  hooks into one `sse.FrameTransformer`.
- **Request-half integration** in `internal/forward`: run the request chain over
  the inbound body/headers before the upstream call.
- **Response-half integration** in `internal/forward`: a prelude pass (status +
  headers, both paths); the buffered-body path including the **buffering-mode
  switch** (read-all → transform → recompute `Content-Length`) fully implemented;
  the stream path wired to the pump's new transform seam.
- **`internal/sse` transform seam:** a minimal `FrameTransformer` interface the
  pump applies via a new `Pump` parameter, with the control-point relocations
  (terminal + keepalive to the output side; stall stays input side;
  finalize-and-re-evaluate on the pre-terminal teardown paths). A `nil`
  transformer preserves today's byte-exact verbatim path at zero cost.
- **Divergence-ledger extension** in `internal/apierror`: an `Error` carrier for a
  shim's deliberate pre-commit rejection, a default `ShimError` kind, and a
  `StreamShimFailed` stream reason for the post-commit terminal.
- **Config:** per-shim enable/disable and the buffered-response cap via the
  existing `ff/v4` conventions.
- **No-fabrication invariant** and the **no-double-up invariant** written into the
  contract.
- TDD unit + integration + end-to-end tests, `-race`, injected clock.

**Out of scope (deferred — see §15):**

- **Every seed shim** (stable item-IDs, model-name mapping, unsupported-param,
  `codex-auto-review`) — Phase 4. This phase proves the *hostable* seeds fit on
  paper (§14) but implements none.
- **Self-heal retries.** The 401/403 re-mint is relocated to the `forward`/identity
  **core** (a shim may not drive an upstream retry — principle #2); the
  thinking-block strip is treated as a client/parity judgment deferred with its
  shim. Neither is part of the shim framework.
- **Any client→server / bidirectional stream handling.** Both surfaces are
  strictly server→client SSE (§16.1); the OpenAI Responses *WebSocket* mode is a
  separate transport copilotd does not forward — an explicit non-goal.
- **Configurable ordering.** Order is registration order; a config-driven reorder
  waits until the catalog is large enough to need it.
- **Per-shim / per-event metrics** beyond the new outcome label, the suppressed-error
  counter (§12), and a startup chain log.
- **Structural repair** (emitting a protocol frame upstream dropped) as a
  capability — see §10.2.
- **Stream-side envelope revision** — a stream shim cannot buffer-then-change the
  status/headers (§7.1, §8.4); such a parity need is pre-commit or buffered-path only.

## 3. Guiding decisions & rationale

| Decision | Choice | Rationale |
| --- | --- | --- |
| Scope ambition | Design-complete, implement (nearly) complete: the full hook surface is built and tested via doubles; only real parity shims defer to Phase 4 | You cannot call something a framework without proving the *hostable* seed shims fit it (§14); the paper-fit is cheap insurance against a contract that needs breaking changes in Phase 4. |
| Shim unit model | A shim is a **per-request instance** produced by a factory `New(ctx, surface, route) any`; its struct fields are its per-request/per-stream state | One logical shim spans several hooks that share state (model-name saves the client's name in the request hook and restores it in two response hooks; stable-ids keeps per-stream id maps). An instance-per-request collapses "statefulness" and "multi-hook shim" into one object, with no locking. `route` gives *response-only* hooks the endpoint (e.g. `/messages` vs `/messages/count_tokens`) without smuggling it through a request hook (§5.2). |
| Hook dispatch | **Small role interfaces + type assertion** (not one monolithic interface) | A shim implements only the hooks it needs; absence of a method is the skip, so there are no dead no-op bodies. It is strictly a superset of the monolithic style (a shim may embed a nop base to opt into it), and the "absent vs no-op" distinction is load-bearing: it is how the framework detects whether *any* enabled shim wants the buffered body (→ the buffering-mode switch) or a stream hook (→ nil vs real `FrameTransformer`). Idiomatic Go — exactly how `http.ResponseController` probes `Flusher`/`Hijacker` and how the existing `statusWriter.Unwrap()` seam works. |
| Streaming directionality | Pure **server→client** event transformer; no bidirectional machinery | Fact-checked against both specs (§16.1): Anthropic Messages and OpenAI Responses `stream:true` are one-directional SSE; the client sends no in-stream events; cancellation is connection-close. |
| Response "whole-body" for streams | Does not exist; replaced by **per-event + finalize**; the shared envelope is the **prelude** hook | A stream is not a value; there is no single body to hold. The response transform splits into a once-per-response prelude (status/headers) plus a per-event body transform plus a finalize. |
| Buffered "whole-body" | A real hook, but it **introduces buffering** (read-all → transform → recompute `Content-Length`); opt-in, detected by presence of a `BufferedTransformer` | Today's buffered path is a verbatim `io.Copy`, never assembled in memory. Whole-body transform forces buffering — a real latency/memory cost made explicit and paid only when a buffered shim is active. |
| Control-point placement | Stall **armed** on input (upstream silence); **terminal + keepalive on output** (post-transform); pre-terminal teardowns **run `Finalize`, flush held frames, then re-evaluate the output-side terminal before synthesizing** | Stall means *Copilot* went quiet. But a shim may lawfully be **holding** a stream upstream already completed, so a pre-terminal teardown must flush held frames and re-check the client-facing terminal — a held-but-complete stream ends `clean`, not discarded as `stall`. Terminal detection sees what the client sees; keepalive tracks client-facing idle. See §8.2/§8.4 for exact ordering, the no-double-up invariant, and what the framework does *not* enforce. |
| No fabrication | A shim **alters / drops / holds / coalesces**; it never invents information with no upstream basis | ADR-0003 extended from the core to shims: copilotd invents nothing on the wire except the core's off-band-marked synthesized terminals. Enforced as a contract invariant, not a type boundary. |
| No upstream access | No hook talks to Copilot or drives a retry | `ROADMAP.md` principle #2. A retry re-invokes the forwarder, which is a core concern; the 401-remint is relocated to `forward`/identity. |
| `sse` independence | `internal/sse` owns a minimal `FrameTransformer` interface; `shim` implements it; `forward` wires it via a new `Pump` parameter. No `sse → shim` dependency | Keeps the engine Copilot-agnostic and payload-opaque (its Phase 2 charter). Dependency inversion: the low-level engine defines the seam, the higher layer supplies the behavior. |
| Package name | `internal/shim`; interfaces keep `…Transformer` names | "Middleware" undersells a mechanism that also transforms events; `shim` is `CONTEXT.md`'s ratified word and yields stutter-free exports (`shim.EventTransformer`, `shim.Chain`, `shim.Registration`). |

## 4. Module layout & package boundaries

```
copilotd/
└── internal/
    ├── shim/       [NEW]  Hook interfaces (Request/Prelude/Buffered/Event/Finalizer) · Registration{Name,Enabled,New} ·
    │                      Chain (per-request instantiation w/ (surface,route) · type-assert dispatch · onion ordering · toggle) ·
    │                      Request/Prelude/Body carrier types · NopShim (canonical example / test fixture) ·
    │                      sseAdapter (folds the enabled Event/Finalizer instances into one sse.FrameTransformer)
    ├── sse/        [CHG]  + FrameTransformer interface; Pump gains a transformer parameter (nil ⇒ today's verbatim path);
    │                      control-point relocation (terminal+keepalive → output side; stall stays input side);
    │                      finalize-and-re-evaluate on pre-terminal teardown; + Outcome value `shim_error`
    │                      (a doc-comment ties the name to the injected FrameTransformer — in production the shim chain);
    │                      + off-band counter for a shim error suppressed post-terminal (§8.4/§12)
    ├── forward/    [CHG]  + injected shim registry dependency; build a per-request Chain (surface, route); run request half
    │                      before Do(); prelude pass at commit; buffered path gains the buffering-mode switch; stream path
    │                      passes the sseAdapter (or nil) into Pump; streamPolicy.RenderError maps OutcomeShimError → StreamShimFailed
    ├── apierror/   [CHG]  + Error{Kind,Msg} carrier + Reject(kind,msg) (shim-shaped pre-commit rejection); + ShimError
    │                      Kind (default 500/api_error); + InvalidRequest Kind (400); + StreamShimFailed StreamReason
    │                      (streamMessages "copilotd: shim failed") rendered through the existing WriteStreamError
    ├── config/     [CHG]  + per-shim enable flags; + max-buffered-response-bytes cap
    └── server/     [CHG]  accessLog surfaces the `shim_error` outcome (+ `streamOutcomeIndexes`/warn-switch learn it); startup logs the enabled chain (names+order)
```

Each new/changed unit — *what it does · how it is used · what it depends on*:

- **`internal/shim`** [NEW] — the mechanism. Defines the hook interfaces and the
  `Chain`, which for each request instantiates one instance per enabled shim
  (`New(ctx, surface, route)`), dispatches each hook by type-assert in onion order,
  and folds the enabled stream hooks into one `sse.FrameTransformer` (§8.1).
  Exposes the canonical ordered registry (§5.4). Copilot-agnostic; depends on
  `context`, `net/http`, `apierror`, and `sse`. Knows nothing about credentials.
- **`internal/sse`** [CHG] — gains a `FrameTransformer` seam the pump applies via a
  new parameter, plus the control-point relocations and the suppressed-error
  counter. Still payload-opaque; a `nil` transformer is the byte-exact Phase-2 path.
- **`internal/forward`** [CHG] — gains an injected shim registry; builds the
  per-request `Chain`, runs the request half before `Do()`, runs the prelude pass,
  owns the buffered-mode switch, hands the pump the `sseAdapter` or `nil`, and maps
  the new stream outcome in `streamPolicy.RenderError`.
- **`internal/apierror`** [CHG] — gains the `Error` carrier + `Reject`, the
  `ShimError` and `InvalidRequest` kinds, and the `StreamShimFailed`
  stream reason. Remains the single home of every copilotd-originated signal.
- **`internal/server`** [CHG] — the access log learns the new outcome; startup
  logs the active chain within the redaction discipline.

## 5. The shim contract (`internal/shim`)

### 5.1 Hook interfaces

Five role interfaces. A shim implements only what it needs; the framework
type-asserts each per request.

```go
// Request half (both paths). Mutate r in place.
type RequestTransformer interface {
    TransformRequest(ctx context.Context, r *Request) error
}

// Response envelope (both paths), once, at the commit point. Mutate p in place.
type PreludeTransformer interface {
    TransformPrelude(ctx context.Context, p *Prelude) error
}

// Buffered response body (buffered path only). Implementing this OPTS INTO buffering.
type BufferedTransformer interface {
    TransformBuffered(ctx context.Context, b *Body) error
}

// Per SSE event (stream path only). Returns 0..n frames, all derived from real content.
type EventTransformer interface {
    TransformEvent(ctx context.Context, f sse.Frame) ([]sse.Frame, error)
}

// End of a stream (stream path only). Flush any HELD frames; no new information.
type StreamFinalizer interface {
    Finalize(ctx context.Context) ([]sse.Frame, error)
}
```

Carrier types (illustrative; finalized in the plan):

```go
type Request struct {                 // the inbound request payload, pre-forward
    Query  string                     // read-only context (verbatim query fidelity is core-owned; not applied from here)
    Header http.Header                // mutable-and-applied, subject to the existing denylist/impersonation overlay
    Body   []byte                     // mutable-and-applied; forward re-reads this after the chain
}
type Prelude struct {                 // the response envelope, at commit
    Status int                        // mutable-and-applied
    Header http.Header                // mutable-and-applied (Content-Length is framework-owned on the buffered path)
}
type Body struct {                    // the buffered response body
    Bytes []byte                      // mutable; the framework recomputes Content-Length from the result
}
```

**Carriers hold only payload; identity comes from instance state.** No carrier
carries `Surface`, `Route`, path, or method. The **endpoint** — the
`(Surface, Route)` pair — is supplied once to `New(ctx, surface, route)` (§5.2) and
read from the shim's own instance state by *every* hook, so a *response-only* shim
that must branch on endpoint needs no `RequestTransformer` merely to capture it.
`Method` is invariantly `POST` for all three routes (add it to `New` if a
non-`POST` route ever lands); the inbound path is derivable from `(Surface, Route)`
and is not surfaced until a consumer needs it. A shim's deliberate rejection is
carried by `apierror.Error` (§9.1), not by these carriers.

### 5.2 Registration, instances, the Chain

```go
// A registered shim: identity + toggle + a factory for per-request state.
type Registration struct {
    Name    string
    Enabled bool
    New     func(ctx context.Context, s apierror.Surface, route Route) any   // per-request instance
}
// Route is the registered upstream path (for example "/v1/messages",
// "/v1/messages/count_tokens", or "/responses"): unique *within a Surface*, never
// assumed globally unique. The endpoint identity is the (Surface, Route) pair, and
// every hook reads it from instance state (so a response-only shim needs no request
// hook merely to capture it).
type Route string
```

Per request, the `Chain`:

1. **Instantiates** one instance per *enabled* shim, in registration order,
   passing `(surface, route)`. A disabled shim's `New` is never called.
2. **Request half** (`1→2→3`): for each instance implementing `RequestTransformer`,
   call `TransformRequest`. A returned error is *pre-commit* → §9.1.
3. **Prelude pass** (`3→2→1`, at commit): for each instance implementing
   `PreludeTransformer`, call `TransformPrelude`. Pre-commit → §9.1.
4. **Body**, by path (§7, §8).

The compile-time assertion `var _ RequestTransformer = (*myShim)(nil)` is the
recommended guard against a typo'd method silently failing to satisfy an
interface (the one footgun of the type-assert model).

### 5.3 The no-op

```go
type NopShim struct{}                 // zero hook methods
// Registration{Name:"nop", Enabled:false, New: func(...) any { return NopShim{} }}
```

`NopShim` implements *nothing*, so every type-assert against it fails and every
hook skips it — yet it still flows through registration, per-request
instantiation, ordered iteration, reverse-order response, and toggle. A struct
with no methods exercises the entire machinery; that is the roadmap's "no-op
passthrough" proof. It is the canonical example and a test fixture, and ships
**disabled by default** (an empty enabled chain in production has zero overhead).

### 5.4 Registry & wiring

- **Registry home & order.** `internal/shim` exposes the canonical ordered
  `[]Registration`. Registration order *is* onion order; no separate ordering
  config this phase.
- **Config fold-in at the composition root.** `cmd/copilotd` folds config's
  `--shim-<name>-enabled` flags into each `Registration.Enabled`, producing the
  *enabled* registry — also the source of §12's startup chain log.
- **Injection into `forward`.** The enabled registry (or a small `ChainFactory`
  closing over it) is passed to `forward.New` as a **new injected dependency**,
  consistent with the forwarder's injected-dependency-for-testability posture —
  **not** a package global. Per request, `forward` calls
  `registry.NewChain(ctx, surface, route)`.
- **Transformer into the pump.** `sse.Pump` gains a parameter —
  `Pump(ctx, cancel, body, dst, policy, transformer)` — where `nil` selects the
  Phase-2 verbatim path. `forward` passes the `sseAdapter` when any enabled
  instance implements `EventTransformer`/`StreamFinalizer`, else `nil`.

## 6. Request-path integration (`internal/forward`)

1. Read the bounded inbound body (existing policy).
2. Build the per-request `Chain` from the injected enabled registry, passing
   `(surface, route)` (§5.4).
3. Run the **request half**: the chain mutates a `shim.Request` carrying the body
   bytes, headers, and (read-only) query (identity — surface + route — is instance
   state, not carried). A hook error → §9.1, rendered before any upstream call.
4. `forward` builds the outbound request from the **post-chain** body and headers,
   then applies the existing denylist + impersonation overlay + `Accept-Encoding:
   identity` (unchanged). It sets `RawQuery`/`ForceQuery` from the inbound URL
   exactly as today (verbatim query fidelity is core-owned; `Request.Query` is
   read-only context). **Impersonation headers always win over a shim's header
   edits** — the credential seam is not a shim's to override.
5. Send to Copilot (unchanged), then branch on the response `Content-Type`.

A shim that rewrites the body parses→mutates→re-serializes JSON — the deliberate,
per-shim break from raw passthrough that principle #1 reserves for these cases.

## 7. Response-path integration — buffered (`internal/forward`)

### 7.1 The prelude pass (both paths)

Before commit, over a `shim.Prelude{Status, Header}` derived from the upstream
response, the chain runs the **prelude pass** (`3→2→1`) — the response-side twin of
the request hook and the one response hook both paths share. The pass *finalizes*
the envelope; the envelope is *written* at commit, which is immediate on the stream
path but **deferred until after buffering** on the buffered-transform path (§7.2).
So the prelude always precedes commit, even though "the commit point" is not a
single fixed moment across the two paths.

**Constraint, applied uniformly:** the prelude is an envelope-only pass that
precedes the body on *both* paths, so **a shim cannot derive response headers from
the response body.** On a stream this is inherent — the `200 text/event-stream`
status/headers commit before any event exists. We apply the same rule to the
buffered path for one consistent model. A consequence worth stating (§8.4): a
stream shim therefore cannot *buffer-then-revise* the envelope (e.g. decide, after
seeing the whole stream, that the response should have been a 4xx) — that need
must be handled pre-commit or on the buffered path.

### 7.2 The buffering-mode switch

After the prelude pass, the buffered path probes the chain: **does any enabled
instance implement `BufferedTransformer`?**

- **No** → today's path: write status + headers, then `io.Copy(deadlineWriter,
  resp.Body)` — verbatim, unbuffered, zero added cost.
- **Yes** → buffer: read `resp.Body` under a bounded reader
  (`--max-buffered-response-bytes`); run the **buffered pass** (`3→2→1`) over a
  `shim.Body{Bytes}`; **recompute `Content-Length`** from the result; then write
  status + (prelude-adjusted, length-corrected) headers + body. Because the write
  is deferred until the transformed length is known, commit moves after the
  transform — buffering inherently delays commit, which is why a buffered hook
  error is still *pre-commit* and produces an HTTP rejection (§9.1).

Buffered transforms operate on the bytes as-is. This is safe because copilotd
already forces `Accept-Encoding: identity` upstream (forwarding-fidelity §3.2), so
a buffered body is uncompressed in practice; a defensive skip-if-non-identity
guard on `Content-Encoding` keeps a surprise compressed body from being corrupted
(it falls back to verbatim copy). **Note the divergence:** forwarding-fidelity §3.4
states the buffered path does *not* inspect `Content-Encoding`; this guard lifts
that invariant, which that design explicitly permits as "an intermediate policy
until compression belongs to middleware" (forwarding-fidelity §3.2) — Phase 3 is
that layer.

Over the size cap → a provider-shaped error before commit (reuses the
`PayloadTooLarge` shape, response-side).

## 8. Response-path integration — streaming (`internal/sse` + `internal/shim` + `internal/forward`)

### 8.1 The transform seam and multi-shim composition

`internal/sse` gains a minimal, payload-opaque interface it owns:

```go
type FrameTransformer interface {
    Transform(ctx context.Context, f Frame) ([]Frame, error)  // 0..n frames
    Finalize(ctx context.Context) ([]Frame, error)            // held frames at stream end
}
```

`internal/shim` supplies an `sseAdapter` that folds the enabled `EventTransformer`
/ `StreamFinalizer` instances (in onion order) into one `FrameTransformer`,
closing over the request context. `forward` passes the adapter into `Pump` — **or
`nil`** when no enabled shim implements either stream hook, selecting the Phase-2
verbatim fast path unchanged.

**Composition semantics (N ≥ 1 event shims).**

- **Per upstream frame**, the adapter folds **inner→outer** (`3→2→1`) with fan-out:
  each output frame of shim *k* is fed one-by-one into shim *k−1*'s
  `TransformEvent`; `shim1`'s output is what the pump receives. A shim returning
  `[]` (drop/hold) contributes no frames to the outer shims for that input.
- **At stream end**, the adapter's single `Finalize` runs one **inner→outer** sweep:
  for each shim from inner to outer, it first pushes any frames still pending from
  inner shims through *this* shim's `TransformEvent`, then appends *this* shim's
  `Finalize()` output — accumulating into the frames it returns.
- **Consequence (a load-bearing guarantee):** an inner shim's held/finalized frames
  still traverse **every** outer shim's `TransformEvent` before reaching the writer.
  So a `hold` shim composed with an `alter` shim behaves correctly. The pump's
  output-side terminal/keepalive checks (§8.2) run only on the fully-composed output.
- **Known constraint (not a bug):** the sweep is "push inner-pending, then append my
  `Finalize`," so a shim cannot emit *its own* held frames *before* an inner shim's
  finalize output. No seed needs cross-shim reordering at EOF; documented so Phase 4
  does not assume it.

**Shim obligation (checked by §13 doubles, not by the framework).** A shim that
holds content **must** also hold the terminal (and any framing that must precede
the held content), releasing them together, in order, at `Finalize`. Letting a
terminal pass through while still holding earlier content produces an invalid
post-terminal-content stream (§8.4) — a shim bug the framework does not police,
because it forwards frames in the order shims emit them (the Phase-2 passthrough
contract). `Finalize` must be **prompt and non-blocking** (CPU-bound
re-marshal/flush only, no I/O, no waiting): it runs after the select loop, so
nothing preempts a blocking `Finalize` (§8.2).

### 8.2 The pump change

The pump tracks one output-side bit, `sawTerminal` — **did a terminal reach the
client?** — set when a *written* frame is terminal (post-transform). Two invariants
govern the loop:

- **Finalize-before-terminal.** A teardown that reaches the pump with
  `!sawTerminal` (the client has *not* yet seen a terminal) runs
  `transformer.Finalize` first, writes the held frames (output-side terminal check
  each), and *then* decides the outcome — so a held-but-complete stream flushes to
  `clean`. Once `sawTerminal` is true, the client's stream is already complete and
  the pump returns without running `Finalize` (any still-held frames are
  post-terminal and intentionally dropped — §8.3, §8.4).
- **No-double-up (ADR-0003).** Once `sawTerminal` is true, copilotd emits **no**
  synthesized terminal of any kind. This subsumes Phase 2's "never double up on a
  terminal the upstream already delivered" and extends it to `shim_error`.

The Phase 2 select loop (`ctx.Done | stall | keepalive | reads`) changes as
follows (non-nil transformer; a `nil` transformer is exactly Phase 2):

| Loop event | Action |
| --- | --- |
| **upstream frame** | stall stopwatch **stops on receipt** (input side); `out, err := transformer.Transform(ctx, frame)`; on err → **error rule** (§8.3); else for each `f` in `out`: write `f.Raw` + flush, and on the **output side** set `sawTerminal` if `terminal(f.Type)` and reset the keepalive timer; then **re-arm stall**. `out == []` (drop/hold) writes nothing, sets no terminal, resets no keepalive. |
| **keepalive tick** | timer fires on interval. If `sawTerminal` → return **`clean`** (no ping, no `Finalize`; the client's stream is complete). Else write `:\n\n` + flush and continue — a `hold` shim creates this client-idle gap while upstream is busy. Does not touch stall. |
| **stall fires** | upstream silence (`--stream-idle-timeout`, excludes our write time). If `sawTerminal` → **`clean`**; else **pre-terminal teardown** (§8.3) with cause `stall`. |
| **ctx.Done / write error / write-deadline** | `client_cancel`; **discard** held frames (client is gone); cancel + join. The only path that skips `Finalize` regardless of `sawTerminal`. |
| **reader done** (EOF or read-error) | If `sawTerminal` → **`clean`**; else **pre-terminal teardown** (§8.3) with cause EOF (⇒ `synthesized` if unterminated) or read-error (⇒ `upstream_error`). |

The **cancel-then-join** lifecycle and the deadline-bounded writer are unchanged.
Delay note: a full-buffering `hold` shim normally flushes at **EOF** (upstream
closes after its terminal) — prompt; only the pathological "terminal delivered but
connection held open" case waits for `stall` (≤ `--stream-idle-timeout`, 90s, with
`:` pings every 15s meanwhile) before the held content flushes. That is the shim's
own coalescing choice, not a framework stall.

**Regression anchor:** a `nil` transformer, and equally an identity transformer,
both produce output byte-exact with Phase 2 — asserted frame-for-frame including
flush boundaries (§13), not merely by concatenation.

### 8.3 Teardown & error precedence

**Pre-terminal teardown (a stall or reader-done exit taken while `!sawTerminal`).**
Run `transformer.Finalize(ctx)`; write its returned frames (output-side terminal
check each, so a held terminal counts toward `sawTerminal`); then, in precedence
order:
1. `sawTerminal` (over the emitted stream, incl. finalize output) → **`clean`**,
   suppressing all synthesis (no-double-up). *(Fixes held-terminal-⇒-stall.)*
2. else if `Finalize` errored → **`shim_error`** (synthesize + teardown).
3. else synthesize the cause's terminal: EOF ⇒ **`synthesized`**; read-error ⇒
   **`upstream_error`**; stall ⇒ **`stall`**.

**Error rule (a `Transform` error mid-stream).** A shim is in a suspect state, so
`Finalize` is **not** run — which means the whole chain's in-flight held frames
(including those of *healthy* outer shims) are discarded. This is the conservative
choice (the monolithic sweep is all-or-nothing and the stream is terminating in
error), stated so reviewers don't assume healthy shims get to flush. Precedence:
`sawTerminal` → **`clean`** (no-double-up, but see §8.4 observability); else →
**`shim_error`**. Then cancel + join.

### 8.4 What the framework does *not* enforce (stated, not silently assumed)

- **Terminal position.** No-double-up guards *duplicate terminals*, not
  *post-terminal content*. The pump writes frames in the order shims emit them
  (matching Phase 2, which likewise forwards any post-terminal upstream frames such
  as a trailing vendor `[DONE]`). If a shim emits content after a terminal (its
  bug — see the §8.1 obligation), that reaches the client as an invalid stream. We
  do **not** drop post-terminal frames, because that would diverge the nil path
  from Phase 2's verbatim passthrough and break the regression anchor. The §13
  hold/coalesce doubles are the guard against this authoring mistake.
- **Suppressed post-terminal errors are still recorded off-band.** When the error
  rule or a post-terminal `Finalize` error is suppressed to `clean` by no-double-up,
  the wire stays `clean` (correct), but the `sse` layer emits a **warn log + a
  `suppressed post-terminal shim error` counter** (§12). This decouples *what
  the client sees* (clean) from *what operators see* (a real shim failure), closing
  the observability hole the suppression would otherwise open.
- **Hook promptness.** `TransformEvent` and `StreamFinalizer.Finalize` run
  synchronously inside the pump loop, outside `select`: on frame receipt the stall
  stopwatch and keepalive timer are both stopped, the write-deadline bounds only
  writes, and `ctx.Done()` cannot preempt a synchronous call. A hook that blocks —
  I/O, a lock, a hot loop, or merely slow re-marshalling — hangs the request past
  stall, keepalive, and the write-deadline. Both hooks **must** be prompt and
  non-blocking (CPU-bound re-marshal only; no I/O, no waiting). Unlike terminal
  position this is not even testable by a double (the test would hang), so it is a
  pure authoring/review invariant. Framework-level bounding is deferred — see
  issue #31.

## 9. Error semantics — divergence-ledger extension

Two new copilotd-originated signals, both rendered from `internal/apierror`,
extending the Phase 2 ledger. Origin stays off-band (`copilotd:` prefix +
`X-Request-Id` + logs); no new wire marker (ADR-0003 binds shims).

### 9.1 Tier 1 — pre-commit rejection (`apierror.Error` / `ShimError`)

A `RequestTransformer` / `PreludeTransformer` / `BufferedTransformer` hook that
returns an error **before commit** is rendered as an HTTP-status signal. Because
hooks return a plain `error`, `apierror` gains a carrier and `forward` extracts it:

```go
// internal/apierror
type Error struct { Kind Kind; Msg string }        // carries a specific provider-shaped kind
func (e *Error) Error() string { return e.Msg }
func Reject(kind Kind, msg string) *Error { return &Error{kind, msg} }

// new kinds
//   ShimError — 500 / api_error  (default for a bare/unexpected hook error)
//   InvalidRequest  — 400 / invalid_request_error  (a shim's deliberate 400; not overloading BackgroundUnsupported)
//   (InvalidRequest and BackgroundUnsupported deliberately share the 400 /
//    invalid_request_error wire shape; they stay distinct Kinds for intent and log
//    identity, so a later reader should not fold them together.)
```

Forward-side rule at every pre-commit hook:

```go
if err != nil {
    var apiErr *apierror.Error
    if errors.As(err, &apiErr) {
        apierror.Write(w, surface, apiErr.Kind, apiErr.Msg)     // shim-shaped, e.g. Reject(InvalidRequest, "...")
    } else {
        apierror.Write(w, surface, apierror.ShimError, "copilotd: shim failed")
    }
    return
}
```

| Signal | Trigger | HTTP | Anthropic `error.type` | OpenAI `type` |
| --- | --- | --- | --- | --- |
| ShimError (default) | a pre-commit hook returned a bare error | 500 | `api_error` | `api_error` |
| Shim-shaped rejection | a hook returned `apierror.Reject(kind, …)` | per kind | per kind | per kind |

### 9.2 Tier 2 — post-commit `shim_error` terminal

An `EventTransformer` / `StreamFinalizer` error **after commit** cannot change the
locked HTTP status, so it routes through the synthesized-terminal machinery as a
new outcome — **but only when no valid terminal has already reached the client**
(§8.2/§8.3 no-double-up). This requires two additions the render vehicle lacks
today (it is *reused*, not sufficient as-is):

- `apierror` gains a **`StreamShimFailed`** `StreamReason` with a
  `streamMessages` entry `copilotd: shim failed`, rendered through the
  existing `WriteStreamError`.
- `forward`'s `streamPolicy.RenderError` gains a **case mapping**
  `sse.OutcomeShimError → apierror.StreamShimFailed`. The switch
  default is `StreamEnded`; without this case the new outcome would silently emit
  "upstream stream ended before a terminal event" — the wrong signal.

| Trigger | Wire | Metric outcome | Off-band |
| --- | --- | --- | --- |
| stream hook failed, client had **not** seen a terminal | `event: error` (`shim failed`) | `shim_error` (warn) | log + metric |
| stream hook failed, a terminal had **already** reached the client | *(nothing — no-double-up)* | `clean` | **warn log + suppressed-error counter** (§8.4) |

A `client_cancel` still emits nothing (and records nothing beyond the existing
cancel accounting).

## 10. Statefulness & the no-fabrication invariant

### 10.1 State

Per-request/per-stream state lives in the instance's struct fields, created by the
factory (with `(surface, route)`) and discarded when the request ends. One
instance per request means no locking and no cross-request leakage. Hooks of the
*same* shim share state for free.

### 10.2 No fabrication (contract invariant)

A shim may **alter, drop, hold, or coalesce** — every emitted frame or body byte
must derive from real upstream content (or the request). It may **not** invent
information with no upstream basis. copilotd invents exactly the terminals in its
divergence ledger (Phase 2 §7 + §9 here), all off-band-marked (ADR-0003); shims add
nothing to that list.

This is a **policy invariant, not a type boundary** — the same variable-fan-out
signature that enables `hold`/`coalesce` mechanically permits fabrication, so it is
enforced by review and the divergence ledger, not by the compiler. The unenforced
terminal-position property (§8.4) is the same kind of trust.

**The one gray area — structural repair.** Emitting a *protocol-required* frame
upstream dropped supplies required structure rather than inventing information. It
is **not pre-authorized.** If a real shim needs it, it becomes a named ledger entry
decided then — never a blanket "shims may inject" capability.

## 11. Configuration

`ff/v4`-backed; precedence flags > env > TOML > default; env `COPILOTD_` +
upper(flag, `-`→`_`).

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| Per-shim enable | `shim-<name>-enabled` | `--shim-<name>-enabled` | `COPILOTD_SHIM_<NAME>_ENABLED` | shim-defined (`nop`: `false`) | Disabled ⇒ never instantiated. Order = registration order (static this phase). |
| Buffered response cap | `max-buffered-response-bytes` | `--max-buffered-response-bytes` | `COPILOTD_MAX_BUFFERED_RESPONSE_BYTES` | `33554432` (32 MiB) | Consulted only when a `BufferedTransformer` is active; over-cap ⇒ pre-commit provider-shaped error. Mirrors `defaultMaxRequestBytes` (also 32 MiB); back it with a named `defaultMaxBufferedResponseBytes` constant, plus the sibling's struct field / `LogValue` / `Resolve` / `overlay` / `validate` wiring. |

Validation fails fast before the listener binds. No new secrets; redaction unchanged.

## 12. Observability

Within the redaction discipline (no bodies, no frame contents, no secrets):

- **Startup:** log the enabled chain — shim names and order — once (empty chain
  logs as such).
- **Request-level:** the access log gains the `shim_error` outcome (bumps the
  line to `warn`, matching the synthesized-terminal treatment). Adding the outcome
  also requires a case in `streamOutcomeIndexes` (`internal/server/metrics.go`) and
  the access-log warn switch (`internal/server/middleware.go`), or the bounded
  counter silently drops it and the line stays `info`.
- **Suppressed post-terminal errors (§8.4):** a shim error suppressed to `clean` by
  no-double-up still emits a **warn log + a distinct counter** at the `sse` layer,
  so a shim failure after the terminal is never invisible even though the
  wire outcome is `clean`.
- **Deferred:** per-shim / per-event latency metrics. The seam exists; YAGNI until
  the catalog grows.

## 13. Deliverable & testing strategy

TDD throughout, `-race`, stdlib `testing` + `net/http/httptest`, injected clock,
`httptest` Copilot stub — the Phase 2 rig. Proven by **test doubles**, since no
real shim ships. §8.2/§8.1 were hardened against specific bugs, so each is tested
on **both** of its arms:

- **true-noop** (`NopShim`) → skipped at every hook; output byte-exact vs. an empty
  chain, both paths.
- **identity double** (all five as identity) → transparency: buffered bytes
  unchanged; **stream byte-exact asserted frame-for-frame including the `(Write,Flush)`
  sequence** (not just concatenation, to catch a coalescing/flush-granularity
  regression); feed the **full Phase-2 Reader corpus** (CRLF+LF, `:` comments,
  multi-line `data`, unknown `event` types) through the identity `Transform` and
  assert `Raw` survives byte-exact.
- **recording double** → request order `1→2→3`, response order `3→2→1`; the **stream
  fold order** distinguished from forward via **non-commutative** event shims (not a
  commuting pair).
- **toggle** → a disabled shim's `New` is never called (observable via the
  constructor); enabling flips behavior.
- **held-but-complete ⇒ `clean`, on BOTH arms:** hold every frame incl. terminal,
  assert `clean` + full held stream delivered — once via **EOF** (upstream closes
  after the held terminal) and once via **stall** (idle-without-EOF past the clock).
- **mid-stream `Transform` error (the error rule), BOTH branches:** terminal
  already written ⇒ `clean`, no synthesized frame, **and the held frame is NOT
  flushed** (proves `Finalize` skipped); no terminal yet ⇒ exactly one
  `shim_error` via `StreamShimFailed`.
- **`Finalize` error, BOTH branches:** after a delivered terminal ⇒ `clean`, no
  duplicate, **suppressed-error counter incremented** (§8.4); before any terminal ⇒
  `shim_error`.
- **finalize-sweep re-transform** → inner `hold` releases `X` at `Finalize`, outer
  `alter` maps `X→X′`; assert the client receives **`X′`** (a naive per-shim-finalize
  concatenation emits `X` and fails).
- **keepalive** → fires during a hold (upstream busy, client idle, injected clock);
  **stops after a terminal** (`!sawTerminal` short-circuit: zero pings post-terminal).
- **buffered double** → `Content-Length` recompute, mode switch on/off, size-cap
  rejection, and the **`Content-Encoding: gzip` skip-guard** (transform skipped,
  body copied verbatim, length untouched).
- **prelude double** → mutates status/headers; assert status+headers reach the
  client **before the first event byte** on the stream path (the falsifiable form of
  "no body-derived headers").
- **impersonation-wins** → a request double sets an impersonation-controlled header;
  assert the outbound request carries the impersonation value, not the shim's.
- **error doubles** → pre-commit `Reject(InvalidRequest,…)` → 400 with that shape;
  bare error → 500 `api_error`; on the **buffered path** (deferred commit) the error
  renders as HTTP, not a half-written body; a `RequestTransformer` rejection hits
  the Copilot stub **zero** times (short-circuit).
- **lifecycle (`-race`)** → the two new error exits (`Transform`/`Finalize` error)
  join the reader and release the upstream connection, like the Phase-2 exits.
- **observability** → the access line for a post-commit failure carries
  `shim_error` + `warn`; a suppressed post-terminal error increments the §8.4
  counter.
- **end-to-end** → server + API key + stubbed identity + stub Copilot, identity
  double enabled, whole request survives a full chain unchanged on both a buffered
  and a streamed response.

## 14. Seed-shim paper-fit (policy-A validation)

The contract is a "framework" only if the *hostable* seed catalog (`ROADMAP.md` §5)
fits without breaking changes.

| Seed shim | Hooks | State | Fit |
| --- | --- | --- | --- |
| Stable Responses item-IDs | `EventTransformer` (pin first-seen id) | `map[output_index]string` for item events **plus** a scalar response-`id` (the top-level `resp_…` on `response.created/…/completed` has no `output_index`; item events dual-key by `item_id` and `output_index`) | **Fits** — alters existing fields |
| Model-name mapping | `RequestTransformer` (save+rewrite) + `BufferedTransformer`/`EventTransformer` (restore) | `clientModel string`; reads `route` to know `count_tokens` bodies carry no top-level `model` | **Fits** — alters existing fields |
| Unsupported-param handling | `RequestTransformer` (strip) or pre-commit `apierror.Reject(InvalidRequest,…)`; reads `route` (the `/messages` vs `count_tokens` param whitelist differs) | none | **Fits** |
| `codex-auto-review` availability | **Fit-unknown; possibly not a shim.** ROADMAP defines it only as "make the behavior the Codex auto-review flow expects available." If it needs a second model call it is core (like self-heal, principle #2); if it needs a response field Copilot omits it is a fabrication ledger decision (§10.2); if it needs an unforwarded endpoint it is Phase-5 support-endpoint territory. Only a model/param map is a clean `RequestTransformer`. | — | **Deferred to Phase 4 after behavior capture** — do not assume "additive hook" |
| Self-heal retries | **Not a shim.** 401/403 re-mint → `forward`/identity core; thinking-block strip → client/parity judgment | — | **Out of the framework** |

**Three seeds fit cleanly; `codex-auto-review` is fit-unknown (a *research* gap,
and possibly a non-shim); self-heal is explicitly core.** Because the type-assert
model makes new hooks additive, a hostable surprise costs a later hook — but as the
codex row shows, some surprises are relocation-to-core or a ledger decision, *not*
an additive hook, so we no longer claim "any surprise costs only an addition."

**Honest limit of this validation:** the three fitting seeds are all pure **1→1
alters on disjoint fields**, so they **commute** and exercise *none* of §8.1's
fan-out / drop-hold / finalize-sweep machinery. That machinery — the load-bearing
part of the stream contract — is validated **only** by the §13 stream-semantics
doubles; it is speculative insurance for a future `hold`/`coalesce`/reorder shim,
not something any current seed requires. We build it now (policy A) with eyes open
to that.

## 15. Deferrals mapped to phases

| Deferred item | Lands in |
| --- | --- |
| Stable item-IDs, model-name, unsupported-param shims | Phase 4 |
| `codex-auto-review` (behavior capture → shim, core relocation, or Phase-5 endpoint) | Phase 4 research first |
| 401/403 self-heal re-mint (core), thinking-block strip | `forward`/identity, outside the framework |
| Configurable shim ordering | When the catalog needs it |
| Per-shim / per-event latency metrics | Later phase (seam pre-positioned) |
| Structural-repair injection; stream-side envelope revision | Only if a shim proves it necessary (§10.2, §8.4) |
| OpenAI Responses WebSocket (bidirectional) mode | Non-goal (separate transport) |

## 16. Notes & open items

### 16.1 Grounding — streaming directionality (fact-checked)

Confirmed against the live provider docs; determines the stream half is a pure
server→client transformer with no bidirectional machinery:

- **Anthropic Messages `stream:true`** — SSE over one HTTP request; no in-stream
  client events; cancel = connection-close. `message_start → (content_block_* ·
  message_delta · ping · error)* → message_stop`; **`message_stop` terminal.**
- **OpenAI Responses `POST /v1/responses` `stream:true`** — SSE over one POST; no
  in-stream client events. Terminals **`response.completed` / `response.failed` /
  `response.incomplete`** + typed `error`; **no `[DONE]` sentinel.** The separate
  `POST /responses/{id}/cancel` applies only to `background:true` (which copilotd
  rejects) — irrelevant here.
- **Scoped out:** the OpenAI Responses **WebSocket** mode (`wss://…`) *is*
  bidirectional but is a different transport copilotd does not forward — a non-goal.

### 16.2 Drift sensitivity

Thin: the `sse.FrameTransformer` seam is internal, and terminal-name assumptions
are unchanged from Phase 2. Payload-opaque keeps blast radius small — a shim pays
the unmarshal/mutate/re-marshal cost only for events it rewrites.

### 16.3 Vocabulary

Follows `CONTEXT.md`. A synthesized terminal — including `shim_error` — is a
copilotd-originated signal, never conflated with an upstream one. Package/term
decision in §1.1; ROADMAP follow-ups noted there.

### 16.4 Applied critique

Hardened by an adversarial critic panel across six lenses (completeness,
contract-soundness, consistency, seed-shim-fit, test-adequacy, and a focused
soundness re-check of the reworked sections), each finding verified against the
source before acceptance. **Design/logic fixes folded in:** the
held-terminal-discarded-as-`stall` bug and the finalize-error-vs-terminal
precedence bug (§8.2/§8.3, via finalize-before-terminal + no-double-up); the
`apierror.Error`/`Reject` carrier and `ShimError`/`InvalidRequest` kinds
(§9.1); the `StreamShimFailed` reason + `RenderError` mapping (§9.2);
multi-shim stream composition semantics (§8.1); the registry origin/injection
wiring (§5.4); the `route` argument to `New` closing the `count_tokens`/path gap
(§5.2/§14); the off-band recording of suppressed post-terminal errors (§8.4/§12);
the keepalive-after-terminal third clean exit (§8.2); and honest reframing of §14
(codex-auto-review reclassified fit-unknown/possibly-non-shim; the fitting seeds
only exercise commuting alters, so §8.1's machinery is validated by tests alone).
**Consistency lens confirmed** every claim the doc makes about existing code
against source (forward/sse/apierror/config seams all check out). **Test additions**
(§13) close the coverage gaps the design's own invariants imply — notably testing
each hardened path on both arms. Refuted findings (a `Request.Query` ambiguity and
a keepalive concern) were left unchanged.
