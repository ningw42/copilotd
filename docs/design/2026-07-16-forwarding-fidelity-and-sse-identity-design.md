# Forwarding Fidelity and SSE Identity Encoding — Design

Status: approved
Date: 2026-07-16
Builds on: `docs/design/2026-07-15-phase-2-sse-streaming-engine-design.md`

## 1. Goal

Close two forwarding gaps found while exercising current Codex and Claude Code
clients against the Phase 2 surface:

1. Preserve every inbound query string exactly when mapping a provider route to
   its Copilot upstream path.
2. Ensure the SSE engine only receives an identity-encoded representation, while
   leaving the buffered response path opaque.

These invariants apply to the three currently registered provider routes. This
design makes no claim about routes added later.

## 2. Non-goals

- Decoding or encoding gzip, deflate, Brotli, Zstandard, or any other content
  coding.
- Negotiating response encoding with the downstream client.
- Changing request-body `Content-Encoding` handling.
- Inspecting or transforming buffered response bodies.
- Defining the forwarding contract for future routes.
- Adding compression settings or an operator-facing toggle.
- Changing Copilot base URL configuration.
- Implementing the Phase 3 middleware framework or a compatibility shim.

## 3. Design

### 3.1 Exact query preservation

The registered route continues to select the upstream path. For example,
`/anthropic/v1/messages` still maps to `/v1/messages`; path forwarding does not
become opaque.

After constructing the upstream request from the credential base URL and the
registered upstream path, the forwarder copies these fields from the inbound
request URL to the upstream request URL:

- `RawQuery`, without parsing or re-encoding it;
- `ForceQuery`, so a bare trailing `?` is not lost.

This preserves query parameter order, duplicate keys, original percent-encoding
and its letter case, keys without values, empty values, and a bare query marker.
Fragments are not forwarded because they are not part of an HTTP request target.

Registered upstream paths remain query-free. Existing Copilot base URL behavior
is unchanged.

### 3.2 Request identity encoding

The forwarder replaces any client-supplied `Accept-Encoding` value with:

```http
Accept-Encoding: identity
```

This applies to every request forwarded by the three current provider routes,
whether the eventual response is streaming or buffered. The client header is
not parsed or validated. Copilot therefore receives one explicit request for an
identity-encoded representation.

This is an intermediate policy until compression belongs to middleware. It does
not attempt downstream content negotiation.

### 3.3 SSE response guard

The upstream response `Content-Type` remains the source of truth for choosing the
SSE or buffered path. Before copying upstream response headers or committing its
status, the forwarder checks `Content-Encoding` only for a `text/event-stream`
response.

The SSE engine may proceed when `Content-Encoding` is:

- absent; or
- one explicit `identity` value, compared case-insensitively after trimming
  surrounding whitespace.

An explicit `identity` value is removed before response headers are copied
downstream because no content coding was applied.

No content-coding parser is introduced. Any value outside the two accepted cases
fails before the SSE engine reads the body, including a compressed coding,
multiple codings, an empty value, or repeated values. Repeated `identity` values
are not normalized in this intermediate design.

The failure uses the existing provider-shaped `BadGateway` response for the
mounted surface. Its exact message is:

```text
upstream returned unsupported Content-Encoding for an event stream
```

The response is HTTP `502` with `api_error` on both surfaces; OpenAI `code` and
`param` are `null`. It does not copy the rejected upstream status, headers, body,
or encoding value. The upstream response body is still closed through the
forwarder's normal lifecycle.

### 3.4 Buffered responses remain opaque

The buffered path does not inspect `Content-Encoding`. It copies the upstream
status, allowed headers, and body bytes exactly as it does today, even if
Copilot unexpectedly returns a compressed response after identity was requested.

All work stays at existing seams in `internal/forward`: upstream request
construction copies the query fields, `outboundHeaders` sets the request
encoding, and the response branch applies the SSE guard before
`copyResponseHeaders`. No new package, policy abstraction, or general validation
framework is introduced.

## 4. Request and response flow

For each of the three current provider routes, the forwarder:

1. Reads the bounded inbound body under the existing policy.
2. Resolves the current Copilot credential.
3. Constructs the upstream URL from the credential base and registered route
   target.
4. Copies `RawQuery` and `ForceQuery` from the inbound URL.
5. Builds outbound headers under the existing policy, then replaces
   `Accept-Encoding` with `identity`.
6. Sends the request to Copilot.
7. Classifies the response from its `Content-Type`.
8. For SSE, applies the identity guard before downstream commitment and then
   runs the existing SSE pump. For a buffered response, preserves the existing
   opaque path.

## 5. Error behavior

| Condition | Downstream behavior |
| --- | --- |
| SSE response has no `Content-Encoding` | Run the SSE pump |
| SSE response has one explicit `identity` value | Remove the header and run the SSE pump |
| SSE response has any other encoding value | Provider-shaped `502` before commit |
| Buffered response has any encoding | Preserve status, allowed headers, and bytes opaquely |

## 6. Verification

### 6.1 Query fidelity

Assert against the upstream server's raw `RequestURI`, not parsed query values.
Cover:

- `?beta=true`;
- ordered duplicate keys;
- percent-encoded values with their original escape case;
- keys without values and keys with empty values;
- a bare trailing `?`;
- a request with no query.

Exercise Anthropic messages, Anthropic token counting, and OpenAI Responses so
every current registered path is covered.

### 6.2 Upstream request encoding

At the forwarder seam, verify that representative mixed, single, absent, and
otherwise arbitrary client `Accept-Encoding` values all become the single
upstream value `identity`. The client value is neither parsed nor rejected.

### 6.3 SSE response encoding

Use a table of upstream header values to verify:

- an absent upstream `Content-Encoding` streams normally;
- one explicit `identity` value streams normally and is not emitted downstream;
- a compressed coding, multiple codings, empty values, and repeated values
  produce the provider-shaped `502` before commitment;
- the rejected upstream status, headers, body, and encoding value do not leak;
- the upstream response body is closed.

For the rejection case, assert the exact error message and native error envelope
once per surface, including the `api_error` type and OpenAI `null` code and
parameter fields.

### 6.4 Buffered response opacity

In one focused case, return an actually compressed buffered body from the
upstream stub despite the identity request. Verify its status,
`Content-Encoding`, other allowed headers, and body bytes reach the downstream
client unchanged.

### 6.5 Full verification

Run formatting checks and the complete Go test suite after the focused
regressions pass.

## 7. Success criteria

- `/anthropic/v1/messages?beta=true` reaches Copilot with the exact query suffix
  intact.
- Every request forwarded by a current provider route advertises only identity
  encoding to Copilot.
- The SSE engine never receives a response declared with a non-identity content
  coding.
- Encoded SSE fails deterministically in the correct provider shape before the
  response is committed.
- Buffered responses retain their existing opaque passthrough behavior.
- No client `Accept-Encoding` parser, codec dependency, route-specific setting,
  or new public error type is introduced.
