# OpenAI Responses WebSocket forwarding (payload-opaque)

Status: approved 2026-07-19. Design for adding a WebSocket transport to
copilotd's OpenAI Responses surface. It is grounded in
[the 2026-07-19 research note](../research/2026-07-19-responses-websocket-mode.md)
and the current code.

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

### Explicit non-goals (YAGNI)

- **No shim / extensibility.** Messages are forwarded opaquely. No
  client-message or server-event hooks are introduced. The existing
  `shim.Registry` is not consulted on this path. (Research slice 9: the canonical
  registry is a disabled no-op today, so opaque pass-through is sufficient.)
- **No catalog change.** `/openai/v1/models` keeps filtering on the exact
  `/responses` route (see §8). WebSocket-only models remain intentionally
  excluded until a future revisit.
- **No new config knobs.** Existing timeout/size settings are reused (§5).
- **No SSE reuse.** The `sse.Pump`/`sse.Writer` path is not the transport; WS is a
  separate path (research: "a new transport path, not a small extension to
  `sse.Pump`").
- **No total-duration cap, no application keepalive pinger** on copilotd's side
  (§5).
- **No Anthropic WebSocket.** There is no public Anthropic Messages WebSocket
  contract to mirror (research §3); this design is OpenAI-only.

## 2. Decisions (resolved during brainstorming)

| Decision | Choice | Rationale |
|---|---|---|
| WebSocket library | `github.com/coder/websocket` (pinned) | Zero transitive deps; context-first; does both Accept and Dial; reaches the underlying conn via `http.ResponseController`, so it honors `statusWriter.Unwrap()` and needs **no** middleware change. |
| Where the code lives | New `internal/wsforward` package | Clean isolation; the HTTP `Forwarder` stays HTTP-only. The WS dial builds its own minimal handshake headers, so little is actually shared with the HTTP header path. |
| Catalog membership | Keep the exact `/responses` filter (no change) | Identical to a union today (every `ws:/responses` model in the capture also advertises `/responses`); never lists a model an HTTP client cannot call; least code. |
| Graceful shutdown | Drain with deadline (WaitGroup + base context) | `http.Server.Shutdown` does not wait for hijacked conns; drain sends a close frame and bounds the wait, giving clean sockets on restart. |
| Pump coordination | `golang.org/x/sync/errgroup` | Already in the module (via `x/sync`); no new dependency. |

## 3. Package `internal/wsforward`

A narrow, independently testable unit.

```go
// Proxy accepts an inbound OpenAI Responses WebSocket and forwards it to
// GitHub Copilot's ws:/responses transport, opaquely and bidirectionally.
type Proxy struct {
    provider        identity.Provider
    dialClient      *http.Client   // proxy-aware, TLS-verifying, NO total timeout
    dialTimeout     time.Duration  // from responseHeaderTimeout; bounds the handshake only
    maxMessageBytes int64          // from maxRequestBytes; per-message read limit
    logger          *slog.Logger

    // shutdown/draining state
    baseCtx  context.Context
    cancel   context.CancelFunc
    wg       sync.WaitGroup
    draining atomic.Bool
}

func New(provider identity.Provider, dialClient *http.Client,
    dialTimeout time.Duration, maxMessageBytes int64, logger *slog.Logger) *Proxy

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
- `baseCtx`/`cancel` are created in `New`. Each session derives its context from
  `baseCtx`, so `Shutdown` cancelling `baseCtx` tears every session down.

### 3.1 Accept handler flow

The handler runs inside the existing middleware chain
(`requestID → accessLog → recover → auth → readiness`), so the local API key and
readiness are already enforced before it executes. Ordering is chosen so every
failure is clean:

1. **Draining check.** If `p.draining.Load()`, write a pre-upgrade `apierror`
   503 (`NotReady`) and return.
2. **Validate the upgrade.** Lightweight header check: `Upgrade: websocket`
   (token-wise), a non-empty `Sec-WebSocket-Key`, and `Sec-WebSocket-Version: 13`.
   A request that is not a valid upgrade gets a pre-upgrade `apierror` 400
   (`NotAWebSocketUpgrade`, new kind — §6) **before any upstream dial** (research
   slice 2).
3. **Resolve the credential.** `cred, err := p.provider.Current(r.Context())`. On
   error, pre-upgrade `apierror` 503 (`NotReady`).
4. **Dial upstream first.** Build the upstream URL and handshake headers (§3.2)
   and dial under `context.WithTimeout(r.Context(), p.dialTimeout)`. On failure,
   map to a pre-upgrade `apierror`: 504 (`GatewayTimeout`) on deadline, else 502
   (`BadGateway`). **This happens before the downstream 101** (research slice 11),
   so the client's handshake library sees a normal non-101 HTTP error.
5. **Accept downstream.** Only now upgrade the client connection (send 101) via
   `websocket.Accept`.
6. **Register and pump.** `wg.Add(1)`; run the two pumps (§3.3); on return,
   record telemetry (§7) and `wg.Done()`.

Because steps 1–4 emit ordinary HTTP responses, they are visible to the outer
`accessLog` as a normal request with a real status code. Only steps 5–6 hijack.

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

Two goroutines under an `errgroup.Group` derived from a shared, cancelable
session context (child of `baseCtx` and of the inbound request context):

- **client → upstream:** loop `mt, data, err := client.Read(ctx)` then
  `upstream.Write(ctx, mt, data)`.
- **upstream → client:** loop `mt, data, err := upstream.Read(ctx)` then
  `client.Write(ctx, mt, data)`.

Properties:

- **Opaque.** The JSON payload is never parsed or routed through the SSE
  reader/writer. Message **type** (text/binary), **payload bytes**, and **order**
  are preserved.
- **Read limit.** `client.SetReadLimit(maxMessageBytes)` and
  `upstream.SetReadLimit(maxMessageBytes)` before pumping.
- **Cancellation.** The first read/write error or context cancellation returns
  from its pump; `errgroup` cancels the shared context, unblocking the sibling.
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
| Draining / not ready | `apierror` 503 `NotReady` | close `1001` (going away) |
| Not a WebSocket upgrade | `apierror` 400 `NotAWebSocketUpgrade` | — |
| Missing/invalid local key | `apierror` 401 (existing auth MW) | — |
| Credential failure | `apierror` 503 `NotReady` | — |
| Upstream dial refused | `apierror` 502 `BadGateway` | — |
| Upstream dial timeout | `apierror` 504 `GatewayTimeout` | — |
| Upstream closed / errored mid-session | — | propagate upstream close code; else `1011` |
| Oversize message | — | `1009` (library-driven) |
| Client vanished / closed | — | cancel sibling; close upstream `1001`/normal |
| Handler panic after upgrade | — | `recoverMW` recovers; both conns closed via defers |

Pre-upgrade errors use the OpenAI `apierror` dialect
([apierror.Write](../../internal/apierror/apierror.go#L102)), so a non-101 HTTP
response carries a well-formed OpenAI error body. Post-upgrade, no HTTP error is
possible; the only signal is the WebSocket close code.

Client disconnects are swallowed (no error surfaced), consistent with the HTTP
path's treatment of a vanished caller
([forward.go](../../internal/forward/forward.go#L347-L349)).

## 5. Limits, timeouts, and liveness

Reuse existing configuration; add nothing.

- **Per-message read limit** = `maxRequestBytes`
  ([config](../../internal/config/config.go#L124-L125)). A `response.create`
  message is the moral equivalent of an HTTP request body. Over-limit → the
  library closes that side with `1009`, which we propagate.
- **Handshake bound** = `responseHeaderTimeout`
  ([config](../../internal/config/config.go#L121-L122)), applied via context to
  the upstream dial only. Dropped once upgraded.
- **No total-duration cap** on copilotd's side. A live turn can be silent for
  long stretches; reusing the SSE idle timeout would be wrong (research slice 6).
  OpenAI enforces its own 60-minute cap and sends a close, which we forward.
- **Liveness.** coder/websocket answers inbound pings automatically, so a peer
  probing copilotd is satisfied for free. copilotd sends **no** periodic keepalive
  ping (bare minimum); dead sockets are reaped by TCP and by shutdown
  cancellation. A periodic ping for faster dead-peer detection is a deferred
  YAGNI item.

## 6. `apierror` addition

Add one `Kind` for the not-an-upgrade case, since no existing kind fits:

- `NotAWebSocketUpgrade` → HTTP 400, OpenAI `invalid_request_error` /
  Anthropic `invalid_request_error`. Added to the kind table
  ([apierror.go](../../internal/apierror/apierror.go#L66-L94)) with a
  well-formed row for every surface so the table stays total, following the
  existing `BackgroundUnsupported` precedent (OpenAI-only in practice but total).

No other `apierror` changes: dial and readiness failures reuse `BadGateway`,
`GatewayTimeout`, and `NotReady`.

## 7. Telemetry, metrics, and logging (bare minimum)

Post-upgrade traffic bypasses `statusWriter`
([middleware](../../internal/server/middleware.go#L101-L133)), so the ordinary
access line would otherwise show a misleading status and zero bytes. Instead of a
second log line, the session **stores a result in a context holder** that
`accessLog` appends to its single per-request line — the exact pattern already
used for `StreamResult`
([middleware](../../internal/server/middleware.go#L45-L75)).

- **`wsforward.SessionResult`** carries: `MessagesC2U`, `MessagesU2C`,
  `BytesC2U`, `BytesU2C`, `CloseCode`, and a `TerminalReason` string. It is
  stored via a `WithSessionResultHolder(ctx)` / `SessionResultFromContext(ctx)`
  pair mirroring `forward.WithStreamResultHolder`.
- **`accessLog`** appends `ws=true`, `status=101`, and the counters/close fields
  to its existing attrs; duration is the middleware's existing `time.Since(start)`
  (naturally the full session length). Level is `info` on a clean close, `warn`
  on an abnormal terminal (upstream error, forced close).
- **Metrics.** A minimal bounded `WsOutcomeCounter` mirroring
  [`StreamOutcomeCounter`](../../internal/server/metrics.go#L31-L51): fixed-label
  series `established`, `dial_failed`, `client_closed`, `upstream_closed`,
  `error`. Fixed arrays keep the label set bounded. Wired through `main` like the
  stream counter; observed once per session at close.

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
is intentional. No `OpenAIResponsesWebSocketRoute` constant is introduced.

## 9. Server and lifecycle wiring

- **Route.** `server/handler.go` registers
  `mux.Handle("GET /openai/v1/responses", guard(apierror.OpenAI, wsProxy.Handler()))`
  using the same `guard` (auth then readiness) as the POST route
  ([handler.go](../../internal/server/handler.go#L37-L50)). `POST` and `GET` on
  the same path coexist on Go's method-aware `ServeMux`.
- **Construction.** `server.New` gains a `*wsforward.Proxy` parameter, threaded to
  `newHandler` for route registration and retained for shutdown.
- **Shutdown.** `server.shutdown()`
  ([server.go](../../internal/server/server.go#L85-L95)) sequences:
  1. `p.draining.Store(true)` (refuse new upgrades → 503).
  2. `s.http.Shutdown(shutdownCtx)` — stops the listener and drains in-flight
     non-hijacked requests (it returns without waiting for hijacked WS conns).
  3. `wsProxy.Shutdown(shutdownCtx)` — `cancel()` the base context (each session
     sends a `1001` going-away close and tears down its pumps), then
     `wg.Wait()` bounded by `shutdownCtx`; on deadline, force-close survivors via
     their `CloseNow` backstops.
  4. Existing hard `s.http.Close()` fallback remains.
- **`cmd/copilotd/main.go`** builds the dial client and the `Proxy`, and passes it
  to `server.New` alongside the existing `Forwarder`
  ([main.go wiring](../../cmd/copilotd/main.go#L319-L322)).

## 10. Dependencies

- Add `github.com/coder/websocket` at a **pinned** version to `go.mod`
  (currently no WebSocket library —
  [go.mod](../../go.mod#L5-L11)).
- Reuse `golang.org/x/sync/errgroup` (the module already depends on
  `golang.org/x/sync`).

## 11. Testing

Table-driven integration tests using `httptest` and local coder/websocket
echo/scripted servers for **both** upstream and downstream. Coverage:

1. Auth and readiness reject **before** upgrade; a wrong local key yields a
   pre-101 401.
2. A plain GET without upgrade headers → pre-101 400, and **no** upstream dial
   occurs.
3. The upstream handshake carries the Copilot bearer + impersonation headers and
   **not** the local API key (secret non-leakage assertion).
4. Exact **text and binary** message forwarding, byte-verbatim, order-preserving.
5. Multiple sequential `response.create` turns on one socket.
6. Oversize message → `1009`.
7. Client-close and upstream-close code propagation.
8. Half-failure: one side dies → the sibling is torn down and both conns close
   exactly once.
9. Upstream dial failure (refused and timeout) → clean pre-101 502 / 504.
10. Handler panic after upgrade → recovered; session closed.
11. Graceful shutdown drains an active session within the deadline, then
    force-closes a straggler.
12. The `ws://` scheme path (the `http → ws` test mapping) and proxy/TLS wiring.
13. Catalog membership unchanged (existing `filter_test.go` stays green).

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
middleware, request-ID correlation, configuration, the access-log single-line
invariant and its context-holder pattern, and the general payload-opaque
forwarding policy.

New: the `internal/wsforward` package (accept, dial, pumps, session lifecycle,
shutdown draining), one `apierror` kind, a WS session context holder + access-log
attrs, a minimal WS-outcome counter, the `coder/websocket` dependency, and the
`GET /openai/v1/responses` route. The HTTP `Forwarder` and the SSE engine are
untouched.
