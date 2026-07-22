# Forward OpenAI Responses WebSocket messages payload-opaquely

**Status:** accepted; message-transform seam added by ADR-0010

**Amendment:** ADR-0010 adds an opt-in, bidirectional message-transform
seam for this transport, reversing the "introduce WebSocket message hooks" non-goal
recorded below. The seam does not reuse the SSE pump, and the payload-opaque forwarding
decision is unchanged: with the canonical no-op registry the transport is
byte-for-byte verbatim, and a message is interpreted only inside a shim that opts in.

copilotd will forward the OpenAI Responses WebSocket transport as a distinct
bidirectional path alongside the existing HTTP/SSE path. The forwarder accepts a
WebSocket on `GET /openai/v1/responses`, dials GitHub Copilot's upstream
`ws:/responses` transport, and preserves each message's type, payload bytes, and
order without interpreting the payload. This extends ADR-0002's payload-opaque
stance to the new transport: copilotd pays only for the transport framing needed
to forward faithfully and does not deserialize application messages.

## Considered options

- **Keep WebSocket forwarding out of scope**: rejected — GitHub Copilot exposes
  the Responses WebSocket transport, and leaving it unreachable prevents
  WebSocket-capable clients from using that mode through copilotd.
- **Parse typed WebSocket messages**: rejected — typed decoding and re-encoding
  could drop unknown fields or reject new event types, violating the
  payload-opaque forwarding policy without a current transformation need.
- **Separate, payload-opaque WebSocket forwarder** (chosen): accept and dial the
  transport independently of HTTP/SSE, then copy message types and payload bytes
  bidirectionally without consulting the shim registry.

## Consequences

- WebSocket forwarding is a separate transport path; it does not reuse the SSE
  pump or introduce WebSocket message hooks.
- Unknown fields and event types pass through untouched because application
  payloads are never deserialized.
- The provider-shaped OpenAI catalog remains keyed to the exact HTTP
  `/responses` Route. Models advertising only `ws:/responses` stay intentionally
  excluded, and no WebSocket-specific catalog Route constant is introduced.

See `docs/design/2026-07-19-openai-responses-websocket-forwarding-design.md`.
