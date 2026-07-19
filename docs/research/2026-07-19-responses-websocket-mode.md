# Responses API WebSocket mode: OpenAI, Anthropic, GitHub Copilot, and copilotd

Status: researched 2026-07-19. This note uses first-party documentation and
source code. It labels GitHub's documented SDK behavior separately from the
undocumented raw Copilot inference service.

## Executive answer

| Question | Answer |
| --- | --- |
| What is missing in copilotd? | A WebSocket-specific route and forwarding path. The current router accepts only HTTP `POST /openai/v1/responses`, and the current forwarder constructs an HTTP `POST`, then handles a buffered body or an SSE response. WebSocket support needs two independent upgrades (client-to-copilotd and copilotd-to-Copilot), a bidirectional message pump, WebSocket lifecycle/shutdown handling, catalog support for `ws:/responses`, and tests/metrics. The SSE pump is not reusable as the transport. ([router](../../internal/server/handler.go#L41-L50), [HTTP forwarder](../../internal/forward/forward.go#L321-L405)) |
| Does Anthropic Messages have a WebSocket mode? | No documented public mode. The public API is REST, `POST /v1/messages`; streaming is HTTP Server-Sent Events (SSE). This does not rule out private consumer transports, but those are not the Messages API contract. ([API overview](https://platform.claude.com/docs/en/api/overview), [Messages reference](https://platform.claude.com/docs/en/api/messages/create), [streaming guide](https://platform.claude.com/docs/en/build-with-claude/streaming)) |
| Does GitHub Copilot support either? | OpenAI Responses WebSocket: yes in GitHub's official Copilot runtime/SDK. GitHub documents `assistant.usage.apiEndpoint = "ws:/responses"`, and its SDK says CAPI WebSocket is enabled by default when the selected model advertises that endpoint. Anthropic Messages WebSocket: no documented equivalent, and the repository's 2026-07-18 live catalog capture advertises only `/v1/messages` for Claude models. ([GitHub event reference](https://docs.github.com/en/copilot/how-tos/copilot-sdk/features/streaming-events), [Copilot SDK type](https://github.com/github/copilot-sdk/blob/c638a5e3/nodejs/src/types.ts), [capture provenance](../../internal/catalog/testdata/README.md), [capture](../../internal/catalog/testdata/copilot-models-2026-07-18.json#L49-L61)) |

## 1. OpenAI Responses WebSocket contract

This is the ordinary Responses API over a persistent transport, not the
separate Realtime API.

### Connection and authentication

- Connect to `wss://api.openai.com/v1/responses` and authenticate the opening
  handshake with `Authorization: Bearer $OPENAI_API_KEY`.
- Send JSON client events. The non-beta public client-event contract currently
  has one event type, `response.create`.
- A `response.create` payload uses the same top-level fields as HTTP
  `POST /v1/responses`, with `type: "response.create"` added. Streaming is
  implicit: do not send `stream`; `background` is unsupported. The guide also
  says the transport-specific `background` field is not used.
- The server sends the existing typed Responses streaming events, in the same
  order as the HTTP streaming model. Common events include
  `response.created`, `response.output_text.delta`, `response.completed`, and
  `error`.

Sources: OpenAI's [WebSocket Mode guide](https://developers.openai.com/api/docs/guides/websocket-mode),
[WebSocket client-event reference](https://developers.openai.com/api/reference/resources/responses/websocket-events),
and [streaming-event reference](https://developers.openai.com/api/reference/resources/responses/streaming-events/).

Minimal first turn:

```json
{
  "type": "response.create",
  "model": "gpt-5.6",
  "store": false,
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [{"type": "input_text", "text": "Say hello."}]
    }
  ]
}
```

### Continuation and state

- For a later turn on the same socket, send another `response.create` with the
  preceding response's ID in `previous_response_id` and only the new input
  items. This is the same logical chaining contract as HTTP, with a faster
  connection-local path.
- The service keeps only the most recent previous-response state in a
  connection-local, in-memory cache. With `store=true`, an older response may
  be hydrated from persisted state; with `store=false` or Zero Data Retention,
  an uncached ID fails with `previous_response_not_found`.
- A failed continuation (`4xx` or `5xx`) evicts its referenced previous response
  from that connection-local cache.
- `response.create` with `generate: false` can warm instructions, tools, and
  messages without producing model output; it still returns an ID that can be
  continued.
- The separate HTTP `/responses/compact` endpoint returns a compacted input
  window, not a response ID. To use it after compaction, start a new WebSocket
  chain without `previous_response_id` and submit the compacted window as
  `input`.

Source: [OpenAI WebSocket Mode: continuation, warmup, and compaction](https://developers.openai.com/api/docs/guides/websocket-mode).

### Limits and recovery

- One WebSocket connection processes one response at a time. Multiple
  `response.create` messages are sequential; there is no multiplexing. Use
  multiple sockets for parallel runs.
- A connection lasts at most 60 minutes. On the limit, the service reports
  `websocket_connection_limit_reached`; reconnect.
- After reconnecting, a stored response can be resumed by ID. A `store=false`
  or ZDR chain normally needs its full input context resubmitted because its
  old connection-local cache is gone.
- OpenAI documents `previous_response_not_found` and
  `websocket_connection_limit_reached` as WebSocket-specific errors.

Sources: [OpenAI WebSocket Mode: connection behavior](https://developers.openai.com/api/docs/guides/websocket-mode)
and [OpenAI error codes](https://developers.openai.com/api/docs/guides/error-codes).

### HTTP/SSE versus WebSocket

| Property | HTTP Responses streaming | Responses WebSocket |
| --- | --- | --- |
| Start a response | Fresh `POST /v1/responses` | `response.create` JSON message on an established `wss://.../v1/responses` socket |
| Enable streaming | `stream=true` | Implicit; omit `stream` |
| Output framing | SSE (`event:` / `data:` records) | WebSocket messages containing the same typed Responses events |
| Tool-result continuation | Another HTTP request | Another `response.create` on the socket with `previous_response_id` and only new items |
| Warm state | State may be persisted, but every continuation is a new request | Most recent response is cached in memory on the active connection |
| Concurrent work | Independent requests can overlap | One in-flight response per socket; use more sockets for parallelism |
| Background mode | Available on the HTTP API | Unsupported |
| Connection lifetime | One HTTP request/response | Maximum 60 minutes |

Sources: [OpenAI HTTP streaming guide](https://developers.openai.com/api/docs/guides/streaming-responses),
[WebSocket guide](https://developers.openai.com/api/docs/guides/websocket-mode),
and [WebSocket event reference](https://developers.openai.com/api/reference/resources/responses/websocket-events).

## 2. What copilotd needs

### What exists now

The current OpenAI surface has only a method-qualified HTTP route,
`POST /openai/v1/responses`, forwarding to `/responses`.
`Forwarder.Handler` reads and caps the entire request body, runs HTTP request
shims, rejects HTTP `background:true`, and calls an HTTP-only path. That path
constructs `POST <credential.BaseURL>/responses`, calls `http.Client.Do`, then
chooses buffered copying or the SSE pump based on `Content-Type`.
([route](../../internal/server/handler.go#L41-L50),
[handler](../../internal/forward/forward.go#L124-L148),
[forward path](../../internal/forward/forward.go#L321-L405))

The identity seam is already useful: one `Credential` supplies the current
Copilot base URL, short-lived bearer token, and opaque impersonation headers.
The token exchange also already obtains the authoritative `endpoints.api`
origin. ([credential](../../internal/identity/identity.go#L20-L38),
[exchange response](../../internal/identity/manager.go#L59-L67),
[mint result](../../internal/identity/manager.go#L319-L338))

### Required implementation slices

1. **Reverse the explicit scope decision.** Two accepted design documents call
   Responses WebSocket a non-goal and deliberately exclude `ws:/responses`-only
   models. Update the roadmap/design record as part of the feature so the new
   transport, its shim boundary, and catalog semantics are intentional rather
   than accidental scope drift. ([middleware design](../design/2026-07-16-phase-3-middleware-framework-design.md#L723),
   [catalog design](../design/2026-07-18-phase-6a-provider-shaped-model-catalogs-design.md#L116-L125))

2. **Add a distinct inbound WebSocket route.** Register the WebSocket opening
   handshake on `GET /openai/v1/responses`, behind the same local API-key and
   readiness middleware as the HTTP route. A normal HTTP GET without a valid
   WebSocket upgrade should fail before any upstream dial.

3. **Add a WebSocket library and a dedicated dialer.** `go.mod` has no
   WebSocket implementation. The selected library must support both accepting
   an inbound socket and dialing the upstream `wss` socket, TLS verification,
   proxy settings, context cancellation, frame/message size limits, deadlines,
   ping/pong, and close-code propagation. The existing `http.Client` and SSE
   transport cannot perform this job.

4. **Dial Copilot separately.** Resolve a fresh credential before accepting or
   committing the downstream upgrade where practical; parse
   `Credential.BaseURL`, map `https -> wss` (`http -> ws` only for tests), and
   append `/responses`. Authenticate the upstream opening handshake with the
   Copilot bearer token plus the configured impersonation and correlation
   headers. Do not forward the caller's local API key. The existing helper
   deliberately strips `Connection` and `Upgrade`, so a WebSocket dial must let
   its library construct those hop-by-hop headers rather than blindly reusing
   the HTTP request map. ([header policy](../../internal/forward/forward.go#L489-L510),
   [credential injection](../../internal/forward/forward.go#L531-L552))

5. **Bridge messages bidirectionally.** Run one pump client -> upstream and one
   upstream -> client. Preserve message type, payload bytes, ordering, and
   close semantics. The lowest-risk first implementation should treat JSON
   messages as opaque and should not route them through the SSE reader/writer.
   On either side's read/write failure or cancellation, cancel the sibling pump
   and close both connections exactly once.

6. **Define WebSocket limits separately.** Reuse the configured request-byte
   cap as a per-message ceiling only if that is an explicit product decision;
   a long-lived socket has no single HTTP body to cap. Bound individual writes
   and the opening handshake. Do not automatically reuse SSE's application
   idle timeout: OpenAI documents the 60-minute connection cap but does not
   promise application messages during every period of model work. WebSocket
   ping/pong liveness and model-event idleness are different policies.

7. **Make middleware upgrade-aware.** The access logger wraps the writer in
   `statusWriter`. It exposes `Unwrap`, which works with
   `http.ResponseController`, but some WebSocket libraries type-assert the
   legacy `http.Hijacker` interface directly. Either choose an implementation
   that honors `Unwrap` or explicitly pass through every required optional
   interface. Record status `101`, connection duration, messages/bytes in each
   direction, and the terminal reason; ordinary HTTP body-byte accounting will
   not see post-upgrade traffic. ([middleware](../../internal/server/middleware.go#L30-L80),
   [writer wrapper](../../internal/server/middleware.go#L101-L133))

8. **Track upgraded connections for shutdown.** Go's `http.Server.Shutdown`
   does not close or wait for hijacked connections such as WebSockets. Register
   active sessions with the server, stop accepting new ones during shutdown,
   send an appropriate close frame, wait within the configured shutdown
   deadline, then force-close survivors. ([Go `Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown),
   [current lifecycle](../../internal/server/server.go#L63-L94))

9. **Decide the shim boundary.** Existing hooks operate on a complete HTTP
   request, an HTTP response prelude/body, or SSE frames; none model
   bidirectional WebSocket messages. The current canonical registry is only a
   disabled no-op, so opaque WebSocket pass-through is sufficient today. If
   future parity shims must edit `response.create` or server events, introduce
   explicit client-message/server-message hooks rather than pretending WebSocket
   messages are SSE frames. ([shim contract](../../internal/shim/shim.go))

10. **Teach the catalog about transport.** The live Copilot catalog uses the
   exact route token `ws:/responses`, but the code defines and filters only
   `/responses`. Add an `OpenAIResponsesWebSocketRoute` constant and decide
   whether `/openai/v1/models` represents the union of HTTP- and WS-callable
   models or only models common to both. The union exposes WebSocket-only models
   honestly for the whole OpenAI surface but means an HTTP client can select a
   model it cannot call; the intersection avoids that but hides valid WS models.
   ([route constants and exact filter](../../internal/catalog/catalog.go#L11-L18),
   [filter](../../internal/catalog/catalog.go#L91-L102),
   [test that currently excludes WS-only models](../../internal/catalog/filter_test.go#L8-L22),
   [captured Copilot routes](../../internal/catalog/testdata/copilot-models-2026-07-18.json#L52-L61))

11. **Test the protocol edges.** Use local WebSocket servers for upstream and
    downstream integration tests. Cover auth/readiness before upgrade, header
    replacement and secret non-leakage, exact text/binary message forwarding,
    multiple sequential `response.create` turns, backpressure, oversize
    messages, client/upstream close codes, half-failure cancellation, upstream
    handshake failures before the downstream `101`, panic/error behavior after
    upgrade, server shutdown, proxy/TLS behavior, and catalog membership.

This is a new transport path, not a small extension to `sse.Pump`. The reusable
parts are identity, local auth/readiness, request IDs, configuration, and the
general "payload-opaque forwarder" policy.

## 3. Anthropic Messages API

Anthropic documents the Claude API as RESTful and lists Messages as
`POST /v1/messages`. A Messages request is stateless: prior turns are sent in
the request's `messages` array. Its public streaming mode is the same HTTP POST
with `stream: true`, returning SSE events such as `message_start`,
`content_block_delta`, and `message_stop`; the official TypeScript SDK also
states explicitly that Messages streaming uses SSE.

Sources: [Claude API overview](https://platform.claude.com/docs/en/api/overview),
[Create a Message](https://platform.claude.com/docs/en/api/messages/create),
[Streaming Messages](https://platform.claude.com/docs/en/build-with-claude/streaming),
and [Anthropic TypeScript SDK](https://platform.claude.com/docs/en/api/sdks/typescript#streaming-responses).

Therefore, as of the research date, there is no public `wss://api.anthropic.com`
Messages endpoint, no Messages WebSocket client-event schema, and no documented
persistent previous-message cache equivalent to OpenAI's Responses WebSocket
mode. This is an absence claim about the public contract, not a claim about how
Claude's consumer applications communicate internally.

MCP is unrelated to this conclusion. The standard MCP transports are `stdio`
and Streamable HTTP (which may use SSE); custom transports are possible, but an
MCP transport connects a tool/client protocol and does not turn Anthropic
Messages into a WebSocket API. ([MCP transport specification](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports))

## 4. GitHub Copilot support

### Documented and official

GitHub's Copilot SDK event reference defines an `assistant.usage.apiEndpoint`
value of `"ws:/responses"` and calls it the WebSocket variant of the Responses
API. The current official SDK source exposes a CAPI session option named
`enableWebSocketResponses` (Python:
`enable_web_socket_responses`). It says WebSocket is the default whenever the
selected model advertises `ws:/responses`, and that setting it to `false` (or
setting `COPILOT_CLI_DISABLE_WEBSOCKET_RESPONSES`) forces HTTP Responses instead.

Sources: [GitHub streaming session events](https://docs.github.com/en/copilot/how-tos/copilot-sdk/features/streaming-events),
[Copilot SDK TypeScript `CapiSessionOptions`](https://github.com/github/copilot-sdk/blob/c638a5e3/nodejs/src/types.ts),
and [Copilot SDK Python `CapiSessionOptions`](https://github.com/github/copilot-sdk/blob/c638a5e3/python/copilot/client.py).

Microsoft's official VS Code source independently shows the raw CAPI behavior:
its model enum defines `WebSocketResponses = "ws:/responses"`; its WebSocket
manager sends a text JSON message consisting of `type: "response.create"`, the
HTTP Responses body minus `stream`, and a Copilot-specific
`initiator: "user" | "agent"` field. It records the last completed response ID
for `previous_response_id`, permits only one active request in its connection
manager, and falls back to HTTP if the WebSocket request fails. These details
are implementation evidence for Copilot, not additions to OpenAI's public
schema. ([endpoint enum](https://github.com/microsoft/vscode/blob/be161a6b320b4d69ed3bc654ca73a6f2adc287b6/extensions/copilot/src/platform/endpoint/common/endpointProvider.ts),
[WebSocket manager](https://github.com/microsoft/vscode/blob/be161a6b320b4d69ed3bc654ca73a6f2adc287b6/extensions/copilot/src/platform/networking/node/chatWebSocketManager.ts))

The same SDK source now also exposes `transport: "http" | "websockets"` for
OpenAI-compatible BYOK providers using `wire_api: "responses"`. That provides a
useful supported client for exercising copilotd once its local WebSocket route
exists. Anthropic provider configurations still use the Messages API, not an
Anthropic WebSocket mode. ([Python provider type](https://github.com/github/copilot-sdk/blob/c638a5e3/python/copilot/session.py),
[GitHub BYOK guide](https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/byok))

### Observable service evidence in this repository

The repository contains a focused, credential-free projection of a real
Copilot `GET /models` response captured through its raw passthrough on
2026-07-18. GPT models advertise `ws:/responses`; Claude models advertise
`/v1/messages` and `/chat/completions`, with no Messages WebSocket route. This
is strong point-in-time evidence, but it is not a stable public service
contract. ([provenance](../../internal/catalog/testdata/README.md),
[capture](../../internal/catalog/testdata/copilot-models-2026-07-18.json#L49-L61))

### Support boundary

The Copilot SDK/CLI behavior is public and supported. GitHub does not document
the underlying short-lived Copilot bearer exchange plus raw inference origin as
a general-purpose OpenAI-compatible customer API. copilotd's direct use of
`GET /copilot_internal/v2/token`, `endpoints.api`, `/models`, `/responses`, and
`ws:/responses` is therefore an internal/observable integration. It can be
tested, but it should retain explicit compatibility tests and must expect
unannounced changes. ([exchange endpoint](../../internal/identity/manager.go#L20-L26),
[exchange shape](../../internal/identity/manager.go#L59-L67))

## 5. Test setup

### A. OpenAI control test

In a WebSocket-capable HTTP client:

1. URL: `wss://api.openai.com/v1/responses`.
2. Opening header: `Authorization: Bearer <OPENAI_API_KEY>`.
3. Send the minimal `response.create` JSON shown above as a text message.
4. Read messages until `response.completed` or `error`.
5. Save the ID from `response.created`; send a second `response.create` with
   that ID as `previous_response_id` and only a new input item.

This verifies client framing independently of Copilot. The source contract is
OpenAI's [WebSocket Mode guide](https://developers.openai.com/api/docs/guides/websocket-mode).

### B. Local copilotd test after implementation

1. Start copilotd normally so it has a valid GitHub OAuth source and reports
   ready.
2. URL: `ws://127.0.0.1:<port>/openai/v1/responses` (or `wss://` behind TLS).
3. Opening header: `Authorization: Bearer <copilotd-managed-api-key>`; do not
   send a GitHub OAuth or Copilot token to the local surface.
4. Select a model whose raw `/models` entry advertises `ws:/responses`.
5. Send the same `response.create` event, consume through
   `response.completed`, then exercise `previous_response_id` on the same
   socket.
6. Also test two negative cases: wrong local key should fail before `101`, and
   two concurrent runs on one socket should be rejected or serialized by the
   upstream contract.

The current Copilot SDK can also be used as a higher-level client for this
local route by configuring an OpenAI-compatible BYOK provider:

```ts
const session = await client.createSession({
  model: "gpt-5.4",
  provider: {
    type: "openai",
    baseUrl: "http://127.0.0.1:<port>/openai/v1",
    apiKey: "<copilotd-managed-api-key>",
    wireApi: "responses",
    transport: "websockets"
  }
});
```

`transport: "websockets"` is in the current official SDK source even though
the GitHub BYOK guide's configuration table does not yet list it. Treat the
source type as the evidence for this test option. ([provider type](https://github.com/github/copilot-sdk/blob/c638a5e3/nodejs/src/types.ts),
[BYOK guide](https://docs.github.com/en/copilot/how-tos/copilot-sdk/auth/byok))

### C. Official Copilot SDK proof

Use a current GitHub Copilot SDK/CLI, select a model advertising
`ws:/responses`, and create a session with CAPI WebSocket enabled (it is the
default; setting it explicitly makes the test intention clear):

```ts
const session = await client.createSession({
  model: "gpt-5.4",
  capi: { enableWebSocketResponses: true },
  // normal permission handler/session options here
});
```

Observe the `assistant.usage` session event and verify `apiEndpoint` is
`"ws:/responses"`. Repeat with `enableWebSocketResponses: false` and expect
`"/responses"`. This is the supported way to establish that GitHub's runtime
uses both transports; it does not expose raw provider frames.

Sources: [Copilot SDK option](https://github.com/github/copilot-sdk/blob/c638a5e3/nodejs/src/types.ts)
and [usage event field](https://docs.github.com/en/copilot/how-tos/copilot-sdk/features/streaming-events#assistantusage).

### D. Raw Copilot exploratory test (unsupported contract)

Only use this to validate copilotd compatibility, not as a supported application
integration:

1. Exchange a GitHub OAuth token at
   `GET https://api.github.com/copilot_internal/v2/token` with
   `Authorization: token <github-oauth-token>`. Use the same version-sensitive
   identity headers as copilotd. Its current defaults are
   `Copilot-Integration-Id: vscode-chat`,
   `Editor-Version: vscode/1.104.1`,
   `Editor-Plugin-Version: copilot-chat/0.26.7`,
   `User-Agent: GitHubCopilotChat/0.26.7`, and
   `X-GitHub-Api-Version: 2025-04-01`; use configured values if they differ.
   Treat both the OAuth token and returned Copilot token as secrets.
2. Read `token` and `endpoints.api` from the response. Use that returned origin;
   do not hard-code a Copilot host.
3. `GET <endpoints.api>/models` with `Authorization: Bearer <copilot-token>` and
   the identity headers. Pick a visible model advertising `ws:/responses`.
4. Change only the origin scheme (`https -> wss`) and connect to
   `<origin>/responses`. Authenticate the opening handshake with the Copilot
   bearer plus the same identity headers.
5. Send a standard OpenAI `response.create` JSON message and record the opening
   status, close code, GitHub request ID, and event sequence. To emulate the
   official VS Code client closely, omit `stream` and add
   `"initiator": "user"` to the event; `initiator` is Copilot-specific and is
   not part of OpenAI's public WebSocket event schema. Do not include secrets
   in the capture.

The path, token shape, and identity headers above come from copilotd's current
integration, not a GitHub public inference-API specification:
[manager](../../internal/identity/manager.go#L20-L26),
[exchange response](../../internal/identity/manager.go#L59-L67),
[version-sensitive defaults](../../internal/config/config.go#L53-L61),
[configured identity headers](../../cmd/copilotd/main.go#L383-L393), and
[authenticated outbound headers](../../internal/forward/forward.go#L531-L552).
The Copilot-specific event envelope is visible in Microsoft's pinned
[VS Code WebSocket manager](https://github.com/microsoft/vscode/blob/be161a6b320b4d69ed3bc654ca73a6f2adc287b6/extensions/copilot/src/platform/networking/node/chatWebSocketManager.ts).

## Bottom line

Implement only the Responses WebSocket surface. There is no Anthropic Messages
WebSocket contract to mirror. GitHub Copilot gives both official client evidence
and live catalog evidence that `ws:/responses` is real, but the raw service edge
remains internal; preserve copilotd's payload-opaque design and put transport,
lifecycle, and compatibility tests around it.
