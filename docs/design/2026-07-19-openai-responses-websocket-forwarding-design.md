# OpenAI Responses WebSocket forwarding (payload-opaque)

Status: approved 2026-07-19; revised 2026-07-19 after a design-grilling pass;
revised 2026-07-20 to add an establishment-time log record; amended for
[#261](https://github.com/ningw42/copilotd/issues/261): admission and drain
registration are one mutex-protected decision, and HTTP and WebSocket drain
concurrently under one shutdown deadline. The earlier unlocked
`Add`-before-check registration and serial HTTP-then-WebSocket drain are
superseded; counting mid-accept work is not. Amended for
[#269](https://github.com/ningw42/copilotd/issues/269): a final non-101 answer
to the upstream handshake is relayed rather than reported as a copilotd 502.
Design for adding a WebSocket transport to copilotd's OpenAI Responses surface.
It is grounded in
[the 2026-07-19 research note](../research/2026-07-19-responses-websocket-mode.md)
and the current code.

> **Superseded historical detail.** This dated design predates the shared
> upstream call and the terminal request summary, and its original text omits
> the pre-upgrade phase's cancellation rules. Text marked "amended by #261" or
> "amended by #269" is current. Where the original text conflicts with the
> contracts below, the contracts win. For current signatures,
> `internal/wsforward` is authoritative.
>
> - **Logging** (§2 "Access logging", §3.1 step 7, §7's records, §11 item 13,
>   §13): superseded by
>   [ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md)
>   and the
>   [terminal request summary design](2026-08-31-terminal-request-summary-design.md).
>   An established session emits one immediate `websocket established` Info
>   milestone (`status=101`, `handshake_duration`) and one terminal access
>   record. There is no separate `websocket session` record. The forwarder
>   publishes the terminal reason, close code, and directional message and
>   byte counts to access through `requestsummary.RecordWebSocket`. Access
>   reports the downstream HTTP body bytes actually written (`0` for a `101`)
>   and the whole-request duration. A pre-upgrade return emits no milestone or
>   session facts. Conditional diagnostic records, such as the
>   `upstream call failed` record for a classified failure, follow their own
>   rules.
> - **Upstream preparation** (§2 "Where the code lives", the §3
>   `Proxy`/`New`/`Handler` sketch, §3.1 step 4, §3.2, §9 "Route"):
>   superseded by
>   [ADR-0013](../adr/0013-govern-authenticated-upstream-calls-in-internal-upstream.md),
>   the
>   [upstream call concentration design](2026-07-26-upstream-call-concentration-design.md),
>   and the typed Endpoint contract
>   ([ADR-0007](../adr/0007-served-endpoints-as-typed-contracts.md)). `New`
>   takes the shared `*upstream.Caller`, and the server registers
>   `Handler(ep)` for `endpoint.OpenAIResponsesWS()`. The handler builds its
>   handshake request with `Caller.Prepare` from that contract's upstream
>   Route. Prepare
>   resolves the credential, joins the Route onto the credential's base URL,
>   and carries the raw query, including a bare `?`. It then applies the
>   shared outbound header policy: bearer token, credential headers, the
>   resolved `X-Request-Id`, and the client's headers minus hop-by-hop,
>   `Connection`-listed, `Authorization`, `X-Api-Key`, `Host`,
>   `Content-Length`, and every `Sec-WebSocket-*` header. `wsforward` still
>   owns `websocket.Dial` and the raw-connection capture.
> - **Pre-upgrade phase and cancellation** (§2 "Session context", §3.1 steps
>   4–5 and the paragraph after them, §4's "Credential failure" row, §7's
>   accept counter): credential resolution and the dial run under a
>   pre-upgrade phase context derived from `r.Context()`. Forced shutdown
>   also cancels that context, and the dial adds `dialTimeout` on top. A
>   credential failure is 503 `NotReady` only while the caller is live; a
>   caller deadline is 504 `GatewayTimeout`. A cancelled phase writes nothing
>   and books no accept outcome, an exception to the once-per-handshake
>   counting in §3.1 and §7. A deadline-bearing error still wins over
>   cancellation (§3.1 step 5). A late request after drain still gets 503 and
>   `rejected`.

## 1. Goal and scope

Add a **pure, dumb, payload-opaque** WebSocket forwarder for the OpenAI Responses
API so a WebSocket-capable client can reach GitHub Copilot's `ws:/responses`
transport through copilotd, exactly as the HTTP path already reaches
`/responses`.

This is the same logical endpoint over a different transport. copilotd accepts an
inbound WebSocket on `GET /openai/v1/responses`, dials Copilot's upstream
`wss://…/responses`, and pumps messages bidirectionally without interpreting
them.

### In scope

- A distinct inbound WebSocket route on `GET /openai/v1/responses`, behind the
  existing local-API-key auth and readiness gates.
- A dedicated `internal/wsforward` package owning accept, upstream dial, the two
  message pumps, session lifecycle, and graceful-shutdown draining.
- Reuse of the existing identity seam for the upstream credential, base URL, and
  impersonation headers.
- Bare-minimum auth, telemetry/metrics, and logging.
- Graceful shutdown that drains in-flight sessions within the configured
  deadline.
- **One** new config knob: a dedicated WebSocket handshake timeout (§5).

### Explicit non-goals (YAGNI)

- **No built-in payload interpretation.** The opt-in bidirectional WebSocket
  message-transform seam now allows `ClientMessageTransformer` and
  `ServerMessageTransformer` hooks through the shared `shim.Registry`, as
  specified in the
  [2026-07-22 design](2026-07-22-websocket-shim-message-transform-design.md).
  The canonical no-op registry yields nil adapters, so the default remains
  payload-opaque, byte-for-byte, kind-preserving forwarding; only an opted-in
  shim interprets a message. No concrete shim is added here: notably, no
  `initiator` field is injected. The HTTP `/responses` path is already
  payload-opaque, omits `initiator`, and is accepted by Copilot, so the WS path
  mirrors it.
- **No catalog change.** `/openai/v1/models` keeps filtering on the exact
  `/responses` route (see §8). WebSocket-only models remain intentionally
  excluded until a future revisit.
- **Only one new config knob** — the WS handshake timeout (§5). Every other
  timeout/size setting is reused. Reusing `responseHeaderTimeout` (600s) for the
  handshake was rejected: it conflates a sub-second handshake with a
  time-to-first-byte wait, so a dead upstream would hang a *handshake* for ten
  minutes.
- **No SSE reuse.** The `sse.Pump`/`sse.Writer` path is not the transport; WS is a
  separate path (research: "a new transport path, not a small extension to
  `sse.Pump`").
- **No total-duration cap, no application keepalive pinger** on copilotd's side
  (§5). (A per-message *write* deadline exists — that is not a total cap.)
- **No Anthropic WebSocket.** There is no public Anthropic Messages WebSocket
  contract to mirror (research §3); this design is OpenAI-only.

## 2. Decisions (resolved during brainstorming and grilling)

| Decision | Choice | Rationale |
|---|---|---|
| WebSocket library | `github.com/coder/websocket` **v1.8.15** (pinned) | Dependency-free `go.mod`; context-first; does both Accept and Dial; `Accept` reaches the conn via `http.ResponseController`/`Hijack`, so it honors `statusWriter.Unwrap()` and needs **no** middleware change (verified in `accept.go`). |
| Where the code lives | New `internal/wsforward` package | Clean isolation; the HTTP `Forwarder` stays HTTP-only. The WS dial builds its own minimal handshake headers, so little is actually shared with the HTTP header path. |
| Catalog membership | Keep the exact `/responses` filter (no change) | Identical to a union today (every `ws:/responses` model in the capture also advertises `/responses`); never lists a model an HTTP client cannot call; least code. |
| Graceful shutdown | Drain with deadline (WaitGroup + base context) | `http.Server.Shutdown` does not wait for hijacked conns; drain sends a close frame and bounds the wait, giving clean sockets on restart. |
| Drain registration | One admission-mutex decision at the **top** of the handler: check admission and `wg.Add(1)` together, before any other work (amended by #261) | Makes the WaitGroup count *accepted* sessions (incl. mid-credential and mid-dial), not just established ones. `StartDrain` closes admission under the same lock and `Shutdown` waits only afterwards, so a rejected late request never touches the WaitGroup and no `Add` can race a `Wait` crossing zero. The original unlocked `Add`-before-check could reuse the WaitGroup during `Wait`. |
| Session context | Phase-split: pre-upgrade under `r.Context()`, post-upgrade pumps under `WithCancel(baseCtx)` only | Each cancel signal is live in exactly one phase; after `Hijack`, `r.Context()` no longer cancels on client death, so `baseCtx` is the single post-upgrade authority and Read errors detect the peer. |
| Handshake timeout | Dedicated `--ws-handshake-timeout` knob, default **10s** | A handshake returns `101` sub-second or is wrong; a tuned explicit knob beats borrowing an unrelated 600s timeout. |
| Write liveness | Per-write deadline = existing `writeTimeout` (90s) | A stuck-but-open slow reader blocks a pump forever with no total cap; a per-write bound reaps it without capping a healthy long turn. |
| Access logging | Two correlated lines: HTTP handshake + WS session | A hijacked request has two lifecycle phases; one line would carry a contradictory `status`/`bytes`. Two single-axis lines stay truthful and drop the context holder. |
| Metrics | Two single-axis counters (accept, session-terminal) | A flat counter smears "did it establish?" with "how did it end?"; two counters each take one increment at one observation point. |
| Not-an-upgrade status | `426 Upgrade Required` | RFC-correct for a WS-only method; matches coder/websocket's own `verifyClientRequest`. |
| Origin verification | `InsecureSkipVerify: true` (for now) | API-key auth — which a browser cannot set on a WS handshake — is the real gate; Origin CSRF protection is redundant and would `403` legitimate non-browser clients. |
| Pump coordination | `golang.org/x/sync/errgroup` | Already in the module (via `x/sync`); no new dependency. |

## 3. Package `internal/wsforward`

A narrow, independently testable unit.

```go
// Proxy accepts an inbound OpenAI Responses WebSocket and forwards it to
// GitHub Copilot's ws:/responses transport, opaquely and bidirectionally.
type Proxy struct {
    provider        identity.Provider
    dialClient      *http.Client   // proxy-aware, TLS-verifying, NO total timeout
    dialTimeout     time.Duration  // from WebSocketHandshakeTimeout; bounds the handshake dial only
    writeTimeout    time.Duration  // from writeTimeout; per-message write deadline
    maxMessageBytes int64          // from maxRequestBytes; per-message read limit
    logger          *slog.Logger
    metrics         WsMetrics      // the two single-axis counters (§7)

    // shutdown/draining state
    baseCtx  context.Context
    cancel   context.CancelFunc
    wg       sync.WaitGroup
    admissionMu     sync.Mutex // guards admission check + wg.Add (§3.1, #261)
    admissionClosed bool
}

func New(provider identity.Provider, dialClient *http.Client,
    dialTimeout, writeTimeout time.Duration, maxMessageBytes int64,
    logger *slog.Logger, metrics WsMetrics) *Proxy

func (p *Proxy) Handler() http.HandlerFunc
func (p *Proxy) Shutdown(ctx context.Context) error
```

- `provider` is the same `identity.Provider` seam the `Forwarder` consumes
  ([identity.go](../../internal/identity/identity.go#L35-L42)); `Current(ctx)`
  mints on demand.
- `dialClient` is a dedicated `*http.Client`: `Proxy: http.ProxyFromEnvironment`,
  default TLS verification, and **no** `Timeout` (a client-level timeout would
  kill the long-lived connection). The handshake is bounded by `dialTimeout` via
  context, not by the client.
- `baseCtx`/`cancel` are created in `New`. It is the **single authority over live
  (post-upgrade) sessions**: each session's pump context is `WithCancel(baseCtx)`,
  so `Shutdown` cancelling `baseCtx` tears every session down.

### 3.1 Accept handler flow

The handler runs inside the existing middleware chain
(`requestID → accessLog → recover → auth → readiness`), so the local API key and
readiness are already enforced before it executes. Ordering is chosen so every
failure is clean, and the handler **blocks for the whole session** (it runs the
pumps under `errgroup.Wait`), which is what lets the outer `accessLog` line carry
a real status and full-request duration (§7):

1. **Admit and register.** **First**, before any other step, take the admission
   mutex: if admission is still open, `wg.Add(1)` under that lock (with
   `defer wg.Done()`), so the drain WaitGroup counts every accepted session —
   including one still resolving its credential or in the dial/handshake below —
   not just established ones.
2. **Reject after drain.** If admission is closed, write a pre-upgrade
   `apierror` 503 (`NotReady`), book `AcceptRejected`, and return without
   registering, creating phase contexts, or doing upstream work. `StartDrain`
   closes admission under the same mutex and `Shutdown` starts `wg.Wait()` only
   afterwards, so an in-flight accept is either registered and awaited or
   rejected without ever touching the WaitGroup. (Amended by #261: the original
   unlocked `Add`-before-check let a rejected late request `Add` while `Wait`
   was crossing zero.)
3. **Validate the upgrade.** Lightweight header check: `Upgrade: websocket`
   (token-wise), a non-empty `Sec-WebSocket-Key`, and `Sec-WebSocket-Version: 13`.
   A request that is not a valid upgrade gets a pre-upgrade `apierror` 426
   (`NotAWebSocketUpgrade`, new kind — §6) **before any upstream dial** (research
   slice 2).
4. **Resolve the credential.** `cred, err := p.provider.Current(r.Context())`. On
   error, pre-upgrade `apierror` 503 (`NotReady`).
5. **Dial upstream first.** Build the upstream URL and handshake headers (§3.2)
   and dial under `context.WithTimeout(r.Context(), p.dialTimeout)`. On failure,
   decide in this order (amended by #269):
   1. a deadline → pre-upgrade `apierror` 504 (`GatewayTimeout`), even when the
      dial also returned a response;
   2. otherwise a cancelled phase (client left, or forced shutdown) → nothing
      written, even when the dial also returned a response;
   3. otherwise a final non-101 upstream answer (for example Copilot's
      `401`/`403`/`429`) → **relayed**: Copilot's status and headers under the
      shared response-header policy, with its body only when provably complete
      (§4);
   4. anything else — no response at all, or a `101` that fails handshake
      verification — → pre-upgrade `apierror` 502 (`BadGateway`).

   Rules 1, 2 and 4 stay with `Caller.Classify`, which keeps classification in
   `internal/upstream` as ADR-0013 requires. Before it relays, the handler only
   checks that neither a deadline nor a cancellation is present. It never
   decides which of the two wins, and it never calls `Classify` for an
   ordinary rejection, because `Classify` logs. The original "504 on deadline,
   else 502" reported a reachable Copilot that rejected the handshake as
   unreachable and dropped its `Retry-After`. **This happens before the
   downstream 101** (research slice 11), so the client's handshake library sees
   a normal non-101 HTTP response. On success, log the upstream `X-Request-Id`
   from the `101` handshake response for correlation, mirroring the HTTP path
   ([logUpstreamRequestID](../../internal/forward/forward.go#L431-L442)).
6. **Accept downstream.** Only now upgrade the client connection (send 101) via
   `websocket.Accept`, with `AcceptOptions{InsecureSkipVerify: true}` (§5) and the
   default `CompressionDisabled` mode. `Accept` calls `w.WriteHeader(101)` before
   hijacking, so the outer `statusWriter` records `101` for free.
7. **Pump.** Run the two pumps (§3.3); on return, record telemetry — the WS
   session log line and the session-terminal counter (§7) — then `wg.Done()`
   (deferred from step 1).

Because steps 1–5 emit ordinary HTTP responses, they are visible to the outer
`accessLog` as a normal request with a real status code. Only steps 6–7 hijack.
The accept counter (§7) is recorded once at handshake resolution — `established`
at step 6, or `rejected`/`dial_failed` on the pre-upgrade returns (steps 2–5);
only the session-terminal counter is recorded at step 7.

### 3.2 Upstream URL and handshake headers

- **URL.** Parse `cred.BaseURL` (a validated `https://host` origin —
  [normalizeCopilotOrigin](../../internal/identity/manager.go#L356-L386)), map
  scheme `https → wss` (`http → ws`, used only by tests), append `/responses`,
  and carry `r.URL.RawQuery` verbatim. Do not hard-code a Copilot host.
- **Headers** are built fresh (not copied wholesale from the inbound request):
  - `Authorization: Bearer <cred.Token>` (the short-lived Copilot token).
  - Every header in `cred.Headers` (the impersonation set), copied onto the
    handshake header map.
  - `X-Request-Id: <resolved id>` from
    [logging.RequestIDFrom](../../internal/forward/forward.go#L549-L551).
  - **Not** the inbound `Authorization`/`x-api-key` — the local API key never
    goes upstream, matching the HTTP path's strip policy
    ([requestStrip](../../internal/forward/forward.go#L503-L506)).
  - **Not** `Connection`/`Upgrade`/`Sec-WebSocket-*` — coder/websocket constructs
    those hop-by-hop headers itself (research slice 4).

These are passed via coder/websocket `DialOptions.HTTPHeader` with
`DialOptions.HTTPClient = p.dialClient`.

### 3.3 Message pumps

Two goroutines under an `errgroup.Group` derived from a **session context that is
`WithCancel(p.baseCtx)`** — not the inbound request context. After `Hijack`,
`r.Context()` no longer cancels on client disconnect (net/http has relinquished
the conn), so it would add nothing post-upgrade; `baseCtx` drives shutdown and a
pump's `Read` error detects the peer:

- **client → upstream:** loop `mt, data, err := client.Read(ctx)` then a
  write-bounded `upstream.Write(ctx, mt, data)`.
- **upstream → client:** loop `mt, data, err := upstream.Read(ctx)` then a
  write-bounded `client.Write(ctx, mt, data)`.

Properties:

- **Opaque.** The JSON payload is never parsed or routed through the SSE
  reader/writer. Message **type** (text/binary), **payload bytes**, and **order**
  are preserved.
- **Read limit.** `client.SetReadLimit(maxMessageBytes)` and
  `upstream.SetReadLimit(maxMessageBytes)` before pumping.
- **Per-write deadline.** Each `Write` runs under
  `context.WithTimeout(sessionCtx, p.writeTimeout)`. A stuck-but-open peer (alive
  TCP, not reading) can otherwise block a `Write` forever — there is no total cap
  — leaking the pump and its upstream conn. This is a per-write bound, so a
  healthy long turn that writes steadily is unaffected; only a write that itself
  stalls past `writeTimeout` trips, failing its pump.
- **Cancellation.** The first read/write error or context cancellation returns
  from its pump; `errgroup` cancels the shared session context, unblocking the
  sibling.
- **Close exactly once.** Each conn gets a `defer conn.CloseNow()` backstop; the
  terminal path issues one explicit `conn.Close(code, reason)` with the resolved
  close code (§4). The observed peer close code
  (`websocket.CloseStatus(err)`) is propagated to the other side where available.

Sequential `response.create` turns on one socket require no special handling: the
pumps simply forward each message. OpenAI serializes in-flight responses per
socket upstream; copilotd does not multiplex or enforce concurrency itself.

## 4. Error and close-code handling

Behavior splits on whether the downstream 101 has been sent.

| Situation | Pre-upgrade (no 101) | Post-upgrade (101 sent) |
|---|---|---|
| Draining / local prerequisites absent | `apierror` 503 `NotReady` | close `1001` (going away) |
| Not a WebSocket upgrade | `apierror` 426 `NotAWebSocketUpgrade` | — |
| Missing/invalid local key | `apierror` 401 (existing auth MW) | — |
| Credential failure | `apierror` 503 `NotReady` | — |
| No relayable upstream response: no response, or a `101` that fails handshake verification (amended by #269) | `apierror` 502 `BadGateway` | — |
| Upstream answers the handshake with a final non-101 (amended by #269) | relayed: Copilot's status and filtered headers; body only when provably complete | — |
| Upstream dial timeout | `apierror` 504 `GatewayTimeout` | — |
| Upstream closed / errored mid-session | — | propagate upstream close code; else `1011` |
| Oversize message | — | `1009` (library-driven) |
| Client vanished / closed | — | cancel sibling; close upstream `1001`/normal |
| Handler panic after upgrade | — | `recoverMW` recovers; both conns closed via defers |

copilotd-originated pre-upgrade errors use the OpenAI `apierror` dialect
([apierror.Write](../../internal/apierror/apierror.go#L102)), so each carries a
well-formed OpenAI error body. A relayed upstream rejection carries Copilot's
own body instead, or none (amended by #269): coder/websocket retains at most
the first 1024 body bytes and discards its read error, so copilotd relays the
body only when `Response.ContentLength` is non-negative and equal to the
retained length, and otherwise sends an empty body with `Content-Length: 0`.
That drop is the divergence ledger's Omission row, and it deliberately
includes complete bodies of unknown length and bodies the dial transport
decompressed. Post-upgrade, no HTTP error is possible; the only signal is the
WebSocket close code.

Client disconnects are swallowed (no error surfaced), consistent with the HTTP
path's treatment of a vanished caller
([forward.go](../../internal/forward/forward.go#L347-L349)).

## 5. Limits, timeouts, and liveness

Reuse existing configuration except the one new handshake knob.

- **Per-message read limit** = `maxRequestBytes`
  ([config](../../internal/config/config.go#L124-L125)). A `response.create`
  message is the moral equivalent of an HTTP request body. Over-limit → the
  library closes that side with `1009`, which we propagate.
- **Per-message write deadline** = `writeTimeout`
  ([config](../../internal/config/config.go#L118-L119)), applied via context to
  each `Write` (§3.3). Bounds a stuck-but-open slow reader; per-write, not a
  total-duration cap.
- **Handshake bound** = `WebSocketHandshakeTimeout` (new; default **10s**),
  applied via context to the upstream dial only, and dropped once upgraded. A
  healthy `101` returns in well under a second; this bounds the dead-upstream
  failure path (≈60× faster than reusing the old 600s `responseHeaderTimeout`).
- **No total-duration cap** on copilotd's side. A live turn can be silent for
  long stretches; reusing the SSE idle timeout would be wrong (research slice 6).
  OpenAI enforces its own 60-minute cap and sends a close, which we forward.
- **Liveness.** coder/websocket answers inbound pings automatically (inside
  `Read`), so a peer probing copilotd is satisfied for free. copilotd sends **no**
  periodic keepalive ping (bare minimum); dead sockets are reaped by the per-write
  deadline, by TCP, and by shutdown cancellation. A periodic ping for faster
  dead-peer detection is a deferred YAGNI item.
- **Compression.** coder/websocket defaults to `CompressionDisabled`
  (verified in `accept.go`); the design relies on that default so frames are
  relayed byte-for-byte and uncompressed. This is intentional — enabling
  permessage-deflate would add CPU and complicate the opaque guarantee.
- **Origin.** `AcceptOptions{InsecureSkipVerify: true}` (for now): `authMW` gates
  every handshake on the local API key, and a browser cannot set
  `Authorization`/`x-api-key` on a WS handshake, so Origin-based CSRF protection
  is redundant here and the default cross-origin rejection would only `403`
  legitimate non-browser clients that send a mismatched `Origin`. Revisit if the
  bind ever moves off loopback.

## 6. `apierror` addition

Add one `Kind` for the not-an-upgrade case, since no existing kind fits:

- `NotAWebSocketUpgrade` → HTTP **426 Upgrade Required**, OpenAI
  `invalid_request_error` / Anthropic `invalid_request_error`. `426` is the
  RFC-correct status for reaching a WebSocket-only method on this path and matches
  coder/websocket's own `verifyClientRequest`; the error *body* stays the surface
  dialect, so no client parsing changes. Added to the kind table
  ([apierror.go](../../internal/apierror/apierror.go#L66-L94)) with a
  well-formed row for every surface so the table stays total, following the
  existing `BackgroundUnsupported` precedent (OpenAI-only in practice but total).

No other `apierror` changes: dial failures with no relayable final non-101
response (no response, or a `101` that fails verification) reuse `BadGateway`,
dial deadlines reuse `GatewayTimeout`, and readiness failures reuse `NotReady`.
A final non-101 upstream answer is relayed, not mapped to a kind (amended by
#269).

## 7. Telemetry, metrics, and logging (bare minimum)

A hijacked WebSocket request has **two lifecycle phases** — the HTTP handshake and
the long-lived session — and forcing both onto a single access-log line would make
its generic `status`/`bytes` fields contradictory (a `101` carries no body; the
session's real byte counts are all post-hijack). copilotd therefore emits **three
correlated records** for an established session: an immediate establishment
record, a close-time session record, and the standard close-time access record.
This also removes the need for a stream-style context holder on this path.

The WS handler **blocks for the whole session** (it runs the pumps under
`errgroup.Wait` and returns only at close), so the outer `accessLog` line is
emitted once, on return, with `duration = time.Since(start)` = the full request
lifetime — the same property the HTTP stream path relies on
([middleware](../../internal/server/middleware.go#L45-L75)).

- **Immediate establishment record** (`websocket established`, emitted directly
  after downstream `101`): `method`, `route`, `status=101`, `bytes=0`, `ws=true`,
  `request_id`, and handshake `duration`. This is the live signal that a client
  is currently using the WebSocket transport.
- **Request access record** (the existing `accessLog`, unchanged and emitted when
  the handler returns): `method`, `route`, `status`, `bytes`, `duration`,
  `request_id`, plus `ws=true`.
  `status` is captured for free — `Accept` calls `w.WriteHeader(101)` before
  hijacking, so `statusWriter` records `101`; a pre-upgrade failure sets its
  status (`426`/`503`/`502`/`504`) through `apierror.Write` on the same writer,
  and a relayed upstream rejection sets Copilot's status (amended by #269).
  `bytes` is `0`, which is truthful for a bodyless `101`. Its duration is the
  full request lifetime. A pre-upgrade failure produces **only** this record (no
  establishment or session record). A relayed rejection, like the HTTP path's
  upstream error status, emits no `upstream call failed` record: its access
  record is Info below 500 and Warn at 500 and above, and carries
  `upstream_request_id` when Copilot's id differs (amended by #269).
- **WS session record** (`websocket session`, emitted by the `wsforward` handler
  at close, post-upgrade only): `msgs_c2u`, `msgs_u2c`, `bytes_c2u`, `bytes_u2c`,
  `close_code`, `terminal_reason`, and `duration` measured **from
  `Accept`/101 to close** (session time only, isolated from handshake/dial
  latency). Correlated to the other records by `request_id`, captured at handler
  entry via
  [logging.RequestIDFrom](../../internal/forward/forward.go#L549-L551) and passed
  explicitly (the post-hijack session context descends from `baseCtx`, which does
  not carry the request id). Level `info` on a clean close, `warn` on an abnormal
  terminal.

Because the handler logs the establishment and session records from local data,
the stream-style `SessionResult` context holder is **not** introduced.

**Metrics.** Two bounded, single-axis counters, each mirroring
[`StreamOutcomeCounter`](../../internal/server/metrics.go#L31-L51)'s fixed-array
discipline (fixed arrays keep labels bounded), each observed exactly once:

- **Accept counter** — `established`, `rejected`, `dial_failed` — observed once at
  handshake resolution. `rejected` covers the pre-upgrade `426`/`503` cases,
  `dial_failed` the `502`/`504` dial cases and relayed final non-101 upstream
  answers (amended by #269), `established` the `101`. (A local `401` never
  reaches this path — `authMW` rejects it first.)
- **Session-terminal counter** — `client_closed`, `upstream_closed`, `error` —
  observed once at close, established sessions only. `client_closed` is the normal
  multi-turn end, `upstream_closed` includes the 60-minute cap, `error` covers
  abnormal terminals (write-stall, oversize `1009`, `1011`).

An established session increments one label on **each** counter (one at open, one
at close) — two increments on two independent single-axis series, never mixed.
`WsMetrics` bundles the two observers; both are wired through `main` like the
stream counter.

Secrets are never logged (the Copilot and OAuth tokens are excluded by
construction and never passed to the logger).

## 8. Catalog: unchanged

`/openai/v1/models` keeps filtering on the exact `OpenAIResponsesRoute`
(`/responses`)
([catalog.go](../../internal/catalog/catalog.go#L15-L18),
[Filter](../../internal/catalog/catalog.go#L93-L102)). In the 2026-07-18 capture,
every model advertising `ws:/responses` also advertises `/responses`, so the
visible list is identical to a union today. WebSocket-only models remain
excluded. A one-line comment is added to
[filter_test.go](../../internal/catalog/filter_test.go#L13) noting the exclusion
is intentional (the earlier "WebSocket is a non-goal" rationale is now stale — the
transport exists, but no WS-only model is currently hidden). No
`OpenAIResponsesWebSocketRoute` constant is introduced.

## 9. Server and lifecycle wiring

- **Route.** `server/handler.go` registers
  `mux.Handle("GET /openai/v1/responses", guard(apierror.OpenAI, wsProxy.Handler()))`
  using the same `guard` (auth then readiness) as the POST route
  ([handler.go](../../internal/server/handler.go#L37-L50)). `POST` and `GET` on
  the same path coexist on Go's method-aware `ServeMux`; the method alone routes
  HTTP-Responses (POST) vs the WS handshake (GET), so this is a pure addition and
  the HTTP path is untouched.
- **Construction.** `server.New` gains a `*wsforward.Proxy` parameter, threaded to
  `newHandler` for route registration and retained for shutdown. The two WS
  counters are threaded like `streamOutcomes`.
- **Config.** `config.go` gains `WebSocketHandshakeTimeout time.Duration`
  (flag `--ws-handshake-timeout`, env `COPILOTD_WS_HANDSHAKE_TIMEOUT`,
  `defaultWebSocketHandshakeTimeout = 10 * time.Second`), validated positive like
  its duration siblings.
- **Shutdown.** `server.shutdown()`
  ([server.go](../../internal/server/server.go)) sequences (amended by #261;
  the original serial HTTP-then-WebSocket drain let an open SSE stream consume
  the WebSocket grace period):
  1. `wsProxy.StartDrain()` closes admission (refuse new upgrades → 503).
  2. Concurrently, under the **same** `shutdownCtx`, and waiting for both:
     - `s.http.Shutdown(shutdownCtx)` — stops the listener and drains in-flight
       non-hijacked requests (it returns without waiting for hijacked WS conns).
     - `wsProxy.Shutdown(shutdownCtx)` — signal the drain (each session's pumps
       send a `1001` going-away close), then `wg.Wait()` bounded by
       `shutdownCtx`; on deadline, cancel the base context and close the
       captured upgraded transports so an in-progress library close handshake
       is interrupted rather than serialized behind. Because registration is
       at the top of the handler (§3.1), `Wait` covers sessions still in
       credential resolution or dial/handshake, not just established ones.
  3. When either drain fails, the existing hard `s.http.Close()` fallback runs.
     If the grace deadline actually expired and each drain failure is that
     deadline, `Run` returns an error matching `server.ErrForcedDrain`;
     `cmd/copilotd` logs it once at Warn and exits 0. Genuine drain errors stay
     ordinary errors (Error log, exit 1).
- **`cmd/copilotd/main.go`** builds the dial client, the two WS counters, and the
  `Proxy` (passing `cfg.WebSocketHandshakeTimeout`, `cfg.WriteTimeout`,
  `cfg.MaxRequestBytes`), and passes the `Proxy` to `server.New` alongside the
  existing `Forwarder`
  ([main.go wiring](../../cmd/copilotd/main.go#L319-L322)).

## 10. Dependencies

- Add `github.com/coder/websocket` at **v1.8.15** (pinned; its `go.mod` is
  dependency-free) to `go.mod` (currently no WebSocket library —
  [go.mod](../../go.mod#L5-L11)).
- Reuse `golang.org/x/sync/errgroup` (the module already depends on
  `golang.org/x/sync`).

## 11. Testing

Table-driven integration tests using `httptest` and local coder/websocket
echo/scripted servers for **both** upstream and downstream. Coverage:

1. Auth and readiness reject **before** upgrade; a wrong local key yields a
   pre-101 401.
2. A plain GET without upgrade headers → pre-101 **426**, and **no** upstream dial
   occurs.
3. The upstream handshake carries the Copilot bearer + impersonation headers and
   **not** the local API key (secret non-leakage assertion).
4. Exact **text and binary** message forwarding, byte-verbatim, order-preserving.
5. Multiple sequential `response.create` turns on one socket.
6. Oversize message → `1009` (→ `error` terminal label).
7. Client-close and upstream-close code propagation.
8. Half-failure: one side dies → the sibling is torn down and both conns close
   exactly once.
9. Upstream dial failure (no response, a `101` that fails verification, and
   timeout) → clean pre-101 502 / 504. A final non-101 upstream answer
   (`401`/`403`/`429`/`503`) → Copilot's status and filtered headers relayed,
   the body only when provably complete (each completeness case tested
   separately), and a deadline or client departure still winning over a
   present response (amended by #269).
10. Handler panic after upgrade → recovered; session closed.
11. Graceful shutdown drains an active session within the deadline, then
    force-closes a straggler; a session still mid-accept (registered at the top
    of the handler) is drained rather than skipped; late upgrades racing the
    last admitted handler are rejected without WaitGroup reuse or upstream work
    (`-race`); and with SSE open, the WebSocket `1001` still arrives promptly
    because both transports drain concurrently (amended by #261).
12. Slow-reader write stall → the per-write deadline trips and the session is torn
    down (no goroutine leak).
13. Three-record logging: the establishment record appears immediately after
    `101`; the session record reports directional counters + `close_code` at
    close; and the access record reports the full request lifetime. A pre-upgrade
    failure emits **only** the access record with the mapped status.
14. Both metric counters increment on the correct axes (accept vs session
    terminal), once each per session.
15. The `ws://` scheme path (the `http → ws` test mapping) and proxy/TLS wiring.
16. Catalog membership unchanged (existing `filter_test.go` stays green).

## 12. Documentation and scope reversal

- Add `docs/adr/0006-openai-responses-websocket-transport.md`: records that the
  payload-opaque WebSocket transport is now in scope and mirrors ADR-0002's
  payload-opaque stance for the new transport.
- Update the prior non-goal notes so the reversal is intentional, not drift:
  the phase-3 middleware design's deferred-items table
  ([line ~723](2026-07-16-phase-3-middleware-framework-design.md)) and the
  phase-6a catalog design's `ws:/responses` note
  ([lines ~116-125](2026-07-18-phase-6a-provider-shaped-model-catalogs-design.md)),
  each pointing at this design/ADR.

## 13. Reusable vs new

Reused unchanged: the identity/credential seam, local auth + readiness
middleware, request-ID correlation, most of the configuration, the access-log
context-holder **pattern** (still used by the stream/shape paths), and the general
payload-opaque forwarding policy. Note the WS path deliberately adds immediate
establishment and close-time session records alongside the standard access log —
a scoped, intentional exception to the one-line-per-request habit, justified by
the two genuine lifecycle phases of a hijacked request.

New: the `internal/wsforward` package (accept, dial, pumps, session lifecycle,
shutdown draining), one `apierror` kind (`NotAWebSocketUpgrade` → 426), the two
WS establishment and session records, two single-axis WS-outcome
counters, one config knob (`WebSocketHandshakeTimeout`), the `coder/websocket`
v1.8.15 dependency, and the
`GET /openai/v1/responses` route. No WS context holder is introduced. The HTTP
`Forwarder` and the SSE engine are untouched.
