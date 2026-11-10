# Forward OpenAI Responses WebSocket messages payload-opaquely, with an opt-in transform seam

**Status:** accepted; incorporates the former ADR-0010 (message-transform seam);
post-commit hook contract set by [ADR-0014](0014-infallible-post-commit-shim-hooks.md)

copilotd forwards the OpenAI Responses WebSocket transport as a distinct
bidirectional path alongside the existing HTTP/SSE path. The forwarder accepts a
WebSocket on `GET /openai/v1/responses`, dials GitHub Copilot's upstream
`ws:/responses` transport, and preserves each message's type, payload bytes, and
order without interpreting the payload. This extends ADR-0002's payload-opaque
stance to the new transport: copilotd pays only for the transport framing needed
to forward faithfully and does not deserialize application messages.

The transport also carries copilotd's composable shim contract. A shim may opt
into two per-direction hooks — `ClientMessageTransformer` (client → upstream) and
`ServerMessageTransformer` (upstream → client) — over the same `shim.Registry` /
`shim.Chain` that backs the HTTP request, the response Prelude, the buffered
body, and the SSE stream. The transport stays payload-opaque by default: with the
canonical no-op registry both directional adapters are nil and every message
follows the verbatim, byte-for-byte, kind-preserving path. A message is
interpreted only inside a shim that opts in.

The transport was first accepted hook-free; the seam was added later, recorded
as ADR-0010, and is merged here so the transport has one decision record.

## Considered options

- **Keep WebSocket forwarding out of scope**: rejected — GitHub Copilot exposes
  the Responses WebSocket transport, and leaving it unreachable prevents
  WebSocket-capable clients from using that mode through copilotd.
- **Parse typed WebSocket messages**: rejected — typed decoding and re-encoding
  could drop unknown fields or reject new event types, violating the
  payload-opaque forwarding policy.
- **Keep the transport hook-free** (the original stance): rejected — a parity gap
  a shim closes on the HTTP/SSE path (for example, stabilizing OpenAI Responses
  item IDs) then cannot be closed on the WebSocket transport, splitting one shim
  concept across transports.
- **A WebSocket-specific transform mechanism**: rejected — a second, parallel
  registry and chain would fork the unified shim concept and duplicate the
  fold/ordering logic, and a shim could not share logic and state across HTTP,
  SSE, and WebSocket.
- **Separate payload-opaque forwarder that reuses `shim.Registry` / `shim.Chain`
  through two opt-in directional interfaces, 1→1 + drop, no holding** (chosen):
  the transport is accepted and dialed independently of HTTP/SSE, and one unified
  shim spans every transport through a linear per-direction fold entered only
  when a shim participates.

## Consequences

- WebSocket forwarding is a separate transport path. Its seam is a per-direction
  fold in the WebSocket pump; it does not reuse the SSE pump or its Finalize
  machinery.
- Unknown fields and event types pass through untouched because application
  payloads are never deserialized outside an opted-in shim.
- A transform maps one message to at most one message, or drops it. It may not
  hold an emission for later release, nor split one message into many. Coalescing
  is expressible through shim state keyed on an in-band terminal / per-item done
  marker.
- The directional hooks are infallible under ADR-0014: a shim that cannot
  interpret a message passes it through unchanged. Only a transform panic is
  fatal to the session: the pump contains it, both sockets close with 1011, and
  the session terminal is classified an error.
- The seam introduces no shim-facing logger or metrics emitter; hook timing is
  observed by the framework under
  [ADR-0016](0016-observe-shim-hook-overruns-without-controlling-execution.md).
- The provider-shaped OpenAI catalog remains keyed to the exact HTTP
  `/responses` Route. Models advertising only `ws:/responses` stay intentionally
  excluded, and no WebSocket-specific catalog Route constant is introduced.

See `docs/design/2026-07-19-openai-responses-websocket-forwarding-design.md` and
`docs/design/2026-07-22-websocket-shim-message-transform-design.md`.
