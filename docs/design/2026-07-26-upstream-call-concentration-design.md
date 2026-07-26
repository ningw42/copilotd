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

Four of the drifts are live defects, fixed here rather than preserved: a dropped
client header set on the WebSocket transport, two missing client-cancel branches,
and a disagreement over the status for an over-cap upstream body. Four further
changes follow from the unified URL join, failure vocabulary, timeout
classification, and response cleanup policy. All eight are enumerated once, in
[Behaviour changes](#behaviour-changes).

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
  `forward/forward.go:161`, `:332`, `wsforward/proxy.go:154`,
  `catalog/fetch.go:25`, `catalog/handler.go:107`.

### The copies have drifted into behaviour, not just repetition

**Client headers.** `forward.authenticatedOutboundHeaders` copies every inbound
header except the strip set. `wsforward.upstreamHeaders` builds a *fresh* map
from `cred.Headers` alone and forwards no client header at all. These serve the
same logical endpoint — `POST` and `GET /openai/v1/responses` — so a client's
custom header reaches Copilot on one transport and vanishes on the other. Neither
function documents the choice; `upstreamHeaders` has no doc comment.

**Client cancel.** There are **three** policies, not two: `forward` and
`PassthroughHandler` return silently, counting nothing; `wsforward` has **no such
branch** (`proxy.go:187–188`), so a mid-handshake disconnect writes a 502 to a
dead socket and books a spurious `AcceptDialFailed`; and `FetchModels` computes a
classification that `catalog/handler.go:37` then discards, catching the
disconnect one level up with a total `if r.Context().Err() != nil { return }`
guard.

The *buffered read* has its own gap, independent of those four branches.
`forward.go:417–419` writes `BadGateway` on any read failure with no cancel check
at all. `catalog` escapes it only because its guard is total and catches read
failures too — so any design that replaces that guard with a classification
confined to execution errors would silently level `catalog` down to `forward`'s
behaviour.

**Over-cap upstream body.** `FetchModels` returns `ErrUpstreamRead` → 502;
`forward` returns `PayloadTooLarge` → 413 for the same condition. 413 describes
the *inbound request entity*, not an upstream response, so the `forward` mapping
is the wrong one.

**Base URL join.** The three `forward` sites concatenate raw:
`cred.BaseURL + string(upstream)`. `wsforward.websocketURL` trims first. Six
other base-URL joins in the repo — across `catalog`, `impersonation`, and
`identity` — use `strings.TrimRight(base, "/") + path`. Only `forward` deviates,
and `cred.BaseURL` is upstream-supplied, from the exchange response, so a
trailing slash is not hypothetical.

### The error vocabulary round-trips

`catalog` declares `FetchErrorKind` with five constants and the `Fetcher`
interface. `forward` imports `catalog` solely to wrap its failures in those
sentinels; `catalog.writeFetchError` then unwraps them back into the same
`apierror.Kind`s the other three sites produce directly, and **all five** sentinel
messages are verbatim copies of `apierror` call-site messages elsewhere in
`forward`.

The round-trip is also lossy. `writeFetchError` has **three** branches, not five:
`ErrNoCredential` → 503 and `ErrUpstreamTimeout` → 504 survive, while
`ErrBuildUpstream`, `ErrUpstreamUnreachable`, and `ErrUpstreamRead` collapse into
one generic 502 whose message discards the three distinct strings the enum went
to the trouble of carrying. The second vocabulary costs a package, an enum, and
an import edge, and then throws away the only thing it added — leaving that edge
pointing from the dumb forwarder to a rendering package, in the opposite
direction from `catalog.Fetcher`, the interface `forward` satisfies.

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
	Route      endpoint.Route // exact upstream path, joined onto the credential's base URL
	Method     string         // outbound method; WebSocket callers pass MethodGet
	Query      string         // inbound RawQuery, forwarded verbatim and never normalized
	ForceQuery bool           // preserves a bare "?" from the inbound URL

	// ClientHeader is the inbound header set to forward under the strip policy.
	// A nil value forwards no client header.
	ClientHeader http.Header
	// Body is the outbound body. A nil or http.NoBody value is wrapped so the
	// Transport treats an otherwise bodyless request as single-attempt.
	Body io.Reader
	// ContentLength is assigned only when non-zero, so a sized body (a
	// *bytes.Reader) keeps the length and GetBody http.NewRequestWithContext
	// derives for it. A caller streaming an inbound body passes r.ContentLength,
	// including -1 for an unknown length.
	ContentLength int64
	// AcceptIdentityEncoding forces Accept-Encoding: identity, so the caller
	// receives an undecoded body it may inspect or stream.
	AcceptIdentityEncoding bool
}
```

`Call` deliberately takes a header set and a body rather than an `*http.Request`.
The current `authenticatedOutboundHeaders` takes an `*http.Request` to reach both
`.Header` and `.Context()`, which forces `forward.forward` to build a shallow
request copy purely to substitute the shim-rewritten header. That copy disappears.

### `Failure`

```go
// Failure is one classified upstream call failure, already mapped to the
// copilotd-originated signal that answers it.
type Failure struct {
	Kind       apierror.Kind // signal to render; not consulted when ClientGone is set
	Message    string        // human-readable text, rendered in the surface's dialect
	ClientGone bool          // caller disconnected; nothing may be written
	Err        error         // underlying cause; logged once at classification, never rendered
}

// RespondTo renders f on w in surface's dialect and reports whether it wrote.
// A ClientGone failure writes nothing and reports false, so callers can skip
// the metrics and logging that only a real failure warrants.
func (f *Failure) RespondTo(w http.ResponseWriter, surface endpoint.Surface) bool
```

`RespondTo`'s boolean is the gate every caller already goes through, so no site
reads `Kind` on a `ClientGone` failure — which matters because `apierror.Kind`'s
zero value is `Unauthorized`. `Err` is exported so a caller may add it to its own
structured logs and so tests can assert on it; it is never rendered, so an
upstream URL or credential detail cannot reach a client.

Classification and response are one step because separating them is precisely
what let the WebSocket copy lose its cancel branch. The boolean is the seam a
caller needs for its own tail — `wsforward` books `AcceptDialFailed` only when
`RespondTo` reports `true`.

**`Err` gains a consumer, which it does not have today.** Not one of the four
current sites logs the cause, and `catalog`'s sentinel wrapping carries it only
as far as `writeFetchError`, which drops it. `Caller` logs it once at
classification, where it holds both the cause and a logger — a **new observable**,
one `WarnContext` per classified upstream failure.

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

// Do executes call and returns the upstream response, or one classified failure.
// ctx is used verbatim — Do never derives a child — so the caller keeps sole
// authority over cancellation and over any post-response deadline. A successful
// response body retains that request context for ReadBounded classification.
func (c *Caller) Do(ctx context.Context, call Call) (*http.Response, *Failure)

// Buffered executes call and reads the whole body under the buffered cap. It
// consumes and closes the response body, so it owns the post-response deadline.
func (c *Caller) Buffered(ctx context.Context, call Call) (int, []byte, *Failure)

// Prepare builds the authenticated outbound request without executing it.
// WebSocket callers consume only its URL and Header.
func (c *Caller) Prepare(ctx context.Context, call Call) (*http.Request, *Failure)

// Classify maps an execution error to one Failure, consulting both err and
// ctx.Err(), and logging the cause once.
func (c *Caller) Classify(ctx context.Context, err error) *Failure

// Correlate logs the upstream request id when it differs from copilotd's own.
func (c *Caller) Correlate(ctx context.Context, header http.Header)

// ReadBounded reads body under the Caller's configured cap — the form every call
// site uses, so the cap has exactly one owner. A body returned by Do retains its
// request context, so cancellation-aware classification happens here.
func (c *Caller) ReadBounded(body io.Reader) ([]byte, *Failure)

// ReadBounded reads body under max. It is the pure primitive the tables drive;
// it takes no context, so a caller whose own deadline fires mid-read sees the
// read failure and decides what that means.
func ReadBounded(body io.Reader, max int64) ([]byte, *Failure)

// RequestIDHeader carries copilotd's resolved correlation id in both directions.
// It is the single declaration; server, forward, and wsforward consume it.
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
`FetchModels` gets this right by construction, and `Buffered` must preserve the
ordering: `inner, cancel := context.WithCancelCause(ctx)` precedes `c.Do(inner,
call)`, so the body is bound to `inner`, and only then is
`time.AfterFunc(c.outboundTimeout, …)` armed. That ordering is what makes the
stalled-read test meaningful rather than accidentally green.

**A failed buffered read consults the context before it answers.** The free
`ReadBounded` stays context-free as the low-level primitive. `Do` binds each
successful response body to the request context, and `Caller.ReadBounded`
inspects that context and its cause before returning its already-classified
`Failure`; `Buffered` and `forward` therefore share the same decision:

| bound context cause | result |
|---|---|
| `context.DeadlineExceeded`, set by the body consumer's post-response timer | `GatewayTimeout` |
| `context.Canceled`, propagated from the inbound request | `ClientGone` |
| neither | `BadGateway`, read failure |

**Deadlines stay with the caller.** `Do` returns as soon as it has a response.
The post-response outbound timer belongs to whoever consumes the body, because
its correct scope differs per site: armed for a buffered read, deliberately *not*
armed for an SSE pump, and `sse.Pump` needs the `cancel` func itself. That is
also why `Do` must not derive its own child context — it would have to hand a
fourth return value back for `sse.Pump`. `Buffered` is the exception: it consumes
the body, so it owns the deadline, absorbing the `context.WithCancelCause` +
`timeoutCause` plumbing now hand-written in `FetchModels`. `outboundTimeout`
therefore has two legitimate holders and `maxBufferedBytes` one, because the line
is consumption, not naming: the cap is read only by code `upstream` owns, while
the timeout is read both by `Buffered` and by timers `forward` genuinely owns.

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

**The table is total across transports.** `wsforward`'s two WebSocket-specific
strings become the generic rows; see behaviour change 5, and
[Alternatives considered](#alternatives-considered) for the per-`Call` message
override that was rejected.

**Precedence is `DeadlineExceeded` before `Canceled`.** A context that timed out
reports `DeadlineExceeded`, so the order matters only when both an ancestor
cancel and a deadline are in play; classifying as a timeout is the informative
choice, and it preserves `wsforward`'s existing belt-and-braces check of both
`err` and `dialCtx.Err()`. On the buffered path the discrimination is by cancel
*cause*, not by `ctx.Err()`, since `Buffered`'s own timer and an inbound
disconnect both surface as `context.Canceled` on the derived context.

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
today to filter `cred.Headers`, not client headers, so the new `Sec-WebSocket-*`
rule is not its successor — the two guard opposite sides. The guard is dropped
because the credential set is closed and copilotd-constructed:
`impersonation.Set.Header()` emits only `Editor-Version`,
`Editor-Plugin-Version`, `User-Agent`, `Copilot-Integration-Id`, and
`X-GitHub-Api-Version`, so no handshake name can appear. `forward` has overlaid
`cred.Headers` unfiltered since day one; this adopts that policy rather than
inventing one.

The `Sec-WebSocket-*` rule is load-bearing, not defensive. `coder/websocket`
v1.8.15 clones `DialOptions.HTTPHeader` and then `Set`s `Connection`, `Upgrade`,
`Sec-WebSocket-Version`, and `Sec-WebSocket-Key` over the top, so those four are
harmless. **Two names survive the clone**, with the same failure mode:
`Sec-WebSocket-Protocol` is overwritten only when `Subprotocols` is non-empty, and
`Sec-WebSocket-Extensions` only when compression is negotiated
(`dial.go:216–221`) — neither holds here. Forwarding a client's offer would let
Copilot select one, and `verifyServerResponse` would then reject the handshake as
a protocol violation. Stripping the prefix covers both, and is what makes
forwarding client headers safe.

`singleAttemptBody` moves here too and is applied when `Call.Body` is nil or
`http.NoBody`, matching both places that hand-apply it today. It is inert for the
`bytes.Reader` POST path, whose `GetBody` the Transport may legitimately replay.

## Consumer changes

### `forward.PassthroughHandler` and `forward.forward`

Both 37-line credential/build/execute/classify blocks collapse to one `Do` call
against a `Call` literal, followed by `failure.RespondTo(w, surface)`.
`PassthroughHandler` passes `r.Header`; `forward` passes `header`, the
shim-rewritten set from `chain.RunRequest`, so the `headerRequest := *r` shallow
copy disappears. Each tail keeps what is genuinely its own: the
`cancel()`-before-`Body.Close()` ordering and its comment, the outbound timer,
`CopyResponseHeaders`, the HEAD check, and the `io.Copy` through `sse.NewWriter`.

`forward` creates a cancel-cause context before `Do`. Only after the response
arrives does a non-SSE path arm its post-response timer with
`context.DeadlineExceeded`; the SSE path still passes an ordinary cancel func to
`sse.Pump` and has no total-duration timer. The buffered branch becomes
`f.caller.ReadBounded(resp.Body)` plus `failure.RespondTo`, and `Forwarder` drops
its `maxBufferedResponseBytes` field, so the cap is read from the one place that
owns it.

### `catalog.Handler`

`catalog` declares its own narrow seam, still trivially fake-able the way
`stubFetcher` is today:

```go
// Source performs one upstream call for the current Copilot model Catalog and
// returns its bounded bytes.
type Source interface {
	Buffered(ctx context.Context, call upstream.Call) (int, []byte, *upstream.Failure)
}
```

`*upstream.Caller` satisfies it directly. `catalog/fetch.go` is deleted whole,
and `writeFetchError` with it; the handler calls `failure.RespondTo`. The
`if r.Context().Err() != nil { return }` guard at `handler.go:37` is deleted,
**and this is only safe because `Buffered` upgrades a cancelled read to
`ClientGone`.** Today's guard is total — it swallows any fetch error when the
client is gone, read failures included. A `ClientGone` produced only by
`Classify` would be partial: a disconnect while draining the models body would
surface as `BadGateway` and write a 502 to a dead socket. The buffered-path cause
check in [`Caller`](#caller) is what makes the deletion honest.

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
dialDeadlineExceeded := dialCtx.Err() == context.DeadlineExceeded
cancelDial()
if err != nil {
	classificationCtx := phaseCtx
	if dialDeadlineExceeded {
		classificationCtx = dialCtx
	}
	if p.caller.Classify(classificationCtx, err).RespondTo(w, surface) {
		p.metrics.observeAccept(AcceptDialFailed)
	}
	return
}
p.caller.Correlate(r.Context(), response.Header)
```

`Prepare` takes `phaseCtx`, not `dialCtx`: an on-demand credential mint is not
bounded by the dial timeout today and must not become so. `acceptOutcome` maps
`NotReady` → `AcceptRejected` and everything else → `AcceptDialFailed`,
preserving today's split between a credential rejection and a dial failure; the
metric vocabulary stays in `wsforward`, where it belongs.

After the dial returns, the handler captures whether `dialCtx` had already
expired before calling `cancelDial`; cleanup must not manufacture a `ClientGone`
classification. The dial's returned `err` often preserves a deadline, but a
transport may return a generic error after its context expires. The
belt-and-braces rule therefore classifies against the deadline-bearing
`dialCtx` only when that pre-cleanup state was `DeadlineExceeded`; otherwise it
uses the still-live `phaseCtx`, which preserves inbound cancellation without
mistaking explicit dial cleanup for client departure. `Classify` keeps timeout
precedence over cancellation.

`wsforward.websocketURL` is **deleted, not shrunk**. `websocket.Dial` accepts an
`https://` URL directly, rewriting the scheme itself, so `outReq.URL.String()` is
passed unchanged. Its `"could not build the upstream WebSocket URL"` failure mode
does not collapse into build-failure classification: a base URL that is
*parseable but wrongly schemed* now builds a perfectly valid `*http.Request` at
`Prepare` and fails inside `websocket.Dial` with `"unexpected url scheme"`, which
`Classify` maps by the "any other execution error" row. Same status, same
`AcceptDialFailed` metric, different stage — and one fewer message.

### `server` and the two constructors

`server/middleware.go` uses `upstream.RequestIDHeader`, retiring the third
declaration. `server` already imports `forward`, `wsforward`, and `catalog`;
`upstream` is a leaf below all three, so no cycle is possible.

`Caller` owns the provider, the outbound client, and the buffered cap, so
`forward.Forwarder` drops all three fields. `forward.New` is net **−2** parameters
(loses `provider`, `client`, and `maxBufferedResponseBytes`, gains `caller`,
leaving seven); `wsforward.New` is net **0** (loses `provider`, gains `caller`,
keeps `dialClient` for `DialOptions.HTTPClient`). `main` constructs one `Caller`
and injects it into `forward.New`, `wsforward.New`, and — through `server.New`,
which already carries `catalog.CodexDescriptor` — the catalog handler.

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

Eight, all deliberate.

**1. WebSocket forwards inbound client headers.** Under the shared strip policy,
extended with `Sec-WebSocket-*`. The two transports of `/openai/v1/responses`
stop disagreeing.

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
becomes `"could not reach the upstream"`. Statuses are unchanged — 504 and 502 as
today. The distinction the old strings carried is not one a client can act on:
the transport is already implied by the request it made.

**6. A buffered read that fails because the client disconnected is silent.**
`forward`'s buffered branch writes `BadGateway` today with no cancel check, so a
mid-body disconnect puts a 502 on a socket with no reader — the same defect as
change 2, on the other path. `Buffered` now classifies it as `ClientGone`. On the
`catalog` path this *preserves* current behaviour, which `handler.go:37`'s guard
already provided; on the `forward` path it is new.

**7. A `forward` buffered read interrupted by its outbound timer returns 504,
not 502.** The timer now supplies `context.DeadlineExceeded` as its cancellation
cause, so the shared read classifier distinguishes an upstream timeout from a
genuine read failure.

**8. The inference response tail cancels before closing the upstream body.** The
old defer registration closed the body first despite the intended
cancel-before-close policy. One ordered cleanup defer now matches the passthrough
tail and prevents a blocking `Close` from waiting on still-live upstream work.

One message refinement follows from the shared table: `catalog`'s single 502 text
`"could not fetch the upstream models catalog"` splits into four specific shared
messages: request construction, unreachable upstream, response read, and
over-cap response. **Every status code `catalog` returns today is preserved** —
503 for a missing credential, 504 for a timeout, 502 for the rest.

One new observable, not a behaviour change on the wire: `Caller` logs the
underlying cause once per classified failure (see [`Failure`](#failure)).

The divergence ledger is unchanged. Its Fabrication row points at
`internal/apierror`'s `Kind` enum, which is untouched, and enumerates neither
statuses nor messages; changes 1, 2, and 6 *reduce* fabrication, and change 5
rewords two fabricated messages without adding or removing one. The
request-direction strip set was never ledgered — the taxonomy governs copilotd's
wire *output*, and hop-by-hop stripping on the way upstream is ordinary proxy
behaviour.

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
agree on every context-free row; the Caller form additionally classifies the
context bound to a body returned by `Do`.

Migrations:

- `forward/models_fetch_test.go` (488 lines) moves to `upstream` largely intact
  as the `Buffered` suite.
- `catalog/handler_test.go` rewrites its error table onto `*upstream.Failure`
  stubs. Every status expectation carries over unchanged; four 502 messages
  become more specific. The `"unknown fetch error"` row is deleted rather than
  rewritten — it exercised `writeFetchError`'s `default` branch, and `*Failure`
  is total, so the case it covered can no longer be constructed.
- `wsforward/preupgrade_test.go:163` and `:190` assert the two unified messages
  byte-exactly and are updated with behaviour change 5. Statuses are unchanged,
  so only the two body literals move.
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

0. **Prefactor: the shadowing renames.** `upstream` is currently a local variable
   name in both `forward` and `wsforward`, shadowing the new package name inside
   the exact functions that must call it. `upstreamRoute` in `forward`,
   `upstreamConn` in `wsforward`, matching the names already beside them. A pure
   rename, kept out of any step that also changes behaviour.
1. **`internal/upstream` with no callers.** `Call`, `Failure`, `Caller`,
   `ReadBounded`, `RequestIDHeader`, the header policy, and the four test tables.
   Written test-first: the tables are the specification. **Import guard 1** — the
   leaf allowlist — lands here, so the invariant every later step rests on is
   protected *during* the migration rather than after it.
2. **Move the header policy.** `forward` deletes its header block and calls into
   `upstream`; `CopyResponseHeaders` is repointed. Behaviour-neutral *except* the
   `Sec-WebSocket-*` strip, which now applies on the HTTP path too — inert in
   practice, since nothing sends a handshake header to `POST /v1/messages`. So
   `forward`'s existing suite is the regression check. **`forward.New` gains
   `caller` here** — nine positional parameters to ten — because `Prepare` is a
   `*Caller` method and the outbound header policy is unreachable without one;
   `provider`, `client`, and `maxBufferedResponseBytes` stay for now.
3. **The two HTTP handlers onto `Do`.** `PassthroughHandler` and `forward`.
   Behaviour changes 3, 4, 7, and 8 land here, with their tests. No constructor
   change.
4. **`Buffered` and `catalog.Source`.** Delete `catalog/fetch.go`,
   `forward.FetchModels`, and the `catalog` import; add **import guard 2**.
   Behaviour change 6 lands here, with its tests. **`forward.New` drops
   `provider`, `client`, and `maxBufferedResponseBytes` here** — ten parameters
   to seven, net **−2** from the original nine.
5. **`wsforward` onto `Prepare` / `Classify` / `Correlate`.** Behaviour changes
   1, 2, and 5 land here, with their tests. `wsforward.New` loses `provider` and
   gains `caller` in this one step, so it is net **0** throughout.
6. **The shared constant and the docs.** `server/middleware.go`, CONTEXT.md,
   ADR-0013.

Steps 2 and 5 are the two constructor changes, and both need the shared `Caller`
in the composition root. They are on independent branches, so whichever lands
first introduces the single construction.

**Decomposition.** This order was decomposed into implementer tickets #123–#131,
with the maintainer's approval, as follows: step 1 splits into three tickets —
one per test table (the failure vocabulary; `Call` and the header policy;
`Do` / `Buffered` / `ReadBounded`) — because written test-first it does not fit
one working context. The resulting graph is a **finer partial order**: step 2
depends only on the header-policy ticket, not on all of step 1, and step 5 runs
parallel to steps 2–4 rather than after them. That preserves this section's
invariant — each ticket compiles and passes the full suite — without preserving
its linear numbering, which is the means rather than the end. Steps 0 and the
guard-1 placement above are part of the same approved decomposition.

## CONTEXT.md changes

One new entry under *Surfaces & forwarding*, after **Forwarder**:

> **Upstream call**:
> The single authenticated request copilotd makes to GitHub Copilot on behalf of
> one inbound request — credential, base URL join, header policy, request-id
> correlation, bounded reading of the response, and failure classification,
> centrally governed across every transport and Endpoint. Lives in
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
(`rawNetworkConn`), which is a `wsforward`-only concern that would appear in a
shared module's signature as a fourth return value. It would also pull
`coder/websocket` into `forward`'s import graph for a library `forward` never
uses.

**Classify without responding.** The module could return `*Failure` and leave
every site to write its own `apierror.Write` / silent-return switch. Rejected:
that is the exact four-way switch whose WebSocket copy lost its cancel branch.
Concentrating the classifier without concentrating the response would fix the
symptom and leave the mechanism.

**A per-`Call` message override**, to keep `wsforward`'s two transport-specific
strings under the shared table. Rejected: its only purpose would be to inject the
word "WebSocket" into two messages, re-asserting the transport distinction this
module exists to erase, and the transport is already implied by the request the
client made.

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

**Re-adding a guard at step 3 of the header policy**, filtering `cred.Headers`
the way `isHandshakeHeader` does today. Rejected: it would require a second strip
set — "`requestStrip` minus `Authorization`", since step 2 deliberately sets a
name that is *in* `requestStrip` — and a near-duplicate set is the pathology this
design exists to remove.

## Related decisions

- [ADR-0007](../adr/0007-served-endpoints-as-typed-contracts.md) — the Endpoint is
  the typed contract this module consumes; `Call.Route` comes from
  `ep.Upstream()`, never from a literal.
- [ADR-0003](../adr/0003-synthesized-stream-terminals-off-band-origin.md) — the
  post-commit stream path this module deliberately does not touch.
- ADR-0013 (**new, with this design**) — the shared policy for every authenticated
  call to GitHub Copilot is governed by `internal/upstream`.
- [Token usage meter](2026-07-26-token-usage-meter-design.md) — **in flight, and
  ordered after this one**, taking ADR-0014. Behaviour change 3 settles ground the
  meter stands on: its §11.1 already expects **502** for an over-cap non-stream
  response, explicitly conditioned on this design landing first. Landing this
  first makes that expectation good rather than provisional, and means the meter
  is written against a settled buffered-read path rather than one moving
  underneath it.
- [Divergence ledger](../divergence-ledger.md) — unchanged; see
  [Behaviour changes](#behaviour-changes).
