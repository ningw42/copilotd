# Phase 2 — SSE streaming engine, both surfaces — Design

Status: approved design (refined via brainstorming session), pending implementation plan
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
stream into SSE frames and unmarshals only each frame's `type` discriminator to
identify it, then re-emits the original frame bytes verbatim. It never
deserializes the event payload, so unknown fields and unknown event types (both
APIs warn these will appear) survive untouched — faithful to raw-passthrough
principle #1. That minimal identification is also the seam a later per-event
middleware plugs into (Phase 3): the engine already knows *which* event each
frame is, so a future transform pays the full unmarshal/mutate/re-marshal cost
only for the specific events it touches, and nothing pays it today.

### 1.1 The onion, this phase

```
 client ◄──────────── SSE frames (flushed per frame) ──────────── copilotd ◄──── text/event-stream ──── GitHub Copilot
   ▲                                                                 │  reader goroutine → frameCh
   │   keepalive (OpenAI: our : comment ; Anthropic: upstream ping)  │  main loop: frame | keepalive | stall | ctx-done | reader-done
   │   terminal always delivered (upstream's, or ours synthesized)   │  writes verbatim + Flush ; detects terminal ; cancels upstream on disconnect
```

The streaming engine underpins the forwarder's response half. Middleware
(Phase 3) will nest *inside* this loop as a frame transformer; this phase builds
the loop and the framing, with no transformer yet.

## 2. Scope

**In scope (Phase 2):**

- **SSE streaming engine** (`internal/sse`, new): a frame reader (split on the
  blank-line boundary; minimal `{type}` peek), a frame writer (emit verbatim +
  flush), and the pump loop that copies upstream → downstream while owning
  keepalive, the idle/stall timeout, terminal-event detection/enforcement, and
  client-cancel propagation.
- **Forward-path branch** (`internal/forward`): after the upstream call returns,
  branch on the *response* `Content-Type` — `text/event-stream` → the pump;
  anything else → Phase 1's buffered copy-back, unchanged. Drop the
  `stream:true` reject; keep the OpenAI `background:true` reject.
- **In-band SSE error renderer** (`internal/apierror`): `WriteStreamError`, so the
  streaming path never hand-rolls an error frame. `apierror` becomes the single
  home of every copilotd-originated signal — HTTP and SSE alike (§7).
- **Timeout model evolution**: a time-to-first-byte bound (`ResponseHeaderTimeout`)
  for both paths; the existing `--outbound-timeout` narrowed to the buffered path;
  an idle/stall timeout governing streams instead of a total-duration cap.
- **Keepalive**: forward Anthropic's upstream `ping` events verbatim (inject
  nothing); inject a surface-agnostic SSE comment on the OpenAI path during idle
  gaps.
- **Observability**: a stream terminal-outcome metric seam and an end-of-stream
  log, within Phase 0's redaction discipline.
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

## 3. Guiding decisions & rationale

| Decision | Choice | Rationale |
| --- | --- | --- |
| Engine depth | Frame-aware, payload-opaque: parse SSE frames, unmarshal only `{type}`, re-emit original bytes | Pays for identification and nothing beyond it. Verbatim re-emit keeps unknown fields/new event types intact (principle #1). The identified `type` is exactly the discriminator terminal-detection needs now and a Phase 3 per-event transform will need later. |
| Event identification | Unmarshal the `data:` payload into `struct{ Type string }` | The authoritative discriminator both APIs carry in-band, and the same key a future middleware branches on — one identification mechanism, engine and middleware shared. Robust to a producer omitting the SSE `event:` line; a non-JSON `data:` is forwarded verbatim as an unknown, non-terminal frame. |
| Stream vs. buffered branch | Branch on the upstream **response** `Content-Type`, not the request `stream` flag | The response is ground truth: an upstream error to a `stream:true` request comes back as JSON and correctly takes the buffered path, where it can still surface a real 502. |
| Flush cadence | Flush after every frame, keepalive, and synthesized error, via `http.ResponseController` | SSE requires per-event delivery. `ResponseController` reaches the socket through the existing `statusWriter.Unwrap()`; HTTP/1.1 chunked framing is provided by the Go server once `Content-Length` is absent. |
| Timeout model | TTFB bound (`ResponseHeaderTimeout`) + idle/stall for streams; total cap kept for buffered only | A total-duration cap guillotines a legitimately long generation. TTFB catches a dead upstream before commit; idle/stall catches one that dies mid-stream; the buffered path keeps a total backstop because a synchronous completion should finish within a bound. |
| Keepalive ownership | Forward Anthropic pings; inject an SSE comment on OpenAI idle | Anthropic defines `ping` as a first-class server-sent event — the origin (Copilot) sends it and we forward it, so ours would be redundant. OpenAI's taxonomy has no ping, so the client hop (which copilotd owns) needs our own comment during idle. A `:` comment is valid on both surfaces and ignored by both, perturbing neither Anthropic's event flow nor OpenAI's `sequence_number` accounting. |
| Keepalive vs. stall | Upstream frames reset the stall timer; keepalive ticks do not | Stall detects a *dead upstream*; keepalive keeps a *live client hop*. They are different concerns with different thresholds. Anthropic's pings are real frames, so they reset stall for free — a healthy Anthropic stream never false-stalls. |
| Terminal enforcement | Before commit → HTTP 502/504; after commit → in-band synthesized `error` event | Once `200 text/event-stream` is flushed the HTTP status is locked, so a mid-stream failure can only be signalled in-band. Synthesizing a terminal `error` event stops the client's SSE parser from hanging forever waiting for `message_stop` / `response.completed`. |
| Synthesized event shape | Bare `error` event on both surfaces; native enum types; no fabricated envelope | The Anthropic spec allows mid-stream `error` events; the Responses taxonomy has a bare `error` event. Choosing it over OpenAI's `response.failed` avoids fabricating a full `Response` envelope — the engine invents nothing beyond a message string. |
| Origin marking | Off-band: `copilotd:` message prefix + `X-Request-Id` header + logs/metrics; wire shape stays native | copilotd-originated signals are our only divergence from a first-party endpoint. Marking origin with a nonstandard wire field would risk a strict-parse client and break the "looks native" promise; the request-id gives operators an authoritative origin channel without polluting the shape. |
| New package | `internal/sse`, Copilot-agnostic and surface-parameterized | The framing/pump mechanics are a distinct concern from the dumb forwarder and the credential seam; isolating them keeps `forward` focused and gives Phase 3 a clean place to insert a transformer. |
| New dependencies | None beyond the Phase 1 set | Frame scanning, timers, and flushing are all stdlib (`bufio`, `time`, `net/http`). |

## 4. Module layout & package boundaries

Extending Phase 1's conventions — small, single-purpose, dependency-injected units.

```
copilotd/
└── internal/
    ├── sse/        [NEW]  Reader (frame split + {type} peek) · Writer (verbatim emit + flush) ·
    │                      Pump (the select loop: frame | keepalive | stall | ctx-done | reader-done) ·
    │                      Outcome (clean | synthesized | stall | client_cancel | upstream_error)
    ├── forward/    [CHG]  peek drops `stream`; Content-Type branch → sse.Pump or buffered copy-back;
    │                      ResponseHeaderTimeout on the transport; --outbound-timeout scoped to buffered
    ├── apierror/   [CHG]  + WriteStreamError(w, surface, reason); StreamUnsupported reject retired
    ├── config/     [CHG]  + stream-idle-timeout, stream-keepalive-interval, response-header-timeout
    └── server/     [CHG]  stream terminal-outcome metric seam (routes unchanged)
```

Each changed/new unit — *what it does · how it is used · what it depends on*:

- **`internal/sse`** [NEW] — the mechanics. A `Reader` splits an `io.Reader` into
  SSE frames and extracts each frame's `type`; a `Writer` emits a frame's raw
  bytes and flushes; `Pump` runs the copy loop, parameterized by the surface's
  policy (terminal predicate, keepalive interval — `0` disables it, synthesized-error
  renderer, injected clock). Used by `forward`. Depends on `net/http`, `bufio`,
  `time`, `context`. Knows nothing about credentials, routing, or how the payload
  is shaped beyond `{type}`.

- **`internal/forward`** [CHG] — the peek no longer inspects `stream` (Anthropic
  peeks nothing; OpenAI still peeks `background`). After `Do()`, it branches on the
  response `Content-Type` and either copies status+headers and hands the body to
  `sse.Pump`, or takes the Phase 1 buffered path. It owns the timeout plumbing
  (below). Depends additionally on `sse`.

- **`internal/apierror`** [CHG] — gains `WriteStreamError(w, surface, reason)`,
  which writes one `event: error` frame in the surface's dialect and flushes. It
  is now the single definition of every proxy-originated signal, HTTP and SSE, so
  no other package hand-rolls one. Still a leaf (`net/http`, `encoding/json`).

**Key boundaries:** `sse` is surface-parameterized but Copilot-agnostic and
payload-opaque; `forward` chooses the path and supplies policy; `apierror` holds
the complete divergence surface.

## 5. The SSE engine (`internal/sse`)

### 5.1 Frame model

An SSE frame is the block of lines up to (and including) the terminating blank
line. The `Reader` accumulates a frame's raw bytes and, from its `data:` line(s),
unmarshals **only** `struct{ Type string }` to obtain the discriminator:

```go
// Frame is one SSE event: its identified type (empty if data is absent or not
// JSON) and the exact bytes to re-emit downstream, blank-line terminator included.
type Frame struct {
    Type string // e.g. "message_stop", "response.completed" — for routing/terminal detection only
    Raw  []byte // original bytes, re-emitted verbatim; never reconstructed from Type
}
```

`Raw` is authoritative for output; `Type` is advisory for control flow. A frame
whose `data:` is absent, empty, or not JSON (e.g. a comment-only frame) yields an
empty `Type` and is forwarded verbatim as a non-terminal unknown. CRLF and LF
line endings are both accepted; output preserves whatever the upstream sent.

### 5.2 The pump loop

`forward` commits the response (copies the upstream `200` + headers, minus
hop-by-hop) and hands the upstream body to `Pump`. `Pump` starts one reader
goroutine that turns the body into frames on a channel, then runs a select loop
maintaining a single `sawTerminal` bool:

| Loop event | Action |
| --- | --- |
| **upstream frame** | write `frame.Raw` verbatim, `Flush`; if `terminal(frame.Type)` set `sawTerminal`; reset the idle/stall timer |
| **keepalive tick** (OpenAI only) | write `:\n\n`, `Flush`; the idle/stall timer is **not** reset |
| **stall timer fires** (idle > `--stream-idle-timeout`) | synthesize error frame, `Flush`; outcome `stall`; cancel upstream; return |
| **ctx.Done() or a write error** | client gone; outcome `client_cancel`; cancel upstream; return (nothing more written) |
| **reader done** | EOF & `sawTerminal` → `clean`; EOF & !`sawTerminal` → synthesize error, `synthesized`; read error → synthesize error, `upstream_error`; `Flush`; return |

Every downstream `write`+`Flush` is error-checked; a failure means the client
disconnected, which is treated as `client_cancel` (cancel upstream, stop). The
idle/stall timer uses an **injected clock** so tests drive it deterministically.

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
  being real frames, also reset the stall timer — so a long thinking/tool pause
  keeps both the client hop alive and the stream healthy for free.
- **OpenAI:** a `time.Ticker` at `--stream-keepalive-interval`. On each tick with
  no intervening upstream frame, write `:\n\n` and flush. Because ticks do not
  reset the stall timer, a genuinely dead OpenAI upstream still trips stall at
  `--stream-idle-timeout`; a live-but-quiet upstream is kept from a false stall
  only by real events, so the stall default is set generously and is a knob.

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
- **otherwise** → the Phase 1 buffered path (`copyResponseHeaders` →
  `WriteHeader` → `io.Copy`), byte-for-byte unchanged.

### 6.3 Timeout plumbing

- The outbound request context derives from `r.Context()` with a plain cancel and
  **no fixed deadline** — client-cancel propagation is preserved exactly as
  Phase 1 (a client disconnect cancels the upstream call).
- `Transport.ResponseHeaderTimeout` (= `--response-header-timeout`, 600s) bounds
  **time-to-first-byte** for both paths. TTFB exceeded before commit → `504`.
  (This is a new bound; Phase 1 had only the total deadline.)
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
on the wire.

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

A **client disconnect** (`client_cancel`) emits nothing on the wire — the client
is gone. It is logged and metered only, and the upstream is cancelled so Copilot
stops generating a stream nobody is reading.

## 8. Configuration

`ff/v4`-backed, extending Phase 1. All on `serve`; precedence flags > env > TOML >
default; env names `COPILOTD_` + upper(flag, `-`→`_`).

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| Stream idle/stall timeout | `stream-idle-timeout` | `--stream-idle-timeout` | `COPILOTD_STREAM_IDLE_TIMEOUT` | `90s` | Both surfaces, stream path; upstream silence ⇒ synthesized terminal error + close |
| Stream keepalive interval | `stream-keepalive-interval` | `--stream-keepalive-interval` | `COPILOTD_STREAM_KEEPALIVE_INTERVAL` | `15s` | OpenAI stream path only; idle gap ⇒ `:` comment to the client |
| Response header timeout | `response-header-timeout` | `--response-header-timeout` | `COPILOTD_RESPONSE_HEADER_TIMEOUT` | `600s` | Both paths; time-to-first-byte bound (TTFB exceeded ⇒ 504) |
| Outbound timeout (existing) | `outbound-timeout` | `--outbound-timeout` | `COPILOTD_OUTBOUND_TIMEOUT` | `600s` | **Now buffered path only** — total backstop for a synchronous completion |

Validation: `stream-idle-timeout` > 0; `stream-keepalive-interval` > 0;
`response-header-timeout` > 0. Invalid config fails fast before the listener binds
(Phase 1 posture). No new secrets, so `LogValue` redaction is unchanged.

## 9. Observability

Phase 0's structured logging + request-id and Phase 1's route-template access log
carry forward. Additions, within the redaction discipline (no frame bodies, no
secrets, ever):

- **Stream terminal-outcome metric seam** — labeled `{clean, synthesized, stall,
  client_cancel, upstream_error}` by surface. This is the roadmap's "stream
  terminal outcomes" signal (§6).
- **End-of-stream log** at `debug`: surface, outcome, frame count, duration — no
  bodies. Synthesized terminals additionally log at `info`/`warn` with the
  request-id so the off-band origin channel is complete.
- The existing access-log line still records total bytes and duration for the
  streamed response (emitted when the pump returns).

## 10. Testing strategy

TDD throughout (red → green → refactor), `-race`, stdlib `testing` +
`net/http/httptest`, with an **injected clock** for the idle/stall and keepalive
timers. Copilot is stubbed with an `httptest` server that writes canned SSE and
flushes on cue.

- **`sse` Reader** — multi-line frames; blank-line boundary; CRLF and LF; comment
  (`:`) frames; `{type}` extraction; a non-JSON `data:` yields empty `Type` and is
  forwarded verbatim; unknown event types pass through with their `type` set.
- **`sse` Pump** — **byte-exact verbatim passthrough** (output bytes equal the
  concatenated upstream frames for a clean stream); terminal detection (`clean`
  when the upstream terminal is seen); `synthesized` on EOF without a terminal;
  `stall` via the injected clock; `upstream_error` on a mid-stream read error;
  **keepalive present on OpenAI and absent on Anthropic**; keepalive ticks do not
  reset stall; a downstream write error / ctx cancel yields `client_cancel` and
  cancels the upstream read.
- **`apierror.WriteStreamError`** — each surface emits the correct `event: error`
  frame shape with the `copilotd:` prefix and a trailing blank line.
- **`forward` branch** — `text/event-stream` → pump; a JSON error to a
  `stream:true` request → buffered path and still able to 502; `stream:true` is
  forwarded verbatim (no longer rejected); `background:true` still rejected;
  header/status copy at the commit point; TTFB timeout → 504 before commit.
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
| Metrics build-out (Prometheus/OTel) beyond the named outcome seam | Later phase |

## 12. Notes & open items

- **No new dependencies.** Everything is stdlib beyond the Phase 1 set.
- **Facts to confirm at implementation (not design forks):**
  1. Copilot's streamed responses set `Content-Type: text/event-stream` on both
     the native Anthropic `/v1/messages` and the Responses `/responses` endpoints
     (the branch key); confirm against a live account.
  2. The terminal event names above match Copilot's actual stream output
     (`message_stop`; `response.completed` / `response.failed` /
     `response.incomplete`), and whether Copilot ever emits a bare `error` frame.
  3. Whether Copilot's Responses stream carries any periodic heartbeat of its own
     (which would ease the OpenAI stall exposure) — informs only the default, not
     the design.
  4. The exact `http.ResponseController` flush behavior through the middleware
     chain on this Go version (the `statusWriter.Unwrap()` seam is already present).
- **Drift sensitivity (ROADMAP §8):** the Content-Type branch key and the terminal
  event names are the drift-exposed surfaces added this phase. The payload-opaque
  design keeps blast radius small — unknown fields and new event types already
  pass through — but a change to the terminal event names would blunt terminal
  detection (degrading to a synthesized terminal at EOF, which is safe but
  noisier). Keeping identification to `{type}` and the terminal set small limits
  exposure.
- **Vocabulary:** "surface" (inbound dialect) and "forwarder" follow `CONTEXT.md`;
  a synthesized terminal is a copilotd-originated signal, never conflated with an
  upstream-forwarded one.
