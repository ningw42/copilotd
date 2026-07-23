# Responses item-id stabilization shim (opt-in, cross-transport)

Status: proposed 2026-07-23.

Design for copilotd's **first concrete shim**: an opt-in transform that stabilizes
the churning per-item `id` GitHub Copilot emits on the OpenAI Responses stream, so
`id`-keyed clients stop corrupting or crashing. It consumes the bidirectional
message-transform seam that
[ADR-0010](../adr/0010-bidirectional-websocket-message-transform-seam.md) and the
[WebSocket shim message-transform design](2026-07-22-websocket-shim-message-transform-design.md)
built and explicitly named "stabilizing OpenAI Responses item IDs" as its
anticipated first consumer, together with the existing SSE stream hook
([Phase 2 SSE streaming engine](2026-07-15-phase-2-sse-streaming-engine-design.md))
and the Phase 3 shim framework
([Phase 3 middleware framework](2026-07-16-phase-3-middleware-framework-design.md)).

It also makes one small, additive **framework** extension — a declarative `Scope`
predicate on a shim registration (§6) — so the shim participates only on the one
transport pair it concerns, and every other surface keeps its byte-verbatim fast
path even when the shim is enabled.

## 1. Problem

For GPT models, copilotd forwards Copilot's native `/responses` transport nearly
verbatim on both the HTTP/SSE path and the WebSocket path. Copilot emits a
**different, ~408-char opaque `id` on every event for the same streamed item** —
across `response.output_item.added` → the intervening
`content_part.*` / `output_text.*` / `function_call_arguments.*` /
`reasoning_summary_*` deltas → `response.output_item.done`, and again in the
terminal `response.completed` snapshot's `output[]`. This holds for both reasoning
and message items. The only stable per-item correlation key upstream is
`output_index`, not `id`.

The OpenAI Responses streaming contract treats an item's `id` as **fixed for its
whole lifecycle**: established on `response.output_item.added` (`item.id`),
reasserted on `response.output_item.done`, and back-referenced by every intervening
per-item event through `item_id`. A stream that changes an item's `id` between its
own events is malformed against that contract.

The divergence is invisible to **position-keyed** clients (OpenAI's `codex` parses
the complete item off `response.output_item.done` and keys reasoning summaries by
`summary_index`, never using the churning `id` as a lookup key) and fatal to
**`id`-keyed** clients (the Vercel AI SDK `@ai-sdk/openai`, e.g. via opencode,
builds an `activeReasoning` map keyed on the `added` id and dereferences it on later
events with a non-null assertion; the id mismatch makes the lookup miss and throws
on the first GPT-5 message). The correct, client-agnostic fix is to make the id
stable — pin one genuine upstream id per `output_index` and rewrite the later
events on that index to it.

### 1.1 Load-bearing assumptions

The remap rests on three premises. They are called out explicitly because a
violation of any of them produces **well-formed-but-wrong** output, not a decode
failure — so the fail-safe posture (§7) does not catch it, and only these premises
and their capture-backed tests (§8) do.

1. **`output_index` is stable for an item while its `id` churns.** *Empirical,
   Copilot-specific.* The OpenAI contract makes the *id* the stable per-item key;
   Copilot breaks that, and this design bets `output_index` is the surviving anchor.
   Nothing in the OpenAI spec guarantees it — it is an observation, and the one
   premise a real capture (§8) must confirm. **If it is false, this design is
   invalidated, not patched.**
2. **In a snapshot's `output[]`, array position `i` equals `output_index` `i`.**
   *OpenAI-contract-grounded.* `output_index` on a streaming event is *defined* as
   that item's index into the final `output[]`, so the terminal rewrite (§4 step 3)
   may key on array position.
3. **WebSocket turns are strictly sequential** — one active response per session, so
   `output_index` restarts at 0 each turn and the per-turn reset (§5.2) is safe.
   *OpenAI-contract-grounded:* the Responses protocol runs one response at a time per
   session, with no concurrent-response pipelining.

No runtime guard is added for any of them: none has a sound cheap check, and a guard
that cannot reliably detect the violation is worse than an honest documented premise.
The evidence is instead a **real captured** churning stream seeded into the tests
(§8), so a future Copilot behavior change surfaces as a fixture diff.

## 2. Goal and scope

Ship one concrete shim, `responses-item-id-stabilizer`, that — **when enabled** —
pins the first-seen genuine upstream id per `output_index` and rewrites the later
id-bearing events on that index to it, on **both** `/responses` transports, using
the **one** `shim.Registry` / `shim.Chain` that already backs the HTTP request,
prelude, buffered body, SSE stream, and WebSocket message hooks. It participates
only on the OpenAI `/responses` endpoint — declared through a `Scope` predicate
(§6) — so on every other endpoint, and whenever the shim is disabled (the default),
both transports stay byte-for-byte verbatim.

### In scope

- A concrete shim implementing `shim.EventTransformer` (HTTP/SSE) and
  `shim.ServerMessageTransformer` (WebSocket upstream → client), sharing one remap
  core, in `internal/shim`.
- A per-instance `output_index → pinned id` map, reset at each turn terminal.
- A declarative `Scope` predicate on `shim.Registration` — a small, additive
  framework extension — so the shim is constructed only for the OpenAI `/responses`
  endpoint and untouched surfaces keep their fast path even when it is enabled.
- One config field + one CLI flag + one canonical-registry registration, off by
  default, threaded through the existing `configuredShimRegistry(cfg)` seam that
  already feeds both `forward.New` and `wsforward.New`.
- Documentation: a `CONTEXT.md` glossary update (the divergence taxonomy + this
  shim), a new **`docs/divergence-ledger.md`**, a new **ADR-0011**, and this design
  doc.
- Tests mirroring the SSE and WS transform suites, plus `Scope`-gating and
  config/CLI coverage.

### Non-goals (YAGNI)

- **No `BufferedTransformer`.** A non-streaming `/responses` response is a single
  JSON snapshot in which each item id appears exactly once — nothing churns. The
  buffered path stays untouched and byte-verbatim.
- **No `ClientMessageTransformer`.** The churn is upstream → client only; client
  (`response.create`) messages carry none of these ids.
- **No `StreamFinalizer` / no holding.** The transform is pure in-place remapping;
  it never holds an emission, so it needs none of the SSE finalize machinery.
- **No synthetic ids.** Only a genuine first-seen upstream id is ever pinned or
  reused; no id is minted. The transform therefore stays within the `shim` package
  policy invariant ("may alter … information derived from an upstream response, but
  must not fabricate information without an upstream basis") — it is an **Alteration**,
  never a **Fabrication** (§9).
- **No change to `encrypted_content`, `content`, `summary`, `summary_index`, or
  `call_id`.** Cross-turn reasoning persistence rides in `encrypted_content`;
  tool-call correlation rides in `call_id`; both are left exactly as upstream sent
  them.
- **No new transport, route, catalog, or dependency.** The remap core uses only
  `encoding/json` and the existing `sse.Frame` / `shim.Message` carriers; the sole
  framework change is the additive `Scope` field on `shim.Registration`.
- **No on-by-default behavior.** With the canonical registry's default the shim is
  disabled and both transports keep their payload-opaque fast path.

## 3. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Transports | **Both SSE and WebSocket**, one shim | The parity gap exists on both `/responses` transports; ADR-0010 built the cross-transport seam precisely so one shim closes it once. |
| Hooks | **`EventTransformer` (SSE) + `ServerMessageTransformer` (WS)** | The two transports the churn appears on; the WS server direction is upstream → client, matching the churn direction. |
| Buffered / client / finalize hooks | **None** | No churn in a single snapshot; no ids on client messages; pure in-place remap holds nothing. |
| Endpoint scope | **Declarative `Scope` predicate**, route-gated to `(OpenAI, /responses)` | Enabling the shim must not pull unrelated surfaces (e.g. Anthropic SSE) off their byte-verbatim fast path; the framework enforces the scope uniformly instead of each factory branching (§6). |
| State | **`map[int]string`, `output_index → pinned id`**, reset per turn terminal | The only stable correlation key upstream is `output_index`; per-request on HTTP and per-session-but-per-turn-reset on WS. |
| Pin source | **First-seen genuine upstream id for an `output_index`** | In practice the `output_item.added` id — exactly the id `id`-keyed clients store; no synthetic id, so within policy. |
| Fields rewritten | **Every id surface present** (`item.id` and/or `item_id`) + terminal `response.output[i].id` by array position | The id surfaces per the OpenAI contract; rewriting all present surfaces (not one) closes a silent gap if an event ever carried both; keyed structurally on `output_index`, not an event-type allowlist, so future id-bearing events are covered. |
| Fidelity | **Targeted rewrite** — decode top level to `map[string]json.RawMessage`, touch only id-bearing sub-objects | Every untouched field keeps its exact value; object key order within a rewritten event may reshuffle (semantically neutral JSON). |
| Malformed input | **Forward verbatim, never error** | Matches the payload-opaque ethos; critical on WS, where a returned error is fatal to the session (1011). |
| Default | **Off**, opt-in | Preserves byte-for-byte transparent forwarding; the first **Alteration** entry in the divergence ledger (§9), enabled per deployment for spec-strict clients. |
| Packaging | **In package `shim`**, new file `responses_item_id.go` (`responsesItemIDStabilizer`) | Mirrors `NopShim`; lets `CanonicalRegistry()` reference the factory with no import cycle (remap core needs only `encoding/json` + `sse`). |

## 4. The remap core

A single transport-neutral function does the real work:

```go
// rewrite returns payload with item ids stabilized per output_index, or the input
// unchanged when the payload is not an id-bearing Responses event or cannot be
// parsed. It never returns an error: a payload it does not understand is forwarded
// verbatim.
func (s *responsesItemIDStabilizer) rewrite(payload []byte) []byte
```

Algorithm, given one event's JSON `payload`:

1. Decode the top level into `map[string]json.RawMessage`. On any decode failure,
   **return `payload` unchanged** (fail-safe). Read the `type` string (used only for
   the turn-reset test in step 4; the rewrite itself is structural).
2. **Per-item branch** — the event has a top-level `output_index`:
   - Decode `output_index` (an int) and locate **every id surface present**:
     `item.id` when an `item` object is present, and/or a top-level `item_id`. On a
     per-item event these all refer to the same item at that `output_index`.
   - If `output_index` is **not yet** in the map, record `map[output_index] = <the
     first-seen surface's id>` (first-seen pin; `item.id` preferred when both are
     present) and leave the bytes unchanged — the first sighting is already the
     genuine id.
   - If `output_index` **is** in the map, overwrite **each** present id surface with
     the pinned value and re-marshal.
3. **Envelope-snapshot branch** — no top-level `output_index`, but the event carries
   a `response` object with an `output` array (empty or partial on
   `response.created` / `response.in_progress`, full on the terminals): for each
   `output[i]`, if `map[i]` exists, overwrite `output[i].id` with it (array position
   `i` = `output_index`); otherwise leave it. Re-marshal. An empty array makes this a
   no-op. **The envelope branch never pins** — it only applies existing pins —
   because pre-item envelopes carry an empty `output[]` in practice, and the
   canonical id should be the per-item lifecycle-start (`added`) id that `id`-keyed
   clients store; letting an envelope pin first could canonicalize to a non-`added`
   id. An item that appears *only* in a terminal `output[]` (no prior per-item event)
   therefore has no pin and is left unchanged — but such an id appears once and never
   churns, so there is nothing to stabilize.
4. **Turn reset** — if `type` is a turn terminal (`response.completed` /
   `response.failed` / `response.incomplete`, or an upstream `error`), **clear the
   map** after any rewrite above. This is gated on the terminal *type*, not on the
   presence of an `output` array, so the pre-item `response.created` /
   `response.in_progress` envelopes never reset a turn's pins. It bounds per-session
   state to one turn on the WS transport (§5.2); on the per-request HTTP path it is
   harmless.
5. Return the payload — the re-marshaled bytes if step 2 or 3 changed it, otherwise
   the original input bytes.

Steps 2 and 3 are mutually exclusive: a per-item event carries a top-level
`output_index` and no `response` snapshot; an envelope event carries the `response`
snapshot and no top-level `output_index`. An `error` terminal matches neither
rewrite branch but still triggers the step-4 reset.

Targeted rewrite keeps fidelity high: only the `item` / `output[i]` sub-objects and
the `item_id` scalar are re-encoded; siblings such as `encrypted_content`,
`content`, `summary`, `summary_index`, and `call_id` retain their exact
`json.RawMessage` bytes. The remap never inspects or copies those fields.

The pin is structural, not an event-type allowlist: it keys on the presence of
`output_index` (per-item events) or `response.output[]` (the terminal), so
reasoning, message, function-call, and reasoning-summary items — and any future
id-bearing per-item event — are covered without enumeration.

## 5. Transport integration

The two hooks are thin adapters over the shared `rewrite` core; only the framing
differs.

### 5.1 SSE — `EventTransformer`

`TransformEvent(ctx, sse.Frame) ([]sse.Frame, error)` receives a frame whose `Raw`
is the exact bytes to re-emit (its `event:` line, `data:` line(s), and terminating
blank line); `Type` is advisory. The adapter:

1. Extracts the `data:` payload from `frame.Raw` (concatenating multiple `data:`
   lines with `\n`, matching the reader's own `data.type` extraction). A frame with
   no `data:` line is returned unchanged.
2. Calls `rewrite(payload)`. If the result is byte-identical, returns
   `[]sse.Frame{frame}` (the original bytes, untouched — so a verbatim frame keeps
   its exact multi-`data:` framing).
3. Otherwise reconstructs `Raw` by replacing only the `data:` payload, preserving
   the `event:` line, any other lines, the line endings, and the terminating blank
   line, and returns the single reframed `sse.Frame` with the unchanged `Type`.

It never returns a non-nil error: a Responses SSE frame is either rewritten or
forwarded verbatim. This keeps the shim off the pump's shim-error path entirely.

### 5.2 WebSocket — `ServerMessageTransformer`

`TransformServerMessage(ctx, *shim.Message) (emit bool, err error)` receives a
message whose `Data` **is** the JSON payload (no SSE framing). The adapter sets
`msg.Data = rewrite(msg.Data)` in place and returns `(true, nil)` — it never drops
and never errors, so it can never close a session with 1011. The `rewrite` core
reads the event type from the payload's own `"type"` field, so the same core serves
both transports unchanged (on SSE the type is also on the frame's `event:` line, but
the core does not depend on that).

**Per-turn reset.** On the WS transport a `shim.Chain` instance is **per session**,
and `output_index` restarts at 0 on every turn. The map is therefore cleared at each
turn terminal (step 4 of §4, after the terminal snapshot is rewritten from the
still-populated map). On the HTTP path an instance is per request (one turn), so the
same clear is simply harmless — the stream ends immediately after. One code path
serves both.

**No synchronization.** Because only the server (upstream → client) WS direction is
implemented, the per-session map is touched by exactly one pump goroutine. The
cross-direction concurrency the transform-seam design warns about does not arise;
no mutex is introduced. (Adding a client-direction hook later would reintroduce it
and require revisiting this.)

## 6. Config, flag, scope, and registry wiring

Following the `ShimNopEnabled` pattern
([config.go](../../internal/config/config.go), [main.go](../../cmd/copilotd/main.go)),
with the toggle names deriving mechanically from the registration name by the rule
`flag = --shim-<name>-enabled`, `config = Shim<Name>Enabled`:

- **Config field.** `ServeConfig.ShimResponsesItemIDStabilizerEnabled bool`, with
  `defaultShimResponsesItemIDStabilizerEnabled = false`. It threads through
  `RegisterServe` (a `fs.BoolLongDefault`), the `Resolve` default block, and the env
  and TOML layers, exactly like `shim-nop-enabled`. An old TOML file lacking the key
  loads as `false` (backward-compatible default), never rejected. It is logged in the
  `ServeConfig` `LogValue` alongside `shim-nop-enabled`.
- **CLI flag.** `--shim-responses-item-id-stabilizer-enabled` (bool, default
  `false`), help text "stabilize churning OpenAI Responses item ids (opt-in)". The
  behavior lives in the help text; the flag *name* stays on the predictable
  `--shim-<name>-enabled` rule.
- **Declarative endpoint scope (framework extension).** `shim.Registration` gains one
  additive field:

  ```go
  type Registration struct {
      Name    string
      Enabled bool
      Scope   func(endpoint.Surface, endpoint.Route) bool // nil ⇒ every endpoint
      New     func(context.Context, endpoint.Surface, endpoint.Route) any
  }
  ```

  `NewChain` gates construction on it, so an out-of-scope shim is never instantiated
  (no no-op placeholder in the chain, and the stream/message adapters stay `nil` on
  untouched endpoints):

  ```go
  if registration.Enabled && (registration.Scope == nil || registration.Scope(surface, route)) {
      chain.instances = append(chain.instances, registration.New(ctx, surface, route))
  }
  ```

  A `nil` `Scope` means "every endpoint," so `nop` and every existing registration are
  unaffected. The predicate keys on **both** `Surface` and `Route` because a `Route`
  value is not globally unique (`CONTEXT.md`): `/responses` is OpenAI-only today, but
  the scope is written `s == endpoint.OpenAI && r == endpoint.RouteOpenAIResponses`
  so it stays correct if a later surface reuses the path.
- **Registry.** `shim.CanonicalRegistry()` gains a second registration after `nop`:

  ```go
  {
      Name:    "responses-item-id-stabilizer",
      Enabled: false,
      Scope: func(s endpoint.Surface, r endpoint.Route) bool {
          return s == endpoint.OpenAI && r == endpoint.RouteOpenAIResponses
      },
      New: func(context.Context, endpoint.Surface, endpoint.Route) any {
          return newResponsesItemIDStabilizer()
      },
  }
  ```

  `configuredShimRegistry(cfg)` gains a `case "responses-item-id-stabilizer"` that
  sets `Enabled = cfg.ShimResponsesItemIDStabilizerEnabled`. The resulting registry
  already flows into **both** `forward.New` and `wsforward.New`, so the single toggle
  governs both transports with no additional wiring, and the `Scope` predicate
  confines participation to the OpenAI `/responses` endpoint on each. The shim's name
  appears in the existing `logShimChain` "configured shim chain" line when enabled.

Registration order (`nop` then `responses-item-id-stabilizer`) is onion order; with
only this shim participating in the stream/message folds, ordering relative to `nop`
(which implements no hook) is immaterial.

## 7. Fail-safe and error posture

The shim's defining invariant is **it never faults a stream or session**. Every path
that cannot confidently rewrite forwards the input verbatim:

- A JSON decode failure at any level → return the original bytes.
- A frame/message with no `output_index` and no `response.output[]` → unchanged.
- A terminal `output[i]` whose index was never seen → left as upstream sent it.

Consequently the SSE `EventTransformer` returns `error == nil` on every input (it
never trips `OutcomeShimError`), and the WS `ServerMessageTransformer` returns
`(true, nil)` on every input (it never triggers the seam's fatal-`1011` path). The
only observable effect of enabling the shim is stabilized ids; a payload it does not
understand is indistinguishable from the disabled path.

Note the fail-safe protects against **malformed** input, not against the §1.1
premises being wrong: a violated premise yields well-formed-but-wrong output that no
cheap guard can distinguish, which is why those premises are documented and
capture-tested rather than guarded.

## 8. Testing

Extends the existing `internal/shim`, `internal/forward`, `internal/wsforward`, and
config suites. The remap fixtures are seeded from a **real captured** churning
`/responses` stream (§1.1; capturing it is the first implementation step), so the
golden data doubles as the evidence for assumption #1 and any future Copilot change
surfaces as a fixture diff.

1. **Remap core (unit).** Table-driven over captured event shapes: `added` pins,
   `done`/deltas rewrite to the pinned id, terminal `output[]` rewrites by position;
   **every id surface present is rewritten** (an event carrying both `item.id` and
   `item_id` stabilizes both); `encrypted_content` / `content` / `summary` /
   `summary_index` / `call_id` are byte-preserved; unknown / `output_index`-less /
   undecodable payloads pass through unchanged.
2. **Envelope pins nothing.** An `output_index` first seen in a `response.created` /
   `response.in_progress` envelope is left un-stabilized, and the later
   `response.output_item.added` establishes the pin (guards the apply-only envelope
   rule, §4 step 3).
3. **SSE (`EventTransformer`).** A mock upstream whose item ids churn per event
   yields exactly one stable id per `output_index` across `added`/`done`/deltas and
   in the terminal `output[]`; the `event:` line and framing are preserved; a
   `data:`-less frame is untouched.
4. **WS (`ServerMessageTransformer`).** The same assertions through the `wsforward`
   server-message seam, **plus multi-turn**: turn 2's `output_index 0` pins a fresh
   id rather than reusing turn 1's, proving the per-turn reset over a single
   per-session instance.
5. **Endpoint scope.** With the shim **enabled**, a request on a non-`/responses`
   endpoint (e.g. Anthropic `/v1/messages` SSE) keeps the byte-verbatim fast path —
   the chain constructs no stabilizer there and `StreamAdapter()` /
   `WSServerAdapter()` stay `nil`. A `NewChain` unit test asserts a registration
   whose `Scope` excludes an endpoint is not instantiated for it.
6. **Gate.** Flag off → byte-for-byte verbatim on both transports (regression guard
   that the seam is inert); flag on → normalized on OpenAI `/responses`.
7. **Fail-safe.** Malformed / `data:`-less / `output_index`-less payloads forward
   untouched and produce **no** error on either transport — asserting a WS session is
   never classified `SessionError`/closed `1011` by this shim.
8. **Config/CLI.** The flag threads flags > env > TOML > default to the registry, and
   a TOML file omitting the key loads `false`; the shim appears in the enabled
   shim-chain log only when on. Mirrors the `shim-nop-enabled` coverage in the
   config/supervisor suites.

## 9. Documentation

- **`CONTEXT.md` glossary.** Introduce the divergence taxonomy and place this shim in
  it:
  - Add **Divergence ledger** (the complete accounting of every way copilotd's wire
    output departs from verbatim forwarding — in two kinds today, Fabrication and
    Alteration — identified off-band, never by a wire field; enumerated in
    `docs/divergence-ledger.md`), **Fabrication** (information copilotd puts on the
    wire with no upstream basis), and **Alteration** (an upstream-basis value
    rewritten to another upstream-basis value, fabricating nothing). Note **Omission**
    as the anticipated third kind, created only when a shipped divergence first drops
    content.
  - **Amend "copilotd-originated signal"** to be the Fabrication kind — drop the "the
    proxy's *only* divergence" clause; it is the only divergence that *fabricates* on
    the wire, while an Alteration may rewrite upstream data without fabricating.
  - Add **Responses item-id stabilizer** (Streaming): a shim-owned, opt-in transform
    that pins one genuine upstream id per `output_index` and rewrites later id-bearing
    events to it, on both `/responses` transports. It is an **Alteration**, **not** a
    copilotd-originated signal: no id is minted, so the wire still carries only
    upstream-basis values.
- **`docs/divergence-ledger.md`** (new): the categorized enumeration — Fabrication
  (the copilotd-originated error signals and synthesized stream terminals), Alteration
  (the Responses item-id stabilizer), Omission (none yet). Each entry points at its
  authoritative source (an `apierror.Kind`; ADR-0003; the shim's registry name +
  flag) plus a one-line "what it diverges," so exhaustiveness holds by construction
  rather than by vigilance.
- **ADR-0011** (new): record the **policy** only — copilotd will, opt-in and off by
  default, stabilize churning Responses item ids to a genuine upstream id, across the
  SSE and WebSocket transports, touching only item-id fields. It is the first
  **Alteration** divergence and preserves the never-fabricate-without-upstream-basis
  invariant. ADR-0010 accepted the *seam*; ADR-0011 accepts the
  *divergence-from-verbatim policy* the seam now carries. It references ADR-0002
  (payload-opaque SSE), ADR-0003 (off-band origin of copilotd's own signals),
  ADR-0006/0010 (WebSocket transport and seam), and this design. The
  Fabrication/Alteration taxonomy is defined in `CONTEXT.md` + the ledger; ADR-0011
  *uses* it, it does not define it.
- **`shim` package doc**: note the additive `Scope` predicate on `Registration`
  (`nil ⇒ every endpoint`) and the stabilizer. No ADR — the field is additive and
  self-documenting.
- **This design doc.**

## 10. Reusable vs new

**Reused unchanged**: the SSE engine and its `EventTransformer` seam, the WebSocket
transport and its `ServerMessageTransformer` seam, `shim.Registry` / `shim.Chain`,
`configuredShimRegistry` / `logShimChain`, the config flag/env/TOML plumbing, and the
`forward.New` / `wsforward.New` registry threading — all already in place.

**New**: the `responsesItemIDStabilizer` shim and its `rewrite` core (`internal/shim/
responses_item_id.go` + test); the additive `Scope` predicate on `shim.Registration`
and its `NewChain` gating; the second `CanonicalRegistry()` registration and its
`configuredShimRegistry` case; the `ShimResponsesItemIDStabilizerEnabled` config
field with its flag/env/TOML/default and `LogValue` line; the `CONTEXT.md` taxonomy
update; `docs/divergence-ledger.md`; and ADR-0011. No new transport, route, catalog,
or dependency.
