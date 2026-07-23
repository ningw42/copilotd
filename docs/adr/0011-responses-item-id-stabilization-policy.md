# Stabilize churning OpenAI Responses item ids, opt-in and off by default

**Status:** accepted

copilotd will, **opt-in and off by default**, stabilize the per-item `id` that GitHub
Copilot churns on the OpenAI Responses stream: it pins one genuine upstream id per
`output_index` and rewrites the later id-bearing events on that index to it, across
**both** the SSE and WebSocket `/responses` transports, touching only item-id fields.
When the toggle is off (the default) both transports stay byte-for-byte verbatim.
The shim is registered as `responses-item-id-stabilizer`; the operator toggle is
`--shim-responses-item-id-stabilizer-enabled`, represented in configuration as
`ShimResponsesItemIDStabilizerEnabled`.

This is a **divergence from verbatim forwarding** — but of the **Alteration** kind,
not **Fabrication** (`CONTEXT.md`; `docs/divergence-ledger.md`): only a genuine
first-seen upstream id is ever reused, so nothing is minted and the wire keeps only
upstream-basis values. It therefore honors the shim policy invariant "must not
fabricate information without an upstream basis," and, like every divergence, it is
identified off-band, never by a field on the wire.

[ADR-0010](0010-bidirectional-websocket-message-transform-seam.md) accepted the
cross-transport transform **seam**; this ADR accepts the
**divergence-from-verbatim policy** that seam now carries — the seam's anticipated
first consumer.

## Why

Copilot emits a different opaque `id` on every event for the same streamed item,
while the OpenAI Responses contract treats an item's `id` as fixed for its whole
lifecycle. The churn is invisible to position-keyed clients (OpenAI `codex`) and
**fatal** to id-keyed clients (the Vercel AI SDK `@ai-sdk/openai`, e.g. via opencode),
which throw on the first GPT-5 message. Making the id stable is the client-agnostic
fix, and the only stable per-item anchor upstream is `output_index`.

## Considered options

- **Do nothing (stay byte-verbatim):** rejected — leaves spec-strict, id-keyed
  clients broken against a Copilot-side contract violation copilotd is positioned to
  absorb.
- **Fabricate a synthetic stable id:** rejected — crosses the Fabrication line and
  breaks the never-fabricate-without-upstream-basis invariant; needless, since a
  genuine upstream id per `output_index` already exists to pin.
- **On by default:** rejected — the divergence must be a governed, per-deployment
  choice; transparent verbatim forwarding stays the default so position-keyed clients
  and byte-for-byte expectations are untouched.
- **Stabilize opt-in, off by default, reusing a genuine upstream id, on both
  transports** (chosen): closes the parity gap once across SSE and WebSocket, stays
  within the Alteration policy, and preserves the verbatim default.

## Consequences

- A new, governed **Alteration** entry in the divergence ledger — the first
  divergence that is not a copilotd-originated signal. The "copilotd-originated
  signal" glossary entry is amended to stop claiming it is the *only* divergence.
- When enabled, a rewritten event's JSON object-key order may reshuffle (semantically
  neutral); untouched fields — `encrypted_content`, `content`, `summary`,
  `summary_index`, `call_id` — keep their exact bytes.
- The transform never faults a stream or session: any payload it cannot confidently
  rewrite is forwarded verbatim, so enabling it can only stabilize ids, never error.
- Scoped to the OpenAI `/responses` endpoint, so enabling it leaves every other
  surface on its byte-verbatim fast path.

References ADR-0002 (payload-opaque SSE), ADR-0003 (off-band origin of copilotd's own
signals), ADR-0006 and ADR-0010 (WebSocket transport and transform seam).

See `docs/design/2026-07-23-responses-item-id-stabilization-design.md`.
