# Phase 2 — SSE streaming engine, both surfaces — Design

Status: approved design (refined via brainstorming + grilling sessions), pending implementation plan
Date: 2026-07-15
Roadmap reference: `ROADMAP.md` §7 "Phase 2 — SSE streaming engine, both surfaces"
Builds on: `docs/design/2026-07-14-phase-1-core-forward-path-design.md`

## 1. Goal & outcome

Phase 1 made the first real call: non-streaming JSON round-trips end to end
against GitHub Copilot on both surfaces, and a `stream:true` request is cleanly
*rejected*. Phase 2 removes that rejection and makes copilotd genuinely usable
with real streaming clients (Claude Code, Codex, the official SDKs).

**Outcome:** a `stream:true` request on either surface —
`POST /anthropic/v1/messages` and `POST /openai/v1/responses` — streams GitHub
Copilot's Server-Sent Events (SSE) back to the client frame by frame, flushed as
they arrive, with keepalive during idle gaps, a guaranteed terminal event even
when the upstream dies mid-stream, and client-disconnect propagated back to
Copilot so a stream nobody is reading stops being generated.

The engine is **frame-aware but payload-opaque**: it splits the upstream byte
stream into SSE frames and classifies each frame by its **`event:` line** —
falling back to a minimal decode of only the `data` JSON `type` field when that
line is absent — then re-emits the original frame bytes verbatim. It never
deserializes the event payload on the hot path, so unknown fields and unknown
event types (both APIs warn these will appear) survive untouched — faithful to
raw-passthrough principle #1. That minimal identification is also the seam a
later per-event middleware plugs into (Phase 3): the engine already knows *which*
event each frame is, so a future transform pays the full
unmarshal/mutate/re-marshal cost only for the specific events it touches, and
nothing pays it today.

### 1.1 The onion, this phase

```
 client ◄──────────── SSE frames (flushed per frame) ──────────── copilotd ◄──── text/event-stream ──── GitHub Copilot
   ▲                                                                 │  reader goroutine → frameCh
   │   keepalive (OpenAI: our : comment ; Anthropic: upstream ping)  │  main loop: frame | keepalive | stall | ctx-done | reader-done
   │   terminal always delivered (upstream's, or ours synthesized)   │  writes verbatim + Flush ; detects terminal ; cancels+joins on disconnect
```

The streaming engine underpins the forwarder's response half. Middleware
(Phase 3) will nest *inside* this loop as a frame transformer; this phase builds
the loop and the framing, with no transformer yet.

## 2. Scope

**In scope (Phase 2):**

- **SSE streaming engine** (`internal/sse`, new): a frame reader (split on the
  blank-line boundary; `event:`-line-first classification with a `data.type`
  fallback), a frame writer (emit verbatim + flush), and the pump loop that
  copies upstream → downstream while owning keepalive, the idle/stall timeout,
  per-write deadlines, terminal-event detection/enforcement, and client-cancel
  propagation with a guaranteed no-leak lifecycle.
- **Forward-path branch** (`internal/forward`): after the upstream call returns,
  branch on the *response* `Content-Type` — `text/event-stream` → the pump;
  anything else → the buffered copy-back. Drop the `stream:true` reject; keep the
  OpenAI `background:true` reject.
- **Buffered-path hardening**: the Phase 1 buffered path gains the same per-write
  deadline (a wedged client that stops reading blocks a downstream write, which
  `r.Context()` cannot detect). This closes a limitation inherited from Phase 1,
  in the same phase, via one shared mechanism.
- **In-band SSE error renderer** (`internal/apierror`): `WriteStreamError`, so the
  streaming path never hand-rolls an error frame. `apierror` becomes the single
  home of every copilotd-originated signal — HTTP and SSE alike (§7).
- **Timeout model evolution**: a time-to-first-byte bound (`ResponseHeaderTimeout`)
  for both paths; the existing `--outbound-timeout` narrowed to the buffered path;
  an idle/stall timeout governing streams instead of a total-duration cap; a
  shared `--write-timeout` bounding each downstream write on both paths.
- **Keepalive**: forward Anthropic's upstream `ping` events verbatim (inject
  nothing); inject a surface-agnostic SSE comment on the OpenAI path during idle
  gaps.
- **Observability**: an event-level fallback-fired counter (sse layer) and a
  request-level stream terminal-outcome fed into the existing access log, within
  Phase 0's redaction discipline.
- TDD unit + integration + end-to-end streaming tests.

**Out of scope (deferred — see §11):**

- **OpenAI async / background mode** (`background:true`). It stays *rejected*
  exactly as in Phase 1. Background mode is gated on the Responses management
  sub-paths (`GET/DELETE /responses/{id}`, `cancel`, `input_items`) — which are
  not mounted — and on the upstream persisting the response (`store=true`), which
  reverse-engineering of Copilot gives no evidence for. It is not a
  streaming-engine concern; it lands with the management sub-paths (Phase 4), and
  only if Copilot supports it upstream.
- The **middleware / onion framework** and any per-event stream transformer
  (Phase 3). This design leaves the seam (each frame's identified `type`) but
  builds no transform hook.
- Every seed shim, including **stable Responses item-ids** (Phase 4). Copilot's
  per-event `id` churn is visible in the stream but is not corrected here.
- Response **reconstruction / accumulation**, SSE **resumability**
  (`starting_after`), or any cross-frame state beyond the single `sawTerminal`
  bit the pump needs.
- **Per-event logging** beyond the fallback-fired counter — the sse layer is
  pre-positioned to own it, but it is YAGNI-deferred.

## 3. Guiding decisions & rationale

| Decision | Choice | Rationale |
| --- | --- | --- |
| Engine depth | Frame-aware, payload-opaque: parse SSE frames, classify by the `event:` line, re-emit original bytes | Pays for identification and nothing beyond it. Verbatim re-emit keeps unknown fields/new event types intact (principle #1). The identified `type` is exactly the discriminator terminal-detection needs now and a Phase 3 per-event transform will need later. (ADR-0002.) |
| Event identification | `event:`-line-first (zero JSON parse); minimal `data.type` decode only when the line is absent/empty; a fallback-fired metric | Grounded: Anthropic **normatively guarantees** the `event:` line and name==data.type; OpenAI Responses shows it in examples only; the SSE standard makes it optional (absent → default `message`). Reading the line avoids parsing multi-KB payloads just to learn the type. The fallback hedges the non-normative surfaces; the metric turns a Copilot regression that drops the line into a signal, not a silent misclassification. |
| Stream vs. buffered branch | Branch on the upstream **response** `Content-Type`, not the request `stream` flag | The response is ground truth: an upstream error to a `stream:true` request comes back as JSON and correctly takes the buffered path, where it can still surface a real 502. |
| Flush cadence | Flush after every frame, keepalive, and synthesized error, via `http.ResponseController` | SSE requires per-event delivery. `ResponseController` reaches the socket through the existing `statusWriter.Unwrap()`; HTTP/1.1 chunked framing is provided by the Go server once `Content-Length` is absent. |
| Timeout model | TTFB bound (`ResponseHeaderTimeout`) + idle/stall for streams; total cap kept for buffered only; a per-write deadline on both | A total-duration cap guillotines a legitimately long generation. TTFB catches a dead upstream before commit; idle/stall catches one that dies mid-stream; the per-write deadline catches a client that stops draining; the buffered path keeps a total backstop because a synchronous completion should finish within a bound. |
| Stall semantics | A stopwatch over `[end-of-write(prev) → receipt(next)]` only: paused while we write, never reset by keepalive | Stall must mean **upstream** silence, not client slowness. Excluding our own write time stops a slow-but-progressing client from tripping a false "dead upstream"; it fully decouples upstream-health (stall) from client-health (the write deadline). Anthropic's pings are real frames, so they reset stall for free — a healthy Anthropic stream never false-stalls. |
| Wedged-client detection | One shared `--write-timeout` per-write deadline on both paths; exceed ⇒ `client_cancel` | A client that holds the connection open but stops reading does **not** cancel `r.Context()` (net/http only detects a *closed* connection), so a write blocks on TCP backpressure forever. A per-write deadline is the only thing that catches it. Applied to both paths because the concept is identical. |
| Reader lifecycle | Cancel-then-join (structured concurrency) | On any early exit the pump cancels the upstream context and closes the body — unblocking the reader at both block points — then waits for it to exit. "Pump returned" ⇒ "reader gone, upstream connection released": no goroutine/connection leak, no continued draining of Copilot, and the property is `-race`-assertable. |
| Keepalive ownership | Forward Anthropic pings; inject an SSE comment on OpenAI idle | Anthropic defines `ping` as a first-class server-sent event — the origin (Copilot) sends it and we forward it, so ours would be redundant. OpenAI's taxonomy has no ping, so the client hop (which copilotd owns) needs our own comment during idle. A `:` comment is valid on both surfaces and ignored by both, perturbing neither Anthropic's event flow nor OpenAI's `sequence_number` accounting. |
| Terminal enforcement | Before commit → HTTP 502/504; after commit → in-band synthesized `error` event | Once `200 text/event-stream` is flushed the HTTP status is locked, so a mid-stream failure can only be signalled in-band. Synthesizing a terminal `error` event stops the client's SSE parser from hanging forever waiting for `message_stop` / `response.completed`. |
| Synthesized event shape | Bare `error` event on both surfaces; native enum types; no fabricated envelope | The Anthropic spec allows mid-stream `error` events; the Responses taxonomy has a bare `error` event. Choosing it over OpenAI's `response.failed` avoids fabricating a full `Response` envelope — the engine invents nothing beyond a message string. |
| Origin marking | Off-band: `copilotd:` message prefix + `X-Request-Id` header + logs/metrics; wire shape stays native | copilotd-originated signals are our only divergence from a first-party endpoint. Marking origin with a nonstandard wire field would risk a strict-parse client and break the "looks native" promise; the request-id gives operators an authoritative origin channel without polluting the shape. (ADR-0003.) |
| New package | `internal/sse`, Copilot-agnostic and surface-parameterized | The framing/pump mechanics are a distinct concern from the dumb forwarder and the credential seam; isolating them keeps `forward` focused and gives Phase 3 a clean place to insert a transformer. |
| New dependencies | None beyond the Phase 1 set | Frame scanning, timers, and flushing are all stdlib (`bufio`, `time`, `net/http`). |

## 4. Module layout & package boundaries

Extending Phase 1's conventions — small, single-purpose, dependency-injected units.

```
copilotd/
└── internal/
    ├── sse/        [NEW]  Reader (frame split + event-line/data-type classify) · deadline-bounded Writer ·
    │                      Pump (select loop: frame | keepalive | stall | ctx-done | reader-done ; cancel-then-join) ·
    │                      Outcome (clean | synthesized | stall | client_cancel | upstream_error) · fallback-fired counter
    ├── forward/    [CHG]  peek drops `stream`; Content-Type branch → sse.Pump or buffered copy-back;
    │                      both paths write through the deadline-bounded writer; ResponseHeaderTimeout on the transport;
    │                      --outbound-timeout scoped to buffered; stashes the stream Outcome on a context holder
    ├── apierror/   [CHG]  + WriteStreamError(w, surface, reason); StreamUnsupported reject retired
    ├── config/     [CHG]  + stream-idle-timeout, stream-keepalive-interval, response-header-timeout, write-timeout
    └── server/     [CHG]  accessLog reads the stream Outcome from the context holder → one enriched line + outcome metric
```

Each changed/new unit — *what it does · how it is used · what it depends on*:

- **`internal/sse`** [NEW] — the mechanics. A `Reader` splits an `io.Reader` into
  SSE frames and classifies each (event line first, `data.type` fallback); a
  deadline-bounded `Writer` emits a frame's raw bytes and flushes; `Pump` runs the
  copy loop with cancel-then-join lifecycle, parameterized by the surface's policy
  (terminal predicate, keepalive interval — `0` disables it, synthesized-error
  renderer, write timeout, injected clock). It emits its own event-level signal
  (the fallback-fired counter) and returns an `Outcome`. Used by `forward`. Depends
  on `net/http`, `bufio`, `time`, `context`. Knows nothing about credentials,
  routing, or how the payload is shaped beyond the `type`.

- **`internal/forward`** [CHG] — the peek no longer inspects `stream` (Anthropic
  peeks nothing; OpenAI still peeks `background`). After `Do()`, it branches on the
  response `Content-Type` and either copies status+headers and hands the body to
  `sse.Pump`, or takes the buffered path; **both** write to the client through the
  deadline-bounded writer. It owns the timeout plumbing (below) and stashes the
  stream `Outcome` on a per-request context holder for `accessLog`. Depends
  additionally on `sse`.

- **`internal/apierror`** [CHG] — gains `WriteStreamError(w, surface, reason)`,
  which writes one `event: error` frame in the surface's dialect and flushes. It
  is now the single definition of every proxy-originated signal, HTTP and SSE, so
  no other package hand-rolls one. Still a leaf (`net/http`, `encoding/json`).

- **`internal/server`** [CHG] — the existing `accessLog` middleware reads the
  stream `Outcome` from the context holder after the handler returns, adds it to
  its single per-request line, and emits the outcome metric by surface (§9).

**Key boundaries:** `sse` is surface-parameterized but Copilot-agnostic and
payload-opaque; `forward` chooses the path and supplies policy; `apierror` holds
the complete divergence surface; `server` owns request-level observability.

## 5. The SSE engine (`internal/sse`)

### 5.1 Frame model & identification

An SSE frame is the block of lines up to (and including) the terminating blank
line. The `Reader` accumulates a frame's raw bytes and classifies it:

1. **Fast path:** read the SSE `event:` line — an O(1) slice, no JSON parsed. This
   is the only path exercised on the Anthropic surface, which normatively
   guarantees the line.
2. **Fallback:** only when the `event:` line is absent or empty, decode the frame's
   `data:` payload just far enough to read its `type` field. Every use of this
   path increments a **fallback-fired counter** (§9) — the drift canary for a
   Copilot regression that stops emitting the event line (most plausible on
   `/responses`, which is examples-only even first-party).
3. **Neither:** a frame with no `event:` line and a non-JSON / absent `data:` is the
   SSE-default `message` type — forwarded verbatim as a non-terminal unknown,
   never dropped.

```go
// Frame is one SSE event: its identified type (empty ⇒ SSE-default "message") and
// the exact bytes to re-emit downstream, blank-line terminator included.
type Frame struct {
    Type string // from the event: line (fallback: data.type) — routing/terminal only
    Raw  []byte // original bytes, re-emitted verbatim; never reconstructed from Type
}
```

`Raw` is authoritative for output; `Type` is advisory for control flow. CRLF and
LF line endings are both accepted; output preserves whatever the upstream sent.
The engine never re-serializes a frame from its parsed `Type`.

### 5.2 The pump loop

`forward` commits the response (copies the upstream `200` + headers, minus
hop-by-hop) and hands the upstream body to `Pump`. `Pump` starts one reader
goroutine that turns the body into frames on a channel, then runs a select loop
maintaining a single `sawTerminal` bool:

| Loop event | Action |
| --- | --- |
| **upstream frame** | write `frame.Raw` verbatim (deadline-bounded) + `Flush`; if `terminal(frame.Type)` set `sawTerminal`; then re-arm the stall stopwatch |
| **keepalive tick** (OpenAI only) | write `:\n\n` (deadline-bounded) + `Flush`; the stall stopwatch is **not** touched |
| **stall fires** (idle > `--stream-idle-timeout`) | synthesize error frame, `Flush`; outcome `stall`; cancel + join; return |
| **ctx.Done() / write error / write-deadline exceeded** | client gone; outcome `client_cancel`; cancel + join; return (nothing more written) |
| **reader done** | EOF & `sawTerminal` → `clean`; EOF & !`sawTerminal` → synthesize error, `synthesized`; read error → synthesize error, `upstream_error`; `Flush`; return |

**Stall is a stopwatch that runs only while the pump waits to receive the next
upstream frame:** armed at the commit point, **stopped the instant a frame is
received**, re-armed **after** that frame is written, and never touched by a
keepalive tick. The interval it measures is exactly `[end-of-write(prev) →
receipt(next)]` — the time copilotd spends writing to the client is excluded, so a
slow-but-progressing client can never trip it; only a genuinely silent upstream
can. The stopwatch uses an **injected clock** so tests drive it deterministically.

**Every downstream write is deadline-bounded** (§5.5) and error-checked; an error
or an exceeded write deadline means the client stopped draining — outcome
`client_cancel`.

**Lifecycle guarantee (cancel-then-join).** On *every* exit path the pump cancels
the upstream context and closes the response body — which unblocks the reader
goroutine at both of its block points: the in-flight `Read` (bound to the request
context, so cancellation tears the connection down under it) and the frame-handoff
`select` on `ctx.Done()` — and then **joins** the reader before returning. "Pump
returned" therefore implies "the reader goroutine has exited and the upstream
connection is released": no goroutine or connection leak on any outcome, and no
continued draining of Copilot for a stream that has already ended on our side. The
exact channel mechanics (a ctx-guarded reader send, closing the frame channel as
the join signal) are the implementer's; the *guarantee* is the design's.

### 5.3 Terminal events

`terminal(type)` is surface-specific:

- **Anthropic:** `message_stop`, or an upstream `error` event.
- **OpenAI:** `response.completed`, `response.failed`, `response.incomplete`, or an
  upstream `error` event.

An upstream-sent terminal (including the upstream's own `error`) is forwarded
verbatim and suppresses synthesis — copilotd never doubles up on a terminal the
upstream already delivered.

### 5.4 Keepalive

- **Anthropic:** no injection. Upstream `ping` frames are forwarded verbatim and,
  being real frames, also reset the stall stopwatch — so a long thinking/tool pause
  keeps both the client hop alive and the stream healthy for free.
- **OpenAI:** a keepalive channel at `--stream-keepalive-interval`. On each tick
  with no intervening upstream frame, write `:\n\n` and flush. Because ticks do not
  touch the stall stopwatch, a genuinely dead OpenAI upstream still trips stall at
  `--stream-idle-timeout`; a live-but-quiet upstream is kept from a false stall
  only by real events, so the stall default is set generously and is a knob.

### 5.5 Bounded writes & the deadline-resetting writer

Both the streaming pump and the buffered copy-back bound **each** downstream write
with `http.ResponseController.SetWriteDeadline`, resetting the deadline **before
every write**. This is required, not incidental: `SetWriteDeadline` sets an
absolute instant, not a per-write duration, so a single deadline spanning a whole
`io.Copy` would guillotine a large-but-progressing transfer to a slow client. A
per-write reset means a client that keeps draining continually pushes the deadline
forward, while a client that stops draining stalls one write for the full
`--write-timeout` and is caught (`client_cancel`).

> **Implementation recommendation — a deliberate exception to this spec's
> design-describes-shape-not-mechanics boundary.** The tidy way to deliver this is a
> small *deadline-resetting writer wrapper* whose `Write` sets
> `SetWriteDeadline(now + writeTimeout)` and then delegates. The buffered path
> becomes `io.Copy(deadlineWriter, resp.Body)` — unchanged shape, now bounded per
> chunk — and the streaming pump writes each frame (and keepalive / synthesized
> error) through that same wrapper. One primitive, both paths. Offered as the
> recommended optimal implementation, not a mandate.

## 6. Forward path changes (`internal/forward`)

### 6.1 The synchronous-only peek, relaxed

The peek stops rejecting `stream:true`. It now reads only the OpenAI
`background` flag; the Anthropic surface peeks nothing and forwards. `stream:true`
is *forwarded* like any other field — the original bytes are unchanged. The
`background:true` reject (OpenAI, `BackgroundUnsupported`, 400) is unchanged.

### 6.2 The branch

After `f.client.Do(outReq)` returns a response (the Phase 1 `Do()`-error handling
— 502/504 before any write — carries over, with one added case: a timeout
awaiting response headers, i.e. `ResponseHeaderTimeout`, classifies as `504`
alongside the existing deadline→504 and dial/unreachable→502; a client-cancel is
still swallowed):

- **`Content-Type` starts with `text/event-stream`** → copy status + headers
  (minus hop-by-hop; note there is no `Content-Length` on a stream), which is the
  **commit point**, then `sse.Pump(...)`.
- **otherwise** → the buffered path: `copyResponseHeaders` → `WriteHeader` →
  `io.Copy(deadlineWriter, resp.Body)`. Same shape as Phase 1, now bounded per
  chunk by the shared `--write-timeout` (§5.5).

### 6.3 Timeout plumbing

- The outbound request context derives from `r.Context()` with a plain cancel and
  **no fixed deadline** — client-cancel propagation is preserved exactly as
  Phase 1 (a client disconnect cancels the upstream call).
- `Transport.ResponseHeaderTimeout` (= `--response-header-timeout`, 600s) bounds
  **time-to-first-byte** for both paths. TTFB exceeded before commit → `504`.
  (This is a new bound; Phase 1 had only the total deadline.)
- **`--write-timeout`** (both paths) bounds each individual downstream write (§5.5);
  exceeded ⇒ `client_cancel` (streaming) / abort (buffered).
- **Buffered path:** keeps `--outbound-timeout` (600s) as a total backstop, armed
  as a timer that trips the request-context cancel and stopped when the read
  completes.
- **Streaming path:** no total cap; §5.2's idle/stall governs, plus the OpenAI
  keepalive ticker.

The forward outbound client is separate from identity's exchange client, so
setting `ResponseHeaderTimeout` here does not affect the token exchange.

## 7. copilotd-originated signals — the divergence ledger

Every response copilotd *originates* (as opposed to forwarding from Copilot) is
enumerated here and rendered from `internal/apierror` alone. This is our only
divergence from a genuine first-party endpoint, so it is kept exhaustive and
auditable. Origin is marked off-band: a `copilotd:` message prefix, the
`X-Request-Id` response header (set by the requestID middleware, present on the
streamed response too), and structured logs/metrics — never a nonstandard field
on the wire. (ADR-0003.)

### 7.1 Tier 1 — HTTP-status signals

Rendered before the response is committed: the auth/readiness gates, the buffered
path, and any stream failure before the first downstream byte.

| Signal | Trigger | HTTP | Anthropic `error.type` | OpenAI `type` (`code`) |
| --- | --- | --- | --- | --- |
| Unauthorized | missing/invalid API key | 401 | `authentication_error` | `invalid_request_error` (`invalid_api_key`) |
| NotReady | no working Copilot credential | 503 | `api_error` | `api_error` |
| BackgroundUnsupported | `background:true` (OpenAI only) | 400 | — | `invalid_request_error` |
| PayloadTooLarge | inbound body over cap | 413 | `invalid_request_error` | `invalid_request_error` |
| BadGateway | could not reach upstream | 502 | `api_error` | `api_error` |
| GatewayTimeout | TTFB exceeded before commit | 504 | `api_error` | `api_error` |

`StreamUnsupported` (Phase 1's 400 for `stream:true`) is **retired** — streaming is
now supported.

### 7.2 Tier 2 — in-band SSE synthesized terminal error

Rendered after commit, on the streaming path only. On the wire it is **one**
`event: error` frame; three triggers feed it, differing only in the message text
and the recorded outcome. Emitted via `apierror.WriteStreamError`.

| Trigger | Metric outcome | Message (after the `copilotd:` prefix) |
| --- | --- | --- |
| upstream EOF with no terminal event seen | `synthesized` | `upstream stream ended before a terminal event` |
| idle/stall timeout fired | `stall` | `upstream stream stalled` |
| upstream read error mid-stream | `upstream_error` | `upstream stream failed` |

Wire shapes (identical across the three triggers):

- **Anthropic:** `event: error\ndata: {"type":"error","error":{"type":"api_error","message":"copilotd: …"}}\n\n`
- **OpenAI:** `event: error\ndata: {"type":"error","code":null,"message":"copilotd: …","param":null}\n\n`

A **client disconnect** (`client_cancel`, whether from `ctx.Done()`, a write error,
or an exceeded write deadline) emits nothing on the wire — the client is gone. It
is logged and metered only, and the upstream is cancelled (and the reader joined)
so Copilot stops generating a stream nobody is reading.

## 8. Configuration

`ff/v4`-backed, extending Phase 1. All on `serve`; precedence flags > env > TOML >
default; env names `COPILOTD_` + upper(flag, `-`→`_`).

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| Stream idle/stall timeout | `stream-idle-timeout` | `--stream-idle-timeout` | `COPILOTD_STREAM_IDLE_TIMEOUT` | `5m` | Both surfaces, stream path; upstream silence (excludes our write time) ⇒ synthesized terminal error + close |
| Stream keepalive interval | `stream-keepalive-interval` | `--stream-keepalive-interval` | `COPILOTD_STREAM_KEEPALIVE_INTERVAL` | `15s` | OpenAI stream path only; idle gap ⇒ `:` comment to the client |
| Write timeout | `write-timeout` | `--write-timeout` | `COPILOTD_WRITE_TIMEOUT` | `90s` | **Both paths**; per-write deadline catching a client that stopped draining; exceeded ⇒ `client_cancel` / abort |
| Response header timeout | `response-header-timeout` | `--response-header-timeout` | `COPILOTD_RESPONSE_HEADER_TIMEOUT` | `600s` | Both paths; time-to-first-byte bound (TTFB exceeded ⇒ 504) |
| Outbound timeout (existing) | `outbound-timeout` | `--outbound-timeout` | `COPILOTD_OUTBOUND_TIMEOUT` | `600s` | **Now buffered path only** — total backstop for a synchronous completion |

Validation: `stream-idle-timeout` > 0; `stream-keepalive-interval` > 0;
`write-timeout` > 0; `response-header-timeout` > 0. Invalid config fails fast
before the listener binds (Phase 1 posture). No new secrets, so `LogValue`
redaction is unchanged.

## 9. Observability

Phase 0's structured logging + request-id and Phase 1's route-template access log
carry forward. Additions, within the redaction discipline (no frame bodies, no
secrets, ever).

**Granularity principle.** *Event-granularity signals belong to the sse layer* —
it is the only layer that sees individual frames; *request-granularity summaries
flow to the server access-log middleware*, which already emits one line per
request.

- **Event-level (sse layer):** the **fallback-fired counter**, incremented whenever
  event identification falls back to the `data.type` decode — the drift canary for
  a Copilot regression that drops the `event:` line. Metadata only, never frame
  bodies. Full per-event logging is a YAGNI-deferred future addition the layer is
  pre-positioned to own.
- **Request-level (server `accessLog`):** `Pump` returns an `Outcome` (`clean |
  synthesized | stall | client_cancel | upstream_error`); `forward` stashes it on a
  per-request context holder; the existing `accessLog` middleware reads it, adds an
  `outcome` attribute (and frame count) to its single per-request line, and emits
  the **stream terminal-outcome metric** by surface (the roadmap's §6 signal). A
  `synthesized` / `stall` / `upstream_error` outcome bumps that line to `warn`,
  completing the off-band origin channel for synthesized terminals.
- The access-log line continues to record total bytes and duration for the streamed
  response.

## 10. Testing strategy

TDD throughout (red → green → refactor), `-race`, stdlib `testing` +
`net/http/httptest`, with an **injected clock** for the stall/keepalive/write
timers. Copilot is stubbed with an `httptest` server that writes canned SSE and
flushes on cue.

- **`sse` Reader** — multi-line frames; blank-line boundary; CRLF and LF; comment
  (`:`) frames; `event:`-line classification; the `data.type` fallback fires only
  when the event line is absent/empty (and increments the counter); a frame with
  neither yields the default `message` type and is forwarded verbatim; unknown
  event types pass through.
- **`sse` Pump** — **byte-exact verbatim passthrough** (output bytes equal the
  concatenated upstream frames for a clean stream); terminal detection (`clean`
  when the upstream terminal is seen); `synthesized` on EOF without a terminal;
  `stall` via the injected clock; `upstream_error` on a mid-stream read error;
  **keepalive present on OpenAI and absent on Anthropic**; keepalive ticks do not
  reset stall; **stall excludes our write time** (a slow-but-progressing client
  does not trip it); a write error / exceeded write deadline / ctx cancel yields
  `client_cancel`; **cancel-then-join leaves no goroutine and releases the upstream
  connection** (asserted under `-race`).
- **`apierror.WriteStreamError`** — each surface emits the correct `event: error`
  frame shape with the `copilotd:` prefix and a trailing blank line.
- **`forward` branch** — `text/event-stream` → pump; a JSON error to a
  `stream:true` request → buffered path and still able to 502; `stream:true` is
  forwarded verbatim (no longer rejected); `background:true` still rejected;
  header/status copy at the commit point; TTFB timeout → 504 before commit; the
  buffered path's per-chunk write deadline catches a wedged client.
- **`config`** — new-field precedence + validation; `--outbound-timeout` no longer
  applied on the stream path.
- **End-to-end streaming** — server + API key + stubbed identity → a stub Copilot
  that flushes SSE frames with pauses → a real client receives frames incrementally
  (asserted by read timing), a clean terminal for a complete stream, and a
  synthesized terminal when the stub truncates; the OpenAI stub's idle gap produces
  a `:` keepalive; a client that hangs up mid-stream causes the stub to observe a
  cancelled upstream.

## 11. Deferrals mapped to phases

| Deferred item | Lands in |
| --- | --- |
| OpenAI async / background mode (`background:true`), Responses management sub-paths (`GET/DELETE /responses/{id}`, cancel, input_items), SSE resumability (`starting_after`) | Phase 4 (and only if Copilot supports it upstream) |
| Middleware / onion framework; per-event stream transformer using the identified-`type` seam | Phase 3 |
| Stable Responses item-ids and every other seed shim | Phase 4 |
| Response reconstruction / accumulation | Not planned (raw passthrough) |
| Per-event logging beyond the fallback-fired counter | Later phase (sse layer already positioned to own it) |
| Metrics build-out (Prometheus/OTel) beyond the named seams | Later phase |

## 12. Notes & open items

- **No new dependencies.** Everything is stdlib beyond the Phase 1 set.
- **Grounding (event-line identification).** A dedicated grounding pass
  established: Anthropic Messages **normatively guarantees** the `event:` line and
  that its name matches the data `type` (both official SDKs dispatch on the line);
  OpenAI Responses shows the line in **examples only** and its SDK treats
  `data.type` as authoritative; the WHATWG SSE standard makes `event:` **optional**
  (absent → default `message`). Hence `event:`-line-first **with** a `data.type`
  fallback and a fallback-fired metric (ADR-0002).
- **Facts to confirm at implementation (live Copilot capture):**
  1. Copilot's streamed responses set `Content-Type: text/event-stream` on both the
     native Anthropic `/v1/messages` and the Responses `/responses` endpoints (the
     branch key).
  2. The terminal event names match Copilot's actual output (`message_stop`;
     `response.completed` / `response.failed` / `response.incomplete`), and whether
     Copilot ever emits a bare `error` frame.
  3. Does Copilot emit an `event:` line on **every** `/responses` frame (and carry
     `sequence_number`), or forward data-only chunks? (Examples-only even for
     first-party OpenAI — the fallback-fired metric is the canary here.)
  4. Does Copilot ever append a `data: [DONE]` sentinel to the Anthropic stream?
     Anthropic's own API does not; if Copilot does, `[DONE]` is non-JSON → falls to
     the default-`message` bucket → forwarded verbatim, which is safe.
  5. Does Copilot inject comment / keepalive / vendor frames that land in the
     default-`message` bucket and exercise the fallback in normal operation?
  6. On mid-stream error/overload, does Copilot emit `event: error` with a matching
     data `type` (Anthropic convention), or an out-of-band shape the event-line
     classifier would not recognize?
  7. `http.ResponseController` flush + `SetWriteDeadline` behavior through the
     middleware chain on this Go version (the `statusWriter.Unwrap()` seam is
     already present).
- **Drift sensitivity (ROADMAP §8):** the Content-Type branch key, the `event:`-line
  assumption on `/responses`, and the terminal event names are the drift-exposed
  surfaces added this phase. The payload-opaque design keeps blast radius small —
  unknown fields and new event types already pass through — the `data.type`
  fallback plus its fallback-fired metric make a dropped event line self-correcting
  and observable, and a change to the terminal event names would blunt terminal
  detection only into a (safe, noisier) synthesized terminal at EOF.
- **Vocabulary:** "surface", "forwarder", "terminal event", "copilotd-originated
  signal", and "synthesized terminal" follow `CONTEXT.md`; a synthesized terminal
  is a copilotd-originated signal, never conflated with an upstream-forwarded one.
