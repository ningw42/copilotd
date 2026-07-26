# Concentrate upstream-call policy in `internal/upstream`

**Status:** proposed
**Date:** 2026-07-26

## Summary

copilotd's outbound half — acquire the credential, build the upstream URL and
header set, execute, correlate the upstream request id, classify the failure — is
written four times, once per call site, and the copies have drifted into
behavioural differences. This design concentrates the shared policy and the work
that composes cleanly into one dependency-light package, `internal/upstream`.
It is the authority for the shared policy on the path from *"I have an Endpoint
and a body"* to *"I have an upstream response or a classified failure"*; call
sites keep transport-specific execution where necessary and their response tail.

Three of the drifts are live defects and are fixed here, not preserved: the
WebSocket transport silently discards every inbound client header that the HTTP
transport of the *same* endpoint forwards; the WebSocket failure classifier has
no client-cancel branch, so a mid-handshake disconnect writes a 502 to a dead
socket and books a spurious `AcceptDialFailed`; and the two buffered-read paths
disagree on the status for an over-cap upstream body. A fourth change follows
from the unified URL join.

The change also removes the `forward` → `catalog` import: `catalog` currently
declares the `Fetcher` interface that `forward` implements, while `forward`
imports `catalog` for error sentinels that round-trip out to catalog vocabulary
and back to the same `apierror.Kind`s.

## Motivation

### The outbound half is written four times

Four call sites each perform the same sequence with hand-written variations:

| Site | File | Credential | Headers | Executes | Classifies into |
|---|---|---|---|---|---|
| `PassthroughHandler` | `forward/forward.go:155–215` | `provider.Current` | forwards client headers | `client.Do` | `apierror.Kind` |
| `forward` | `forward/forward.go:323–434` | `provider.Current` | forwards client headers + identity encoding | `client.Do` | `apierror.Kind` |
| `FetchModels` | `forward/forward.go:220–268` | `provider.Current` | no client headers + identity encoding | `client.Do` | `catalog` sentinels |
| `Proxy.Handler` | `wsforward/proxy.go:128–233` | `provider.Current` | **drops all client headers** | `websocket.Dial` | `apierror.Kind` + metric |

The repetition is measurable rather than impressionistic:

- `logUpstreamRequestID` is **byte-identical** in `forward/forward.go:436–447` and
  `wsforward/preupgrade.go:13–24`. Normalising the receiver name yields a zero-hunk
  diff.
- `const requestIDHeader = "X-Request-Id"` is declared in **three** packages:
  `server/middleware.go:15`, `forward/forward.go:38`, `wsforward/proxy.go:28`.
- The string `"no upstream credential available"` appears at **five** sites:
  `forward/forward.go:161`, `forward/forward.go:332`, `wsforward/proxy.go:154`,
  `catalog/fetch.go:25`, `catalog/handler.go:107`.

### The copies have drifted into behaviour, not just repetition

**Client headers.** `forward.authenticatedOutboundHeaders` (`forward.go:540`)
copies every inbound header except the strip set. `wsforward.upstreamHeaders`
(`proxy.go:301`) builds a *fresh* map from `cred.Headers` alone and forwards no
client header at all. These serve the same logical endpoint —
`POST /openai/v1/responses` and `GET /openai/v1/responses` — so a client's custom
header reaches Copilot on one transport and vanishes on the other. Neither
function documents the choice; `upstreamHeaders` has no doc comment.

**Client cancel.** There are **three** policies, not two:

- `forward` and `PassthroughHandler` test `errors.Is(r.Context().Err(),
  context.Canceled)` and return silently, counting nothing
  (`forward.go:353`, `forward.go:187`).
- `wsforward` has **no such branch**. A client that disconnects mid-handshake
  falls through to `apierror.Write(w, surface, BadGateway, …)` and
  `observeAccept(AcceptDialFailed)` (`proxy.go:187–188`) — a 502 written to a dead
  socket plus a metric recording an upstream failure that never happened.
- `FetchModels` has no branch either; it returns `ErrUpstreamUnreachable` wrapping
  `context.Canceled`, and `catalog/handler.go:37` catches the disconnect one level
  up with `if r.Context().Err() != nil { return }`. The classification is computed
  and then discarded.

**Over-cap upstream body.** `FetchModels` returns `ErrUpstreamRead` → 502
(`forward.go:264`); `forward` returns `PayloadTooLarge` → 413
(`forward.go:421`) for the same condition. 413 describes the *inbound request
entity*, not an upstream response, so the `forward` mapping is the wrong one.

**Base URL join.** The three `forward` sites concatenate raw:
`cred.BaseURL + string(upstream)` (`forward.go:175`, `:227`, `:339`).
`wsforward.websocketURL` trims first (`proxy.go:294`). Six other base-URL joins in
the repo use `strings.TrimRight(base, "/") + path` —
`catalog/codex_models_cache.go:115`, `impersonation/discovery.go:48` and `:100`,
`identity/manager.go:298`, `identity/deviceflow.go:178`, `:213` and `:249`. Only
`forward` deviates. `cred.BaseURL` is upstream-supplied (`identity/manager.go:457`,
from the exchange response), so a trailing slash is not hypothetical.

### The error vocabulary round-trips

`catalog` declares `FetchErrorKind` with five constants and the `Fetcher`
interface (`catalog/fetch.go`). `forward` imports `catalog` solely to wrap its
failures in those sentinels; `catalog.writeFetchError` (`handler.go:104`) then
unwraps them back into the same `apierror.Kind`s the other three sites produce
directly. Two of the five sentinel messages are verbatim copies of `apierror`
call-site messages elsewhere in `forward`.

The result is an import edge — `forward` → `catalog` — pointing from the dumb
forwarder to a rendering package, in the opposite direction from
`catalog.Fetcher`, the interface `forward` satisfies.

## Goals

- One module governs the shared policy between "I have an Endpoint and a body"
  and "I have an upstream response or a classified failure."
- One centrally governed header policy for every upstream call, in one file.
- One failure classifier and one response step, inseparable, so callers do not
  maintain separate branch sets.
- Remove the `forward` → `catalog` import and `catalog`'s parallel error enum.
- Fix the WebSocket header drop and the missing client-cancel branch.
- Leave the module's interface as the test surface: the header policy and the
  classification table become tables, tested once.

## Non-goals

- **No transport abstraction.** `websocket.Dial` is not wrapped behind a
  `Do`-shaped interface. See
  [Alternatives considered](#alternatives-considered).
- **No config-struct constructors.** Shrinking `forward.New`'s nine positional
  parameters is a separate candidate. This design happens to remove two of them,
  but does not take on the rest.
- **No change to the post-commit stream path.** `sse.Pump`,
  `apierror.WriteStreamError`, and the synthesized-terminal rules are untouched.
  This module covers pre-commit failures only.
- **No `--max-websocket-message-bytes` flag.** The conflation of
  `--max-request-bytes` with the WebSocket read limit is a separate candidate.
- **No new configuration.** No flag is added, removed, or renamed.

## The concept

An **upstream call** is the single authenticated request copilotd makes to
GitHub Copilot on behalf of one inbound request. Its credential source, header
policy, correlation rule, and failure vocabulary are centrally governed,
regardless of which transport carries it or which endpoint occasioned it.

The module owns the shared call policy and the HTTP execution that composes
behind it. WebSocket dialing remains transport-specific. The module does not own
what the caller does with the response: pumping an SSE stream, upgrading a
WebSocket, decoding a catalog, and copying bytes verbatim are four different
tails on one shared trunk.

## The `internal/upstream` package

A leaf package importing only `apierror`, `endpoint`, `identity`, `logging`, and
the standard library. Nothing in the repository imports it in reverse.

### `Call`

```go
// Call describes one authenticated upstream call, independent of transport.
type Call struct {
	// Route is the exact upstream path, joined onto the credential's base URL.
	Route endpoint.Route
	// Method is the outbound HTTP method. WebSocket callers pass MethodGet.
	Method string
	// Query is the inbound RawQuery, forwarded verbatim and never normalized.
	Query string
	// ForceQuery preserves a bare "?" from the inbound URL.
	ForceQuery bool
	// ClientHeader is the inbound header set to forward under the strip policy.
	// A nil value forwards no client header.
	ClientHeader http.Header
	// Body is the outbound body. A nil or http.NoBody value is wrapped so the
	// Transport treats an otherwise bodyless request as single-attempt.
	Body io.Reader
	// ContentLength is assigned to the outbound request only when non-zero, so a
	// sized body (a *bytes.Reader) keeps the length and GetBody that
	// http.NewRequestWithContext derives for it. A caller streaming an inbound
	// body passes r.ContentLength, including -1 for an unknown length.
	ContentLength int64
	// AcceptIdentityEncoding forces Accept-Encoding: identity, so the caller
	// receives an undecoded body it may inspect or stream.
	AcceptIdentityEncoding bool
}
```

`Call` deliberately takes a header set and a body rather than an `*http.Request`.
The current `authenticatedOutboundHeaders` takes an `*http.Request` in order to
reach both `.Header` and `.Context()`, which forces `forward.forward` to build a
shallow request copy purely to substitute the shim-rewritten header
(`forward.go:346–347`). That copy disappears.

### `Failure`

```go
// Failure is one classified upstream call failure, already mapped to the
// copilotd-originated signal that answers it.
type Failure struct {
	// Kind is the signal to render. Meaningless when ClientGone is set.
	Kind apierror.Kind
	// Message is the human-readable text rendered in the surface's dialect.
	Message string
	// ClientGone reports that the caller disconnected before the call resolved.
	// Nothing may be written; there is no one left to answer.
	ClientGone bool
	// Err is the underlying cause, for logs only. It is never rendered, so an
	// upstream URL or credential detail cannot reach a client.
	Err error
}

// RespondTo renders f on w in surface's dialect and reports whether it wrote.
// A ClientGone failure writes nothing and reports false, so callers can skip
// the metrics and logging that only a real failure warrants.
func (f *Failure) RespondTo(w http.ResponseWriter, surface endpoint.Surface) bool
```

Classification and response are one step because separating them is precisely
what let the WebSocket copy lose its cancel branch. `RespondTo`'s boolean is the
seam a caller needs for its own tail — `wsforward` books `AcceptDialFailed` only
when `RespondTo` reports `true`.

`Err` is carried but never rendered. Today `catalog`'s sentinel wrapping means the
cause travels in the error chain and is dropped at `writeFetchError`; keeping it
on a field preserves the information for logs without risking it on the wire.

### `Caller`

```go
// Caller applies copilotd's shared upstream-call policy and executes HTTP calls.
type Caller struct {
	provider         identity.Provider
	client           *http.Client
	logger           *slog.Logger
	outboundTimeout  time.Duration
	maxBufferedBytes int64
}

// Do executes call and returns the upstream response, or one classified
// failure. ctx governs cancellation of the upstream request; the caller owns
// ctx's cancel func and any deadline that should apply once the response
// arrives.
func (c *Caller) Do(ctx context.Context, call Call) (*http.Response, *Failure)

// Buffered executes call and reads the whole response body under the buffered
// cap, returning the upstream status and bytes. It owns its own post-response
// deadline, so a stalled read is classified as a timeout rather than a read
// failure.
func (c *Caller) Buffered(ctx context.Context, call Call) (int, []byte, *Failure)

// Prepare builds the authenticated outbound request without executing it.
// WebSocket callers consume only its URL and Header; the bound context is for
// the HTTP path.
func (c *Caller) Prepare(ctx context.Context, call Call) (*http.Request, *Failure)

// Classify maps an execution error to one Failure, consulting both err and
// ctx.Err().
func (c *Caller) Classify(ctx context.Context, err error) *Failure

// Correlate logs the upstream request id when it differs from copilotd's own.
func (c *Caller) Correlate(ctx context.Context, header http.Header)

// ReadBounded reads body under max, classifying an over-cap or failed read.
// It takes no context: a caller whose own deadline fires mid-read sees the
// read failure, matching the existing buffered-branch behaviour.
func ReadBounded(body io.Reader, max int64) ([]byte, *Failure)

// RequestIDHeader carries copilotd's resolved correlation id in both
// directions. It is the single declaration; server, forward, and wsforward
// consume it.
const RequestIDHeader = "X-Request-Id"
```

`Do` composes `Prepare` → `client.Do` → `Correlate` → `Classify`. `Buffered`
composes `Do` → its own deadline → `ReadBounded`. The three primitives are
exported only because `websocket.Dial` cannot be composed into `Do`. That is the
one transport-specific seam in the design, and it is named rather than implied.

**Deadlines stay with the caller.** `Do` returns as soon as it has a response.
The post-response outbound timer (`time.AfterFunc(outboundTimeout, cancel)`)
belongs to whoever consumes the body, because its correct scope differs per site:
armed for a buffered read, deliberately *not* armed for an SSE pump
(`forward.go:370–374`), and `sse.Pump` needs the `cancel` func itself
(`forward.go:397`). `Buffered` is the exception — it consumes the body, so it owns
the deadline, absorbing the `context.WithCancelCause` + `timeoutCause` plumbing
now hand-written at `forward.go:249–262`.

### The classification table

One table replaces five message sites and five `catalog` sentinels:

| Condition | `Kind` | Message |
|---|---|---|
| `provider.Current` returns an error | `NotReady` | `no upstream credential available` |
| request build fails (including a bad base URL) | `BadGateway` | `could not build the upstream request` |
| `ctx.Err()` is `context.Canceled` | — | `ClientGone`; nothing written |
| `err` or `ctx.Err()` is `context.DeadlineExceeded`, or the cancel cause is the outbound timeout | `GatewayTimeout` | `the upstream request timed out` |
| any other execution error | `BadGateway` | `could not reach the upstream` |
| body read fails | `BadGateway` | `could not read the upstream response` |
| body exceeds the buffered cap | `BadGateway` | `upstream response body exceeds the maximum allowed size` |

**Precedence is `DeadlineExceeded` before `Canceled`.** A context that timed out
reports `DeadlineExceeded`, so the order matters only when both an ancestor
cancel and a deadline are in play; classifying as a timeout is the informative
choice, and it preserves `wsforward`'s existing belt-and-braces check of both
`err` and `dialCtx.Err()` (`proxy.go:182`).

### The header policy

One file owns which headers cross which boundary in either direction.
`hopByHop`, `requestStrip`, `responseStrip`, `withExtra`, `connectionTokens`, and
`copyResponseHeaders` all move here from `forward`; `CopyResponseHeaders` is
exported for `forward`'s two response paths.

`requestStrip` gains a `Sec-WebSocket-*` prefix rule, so both current transports
consume the same centrally governed rule set without a per-transport parameter.

Outbound order, applied once:

1. Copy every `ClientHeader` entry except `requestStrip` and any name listed in
   the inbound `Connection` header.
2. Set `Authorization: Bearer <cred.Token>`.
3. Overlay `cred.Headers` (canonicalized, copied onto the fresh map, never
   mutated).
4. Set `RequestIDHeader` from the context, when one is resolved.
5. Set `Accept-Encoding: identity` when `AcceptIdentityEncoding` is set.

The `Sec-WebSocket-*` rule is load-bearing, not defensive. `coder/websocket`
v1.8.15 clones `DialOptions.HTTPHeader` and then `Set`s `Connection`, `Upgrade`,
`Sec-WebSocket-Version`, and `Sec-WebSocket-Key` over the top
(`dial.go:211–215`), so those four are harmless. But `Sec-WebSocket-Protocol`
survives the clone when `DialOptions.Subprotocols` is empty — which it always is
here. Forwarding a client's subprotocol offer upstream would let Copilot select
one, and `verifyServerResponse` would then reject the handshake as a protocol
violation (`dial.go:282`), because copilotd's own `websocket.Accept` negotiates no
subprotocol. Stripping the prefix is what makes forwarding client headers safe.

`singleAttemptBody` moves here too and is applied when `Call.Body` is nil or
`http.NoBody`. That matches both places that hand-apply it today
(`forward.go:173`, `forward.go:227`) and is inert for the `bytes.Reader` POST
path, whose `GetBody` the Transport may legitimately replay.

## Consumer changes

### An identifier collision to resolve first

`upstream` is currently a local variable name at `forward/forward.go:130`, `:156`,
and `:324` (`upstream := ep.Upstream()`) and at `wsforward/proxy.go:172`
(`upstream, response, err := websocket.Dial(...)`). Each shadows the new package
name inside the exact functions that must call it. They are renamed as part of
the step that touches each function — `upstreamRoute` in `forward` (matching
`wsforward/proxy.go:130`'s existing name) and `upstreamConn` in `wsforward`
(matching the `upstreamTransport` beside it). `forward.FetchModels`' `upstream`
parameter is deleted with the function.

### `forward.PassthroughHandler`

The 37-line credential/build/execute/classify block (`forward.go:159–195`)
becomes:

```go
resp, failure := f.caller.Do(ctx, upstream.Call{
	Route: upstreamRoute, Method: r.Method,
	Query: r.URL.RawQuery, ForceQuery: r.URL.ForceQuery,
	ClientHeader: r.Header, Body: r.Body, ContentLength: r.ContentLength,
})
if failure != nil {
	failure.RespondTo(w, surface)
	return
}
```

The tail keeps what is genuinely its own: the `cancel()`-before-`Body.Close()`
ordering and its comment, the outbound timer, `CopyResponseHeaders`, the HEAD
check, and the `io.Copy` through `sse.NewWriter`.

### `forward.forward`

The same block at `forward.go:326–364` collapses identically, passing
`ClientHeader: header` — the shim-rewritten set from `chain.RunRequest`. The
`headerRequest := *r` shallow copy disappears. The buffered branch
(`forward.go:412–424`) becomes `upstream.ReadBounded(resp.Body,
f.maxBufferedResponseBytes)` plus `failure.RespondTo`.

### `catalog.Handler`

`catalog` declares its own narrow seam, still trivially fake-able the way
`stubFetcher` is today:

```go
// Source fetches one current Copilot model catalog as bounded bytes.
type Source interface {
	Buffered(ctx context.Context, call upstream.Call) (int, []byte, *upstream.Failure)
}
```

`*upstream.Caller` satisfies it directly. `catalog/fetch.go` is deleted whole —
`FetchErrorKind`, its five constants, its `Error()` method, and the `Fetcher`
interface. `writeFetchError` is deleted; the handler calls `failure.RespondTo`.
The `if r.Context().Err() != nil { return }` guard at `handler.go:37` is deleted;
`ClientGone` carries that now.

`catalog`'s own non-200 case (`"upstream models request failed"`) stays in
`catalog`. It is not a call failure — the call succeeded and returned a status
the catalog cannot use.

### `wsforward.Proxy.Handler`

```go
outReq, failure := p.caller.Prepare(phaseCtx, upstream.Call{
	Route: upstreamRoute, Method: http.MethodGet,
	Query: r.URL.RawQuery, ForceQuery: r.URL.ForceQuery,
	ClientHeader: r.Header,
})
if failure != nil {
	if failure.RespondTo(w, surface) {
		p.metrics.observeAccept(acceptOutcome(failure))
	}
	return
}

// … dialCtx, httptrace GotConn …
upstreamConn, response, err := websocket.Dial(dialCtx, outReq.URL.String(),
	&websocket.DialOptions{
		HTTPClient:      p.dialClient,
		HTTPHeader:      outReq.Header,
		CompressionMode: websocket.CompressionDisabled,
	})
cancelDial()
if err != nil {
	if p.caller.Classify(dialCtx, err).RespondTo(w, surface) {
		p.metrics.observeAccept(AcceptDialFailed)
	}
	return
}
p.caller.Correlate(r.Context(), response.Header)
```

`Prepare` takes `phaseCtx`, not `dialCtx`: an on-demand credential mint is not
bounded by the dial timeout today (`proxy.go:152`) and must not become so.

`acceptOutcome` maps `NotReady` → `AcceptRejected` and everything else →
`AcceptDialFailed`, preserving today's split between a credential rejection and a
dial failure. The metric vocabulary stays in `wsforward`, where it belongs.

`wsforward.websocketURL` (`proxy.go:281–299`) is **deleted, not shrunk**.
`websocket.Dial` accepts an `https://` URL directly, rewriting the scheme itself
(`dial.go:193–201`), so `outReq.URL.String()` is passed unchanged. Its
`"could not build the upstream WebSocket URL"` failure mode collapses into the
module's build-failure classification.

### `server`

`server/middleware.go:15` uses `upstream.RequestIDHeader`, retiring the third
declaration. `server` already imports `forward`, `wsforward`, and `catalog`;
`upstream` is a leaf below all three, so no cycle is possible.

### Two constructors shrink

`Caller` owns the provider and the outbound client. `forward.Forwarder` drops
both fields — `provider` is read only at `forward.go:159`, `:221`, `:326` and
`client` only at `:184`, `:235`, `:350`, all of which become `Caller` calls. So
`forward.New` is net **−1** parameter (loses `provider` and `client`, gains
`caller`) and `wsforward.New` is net **0** (loses `provider`, gains `caller`,
keeps `dialClient` for `DialOptions.HTTPClient`).

`main` constructs one `Caller` and injects it into `forward.New`, `wsforward.New`,
and — through `server.New`, which already carries `catalog.CodexDescriptor` — the
catalog handler.

### Deletions

| Package | Removed |
|---|---|
| `forward` | `requestIDHeader`, `logUpstreamRequestID`, `hopByHop`, `requestStrip`, `responseStrip`, `withExtra`, `outboundHeaders`, `authenticatedOutboundHeaders`, `copyResponseHeaders`, `connectionTokens`, `singleAttemptBody`, `FetchModels`, `var _ catalog.Fetcher`, the `catalog` import |
| `wsforward` | `requestIDHeader`, `logUpstreamRequestID`, `upstreamHeaders`, `isHandshakeHeader`, `websocketURL` |
| `catalog` | `fetch.go` entire (`FetchErrorKind`, five constants, `Error()`, `Fetcher`), `writeFetchError`, the `r.Context().Err()` guard |
| `server` | `requestIDHeader` |

Roughly 150 of `forward.go`'s 590 lines leave outright; another ~45 collapse at
the two handlers.

## Behaviour changes

Four, all deliberate.

**1. WebSocket forwards inbound client headers.** Under the shared strip policy,
extended with `Sec-WebSocket-*`. The two transports of
`/openai/v1/responses` stop disagreeing.

**2. WebSocket client-cancel is silent.** A disconnect during the upstream
handshake writes nothing and books no `AcceptDialFailed`, matching the HTTP
paths. The current behaviour writes a 502 to a socket with no reader and records
an upstream dial failure that did not occur.

**3. Over-cap upstream response body returns 502, not 413.** `forward`'s buffered
path currently returns `PayloadTooLarge`; 413 describes the inbound request
entity, not an upstream response. `FetchModels`' existing 502 mapping is the
correct one and becomes the only one.

**4. The URL join trims a trailing slash.** `strings.TrimRight(cred.BaseURL, "/")
+ string(route)`, matching the repo's six other base-URL joins. Invisible with
the production base URL; correct if the exchange ever returns one with a trailing
slash.

One message refinement follows from the shared table: `catalog`'s single 502 text
`"could not fetch the upstream models catalog"` splits into the three specific
messages above. **Every status code `catalog` returns today is preserved** — 503
for a missing credential, 504 for a timeout, 502 for the rest.

### No divergence-ledger change

The ledger's Fabrication row points at `internal/apierror`'s `Kind` enum as its
source of truth and enumerates neither statuses nor messages; the enum is
untouched. Changes 1 and 2 *reduce* fabrication — more verbatim forwarding, one
fewer invented 502. The request-direction strip set was never ledgered: the
taxonomy governs copilotd's wire *output*, and hop-by-hop stripping on the way
upstream is ordinary proxy behaviour, not a divergence.

## Testing

`internal/upstream` owns four tables that exist nowhere today.

**Header policy.** The strip set (hop-by-hop, `Authorization`, `X-Api-Key`,
`Host`, `Content-Length`, `Sec-WebSocket-*`, and names listed in the inbound
`Connection` header); `cred.Headers` overlaying a same-named client header;
`RequestIDHeader` set from the context and absent without one;
`Accept-Encoding: identity` present only when asked; `cred.Headers` not mutated.

**Classification.** All seven rows, plus the precedence rule: a context that is
both cancelled and past its deadline classifies as a timeout, and both `err` and
`ctx.Err()` are consulted.

**`RespondTo`.** Three Surfaces × representative Kinds, asserting the dialect and
status; `ClientGone` writes no bytes, sets no status, and returns `false`.

**`Buffered` / `ReadBounded`.** Bodies under, at, and over the cap; a read error;
a stalled read classified as a timeout via the cancel cause rather than as a read
failure.

Migrations:

- `forward/models_fetch_test.go` (488 lines) moves to `upstream` largely intact
  as the `Buffered` suite.
- `catalog/handler_test.go:204–216` rewrites its error table onto
  `*upstream.Failure` stubs. Every status expectation carries over unchanged;
  three 502 messages become more specific. The `"unknown fetch error"` row is
  deleted rather than rewritten — it exercised `writeFetchError`'s `default`
  branch for an error matching none of the five sentinels, and `*Failure` is
  total, so the case it covered can no longer be constructed.
- `wsforward/proxy_test.go` gains two cases that fail against today's code: a
  client-cancel mid-handshake asserting no response bytes and no metric, and a
  client-header forwarding case asserting that a custom inbound header reaches the
  upstream handshake while `Sec-WebSocket-Protocol` does not.

**One structural guard**, in the spirit of the config descriptor invariants: a
test using stdlib `go/build` asserting that `internal/forward`'s import set
excludes `internal/catalog`, so the edge cannot quietly return. No new
dependency.

## Migration order

Each step compiles and passes the full suite before the next begins.

1. **`internal/upstream` with no callers.** `Call`, `Failure`, `Caller`,
   `ReadBounded`, `RequestIDHeader`, the header policy, and the four test tables.
   Written test-first: the tables are the specification.
2. **Move the header policy.** `forward` deletes its header block and calls into
   `upstream`; `CopyResponseHeaders` is repointed. Behaviour-neutral, so
   `forward`'s existing suite is the regression check.
3. **The two HTTP handlers onto `Do`.** `PassthroughHandler` and `forward`.
   Behaviour changes 3 and 4 land here, with their tests.
4. **`Buffered` and `catalog.Source`.** Delete `catalog/fetch.go`,
   `forward.FetchModels`, and the `catalog` import; add the import guard.
5. **`wsforward` onto `Prepare` / `Classify` / `Correlate`.** Delete
   `websocketURL`, `upstreamHeaders`, `isHandshakeHeader`, and the duplicate
   `logUpstreamRequestID`. Behaviour changes 1 and 2 land here, with their tests.
6. **The shared constant and the docs.** `server/middleware.go`, CONTEXT.md,
   ADR-0013.

## CONTEXT.md changes

One new entry under *Surfaces & forwarding*, after **Forwarder**:

> **Upstream call**:
> The single authenticated request copilotd makes to GitHub Copilot on behalf of
> one inbound request — credential, base URL join, header policy, request-id
> correlation, and failure classification, centrally governed across every
> transport and endpoint. Lives in `internal/upstream`; what a caller does with
> the response (pump, upgrade, decode, copy) stays with the caller.
> _Avoid_: outbound request (unqualified), fetch.

No other entry changes. **Forwarder** still names the dumb core; the upstream call
is the edge it now shares with the WebSocket forwarder and the Catalog.

## Alternatives considered

**Full transport ownership.** The module could own `websocket.Dial` too, so
exactly one place in the repo knows how an upstream call is made. Rejected: the
WebSocket dial must surface the raw `net.Conn` for force-close authority
(`proxy.go:167–171`, `rawNetworkConn`), which is a `wsforward`-only concern that
would appear in a shared module's signature as a fourth return value. It would
also pull `coder/websocket` into `forward`'s import graph for a library `forward`
never uses.

**Classify without responding.** The module could return `*Failure` and leave
every site to write its own `apierror.Write` / silent-return switch. Rejected:
that is the exact four-way switch whose WebSocket copy lost its cancel branch.
Concentrating the classifier without concentrating the response would fix the
symptom and leave the mechanism.

**Keep `catalog`'s sentinel vocabulary** as the module's error type. Rejected: it
preserves a second enum that round-trips out to catalog words and back to the
same `apierror.Kind`s, which is the duplication rather than a fix for it.

**Executor callback** — `Exchange(ctx, call, exec func(*http.Request)
(*http.Response, error))`, letting WebSocket pass a dialing closure. Rejected:
the closure would need a side channel to return the `*websocket.Conn` and the raw
transport, which is less legible than three named primitives.

**Stop at the response**, leaving `FetchModels` on the `Forwarder` and the
bounded read written twice. Rejected: the read is genuinely duplicated and the
two copies disagree on the over-cap status, which is a defect the concentration
resolves for free.

## Related decisions

- [ADR-0007](../adr/0007-served-endpoints-as-typed-contracts.md) — the Endpoint is
  the typed contract this module consumes; `Call.Route` comes from
  `ep.Upstream()`, never from a literal.
- [ADR-0003](../adr/0003-synthesized-stream-terminals-off-band-origin.md) — the
  post-commit stream path this module deliberately does not touch.
- ADR-0013 (**new, with this design**) — the shared policy for every authenticated
  call to GitHub Copilot is governed by `internal/upstream`.
- [Divergence ledger](../divergence-ledger.md) — unchanged; see
  [No divergence-ledger change](#no-divergence-ledger-change).
