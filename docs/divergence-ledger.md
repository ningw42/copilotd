# Divergence ledger

The complete accounting of **every way copilotd's wire output departs from pure
verbatim forwarding of Copilot.** copilotd is a transparent proxy; each entry here is
a deliberate, governed exception to that transparency. Every divergence is identified
**off-band** (request-id, logs) and **never by a field on the wire** — a client
cannot tell a diverged response from a genuine first-party one by inspecting the
payload.

The taxonomy (`Fabrication`, `Alteration`, `Omission`) is defined in `CONTEXT.md`;
this doc only enumerates. Each entry **points at its authoritative source** rather
than restating it, so the ledger stays exhaustive by construction: the source is the
single place the divergence is defined, and this row is a pointer to it.

## Fabrication — information on the wire with no upstream basis

| Divergence | Source of truth | What it diverges |
|---|---|---|
| copilotd-originated error signals | `internal/apierror` — the `Kind` enum ("every copilotd-originated error condition"): `Unauthorized`, `NotReady`, `BackgroundUnsupported`, `NotAWebSocketUpgrade`, `PayloadTooLarge`, `BadGateway`, `GatewayTimeout`, `ShimError`, `InvalidRequest` | copilotd returns its own surface-shaped error response instead of forwarding one from Copilot (missing/invalid API key, no credential, oversized body, unreachable/slow upstream, a shim rejection). |
| Synthesized stream terminals | [ADR-0003](adr/0003-synthesized-stream-terminals-off-band-origin.md) + the SSE engine | copilotd appends a terminal error event when an upstream SSE stream on an SSE-semantic Route dies without one, so a client's SSE parser never hangs. |

## Alteration — an upstream-basis value rewritten to another, fabricating nothing

| Divergence | Source of truth | What it diverges | Toggle |
|---|---|---|---|
| Responses item-id stabilizer | [ADR-0011](adr/0011-responses-item-id-stabilization-policy.md) + [design](design/2026-07-23-responses-item-id-stabilization-design.md); shim `responses-item-id-stabilizer` | Rewrites Copilot's churning per-event item `id` to one genuine upstream id per `output_index`, on both OpenAI `/responses` transports (SSE + WebSocket), so `id`-keyed clients stop corrupting. No id is minted — every value on the wire retains an upstream basis. | `--shim-responses-item-id-stabilizer-enabled` / `ShimResponsesItemIDStabilizerEnabled` — **off by default** |

## Omission — dropping or coalescing upstream content

None yet. The shim seam permits it (`emit=false`, coalesce-via-state), but no shipped
divergence drops or coalesces content, so this kind has no entries. This section gains
rows only when an Omission divergence first ships.
