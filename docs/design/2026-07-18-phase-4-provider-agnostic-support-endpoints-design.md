# Phase 4 — Provider-agnostic support endpoints — Design

Status: proposed design (approved in brainstorming), pending written-spec review
Date: 2026-07-18
Roadmap reference: `ROADMAP.md` §7 "Phase 4 — Provider-agnostic support endpoints"
Builds on: `docs/design/2026-07-14-phase-1-core-forward-path-design.md`,
`docs/design/2026-07-16-forwarding-fidelity-and-sse-identity-design.md`

## 1. Goal and outcome

Phase 4 exposes GitHub Copilot's raw model catalog at the neutral
`HOST/models` path. Each authenticated call makes one uncached request to
Copilot's `GET /models` route and returns the response without parsing,
filtering, sanitizing, reshaping, retrying, or falling back to another identity.

**Outcome:** an operator can retrieve the account's current Copilot model data
from `GET /models`, with Copilot's status, body bytes, and legal end-to-end
response headers preserved. `HEAD /models` explicitly exposes the normal HEAD
view of the same upstream GET response.

"Raw" here means semantic HTTP passthrough: copilotd does not interpret or
re-encode the body, and it preserves status and end-to-end header values subject
to the mandatory proxy rules in §6. It does not mean reproducing the upstream
wire representation: hop-by-hop framing, header order/casing, connection
protocol, and trailers are outside the contract.

## 2. Scope

### 2.1 In scope

- Two explicit inbound route mappings:
  - `GET /models` → Copilot `GET /models`
  - `HEAD /models` → Copilot `GET /models`
- The existing API-key gate followed by the existing readiness gate.
- A focused passthrough handler in `internal/forward`, separate from the
  inference handler's shim, JSON-peek, and SSE paths.
- Verbatim raw query-string and request-body forwarding.
- Client ownership of end-to-end request headers, except for the safety,
  credential, impersonation, and correlation rules in §6.
- Raw forwarding of every Copilot response, including non-2xx responses.
- Anthropic-shaped copilotd-originated failures, chosen explicitly as a
  temporary local-error dialect for this neutral route.
- A global correction to ensure copilotd's resolved `X-Request-Id` is the sole
  downstream correlation value on every route.
- Disabling Go transport-level automatic compression/decompression so the
  support route can preserve the client's `Accept-Encoding` decision and the
  corresponding upstream response bytes.
- Automated unit, boundary, and real-listener tests using a stub upstream.

### 2.2 Out of scope

- Provider/client-shaped catalogs (`/anthropic/models`, `/openai/models`, or a
  Codex catalog). Those remain Phase 6.
- Any generic support-route registry, routing framework, or Phase 6 scaffolding.
- Model types, validation, parsing, filtering, capability inference, aliases,
  sanitization, or response transformation.
- Caching, conditional-response synthesis, refresh jobs, or state at rest.
  Client conditional headers are forwarded, but Copilot alone decides the
  resulting response.
- Retry, self-healing, or fallback to another token or integration identity.
  The normal identity manager may perform an on-demand mint before the call;
  this is credential lifecycle, not a retry of `GET /models`.
- Applying the shim onion or SSE engine to the support response, regardless of
  its reported `Content-Type`.
- A new provider-agnostic error Surface. It may become worthwhile when more
  neutral support routes exist, but one route does not justify it.
- Live-Copilot tests in the automated suite.

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Upstream behavior | One Copilot `GET /models` call per inbound call | Makes the response describe that exact upstream interaction; no cache or fallback can make it stale or synthetic. |
| Implementation boundary | Focused passthrough handler on the existing `forward.Forwarder` | Reuses credential, transport, cancellation, timeout, and header machinery without a new package or route-policy framework. |
| Existing inference handler | Leave its public contract and shim/SSE pipeline intact | `/models` is not an inference Surface and must not acquire parity transformations accidentally. |
| Route registration | Register GET and HEAD explicitly; both select upstream GET | Makes the mapping visible and testable instead of relying on `ServeMux`'s implicit GET→HEAD match. |
| HEAD body | Drain and close the upstream GET body; emit no downstream body | Preserves GET status/headers and connection reuse while providing standard HEAD semantics. |
| Request ownership | Preserve raw query, body, and legal client headers | The client owns the request except for authentication/authorization to Copilot and copilotd's correlation invariant. |
| Response ownership | Preserve upstream status, body bytes, and legal end-to-end headers | "Raw Copilot response" forbids parsing or replacement whenever an upstream response exists. |
| Local errors | Reuse `apierror.Anthropic` explicitly | Existing local errors are Surface-shaped. Anthropic is a deliberate temporary choice rather than an accidental claim that `/models` belongs to that Surface. |
| Compression | Disable Go transport auto-compression; do not force identity encoding on `/models` | An absent or client-supplied `Accept-Encoding` must remain the client's choice, and the transport must not silently decompress the response. |
| Correlation | Send copilotd's resolved ID upstream and suppress any upstream `X-Request-Id` downstream | One ID must span the client request, copilotd logs, the Copilot request, and the client response. |
| Future support routes | Defer abstractions until Phase 6 | The standard library router is already sufficient; future transformations should create only the seams their concrete requirements need. |

## 4. Architecture and package boundaries

No package is added.

### 4.1 `internal/server`

`newHandler` continues to own the explicit inbound→upstream route map and the
gate order. It registers both support patterns independently, each visibly
selecting an upstream GET handler:

```go
mux.Handle("GET /models",
	guard(apierror.Anthropic,
		fwd.PassthroughHandler(http.MethodGet, "/models", apierror.Anthropic)))
mux.Handle("HEAD /models",
	guard(apierror.Anthropic,
		fwd.PassthroughHandler(http.MethodGet, "/models", apierror.Anthropic)))
```

The two registrations, their handler arguments, and both inbound→upstream
method mappings are normative. The existing `guard` order is unchanged:

```text
request ID → access log → recovery → API-key auth → readiness → passthrough
```

`POST /models` and every other unregistered method receive `net/http`'s normal
method-not-allowed response. No wildcard `/models/` subtree is introduced.

### 4.2 `internal/forward`

`Forwarder` gains one focused entry point with this contract:

```go
func (f *Forwarder) PassthroughHandler(
	upstreamMethod string,
	upstreamPath string,
	localErrors apierror.Surface,
) http.HandlerFunc
```

It uses the same injected `identity.Provider`, `http.Client`, timeouts, logger,
and low-level header helpers as inference forwarding. It does not call
`shim.Registry.NewChain`, inspect JSON, classify `Content-Type`, invoke the SSE
pump, buffer the response, or enforce a response-body size cap.

This is a second, intentionally small policy path inside the module that already
owns upstream I/O. It is not a general route descriptor: existing inference
routes retain `Forwarder.Handler`, and Phase 4 adds no flags for shims,
streaming, request peeks, or response modes.

### 4.3 Shared transport and response-header policy

The shared HTTP transport disables its automatic compression behavior. Existing
inference calls continue to set `Accept-Encoding: identity`, so their behavior
does not change. The new passthrough handler preserves the client's header; when
the client supplies none, the transport supplies none.

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
6. The handler builds one outbound request using method `GET`, path `/models`,
   the incoming body stream, and the exact raw query representation.
7. It applies the request-header policy in §6.1 and calls Copilot once.
8. If Copilot returns a response, the handler commits its status and filtered
   headers without interpreting the status or body.
9. For inbound GET, it streams the body directly to the client. For inbound
   HEAD, it drains the upstream GET body without writing a downstream body.
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

Both inbound GET and HEAD use this same request policy and both go upstream as
GET. If a client supplies a body on HEAD, it therefore becomes the body of that
upstream GET; copilotd does not inspect or reinterpret it.

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
version. This is standard proxy behavior and does not alter the body bytes.

For HEAD, the handler commits the upstream GET status and headers, drains the
body to enable connection reuse, and writes no body. In particular, it preserves
the upstream GET `Content-Length` when present; that value describes the
representation a corresponding GET would return.

## 7. Error, timeout, and cancellation behavior

### 7.1 Upstream responses are authoritative

Every response received from Copilot is forwarded under §6.2, even when its
status is 401, 403, 429, 500, or otherwise unexpected. copilotd does not replace
the body with `apierror`, remint and retry, switch integration identity, or
interpret the response as model data.

### 7.2 copilotd-originated failures

Only failures for which no upstream response exists use the existing
Anthropic-shaped renderer:

| Failure | Status | Existing kind / message |
| --- | ---: | --- |
| Missing or invalid API key | 401 | `Unauthorized` / `missing or invalid API key` |
| Readiness gate rejects | 503 | `NotReady` / `service not ready` |
| `Provider.Current` cannot supply a credential | 503 | `NotReady` / `no upstream credential available` |
| Outbound request construction fails | 502 | `BadGateway` / `could not build the upstream request` |
| Copilot cannot be reached before headers | 502 | `BadGateway` / `could not reach the upstream` |
| A pre-header deadline expires | 504 | `GatewayTimeout` / `the upstream request timed out` |

The route passes `apierror.Anthropic` explicitly through auth, readiness, and
forwarding. This is deliberate reuse of an existing dialect, not classification
of `/models` as an Anthropic Surface. A future neutral Surface is a separate
design decision when enough support routes exist to define one coherently.

### 7.3 Post-commit failures

`ResponseHeaderTimeout` continues to bound the wait for response headers.
`OutboundTimeout` is the total backstop while copying or draining the support
response after headers arrive, and `WriteTimeout` bounds each downstream write.
The SSE-only idle and keepalive timers do not apply.

After response headers are committed, a read failure, body timeout, write
failure, or client disconnect cannot be replaced with a JSON error without
corrupting the raw response. The handler stops copying, cancels the outbound
request, and closes the body. It never appends a synthesized JSON body or SSE
terminal. Client cancellation is propagated upstream and produces no additional
downstream signal.

## 8. Observability and security

- Existing access logging records one line with the explicit route template
  (`GET /models` or `HEAD /models`), status, downstream byte count, duration,
  and the resolved correlation ID.
- The same correlation ID appears in copilotd logs, the outbound Copilot
  request, and the downstream response.
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

- GET and HEAD invocations both produce upstream method GET and path `/models`.
- Raw query and `ForceQuery` fidelity cover duplicate parameters, mixed escaping,
  valueless parameters, and a bare `?`.
- A request body and its length semantics reach the upstream unchanged.
- Legal headers, conditional headers, and client `Accept-Encoding` survive.
- Hop-by-hop headers, connection-nominated headers, inbound `Authorization`,
  `X-Api-Key`, and `Host` do not survive.
- The Copilot token, impersonation headers, and resolved request ID replace
  client values.
- The transport does not synthesize `Accept-Encoding` or transparently
  decompress a compressed response.
- Upstream 2xx and non-2xx status, body bytes, and legal headers pass through.
- Hop-by-hop response headers and upstream `X-Request-Id` do not pass through.
- GET streams the body; HEAD drains it and returns no body while preserving
  status, headers, and representation length.
- A registry containing a fail-fast test shim is never instantiated or called;
  an SSE-looking content type is still copied as an ordinary raw body.
- Provider, construction, transport, timeout, post-commit copy, and client-cancel
  paths follow §7 and release the upstream body.

### 9.2 Server boundary and real-listener tests

- Both API-key forms authorize GET and HEAD.
- Invalid auth returns 401 before a not-ready identity can return 503.
- Authenticated not-ready calls return the approved Anthropic-shaped 503.
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

### 9.3 Regression verification

- Existing inference tests continue to prove identity encoding, query fidelity,
  raw response copying, shim composition, and SSE behavior.
- `go test ./...`
- `go test -race ./...`

## 10. Acceptance criteria

Phase 4 is complete when all of the following hold:

1. `GET /models` and `HEAD /models` are explicit authenticated, readiness-gated
   routes, and both issue an upstream `GET /models`.
2. Each inbound call issues exactly one models request; no model response is
   cached, retried, refreshed, or obtained through a fallback identity.
3. The request contract in §6.1 is preserved, including raw query fidelity and
   client-controlled `Accept-Encoding`.
4. Every actual Copilot response follows §6.2 without parsing, reshaping, or
   replacement, including non-2xx responses.
5. HEAD returns GET status and headers with no body and drains the upstream body.
6. Only failures without an upstream response use the explicitly selected
   Anthropic error dialect.
7. Exactly one resolved `X-Request-Id` spans each request and response on every
   copilotd route; upstream response IDs cannot create duplicates.
8. The route bypasses shims, SSE processing, model types, and caching.
9. No new routing framework, support registry, Phase 6 scaffolding,
   configuration, metric, background task, or durable state is introduced.
10. The automated suite and race detector pass.

## 11. Phase 6 handoff

Phase 6 may add provider/client-shaped routes that parse and transform Copilot's
model data. Phase 4 makes no promise that its handler is their final abstraction.
Those routes can reuse, extract, or replace internals when their concrete data
and error requirements are known. The public Phase 4 contract remains the raw,
uncached `/models` passthrough described here.
