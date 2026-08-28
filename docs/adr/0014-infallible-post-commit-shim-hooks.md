# Make post-commit shim hooks infallible

**Status:** accepted

Post-commit shim hooks are infallible across the SSE and WebSocket transports.
`EventTransformer.TransformEvent` and `StreamFinalizer.Finalize` return frames
only. `ClientMessageTransformer.TransformClientMessage` and
`ServerMessageTransformer.TransformServerMessage` return only whether to emit the
message. A post-commit shim that cannot confidently interpret an input declines by
passing it through unchanged; intentional drop or hold semantics remain available
through an empty frame result or `emit=false`.

Pre-commit hooks remain fallible. `RequestTransformer`, `PreludeTransformer`, and
`BufferedTransformer` retain their error returns because the forwarder can still
honor those errors as a clean HTTP rejection before committing the response.

## Why

Once an HTTP stream is committed, a returned error cannot change its status or
replace its body. Once a WebSocket session is established, treating one
unrecognized message as fatal destroys the whole multi-turn session. The only
responses available after commit are therefore disproportionate: corrupt a stream
that has already started or tear down a live session. Neither is sound for the
ordinary case where a parity shim does not understand a payload.

Passthrough is always available and defensible for a parity proxy: forwarding what
Copilot sent is copilotd's baseline behavior. Making that discipline part of the
hook signatures lets the compiler prevent routine post-commit error propagation
rather than relying on review.

This decision is not based on current usage counts. The pre-commit hooks also have
no shipped implementations, yet their errors remain sound because they can be
rendered before commit. Current usage only makes this contract change inexpensive;
the commit boundary makes it correct.

## Considered options

- **Keep fallible post-commit hooks and retain transport error machinery:**
  rejected. The signature promises a recoverable outcome that neither transport
  can honor soundly, and the SSE finalize sweep must preserve subtle partial-output
  rules solely to service that promise.
- **Make every shim hook infallible:** rejected. Before commit, a request, Prelude,
  or buffered-body transform can legitimately reject with a provider-shaped HTTP
  response. Removing that channel would discard useful behavior for no
  post-commit reason.
- **Make only post-commit hooks infallible and decline by passthrough** (chosen):
  the type boundary follows the point at which an error can no longer be honored.

## Consequences

- The SSE frame-transformer seam and shim stream adapter are infallible. Their
  per-frame fold and finalize sweep are straight inner-to-outer compositions; the
  retain-versus-discard state space and its content-loss limitation disappear.
- The WebSocket directional adapters are infallible linear folds. `emit=false`
  still deliberately drops a message, while uncertainty leaves the message
  byte-verbatim and returns `emit=true`.
- A panic remains a programmer bug and is contained at the transport pump. Before
  an SSE terminal, it renders the existing shim-failure terminal and
  `shim_error` outcome. After a terminal, no further bytes are emitted; the clean
  wire outcome and suppressed-shim-error counter preserve no-double-up while a
  metadata-only warning reports the bug. WebSocket panics close both peers with
  1011 and classify the session terminal as `error`.
- The panic paths reuse the existing bounded metric outcomes and warn-level access
  or session logging. They do not expose payloads, panic values, or stacks on the
  client-visible surface.
- The no-op registry continues to select the byte-verbatim fast paths. The
  Responses item-id stabilizer is the canonical decline-by-passthrough example:
  malformed or unrecognized payloads remain unchanged on both transports.
- Promptness and SSE terminal position remain review-enforced authoring
  obligations; this decision adds no execution-time bound and no framework
  policing of post-terminal content.
- [ADR-0010](0010-bidirectional-websocket-message-transform-seam.md) is amended,
  not superseded. Its bidirectional seam, fold order, one-to-at-most-one
  cardinality, and payload-opacity decisions remain in force; only routine
  transform errors are removed, leaving panic as the shim-originated fatal path.

See `docs/design/2026-07-16-phase-3-middleware-framework-design.md` and
`docs/design/2026-07-22-websocket-shim-message-transform-design.md`.
