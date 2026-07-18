# Phase 4 — GitHub Copilot support endpoint — Design

Status: proposed design (polished via brainstorming and grilling), pending final written-spec review
Date: 2026-07-18
Roadmap reference: `ROADMAP.md` §7 "Phase 4 — GitHub Copilot support endpoint"
Builds on: `docs/design/2026-07-14-phase-1-core-forward-path-design.md`,
`docs/design/2026-07-16-forwarding-fidelity-and-sse-identity-design.md`

## 1. Goal and outcome

Phase 4 exposes GitHub Copilot's raw model catalog as the endpoint
`(GitHubCopilot, /models)` at `HOST/models`. Each authenticated, ready call that
obtains a credential makes one uncached request with the matching GET or HEAD
method to GitHub Copilot's `/models` Route and returns the response without
parsing, filtering, sanitizing, reshaping, retrying, or falling back to another
identity.

**Outcome:** an operator can retrieve the account's current Copilot model data
from `GET /models`, with Copilot's status, body bytes, and legal end-to-end
response headers preserved. `HEAD /models` explicitly exposes the normal HEAD
operation on the same GitHub Copilot Route.

"Raw" here means semantic HTTP passthrough: copilotd does not interpret or
re-encode the body, and it preserves status and end-to-end header values subject
to the mandatory proxy rules in §6. It does not mean reproducing the upstream
wire representation: hop-by-hop framing, header order/casing, connection
protocol, informational 1xx responses, trailers, and normal `net/http` header
synthesis are outside the contract.

## 2. Scope

### 2.1 In scope

- Two explicit inbound route mappings:
  - `GET /models` → Copilot `GET /models`
  - `HEAD /models` → Copilot `HEAD /models`
- The existing API-key gate followed by the existing readiness gate.
- A focused passthrough handler in `internal/forward`, separate from the
  inference handler's shim, JSON-peek, and SSE paths.
- Verbatim raw query-string and request-body forwarding.
- Client ownership of end-to-end request headers, except for the safety,
  credential, impersonation, and correlation rules in §6.
- Raw forwarding of every Copilot response, including non-2xx responses.
- A `GitHubCopilot` Surface tag whose copilotd-originated failures explicitly
  reuse the Anthropic envelope for now.
- A global correction to ensure copilotd's resolved `X-Request-Id` is the sole
  downstream correlation value on every route.
- Disabling Go transport-level automatic compression/decompression so the
  support route can preserve the client's `Accept-Encoding` decision and the
  corresponding upstream response bytes.
- Disabling automatic redirect following for every forwarded Copilot call so
  the first upstream response remains authoritative.
- Automated unit, boundary, and real-listener tests using a stub upstream.

### 2.2 Out of scope

- Provider/client-shaped catalogs (`/anthropic/v1/models`, `/openai/v1/models`, or a
  Codex catalog). Those remain Phase 6.
- The Surface identity, local-error dialect, and internal data-source boundary
  of those Phase 6 catalogs. Their future paths do not decide those questions
  here.
- Any generic support-route registry, routing framework, or Phase 6 scaffolding.
- Model types, validation, parsing, filtering, capability inference, aliases,
  sanitization, or response transformation.
- Caching, conditional-response synthesis, refresh jobs, or state at rest.
  Client conditional headers are forwarded, but Copilot alone decides the
  resulting response.
- Retry, self-healing, or fallback to another token or integration identity.
  The normal identity manager may perform an on-demand mint before the call;
  this is credential lifecycle, not a retry of the `/models` request.
- Applying the shim onion or SSE engine to the support response, regardless of
  its reported `Content-Type`. ADR-0003's synthesized-terminal guarantee applies
  only to Routes whose contracts include SSE semantics; a raw passthrough Route
  does not opt in through `Content-Type` alone.
- Applying the inference request-body cap to the GitHub Copilot endpoint. GET
  and HEAD bodies are unusual but stream through without buffering or a local
  size limit.
- A distinct GitHub Copilot local-error envelope. The `GitHubCopilot` Surface
  delegates to the Anthropic envelope until more native routes justify defining
  a separate wire shape.
- Live-Copilot tests in the automated suite.
- Logging GitHub's response request ID alongside copilotd's correlation ID.
  Phase 4 suppresses that upstream value downstream and otherwise discards it;
  issue #39 tracks retaining both IDs in structured logs.

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Upstream behavior | One matching Copilot GET or HEAD `/models` call after credential acquisition | Auth, readiness, and credential failures issue no models call; every forwarded request describes one exact upstream interaction. |
| Implementation boundary | Focused passthrough handler on the existing `forward.Forwarder` | Reuses credential, transport, cancellation, timeout, and header machinery without a new package or route-policy framework. |
| Existing inference handler | Leave its public contract and shim/SSE pipeline intact | `/models` is not an inference Surface and must not acquire parity transformations accidentally. |
| Route registration | Register GET and HEAD explicitly; preserve the method upstream | Makes both mappings visible and testable instead of relying on `ServeMux`'s implicit GET→HEAD match. |
| HEAD behavior | Forward HEAD as HEAD and emit no downstream body | Preserves client ownership of the method and avoids downloading a GET representation for a bodyless request. |
| Request ownership | Preserve raw query, body, and legal client headers | The client owns the request except for authentication/authorization to Copilot and copilotd's correlation invariant. |
| Request body | Stream without buffering or `MaxRequestBytes` | GET/HEAD bodies are unusual; adding a special buffering path and local 413 is not justified for this passthrough. |
| Response ownership | Preserve upstream status, body bytes, and legal end-to-end headers | "Raw Copilot response" forbids parsing or replacement whenever an upstream response exists. |
| Endpoint identity | `(GitHubCopilot, /models)` | Surface and Route select the matching upstream API; `/models` is native GitHub Copilot rather than Anthropic or OpenAI. |
| Local errors | Tag with `apierror.GitHubCopilot`, whose renderer explicitly delegates to the Anthropic envelope | Endpoint identity stays truthful while the temporary wire-shape choice remains visible and independently changeable. |
| Compression | Disable Go transport auto-compression; do not force identity encoding on `/models` | An absent or client-supplied `Accept-Encoding` must remain the client's choice, and the transport must not silently decompress the response. |
| Redirects | Never follow an upstream redirect on support or inference routes | Following would replace Copilot's first response, may change the method, and makes copilotd decide how credentials cross redirect boundaries. |
| Correlation | Send copilotd's resolved ID upstream and suppress any upstream `X-Request-Id` downstream | One ID must span the client request, copilotd logs, the Copilot request, and the client response. |
| Future support routes | Defer abstractions until Phase 6 | The standard library router is already sufficient; future transformations should create only the seams their concrete requirements need. |

## 4. Architecture and package boundaries

No package is added.

### 4.1 `internal/server`

`newHandler` continues to own the explicit inbound→upstream route map and the
gate order. It registers both support patterns independently, each visibly
selecting the corresponding upstream method:

```go
mux.Handle("GET /models",
	guard(apierror.GitHubCopilot,
		fwd.PassthroughHandler(http.MethodGet, "/models", apierror.GitHubCopilot)))
mux.Handle("HEAD /models",
	guard(apierror.GitHubCopilot,
		fwd.PassthroughHandler(http.MethodHead, "/models", apierror.GitHubCopilot)))
```

The two registrations, their handler arguments, and both inbound→upstream
method mappings are normative. The existing `guard` order is unchanged:

```text
request ID → access log → recovery → API-key auth → readiness → passthrough
```

`POST /models` and every other unregistered method receive `net/http`'s normal
method-not-allowed response. No wildcard `/models/` subtree is introduced.

The implementation tickets are cumulative and independently mergeable. Because
a Go `GET` pattern also matches `HEAD`, the first server-facing tracer registers
both explicit patterns and minimally proves `GET`→`GET`, `HEAD`→`HEAD`, and no
downstream HEAD body. Later request-, response-, and HEAD-focused tickets deepen
the shared fidelity and failure coverage; they do not repair an intentionally
incorrect intermediate HEAD mapping. Phase 4 is complete only after its final
real-listener capstone.

### 4.2 `internal/forward`

`Forwarder` gains one focused entry point with this contract:

```go
func (f *Forwarder) PassthroughHandler(
	upstreamMethod string,
	upstreamPath string,
	surface apierror.Surface,
) http.HandlerFunc
```

It uses the same injected `identity.Provider`, `http.Client`, timeouts, logger,
and low-level header helpers as inference forwarding. It does not call
`shim.Registry.NewChain`, inspect JSON, classify `Content-Type`, invoke the SSE
pump, buffer either body, or enforce request/response body size caps.

This is a second, intentionally small policy path inside the module that already
owns upstream I/O. It is not a general route descriptor: existing inference
routes retain `Forwarder.Handler`, and Phase 4 adds no flags for shims,
streaming, request peeks, or response modes.

### 4.3 `internal/apierror`

`apierror.Surface` gains `GitHubCopilot`. Its non-streaming renderer explicitly
uses the same status, type, and JSON envelope as Anthropic for every existing
error kind. This is a renderer policy, not an alias: callers identify the
endpoint truthfully as GitHub Copilot, and a future design can change its local
wire shape without re-tagging routes. The support handler never calls the stream
error renderer. For the same kind and message, a call to `apierror.Write` with
`GitHubCopilot` produces the same status, headers, and body bytes as a call with
`Anthropic`.

### 4.4 Shared transport and response-header policy

The shared HTTP transport disables its automatic compression behavior. Existing
inference calls continue to set `Accept-Encoding: identity`, so their behavior
does not change. The new passthrough handler preserves the client's header; when
the client supplies none, the transport supplies none.

The shared HTTP client also sets a no-follow redirect policy. A 3xx response is
returned to the caller with its original `Location` and body instead of causing
a second request. This intentionally corrects all forwarded routes because raw
passthrough makes the first Copilot response authoritative everywhere.

The shared response-header copier adds `X-Request-Id` to its strip set. This is
an intentional global correction for Anthropic, OpenAI, and support routes:
the outer request-ID middleware has already installed copilotd's resolved value,
so an upstream value must never be appended as a second correlation ID.

## 5. End-to-end flow

For either registered route:

1. The request-ID middleware validates the inbound `X-Request-Id` or generates a
   replacement, stores it in context, and sets it on the downstream response.
2. Access logging and panic recovery wrap the request as they do every route.
3. API-key auth accepts either `Authorization: Bearer` or `X-Api-Key` and rejects
   an invalid caller before exposing readiness.
4. The readiness gate rejects the authenticated request if the last mint failed
   or no mint has yet succeeded.
5. The passthrough handler asks `identity.Provider.Current` for the current
   Copilot credential. The provider may perform its normal on-demand mint.
6. The handler builds one outbound request using the registered GET or HEAD
   method, path `/models`, the incoming body stream, and the exact raw query
   representation.
7. It applies the request-header policy in §6.1 and calls Copilot once.
8. If Copilot returns a response, the handler commits its status and filtered
   headers without interpreting the status or body.
9. For inbound GET, it streams the body directly to the client. For inbound
   HEAD, it commits the upstream HEAD status and headers without writing a body.
10. It closes the upstream body and releases the connection normally.

There is no result cache or single-flight layer. Two sequential or concurrent
client calls cause two independent Copilot `/models` calls. Credential caching
inside `identity.Provider` remains unchanged and is not model-response caching.

## 6. Passthrough contract

### 6.1 Request

The handler preserves:

- `r.URL.RawQuery` and `r.URL.ForceQuery`, including duplicate parameters,
  original escaping, ordering, valueless parameters, and a bare trailing `?`;
- the request body stream and its length semantics, even though a normal models
  request has no body;
- every end-to-end request header not listed below, including conditional and
  content-negotiation headers; and
- a client-supplied `Accept-Encoding` exactly as supplied. If it is absent, it
  stays absent.

It removes or replaces only what a safe authenticated proxy must own:

- Strip the standard hop-by-hop set and every header named by `Connection`.
- Strip inbound `Authorization` and `X-Api-Key`; neither API-key form may reach
  Copilot.
- Do not forward the inbound `Host`; the outbound request targets the current
  credential's Copilot origin.
- Do not blindly copy the `Content-Length` header. Preserve length through the
  outbound request's structured content-length metadata so Go serializes it
  consistently with the body.
- Set `Authorization: Bearer <Copilot token>`.
- Overlay the credential's impersonation headers, replacing client values for
  the same names.
- Set the resolved `X-Request-Id`, replacing any raw client value that failed
  validation.

Both inbound GET and HEAD use this same request policy and preserve their method
upstream. If a client supplies a body on either method, copilotd forwards it
without inspection or reinterpretation.

### 6.2 Response

Once Copilot supplies response headers, copilotd preserves:

- the exact HTTP status code, including every 3xx, 4xx, and 5xx;
- body bytes in the order received, with no JSON decode/re-encode, decompression,
  buffering, model filtering, or SSE transformation; and
- every end-to-end response-header value except `X-Request-Id`.

It strips:

- the standard hop-by-hop set and every header named by `Connection`; and
- Copilot's `X-Request-Id`, if present, so the response contains only
  copilotd's resolved correlation ID.

Go may choose downstream connection framing appropriate to the negotiated HTTP
version and may add normal server-generated headers such as `Date`, inferred
`Content-Type`, or calculated `Content-Length` when Copilot omitted them. Phase 4
adds no suppression machinery for those standard behaviors and does not alter
header values that Copilot did supply.

For HEAD, the handler commits the upstream HEAD status and headers and writes no
body. It preserves the upstream `Content-Length` when present.

## 7. Error, timeout, and cancellation behavior

### 7.1 Upstream responses are authoritative

Every response received from Copilot is forwarded under §6.2, even when its
status is 3xx, 401, 403, 429, 500, or otherwise unexpected. copilotd does not
follow `Location`, replace the body with `apierror`, remint and retry, switch
integration identity, or interpret the response as model data.

### 7.2 copilotd-originated failures

Only registered-endpoint failures for which no upstream response exists use the
`GitHubCopilot` renderer, which currently emits the existing Anthropic shape:

| Failure | Status | Existing kind / message |
| --- | ---: | --- |
| Missing or invalid API key | 401 | `Unauthorized` / `missing or invalid API key` |
| Readiness gate rejects | 503 | `NotReady` / `service not ready` |
| `Provider.Current` cannot supply a credential | 503 | `NotReady` / `no upstream credential available` |
| Outbound request construction fails | 502 | `BadGateway` / `could not build the upstream request` |
| Copilot cannot be reached before headers | 502 | `BadGateway` / `could not reach the upstream` |
| A pre-header deadline expires | 504 | `GatewayTimeout` / `the upstream request timed out` |

The route passes `apierror.GitHubCopilot` explicitly through auth, readiness,
and forwarding. The renderer's deliberate reuse of the Anthropic envelope does
not classify `/models` as an Anthropic endpoint. Router-level 404/405 responses
and the existing panic recovery remain generic server signals rather than
Surface-rendered errors.

### 7.3 Post-commit failures

`ResponseHeaderTimeout` continues to bound the wait for response headers.
`OutboundTimeout` is the total backstop while copying the support response after
headers arrive, and `WriteTimeout` bounds each downstream write.
The SSE-only idle and keepalive timers do not apply.

ADR-0003 does not require a synthesized terminal here because the `/models`
Route contract is raw passthrough rather than SSE. A reported
`Content-Type: text/event-stream` remains opaque response metadata and does not
change the selected response policy.

After response headers are committed, a read failure, body timeout, write
failure, or client disconnect cannot be replaced with a JSON error without
corrupting the raw response. The handler stops copying, cancels the outbound
request, and closes the body. It never appends a synthesized JSON body or SSE
terminal. Client cancellation is propagated upstream and produces no additional
downstream signal.

## 8. Observability and security

- Existing access logging records one line with the explicit route template
  (`GET /models` or `HEAD /models`), status, recorded byte count, duration,
  and the resolved correlation ID.
- On a forwarded request, the same correlation ID appears in copilotd logs, the
  outbound Copilot request, and the downstream response.
- A different `X-Request-Id` returned by GitHub is neither exposed nor logged in
  this phase; issue #39 tracks logging it alongside copilotd's ID later.
- The API key, GitHub OAuth token, and Copilot token are never logged or copied
  to the wrong side of the boundary.
- Model response data, query values, and request/response bodies are not logged.
- No new metrics, configuration keys, background work, cache state, or durable
  state are introduced.
- `/healthz` and `/readyz` retain their existing public behavior. `/models` is
  protected because it performs an account-authorized Copilot operation and
  exposes account-specific support data.

## 9. Test design

All automated tests use `httptest` upstreams; they do not require a GitHub
account or network access.

### 9.1 Forwarder unit tests

- Every non-streaming `apierror.Kind` rendered for `GitHubCopilot` matches the
  Anthropic status, headers, and body bytes for the same message.
- GET produces upstream GET and HEAD produces upstream HEAD, both at `/models`.
- Raw query and `ForceQuery` fidelity cover duplicate parameters, mixed escaping,
  valueless parameters, and a bare `?`.
- A request body and its length semantics reach the upstream unchanged.
- The body is streamed rather than buffered, and `MaxRequestBytes` does not
  produce a local 413 on this endpoint.
- Legal headers, conditional headers, and client `Accept-Encoding` survive.
- Hop-by-hop headers, connection-nominated headers, inbound `Authorization`,
  `X-Api-Key`, and `Host` do not survive.
- The Copilot token, impersonation headers, and resolved request ID replace
  client values.
- The transport does not synthesize `Accept-Encoding` or transparently
  decompress a compressed response.
- A redirect is returned verbatim and causes no second upstream call.
- Upstream 2xx and non-2xx status, body bytes, and legal headers pass through.
- Hop-by-hop response headers and upstream `X-Request-Id` do not pass through.
- GET streams the body; HEAD returns no body while preserving the upstream HEAD
  status and headers.
- A registry containing a fail-fast test shim is never instantiated or called;
  an SSE-looking content type is still copied as an ordinary raw body.
- Provider, construction, transport, timeout, post-commit copy, and client-cancel
  paths follow §7 and release the upstream body.

### 9.2 Server boundary and real-listener tests

- Both API-key forms authorize GET and HEAD.
- Invalid auth returns 401 before a not-ready identity can return 503.
- Authenticated GET calls against a not-ready identity return the
  `GitHubCopilot` renderer's approved Anthropic-shaped 503. HEAD returns the same
  status and headers with no wire body, as required by HEAD semantics.
- The explicit patterns are visible to access logging as `GET /models` and
  `HEAD /models`.
- Unregistered methods receive the standard 405 and never reach Copilot.
- Two client calls cause exactly two upstream `/models` calls.
- A conditional request can receive Copilot's raw 304 response.
- A real HTTP listener proves the HEAD response has no wire body.
- A deliberately different upstream `X-Request-Id` is suppressed and exactly
  one copilotd correlation value reaches the client.
- Regression coverage applies that same single-ID invariant to the existing
  Anthropic and OpenAI routes.
- Regression coverage proves existing Anthropic and OpenAI routes also return
  the first redirect response without following it.

### 9.3 Regression verification

- Existing inference tests continue to prove identity encoding, query fidelity,
  raw response copying, shim composition, and SSE behavior.
- `go test ./...`
- `go test -race ./...`

## 10. Acceptance criteria

Phase 4 is complete when all of the following hold:

1. `GET /models` and `HEAD /models` are explicit authenticated, readiness-gated
   endpoints that preserve their respective method on upstream `/models`.
2. Each authenticated, ready call that obtains a credential issues exactly one
   models request; no model response is cached, retried, refreshed, or obtained
   through a fallback identity.
3. The request contract in §6.1 is preserved, including raw query fidelity and
   client-controlled `Accept-Encoding`.
4. Every actual Copilot response follows §6.2 without parsing, reshaping, or
   replacement, including redirects and other non-2xx responses.
5. HEAD returns the upstream HEAD status and headers with no body.
6. Registered-endpoint failures without an upstream response use
   `apierror.GitHubCopilot`, which explicitly delegates to the Anthropic error
   dialect; router and recovery errors retain their existing generic behavior.
7. Exactly one resolved `X-Request-Id` spans each request and response on every
   copilotd route; upstream response IDs cannot create duplicates.
8. The route bypasses shims, SSE processing, model types, and caching.
9. It also bypasses inference request buffering and request/response body caps.
10. No new routing framework, support registry, Phase 6 scaffolding,
   configuration, metric, background task, or durable state is introduced.
11. The automated suite and race detector pass.

## 11. Phase 6 handoff

Phase 6 may add provider/client-shaped routes that parse and transform Copilot's
model data. Phase 4 makes no promise that its handler is their final abstraction.
Their Surface identity, error dialect, and internal reuse strategy are deferred
to the Phase 6 design. The public Phase 4 contract remains the raw, uncached
`/models` passthrough described here.
