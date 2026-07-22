# Add an opt-in bidirectional WebSocket message-transform seam

**Status:** proposed

copilotd extends its composable shim contract to the OpenAI Responses WebSocket
transport. A shim gains two opt-in, per-direction hooks — `ClientMessageTransformer`
(client → upstream) and `ServerMessageTransformer` (upstream → client) — over the
same `shim.Registry` / `shim.Chain` that already backs the HTTP request, the
response Prelude, the buffered body, and the SSE stream. This reverses ADR-0006's
non-goal that the transport "does not ... introduce WebSocket message hooks."

The transport stays payload-opaque by default. With the canonical no-op registry
both directional adapters are nil and every message follows the current verbatim,
byte-for-byte, kind-preserving path; a message is interpreted only inside a shim
that opts in.

## Considered options

- **Keep the WebSocket transport hook-free** (ADR-0006's stance): rejected — a
  parity gap a shim closes on the HTTP/SSE path (for example, stabilizing OpenAI
  Responses item IDs) then cannot be closed on the WebSocket transport, splitting
  one shim concept across transports.
- **A WebSocket-specific transform mechanism**: rejected — a second, parallel
  registry and chain would fork the unified shim concept and duplicate the
  fold/ordering logic, and a shim could not share logic and state across HTTP, SSE,
  and WebSocket.
- **Reuse `shim.Registry` / `shim.Chain` with two opt-in directional interfaces,
  1→1 + drop, and no holding** (chosen): one unified shim spans every transport; the
  seam is a linear per-direction fold entered only when a shim participates, with no
  Finalize machinery.

## Consequences

- Default behavior is unchanged: the no-op registry yields nil adapters and a
  verbatim transport, preserving ADR-0006's payload-opaque guarantee and its
  performance.
- The seam is a per-direction fold in the WebSocket pump; it does not reuse the SSE
  pump or its Finalize machinery.
- A transform maps one message to at most one message, or drops it. It may not hold
  an emission for later release, nor split one message into many. Coalescing is
  expressible through shim state keyed on an in-band terminal / per-item done marker.
- A transform error is fatal to the session: both sockets close with 1011 and the
  session terminal is classified an error, reusing the existing close-code and
  terminal machinery.
- Shim-facing observability (a logger and a metrics emitter) is not introduced here;
  it is a separate, transport-agnostic decision.
- ADR-0006 is amended, not superseded: its payload-opaque forwarding decision
  stands; only its "WebSocket message hooks" non-goal is reversed.

See `docs/design/2026-07-22-websocket-shim-message-transform-design.md`.
