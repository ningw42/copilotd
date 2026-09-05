# Inference model-name mapping

copilotd does not provide a generic inference shim that translates a client's
model alias to a different GitHub Copilot model ID and then restores the
client's requested name in responses.

## Why this is out of scope

The inference path is raw-passthrough-first. Rewriting a model ID changes the
meaning of a request and requires copilotd to own an alias policy that can drift
from GitHub Copilot's live model support. Restoring the requested name would
also expand the Alteration across buffered responses, SSE events, and WebSocket
messages, including every response field in which the model can appear.

No concrete compatibility failure currently justifies that cost. The recorded
Anthropic model-ID probe found that GitHub Copilot accepted both the dotted
catalog spellings and the hyphenated provider spellings tested. That mismatch
was therefore a catalog-presentation concern and is handled by the optional
Anthropic catalog model-ID normalization without changing inference requests.
Likewise, Codex catalog aliases advertise real, live Copilot model IDs and Codex
sends those IDs unchanged; they do not require inference rewriting.

Keeping a generic mapping feature in anticipation of an unknown client alias
would add policy and transport complexity without a demonstrated parity gap.
The existing shim framework could host a narrowly scoped mapping later, but
framework capability alone is not a reason to ship an Alteration.

## Reconsideration criteria

Reconsider this decision when there is a reproducible client compatibility
failure that identifies:

- the exact client alias rejected by GitHub Copilot and the accepted Copilot
  model ID it must map to;
- the affected Surface, Route, and HTTP, SSE, or WebSocket transports;
- the authoritative mapping source and behavior for unknown or stale aliases;
- the response fields that must preserve the client's requested name; and
- why catalog advertisement or client configuration cannot solve the mismatch.

Any accepted implementation must remain opt-in, preserve unknown fields,
decline by passthrough when it cannot interpret a post-commit payload, avoid
cross-family translation and upstream calls from the shim, and be recorded as
an Alteration in the divergence ledger.

## Prior requests

- [#188 — Evaluate inference model-name mapping](https://github.com/ningw42/copilotd/issues/188)
