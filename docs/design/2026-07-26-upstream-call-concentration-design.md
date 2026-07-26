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

Four of the drifts are live defects and are fixed here, not preserved: the
WebSocket transport silently discards every inbound client header that the HTTP
transport of the *same* endpoint forwards; the WebSocket failure classifier has
no client-cancel branch, so a mid-handshake disconnect writes a 502 to a dead
socket and books a spurious `AcceptDialFailed`; `forward`'s buffered read has no
client-cancel branch either, so a disconnect mid-body writes a 502 to a socket
with no reader; and the two buffered-read paths disagree on the status for an
over-cap upstream body. Two further changes follow from the unified URL join and
the unified failure vocabulary.

The change also removes the `forward` → `catalog` import: `catalog` currently
declares the `Fetcher` interface that `forward` implements, while `forward`
imports `catalog` for error sentinels that round-trip out to catalog vocabulary
and back to the same `apierror.Kind`s — losing three of the five distinct
messages on the way.

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

And the *buffered read* has its own gap, independent of the four execution
branches above. `forward.go:417–419` writes `BadGateway` on any read failure with
no cancel check at all, so a client that disconnects mid-body gets a 502 on a
socket with no reader — the same defect as `wsforward`'s. `catalog` escapes it
only because `handler.go:37`'s guard is total and catches read failures too. Any
design that replaces that guard with a classification confined to execution
errors would silently level `catalog` down to `forward`'s behaviour.

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
directly. **All five** sentinel messages are verbatim copies of `apierror`
call-site messages elsewhere in `forward` — `fetch.go:25`/`forward.go:161`,
`:27`/`:177`, `:29`/`:192`, `:31`/`:190`, `:33`/`:418`.

The round-trip is also lossy, not merely redundant. `writeFetchError` has
**three** branches, not five: `ErrNoCredential` → 503 and `ErrUpstreamTimeout` →
504 survive, while `ErrBuildUpstream`, `ErrUpstreamUnreachable`, and
`ErrUpstreamRead` all collapse into one generic 502 whose message
(`"could not fetch the upstream models catalog"`) discards the three distinct
strings the enum went to the trouble of carrying. The second vocabulary costs a
package, an enum, and an import edge, and then throws away the only thing it
added.

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
- Fix the WebSocket header drop and the two missing client-cancel branches.
- Leave the module's interface as the test surface: the header policy and the
  classification table become tables, tested once.

## Non-goals

- **No transport abstraction.** `websocket.Dial` is not wrapped behind a
  `Do`-shaped interface. See
  [Alternatives considered](#alternatives-considered).
- **No config-struct constructors.** Shrinking `forward.New`'s nine positional
  parameters is a separate candidate. This design happens to take it to seven,
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
	// Kind is the signal to render. It is not consulted when ClientGone is set;
	// RespondTo's boolean is the gate every caller already goes through, so no
	// site reads Kind on a ClientGone failure. (apierror.Kind's zero value is
	// Unauthorized, so reading it ungated would render a 401.)
	Kind apierror.Kind
	// Message is the human-readable text rendered in the surface's dialect.
	Message string
	// ClientGone reports that the caller disconnected before the call resolved.
	// Nothing may be written; there is no one left to answer.
	ClientGone bool
	// Err is the underlying cause. Caller logs it once at classification; it is
	// never rendered, so an upstream URL or credential detail cannot reach a
	// client. The field is exported so a caller may add it to its own
	// structured logs, and so tests can assert on it.
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

**`Err` gains a consumer, which it does not have today.** Not one of the four
current sites logs the cause: `forward.go:186–194`, `:351–362`, `:238–241`, and
`proxy.go:178–189` all discard it, and `catalog`'s sentinel wrapping carries it
only as far as `writeFetchError`, which drops it. `Caller` logs it once at
classification, where it holds both the cause and a logger. That is a **new
observable** — one `WarnContext` per classified upstream failure, carrying `err`
— not a preserved one, and it is called out here rather than left for whoever
notices the log volume change.

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
// failure. ctx is used verbatim — Do never derives a child — so the caller
// keeps sole authority over cancellation and owns any deadline that should
// apply once the response arrives.
func (c *Caller) Do(ctx context.Context, call Call) (*http.Response, *Failure)

// Buffered executes call and reads the whole response body under the buffered
// cap, returning the upstream status and bytes. It consumes and closes the
// response body, so it owns the post-response deadline: a stalled read is
// classified as a timeout rather than a read failure, and a read that fails
// because the inbound client disconnected is classified as ClientGone.
func (c *Caller) Buffered(ctx context.Context, call Call) (int, []byte, *Failure)

// Prepare builds the authenticated outbound request without executing it.
// WebSocket callers consume only its URL and Header; the bound context is for
// the HTTP path.
func (c *Caller) Prepare(ctx context.Context, call Call) (*http.Request, *Failure)

// Classify maps an execution error to one Failure, consulting both err and
// ctx.Err(), and logging the cause once.
func (c *Caller) Classify(ctx context.Context, err error) *Failure

// Correlate logs the upstream request id when it differs from copilotd's own.
func (c *Caller) Correlate(ctx context.Context, header http.Header)

// ReadBounded reads body under the Caller's configured cap. It is the form
// every call site uses, so the cap has exactly one owner.
func (c *Caller) ReadBounded(body io.Reader) ([]byte, *Failure)

// ReadBounded reads body under max, classifying an over-cap or failed read.
// It is the pure primitive the tables drive; it takes no context, so a caller
// whose own deadline fires mid-read sees the read failure and decides what that
// means. Caller.ReadBounded and Buffered are what production code calls.
func ReadBounded(body io.Reader, max int64) ([]byte, *Failure)

// RequestIDHeader carries copilotd's resolved correlation id in both
// directions. It is the single declaration; server, forward, and wsforward
// consume it.
const RequestIDHeader = "X-Request-Id"
```

`Do` composes `Prepare` → `client.Do` → `Correlate` → `Classify`. The three
primitives are exported only because `websocket.Dial` cannot be composed into
`Do`. That is the one transport-specific seam in the design, and it is named
rather than implied.

**`Buffered` wraps the context before calling `Do`, not after.** A response body
is cancellable only through the context its *request* was built with, so a timer
armed on a context created after `Do` returns cannot interrupt a stalled read —
it would hang until the parent died and then classify as a read failure. Today's
`FetchModels` gets this right by construction (`forward.go:226–227`: the
`WithCancelCause` precedes `http.NewRequestWithContext`), and `Buffered` must
preserve the ordering:

```
inner, cancel := context.WithCancelCause(ctx)   // ← before Do
resp, failure := c.Do(inner, call)              // ← body bound to inner
… arm time.AfterFunc(c.outboundTimeout, func() { cancel(timeoutCause) }) …
body, failure := c.ReadBounded(resp.Body)
```

That ordering is what makes the stalled-read test meaningful rather than
accidentally green.

**A failed buffered read consults the context before it answers.** `ReadBounded`
stays context-free — it is the low-level primitive — and `Buffered`, which holds
the context, decides what the failure meant:

| `context.Cause(inner)` | result |
|---|---|
| the outbound-timeout sentinel | `GatewayTimeout` |
| `context.Canceled`, propagated from the inbound request | `ClientGone` |
| neither | `BadGateway`, read failure |

Without this, deleting `catalog/handler.go:37`'s guard would reintroduce the
exact defect behaviour change 2 removes from the WebSocket path — a 502 written
to a socket with no reader — and would leave `forward`'s buffered branch
(`forward.go:417–419`, which has no cancel check today) unfixed.

**Deadlines stay with the caller.** `Do` returns as soon as it has a response.
The post-response outbound timer (`time.AfterFunc(outboundTimeout, cancel)`)
belongs to whoever consumes the body, because its correct scope differs per site:
armed for a buffered read, deliberately *not* armed for an SSE pump
(`forward.go:370–374`), and `sse.Pump` needs the `cancel` func itself
(`forward.go:397`). That is also why `Do` must not derive its own child context:
it would have to hand a fourth return value back for `sse.Pump`. `Buffered` is
the exception — it consumes the body, so it owns the deadline, absorbing the
`context.WithCancelCause` + `timeoutCause` plumbing now hand-written at
`forward.go:249–262`.

**`outboundTimeout` legitimately has two holders; `maxBufferedBytes` has one.**
The line is consumption, not naming. The cap is read only by code `upstream`
owns (`ReadBounded`), so `forward` drops its `maxBufferedResponseBytes` field
and calls `c.ReadBounded`. The timeout is read by timers `forward` genuinely
owns — the SSE-exempt one at `forward.go:371–374` and the passthrough one at
`:205` — and by `Buffered`. Two real consumers, two legitimate holders, one
config value wired twice on purpose.

### The classification table

One table replaces five message sites, five `catalog` sentinels, and
`wsforward`'s two transport-specific strings:

| Condition | `Kind` | Message |
|---|---|---|
| `provider.Current` returns an error | `NotReady` | `no upstream credential available` |
| request build fails (including a bad base URL) | `BadGateway` | `could not build the upstream request` |
| `ctx.Err()` is `context.Canceled` | — | `ClientGone`; nothing written |
| `err` or `ctx.Err()` is `context.DeadlineExceeded`, or the cancel cause is the outbound timeout | `GatewayTimeout` | `the upstream request timed out` |
| any other execution error | `BadGateway` | `could not reach the upstream` |
| body read fails, inbound request cancelled | — | `ClientGone`; nothing written |
| body read fails otherwise | `BadGateway` | `could not read the upstream response` |
| body exceeds the buffered cap | `BadGateway` | `upstream response body exceeds the maximum allowed size` |

**The table is total across transports.** `wsforward` currently writes
`"the upstream WebSocket handshake timed out"` and
`"could not reach the upstream WebSocket"` (`proxy.go:183`, `:187`); under the
shared table those become the generic rows. That is deliberate — see behaviour
change 5. A `Call`-level message override was considered and rejected: its only
purpose would be to inject the word "WebSocket" into two strings, re-asserting
the transport distinction this module exists to erase, and the transport is
already implied by the request the client made.

**Precedence is `DeadlineExceeded` before `Canceled`.** A context that timed out
reports `DeadlineExceeded`, so the order matters only when both an ancestor
cancel and a deadline are in play; classifying as a timeout is the informative
choice, and it preserves `wsforward`'s existing belt-and-braces check of both
`err` and `dialCtx.Err()` (`proxy.go:182`). On the buffered path the
discrimination is by cancel *cause*, not by `ctx.Err()`, since `Buffered`'s own
timer and an inbound disconnect both surface as `context.Canceled` on the
derived context.

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
   mutated), **unfiltered**.
4. Set `RequestIDHeader` from the context, when one is resolved.
5. Set `Accept-Encoding: identity` when `AcceptIdentityEncoding` is set.

**Step 3 is deliberately unfiltered, and that retires a guard.** The strip set
applies to `ClientHeader` at step 1 only. `wsforward.isHandshakeHeader` exists
today for exactly one purpose — filtering `cred.Headers` (`proxy.go:303`), not
client headers, which `wsforward` does not forward at all — so the new
`Sec-WebSocket-*` rule is not its successor; the two guard opposite sides. The
guard is dropped because the credential set is closed and copilotd-constructed:
`impersonation.Set.Header()` (`set.go:72–84`) emits only `Editor-Version`,
`Editor-Plugin-Version`, `User-Agent`, `Copilot-Integration-Id`, and
`X-GitHub-Api-Version`, so no handshake name can appear. `forward` has overlaid
`cred.Headers` unfiltered since day one (`forward.go:551`); this adopts that
policy rather than inventing one. Re-adding a guard at step 3 would require a
second strip set — "`requestStrip` minus `Authorization`", since step 2
deliberately sets a name that is *in* `requestStrip` — and a near-duplicate set
is the pathology this design exists to remove.

The `Sec-WebSocket-*` rule is load-bearing, not defensive. `coder/websocket`
v1.8.15 clones `DialOptions.HTTPHeader` and then `Set`s `Connection`, `Upgrade`,
`Sec-WebSocket-Version`, and `Sec-WebSocket-Key` over the top
(`dial.go:211–215`), so those four are harmless. **Two names survive the clone:**

- `Sec-WebSocket-Protocol` is `Set` only when `DialOptions.Subprotocols` is
  non-empty (`dial.go:216–218`) — which it never is here. Forwarding a client's
  subprotocol offer upstream would let Copilot select one, and `verifySubprotocol`
  would then reject the handshake as a protocol violation (`dial.go:282`, reached
  from `verifyServerResponse` at `:270`), because copilotd's own
  `websocket.Accept` negotiates no subprotocol.
- `Sec-WebSocket-Extensions` is `Set` only when compression is negotiated
  (`dial.go:219–221`), and copilotd dials with `CompressionMode:
  CompressionDisabled`, so `copts` is nil and the header is left untouched.
  Forwarding a client's `permessage-deflate` offer would let Copilot accept it,
  and `verifyServerExtensions` fails on any extension when `copts` is nil.

Same failure mode, two headers. Stripping the prefix covers both, and is what
makes forwarding client headers safe.

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
(`forward.go:412–424`) becomes `f.caller.ReadBounded(resp.Body)` plus
`failure.RespondTo`; `Forwarder` drops its `maxBufferedResponseBytes` field, so
the cap is read from the one place that owns it.

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

The `if r.Context().Err() != nil { return }` guard at `handler.go:37` is deleted,
**and this is only safe because `Buffered` upgrades a cancelled read to
`ClientGone`.** Today's guard is total — it swallows any fetch error when the
client is gone, read failures included. A `ClientGone` produced only by
`Classify` would be partial: a disconnect while draining the models body would
surface as `BadGateway` and write a 502 to a dead socket. The buffered-path
cause check in [`Caller`](#caller) is what makes the deletion honest.

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
(`dial.go:194–202`), so `outReq.URL.String()` is passed unchanged.

Its `"could not build the upstream WebSocket URL"` failure mode does not collapse
into build-failure classification, and the doc should not claim it does. A base
URL that is *parseable but wrongly schemed* now builds a perfectly valid
`*http.Request` at `Prepare` and fails inside `websocket.Dial` with
`"unexpected url scheme"`, which `Classify` maps by the "any other execution
error" row to `BadGateway` / `"could not reach the upstream"`. Same status, same
`AcceptDialFailed` metric, different stage — and one fewer message, since the
scheme check no longer has its own.

### `server`

`server/middleware.go:15` uses `upstream.RequestIDHeader`, retiring the third
declaration. `server` already imports `forward`, `wsforward`, and `catalog`;
`upstream` is a leaf below all three, so no cycle is possible.

### Two constructors shrink

`Caller` owns the provider, the outbound client, and the buffered cap.
`forward.Forwarder` drops all three fields — `provider` is read only at
`forward.go:159`, `:221`, `:326`; `client` only at `:184`, `:235`, `:350`; and
`maxBufferedResponseBytes` only at `:254`, `:264`, `:413`, `:421` — all of which
become `Caller` calls. So `forward.New` is net **−2** parameters (loses
`provider`, `client`, and `maxBufferedResponseBytes`, gains `caller`, leaving
seven) and `wsforward.New` is net **0** (loses `provider`, gains `caller`, keeps
`dialClient` for `DialOptions.HTTPClient`).

`outboundTimeout` stays on both `Caller` and `Forwarder` on purpose; see
[`Caller`](#caller) for why consumption, not naming, is the line.

`main` constructs one `Caller` and injects it into `forward.New`, `wsforward.New`,
and — through `server.New`, which already carries `catalog.CodexDescriptor` — the
catalog handler.

### Deletions

| Package | Removed |
|---|---|
| `forward` | `requestIDHeader`, `logUpstreamRequestID`, `hopByHop`, `requestStrip`, `responseStrip`, `withExtra`, `outboundHeaders`, `authenticatedOutboundHeaders`, `copyResponseHeaders`, `connectionTokens`, `singleAttemptBody`, `FetchModels`, `var _ catalog.Fetcher`, the `provider`, `client`, and `maxBufferedResponseBytes` fields, the `catalog` import |
| `wsforward` | `requestIDHeader`, `logUpstreamRequestID`, `upstreamHeaders`, `isHandshakeHeader`, `websocketURL` |
| `catalog` | `fetch.go` entire (`FetchErrorKind`, five constants, `Error()`, `Fetcher`), `writeFetchError`, the `r.Context().Err()` guard |
| `server` | `requestIDHeader` |

Roughly 150 of `forward.go`'s 590 lines leave outright; another ~45 collapse at
the two handlers.

## Behaviour changes

Six, all deliberate.

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

**5. The two WebSocket-specific failure messages unify.**
`"the upstream WebSocket handshake timed out"` becomes
`"the upstream request timed out"`, and `"could not reach the upstream WebSocket"`
becomes `"could not reach the upstream"` (`proxy.go:183`, `:187`). Statuses are
unchanged — 504 and 502 as today. The distinction the old strings carried is not
one a client can act on: the transport is already implied by the request it made,
and keeping it would mean a per-transport message override on `Call` whose only
job is to inject one word. `preupgrade_test.go:163` and `:190` assert these byte-
exactly and are updated with the change.

**6. A buffered read that fails because the client disconnected is silent.**
`forward`'s buffered branch writes `BadGateway` today with no cancel check
(`forward.go:417–419`), so a mid-body disconnect puts a 502 on a socket with no
reader — the same defect as change 2, on the other path. `Buffered` now
classifies it as `ClientGone`. On the `catalog` path this *preserves* current
behaviour, which `handler.go:37`'s guard already provided; on the `forward` path
it is new.

One message refinement follows from the shared table: `catalog`'s single 502 text
`"could not fetch the upstream models catalog"` splits into the three specific
messages above. **Every status code `catalog` returns today is preserved** — 503
for a missing credential, 504 for a timeout, 502 for the rest.

One new observable, not a behaviour change on the wire: `Caller` logs the
underlying cause once per classified failure (see [`Failure`](#failure)). No
current site logs it, so this is added log volume — roughly one `WarnContext` per
upstream failure.

### No divergence-ledger change

The ledger's Fabrication row points at `internal/apierror`'s `Kind` enum as its
source of truth and enumerates neither statuses nor messages; the enum is
untouched. Changes 1, 2, and 6 *reduce* fabrication — more verbatim forwarding,
two fewer invented 502s. Change 5 rewords two fabricated messages without adding
or removing one. The request-direction strip set was never ledgered: the
taxonomy governs copilotd's wire *output*, and hop-by-hop stripping on the way
upstream is ordinary proxy behaviour, not a divergence.

## Testing

`internal/upstream` owns four tables that exist nowhere today.

**Header policy.** The strip set (hop-by-hop, `Authorization`, `X-Api-Key`,
`Host`, `Content-Length`, `Sec-WebSocket-*`, and names listed in the inbound
`Connection` header); `cred.Headers` overlaying a same-named client header;
`RequestIDHeader` set from the context and absent without one;
`Accept-Encoding: identity` present only when asked; `cred.Headers` not mutated.

**Classification.** All eight rows, plus the precedence rule: a context that is
both cancelled and past its deadline classifies as a timeout, and both `err` and
`ctx.Err()` are consulted. Plus: every classified failure logs its cause exactly
once, and the cause never appears in the rendered body.

**`RespondTo`.** Three Surfaces × representative Kinds, asserting the dialect and
status; `ClientGone` writes no bytes, sets no status, and returns `false`.

**`Buffered` / `ReadBounded`.** Bodies under, at, and over the cap; a read error;
a stalled read classified as a timeout via the cancel cause rather than as a read
failure; **a read failing because the parent context was cancelled classified as
`ClientGone`, not as a read failure** — the case that makes deleting
`catalog/handler.go:37` safe. `Caller.ReadBounded` and the free `ReadBounded`
agree on every row.

Migrations:

- `forward/models_fetch_test.go` (488 lines) moves to `upstream` largely intact
  as the `Buffered` suite.
- `catalog/handler_test.go:204–216` rewrites its error table onto
  `*upstream.Failure` stubs. Every status expectation carries over unchanged;
  three 502 messages become more specific. The `"unknown fetch error"` row is
  deleted rather than rewritten — it exercised `writeFetchError`'s `default`
  branch for an error matching none of the five sentinels, and `*Failure` is
  total, so the case it covered can no longer be constructed.
- `wsforward/preupgrade_test.go:163` and `:190` update their expected message
  strings for behaviour change 5. Statuses are unchanged, so only the two body
  literals move.
- `wsforward/proxy_test.go` gains two cases that fail against today's code: a
  client-cancel mid-handshake asserting no response bytes and no metric, and a
  client-header forwarding case asserting that a custom inbound header reaches the
  upstream handshake while `Sec-WebSocket-Protocol` and `Sec-WebSocket-Extensions`
  do not.

**Two structural guards**, in the spirit of the config descriptor invariants and
`endpoint/api_boundary_test.go`, in one stdlib `go/build` test — no new
dependency:

1. **`internal/upstream`'s imports match an allowlist** — `apierror`, `endpoint`,
   `identity`, `logging`, and the standard library, and nothing else. This is the
   load-bearing invariant: every consumer decision in this design rests on
   `upstream` being a leaf, and it is the one an ordinary change breaks by
   accident. An allowlist rather than a denylist, so a *new* internal import fails
   loudly and forces a deliberate amendment here.
2. **`internal/forward`'s import set excludes `internal/catalog`**, so the removed
   edge cannot quietly return.

## Migration order

Each step compiles and passes the full suite before the next begins.

1. **`internal/upstream` with no callers.** `Call`, `Failure`, `Caller`,
   `ReadBounded`, `RequestIDHeader`, the header policy, and the four test tables.
   Written test-first: the tables are the specification.
2. **Move the header policy.** `forward` deletes its header block and calls into
   `upstream`; `CopyResponseHeaders` is repointed. Behaviour-neutral *except* the
   `Sec-WebSocket-*` strip, which now applies on the HTTP path too — inert in
   practice, since nothing sends a handshake header to `POST /v1/messages`. So
   `forward`'s existing suite is the regression check.
3. **The two HTTP handlers onto `Do`.** `PassthroughHandler` and `forward`.
   Behaviour changes 3 and 4 land here, with their tests.
4. **`Buffered` and `catalog.Source`.** Delete `catalog/fetch.go`,
   `forward.FetchModels`, `Forwarder.maxBufferedResponseBytes`, and the `catalog`
   import; add both import guards. Behaviour change 6 lands here, with its tests.
5. **`wsforward` onto `Prepare` / `Classify` / `Correlate`.** Delete
   `websocketURL`, `upstreamHeaders`, `isHandshakeHeader`, and the duplicate
   `logUpstreamRequestID`. Behaviour changes 1, 2, and 5 land here, with their
   tests.
6. **The shared constant and the docs.** `server/middleware.go`, CONTEXT.md,
   ADR-0013.

## CONTEXT.md changes

One new entry under *Surfaces & forwarding*, after **Forwarder**:

> **Upstream call**:
> The single authenticated request copilotd makes to GitHub Copilot on behalf of
> one inbound request — credential, base URL join, header policy, request-id
> correlation, bounded reading of the response, and failure classification,
> centrally governed across every transport and endpoint. Lives in
> `internal/upstream`. Reading a bounded response body is part of the call;
> **interpreting** it is not — pumping, upgrading, decoding, and copying stay
> with the caller.
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
- [Token usage meter](2026-07-26-token-usage-meter-design.md) — **in flight, and
  ordered after this one.** The two were designed in parallel and both originally
  claimed ADR-0013; this design takes 0013 and the meter takes 0014, because this
  one is a refactor with no new dependency while the meter adds a package, a
  schema, a background writer, and a SQLite dependency. Behaviour change 3 also
  moves ground the meter stands on: its §11.1 and §12 describe an over-cap
  non-stream response as a 413, which becomes a 502 here. Landing this first means
  the meter is written against a settled buffered-read path rather than one moving
  underneath it.
- [Divergence ledger](../divergence-ledger.md) — unchanged; see
  [No divergence-ledger change](#no-divergence-ledger-change).
