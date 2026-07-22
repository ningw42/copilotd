# WebSocket shim message transform (bidirectional, opt-in)

Status: proposed 2026-07-22.
Design for extending copilotd's composable **shim** contract from the HTTP
(buffered) and SSE paths to the OpenAI Responses **WebSocket** transport. It builds
on the payload-opaque
[WebSocket forwarding design](2026-07-19-openai-responses-websocket-forwarding-design.md)
and the [Phase 3 middleware framework design](2026-07-16-phase-3-middleware-framework-design.md),
and it reverses that WebSocket design's explicit "no shim / extensibility"
non-goal.

## 1. Goal and scope

Give a shim a way to observe and transform **individual WebSocket messages in both
directions** — client → upstream and upstream → client — on the OpenAI Responses
WebSocket path, using the **same `shim.Registry`/`shim.Chain`** that already backs
the HTTP request, response prelude, buffered body, and SSE stream hooks. One shim
can implement HTTP, SSE, and WebSocket hooks and share logic and state across them.

This delivers the *capability*; it ships **no concrete shim**. The canonical
registry stays a disabled no-op, so default behavior is byte-for-byte identical to
the current payload-opaque forwarder. A future parity shim (for example, stabilizing
OpenAI Responses item IDs that Copilot does not preserve) is the anticipated first
consumer, but this design is deliberately **not bound** to that task — it defines a
general bidirectional seam.

### In scope

- Two new opt-in per-shim interfaces in `internal/shim`:
  `ClientMessageTransformer` (client → upstream) and `ServerMessageTransformer`
  (upstream → client).
- A mutable `shim.Message` carrier and a transport-neutral `shim.MessageKind`
  (Text | Binary), joining the existing `Request` / `Prelude` / `Body` carriers.
- Two `shim.Chain` adapters, `WSClientAdapter()` and `WSServerAdapter()`, each
  folding the enabled directional hooks into a single transform, or returning `nil`
  when no shim participates in that direction.
- A per-direction transform seam inside the `wsforward` message pump, entered only
  when the corresponding adapter is non-`nil`.
- Per-session chain construction in `wsforward.Proxy`, threading the existing
  `shim.Registry` in through `wsforward.New`.
- Documentation updates: the `shim` package doc, the `CONTEXT.md` glossary, the
  prior WebSocket-forwarding non-goal, and ADR-0006.

### Explicit non-goals (YAGNI)

- **No emission-holding, no `Finalize`.** A transform decides each message
  synchronously: rewrite in place, or drop. There is no framework buffer that holds
  a message for later release, and therefore none of the SSE `StreamFinalizer`
  interleaving machinery (which issue #38 exists to simplify). A shim may keep
  *knowledge* in its own fields; it may not make the framework hold an *emission*.
  Rationale in §5.
- **No 1→N / splitting / injection.** A transform maps one message to at most one
  message (§4). Coalescing (N→1) is still expressible through shim state; only true
  splitting/injection is excluded, and no consumer needs it. Widening to 1→N later
  is a mechanical, non-breaking change (the interfaces are internal with only a
  no-op implementer).
- **No concrete shim.** No `initiator` injection, no ID stabilization, no model
  rewriting. This is the seam only.
- **No new transport, route, catalog, config, or dependency.** The WebSocket
  transport, its `GET /openai/v1/responses` route, catalog membership, timeouts,
  limits, and the `coder/websocket` dependency are all unchanged from the
  forwarding design.
- **No change to default behavior.** With the no-op registry, both adapters are
  `nil` and the pump keeps its current verbatim fast path.
- **No Anthropic WebSocket.** There is no public Anthropic Messages WebSocket
  contract (forwarding-design research §3); this is OpenAI-only.

## 2. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Direction model | **Two opt-in interfaces**, one per direction | Client (`response.create`) and server (typed event stream) carry different vocabularies; a one-directional shim implements one side with zero boilerplate; matches the existing "presence = participation" idiom; preserves onion fold symmetry. |
| Cardinality | **1→1 + drop**, in-place mutation | Covers rewrite, drop/filter, and coalesce-via-state; keeps the adapter a linear early-exit fold; no consumer for split/inject; wideable later. |
| Holding / `Finalize` | **None** | The WS turn terminal (`response.completed` / `response.failed` / `error`) arrives in-band as an observable message, so a shim can flush accumulated state synchronously; state-holding covers the real cases; avoids the SSE finalize complexity. |
| Shim state scope | **Per session** | A WebSocket session is long-lived and multi-turn. The chain is built once per accepted session; a both-directions shim shares its remap state through its own struct across turns and directions. A shim instance is thus per-session on WebSocket but per-request on HTTP; a both-transports shim must not assume a request-scoped lifetime. |
| Cross-direction concurrency | **Shim-owned synchronization** | The client→upstream and upstream→client pumps run in separate goroutines over one per-session instance, so a both-directions shim's two hooks can be called concurrently; the shim guards its own shared state. The framework does not serialize cross-direction calls. A short CPU-bound critical section (e.g. a mutex over the remap map) is compatible with the prompt/non-blocking rule; the alternative — framework serialization — would add a lock in the hot path and block one direction while the other transforms, forfeiting the per-direction independence. |
| Registry / chain | **Reuse `shim.Registry` / `shim.Chain`** | One unified shim concept spanning HTTP, SSE, and WebSocket; a shim can close a parity gap across transports at once. |
| Seam location (packaging) | **Carriers + interfaces + adapters in `shim`; `wsforward` imports `shim`** | `shim` already owns every mutable carrier; one-way dependency (`wsforward → shim`, mirroring `forward → shim`); no cycle, no wiring package, no injected factory. |
| Message unit | **Reassembled coder/websocket message**, named **Message** (not "Frame") | `coder/websocket` `Read` returns whole messages; `CONTEXT.md` reserves "frame" for SSE records. |
| Message kind | **Neutral `shim.MessageKind`** (Text \| Binary) | Keeps the `coder/websocket` type out of `shim`; `wsforward` maps `websocket.MessageType` ↔ `MessageKind` at the seam. |
| Default (no-op) path | **`nil` adapter → verbatim fast path** | Preserves the payload-opaque guarantee and current performance; mirrors `sse.Pump`'s nil-transformer fast path. |
| Transform error | **Fatal to the session**: close `1011`, `SessionError` terminal | Post-upgrade there is no HTTP error channel; reuse the existing WS close-code / terminal machinery; mirrors the SSE pump treating a pre-terminal shim error as `OutcomeShimError`. |

## 3. Package `shim`: carrier and interfaces

New carrier, alongside `Request`, `Prelude`, and `Body`
([shim.go](../../internal/shim/shim.go#L24-L44)):

```go
// MessageKind is the transport-neutral WebSocket message kind. It decouples the
// shim contract from the coder/websocket message-type enum; wsforward maps
// between them at the pump seam.
type MessageKind int

const (
    MessageText MessageKind = iota
    MessageBinary
)

// Message carries one mutable WebSocket message (a reassembled coder/websocket
// message, not a wire frame). A transformer mutates Kind and/or Data in place.
type Message struct {
    Kind MessageKind
    Data []byte
}
```

Two directional hooks, each opt-in by interface presence (a shim may implement one,
the other, or both):

```go
// ClientMessageTransformer transforms one client → upstream message. It runs
// synchronously in the WebSocket pump and must be prompt and non-blocking
// (CPU-bound transformation only; no I/O or waiting). Return emit=false to drop
// the message (it is not forwarded upstream). Mutate *Message in place.
type ClientMessageTransformer interface {
    TransformClientMessage(context.Context, *Message) (emit bool, err error)
}

// ServerMessageTransformer transforms one upstream → client message, under the
// same prompt/non-blocking and in-place-mutation rules. Return emit=false to drop
// the message (it is not delivered to the client).
type ServerMessageTransformer interface {
    TransformServerMessage(context.Context, *Message) (emit bool, err error)
}
```

The two returns are **distinct refusal paths**: `emit=false` is an intentional
policy drop (the message is deliberately not forwarded), while a returned `err`
signals an internal or impossible-state failure and is fatal to the session (§7).
A shim that merely declines to forward a message must drop it, never error.

These inherit the `shim` package's existing **policy invariants**: a transform may
alter or drop information derived from a message but must not fabricate information
without an upstream basis, must not access Copilot, and must not drive an upstream
retry. As with the SSE stream hooks, these are review-enforced, not type-enforced.

A single transform seam type lets the pump call an adapter uniformly:

```go
// MessageTransform folds the enabled directional hooks for one direction into a
// single call. nil means no shim participates in that direction (verbatim path).
type MessageTransform func(context.Context, *Message) (emit bool, err error)
```

## 4. Package `shim`: chain adapters and fold order

Two adapters on `*Chain`, mirroring `StreamAdapter()`
([shim.go](../../internal/shim/shim.go#L158-L173)). Each returns `nil` when no
enabled instance implements the corresponding interface, selecting the pump's
verbatim fast path for that direction:

```go
func (c *Chain) WSClientAdapter() MessageTransform // client → upstream, or nil
func (c *Chain) WSServerAdapter() MessageTransform // upstream → client, or nil
```

Each adapter is a **linear early-exit fold** — there is no fan-out, because a
transform yields at most one message:

- **`WSClientAdapter` (client → upstream)** applies participating shims in
  registration order **0 → n** (outermost first), matching `RunRequest`
  ([shim.go](../../internal/shim/shim.go#L108-L119)). The request travels inward
  toward the upstream.
- **`WSServerAdapter` (upstream → client)** applies participating shims in reverse
  order **n → 0** (innermost first), matching the prelude / buffered / stream half
  ([shim.go](../../internal/shim/shim.go#L122-L145)). The response travels outward
  toward the client.

A shim that returns `emit=false` **short-circuits** the remaining chain for that
message (it is dropped and no outer/inner shim sees it). A shim that returns an
error aborts the fold and propagates the error to the pump (§7). Fold semantics:

```go
// WSClientAdapter (sketch); WSServerAdapter is the reverse traversal.
func (c *Chain) WSClientAdapter() MessageTransform {
    var participants []ClientMessageTransformer
    for _, instance := range c.instances { // 0 → n
        if t, ok := instance.(ClientMessageTransformer); ok {
            participants = append(participants, t)
        }
    }
    if len(participants) == 0 {
        return nil
    }
    return func(ctx context.Context, m *Message) (bool, error) {
        for _, t := range participants {
            emit, err := t.TransformClientMessage(ctx, m)
            if err != nil {
                return false, err
            }
            if !emit {
                return false, nil
            }
        }
        return true, nil
    }
}
```

## 5. Why no holding / `Finalize`

Emission-holding — the framework carrying a message the shim declined to emit now,
to release later or flush at teardown — adds exactly one capability over a stateful
shim with 1→1 + drop: a flush opportunity at teardown for content accumulated but
not yet emitted. On this transport that capability has no consumer:

- The WebSocket **turn terminal** (`response.completed` / `response.failed` /
  `error`) arrives **in-band** as a message the `ServerMessageTransformer` observes.
  A shim that accumulated state can act on it synchronously. SSE needs `Finalize`
  precisely because its teardown can be **out-of-band** (upstream death or stall
  with no terminal), stranding a mid-stream client.
- The only residual gap is a socket dying **mid-turn**, before the terminal, losing
  a shim's accumulated-but-unemitted state. Flushing a partial reconstruction to a
  client whose upstream just died is dubious value, and not worth importing the
  finalize machinery.
- The horizon consumer (item-ID stabilization) is pure in-place remapping — state
  only, zero holding. Coalescing (N→1) is expressible whenever an in-band "last"
  marker — OpenAI's per-item `response.*.done` event, or the turn terminal —
  identifies the final input: accumulate in state, drop each input, and emit the
  merged message in place of that final input. The merged emission *replaces* the
  final input; a coalesce that must also forward the final input verbatim would be
  a 1→2 split (§1 non-goals) and stays excluded. Absent such a marker, choosing the
  emission point would require holding, which this design omits.

Consequently the WS adapter is a plain per-direction fold with no finalize sweep,
and the "must be prompt and non-blocking" rule (a transform runs inline in the pump
and blocks that direction while it runs) is the only *timing* obligation on a shim.
A both-directions shim carries one further obligation — synchronizing its own
cross-direction shared state, since the two pumps run concurrently over the single
per-session instance (§2, *Cross-direction concurrency*).

## 6. Package `wsforward`: the pump seam and session wiring

### 6.1 Pump seam

`pump` gains one nil-able `shim.MessageTransform` parameter and one insertion
between read and write
([session.go](../../internal/wsforward/session.go#L107-L128)):

```go
func pump(ctx context.Context, source, destination *websocket.Conn,
    sourcePeer, destinationPeer sessionPeer, writeTimeout time.Duration,
    transform shim.MessageTransform) (pumpStats, pumpFailure) {
    var stats pumpStats
    for {
        messageType, payload, err := source.Read(ctx)
        if err != nil {
            return stats, pumpFailure{peer: sourcePeer, operation: readOperation, err: err}
        }
        if transform != nil {
            message := shim.Message{Kind: kindFromType(messageType), Data: payload}
            emit, terr := transform(ctx, &message)
            if terr != nil {
                return stats, pumpFailure{peer: sourcePeer, operation: transformOperation, err: terr}
            }
            if !emit {
                continue // dropped: read the next message, write nothing
            }
            messageType, payload = typeFromKind(message.Kind), message.Data
        }
        // ... existing write-bounded destination.Write, stats accounting ...
    }
}
```

`kindFromType` / `typeFromKind` map `websocket.MessageType` ↔ `shim.MessageKind`.
Only Text and Binary are valid, but `MessageKind` is an open integer, so `typeFromKind`
is **total**: `MessageBinary` maps to binary and every other value — including an
out-of-range kind a shim may set — maps to text, so an invalid kind can never panic
the pump. A new `transformOperation` joins the existing
`readOperation` / `writeOperation` so a transform failure is attributed to the
originating peer rather than mislabeled as a read or write.

`runSession` builds the two transforms from the per-session chain and hands each to
its pump ([session.go](../../internal/wsforward/session.go#L52-L94)):

```go
clientToUpstream := chain.WSClientAdapter() // may be nil
upstreamToClient := chain.WSServerAdapter() // may be nil
startPump(client, upstream, clientPeer, upstreamPeer, &clientToUpstreamStats, clientToUpstream)
startPump(upstream, client, upstreamPeer, clientPeer, &upstreamToClientStats, upstreamToClient)
```

When both adapters are `nil` (the no-op default), every message follows the current
verbatim path and the transport remains byte-for-byte opaque.

### 6.2 Chain construction and threading

- `wsforward.New` gains a `registry shim.Registry` parameter, defensively copied as
  `forward.New` does (`append(shim.Registry(nil), registry...)`,
  [forward.go](../../internal/forward/forward.go#L74-L75)), and stored on `Proxy`.
- `Proxy.Handler` builds the chain **once per session**, immediately after
  `websocket.Accept` succeeds and before `runSession`
  ([proxy.go](../../internal/wsforward/proxy.go#L191-L228)), using the endpoint's
  surface and upstream route:

  ```go
  chain := p.registry.NewChain(r.Context(), surface, upstream)
  ```

  Construction is synchronous and immediate, so the request context is still usable
  for the shim factories; the resulting instances then live for the whole session.
  The `TransformClientMessage` / `TransformServerMessage` calls receive the **pump**
  context (session-scoped, derived from `baseCtx`), so cancellation and shutdown
  propagate into a running transform.

  These two contexts differ in **scope**, and a shim must not confuse them: the
  construction context (`r.Context()`) carries request-scoped values — notably the
  correlation request-id installed by the logging middleware — whereas the pump
  context is rooted in `baseCtx` and carries **none** of them. A shim that needs a
  request-scoped value at transform time must therefore **capture it at
  construction** and hold it in its own struct; the transform context is for
  cancellation, not request-scoped lookup. Rooting the transform context in
  `baseCtx` rather than the request context is deliberate, so shutdown cancels a
  running transform.
- `cmd/copilotd/main.go` passes the existing `configuredShimRegistry(cfg)` instance
  to `wsforward.New` alongside the forwarder
  ([main.go](../../cmd/copilotd/main.go#L341-L349)); the same registry backs both
  transports.

### 6.3 Imports

`wsforward` adds an import of `internal/shim`. `shim` imports only `endpoint` and
`sse`, neither of which imports `wsforward`, so the dependency stays one-way
(`wsforward → shim`), exactly like `forward → shim`. No cycle.

## 7. Error handling and close codes

A transform error is **fatal to the session**. The pump returns a `pumpFailure`
carrying a `transformOperation`; `runSession` then follows the existing terminal
path ([session.go](../../internal/wsforward/session.go#L75-L94)):

- Both sockets are closed with **`1011` (internal error)**.
- The session terminal is classified `SessionError` (log level `warn`), and the
  session-terminal metric records `error`.
- `errgroup` cancellation tears down the sibling pump, as it does for any
  half-failure today.

This classification largely falls out of the existing terminal logic once
`transformOperation` is a distinct operation value
([session.go](../../internal/wsforward/session.go#L130-L173)): a transform error is
not a sendable WebSocket close status, so `terminalClose` reaches its default and
resolves to `StatusInternalError` (`1011`), and `terminalReason` classifies an
abnormal close code as `SessionError`. The one thing the new value must guarantee is
that a transform failure is **not** mistaken for an abrupt client disconnect —
`isAbruptClientDisconnect` is gated on `readOperation`, so a `transformOperation`
failure correctly bypasses it. No new close-code taxonomy and no new metric label
are introduced — a transform failure reuses the existing `error` terminal. Because a
transform runs inline in the pump, a client disconnect that races a transform is
detectable by the pump's read/write error paths but not guaranteed to be the observed
terminal. The two directions are independent goroutines meeting only at the terminal
channel, so when a disconnect and a transform error occur concurrently the session is
classified by whichever pump reports first — a benign, telemetry-only difference
(`SessionClientClosed` vs. `SessionError`). If the transform error is reported first
the disconnect may go **unobserved**; teardown is safe either way — both sockets close,
and the client, already gone, observes neither close code. No deterministic winner is required or
imposed; forcing disconnect precedence would need cross-pump coordination this design
omits, unlike the single-loop SSE pump that re-checks client cancellation at each step.

## 8. Telemetry

Unchanged in shape. Dropped messages are read but not forwarded, so the existing
directional forwarded counts (`msgs_c2u` / `msgs_u2c` and their byte siblings,
[proxy.go](../../internal/wsforward/proxy.go#L231-L250)) naturally exclude them —
consistent with the SSE result counting only written client-facing frames
([pump.go](../../internal/sse/pump.go#L36-L42)). No new counters. The stats
increment stays where it is (after a successful write), so a drop simply does not
advance the forwarded count for that direction.

Shim-facing observability — a logger and metrics emitter handed to a shim so it can
account for its own transforms and drops — and any framework dropped-message counter
are deliberately **out of scope here**. They are a separate, transport-agnostic
design, because they change the shared shim construction contract that the HTTP and
SSE shims also build through.

## 9. Testing

Table-driven tests with local `coder/websocket` echo and scripted servers for both
peers, extending the existing `wsforward` suite:

1. **No-op default is verbatim** both directions — regression guard that the seam
   is inert when no shim participates (byte-for-byte, kind-preserving).
2. **Client-only shim**: rewrites/drops client → upstream messages while
   upstream → client stays verbatim; and the mirror for a **server-only** shim.
3. **Stateful both-directions shim**: proves a single per-session instance shares
   state across turns and across directions (for example, a test remapper that
   assigns a stable tag on the way down and rewrites it on the way up).
4. **Drop semantics**: a dropped client message is never written upstream; a dropped
   server message is never delivered to the client; the forwarded count excludes it.
5. **Fold order**: two participating shims compose 0 → n on client → upstream and
   n → 0 on upstream → client; a drop short-circuits the remaining chain.
6. **Transform error → `1011`**: the session closes with internal-error, records the
   `SessionError` terminal and `error` metric, tears down the sibling pump, and
   leaks no goroutine.
7. **Kind preservation**: text and binary messages round-trip their kind through the
   transform seam.
8. **Fresh chain per socket**: two sequential sessions get independent shim state.
9. **Adapter unit tests** in `shim`: `WSClientAdapter` / `WSServerAdapter` return
   `nil` with no participants, fold in the correct order, short-circuit on drop, and
   propagate errors.
10. **Kind mutation**: a transform that flips a message's kind (Text→Binary and the
    reverse) is honored end-to-end — the destination peer observes the mutated kind,
    distinct from the kind-preservation guard above.
11. **Cancellation mid-transform**: base-context cancellation (shutdown) while a
    transform is running tears the session down cleanly — both sockets close, the
    sibling pump unwinds, and no goroutine leaks.

## 10. Documentation and scope reversal

- **`shim` package doc** ([shim.go](../../internal/shim/shim.go#L1-L14)): extend the
  contract description to cover the two WebSocket message hooks, the
  no-holding / 1→1 + drop rules, and the instance-lifetime asymmetry — a shim
  instance is per-request on the HTTP path but per-session (long-lived, multi-turn)
  on the WebSocket path, so a both-transports shim must not assume a request-scoped
  lifetime and must bound any per-turn accumulation.
- **`CONTEXT.md` glossary**: extend the **Shim** entry to note the WebSocket message
  transform (bidirectional, opt-in, no holding), and add vocabulary for the
  directional hooks and the **Message** unit (distinct from an SSE **frame**).
- **WebSocket forwarding design**
  ([2026-07-19](2026-07-19-openai-responses-websocket-forwarding-design.md#L36-L41)):
  update its "No shim / extensibility" non-goal to record that the seam now exists
  and remains opaque by default, pointing at this design.
- **ADR-0010** (new): record that the payload-opaque WebSocket transport now carries
  an opt-in bidirectional transform seam — opaque by default (no-op registry),
  interpreting a message only inside a shim that opts in — reversing ADR-0006's "no
  WebSocket message hooks" non-goal. Following the repo's amendment convention
  (ADR-0009 amends ADR-0005 / ADR-0008), **ADR-0006**
  ([0006-openai-responses-websocket-transport.md](../adr/0006-openai-responses-websocket-transport.md))
  gains a `Status: accepted; message-transform seam added by ADR-0010` line and an
  **Amendment:** back-pointer; its body is not rewritten. Until this seam is accepted
  both ADRs read `proposed` (ADR-0006's amendment marked provisionally); flipping
  ADR-0010 to `accepted` and ADR-0006's amendment to the definitive wording above is
  itself part of the shipping documentation work.

## 11. Reusable vs new

**Reused unchanged**: the WebSocket transport, accept/dial/session lifecycle,
shutdown draining, route, catalog membership, timeouts, limits, the
`coder/websocket` dependency, the `shim.Registry` / `shim.Chain` machinery, and the
per-session terminal / close-code / metric plumbing.

**New**: `shim.Message` + `shim.MessageKind`, the `ClientMessageTransformer` /
`ServerMessageTransformer` interfaces, the `MessageTransform` seam type, the
`WSClientAdapter` / `WSServerAdapter` chain adapters, the nil-able transform
parameter and `transformOperation` branch in the `wsforward` pump, per-session chain
construction in `Proxy` with a `registry` parameter on `wsforward.New`, and the
`wsforward → shim` import. The transport core stays payload-opaque; the SSE engine
and the HTTP `Forwarder` are untouched.
