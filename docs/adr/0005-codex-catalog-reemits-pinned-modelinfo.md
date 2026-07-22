# Codex catalog re-emits Codex's own complete release `ModelInfo`, mutating only named fields, opt-in

**Status:** accepted; freshness/pinning amended by ADR-0009

**Amendment:** ADR-0009 changes the fixed `rust-v0.144.5` vendored snapshot
from the sole served source into the embedded fallback of a memory-only cached
value that follows the latest immutable Codex release tag. This ADR's fidelity
contract remains in force: every accepted entry must be a complete Codex
`ModelInfo`, re-emitted field-for-field except for the enumerated reviewer and
limit overlays.

The Codex client-shaped catalog (Phase 6b), served on
`GET /openai/v1/models?client_version=…`, re-emits an accepted release of Codex's
own `models.json` (latest immutable release tag, with the vendored
`rust-v0.144.5` floor) **field-for-field per slug**,
overwriting only an enumerated set of keys — `auto_review_model_override` (injected
from a per-main-model `codex-auto-review-model-overrides` entry or the global
`codex-auto-review-model` fallback) and, under the opt-in
`codex-catalog-override-limits`, `context_window` / `max_context_window`. We do
this because Codex, under command auth, merges a fetched catalog **wholesale per
slug** (`apply_remote_models`:
`existing_models[i] = model`) with no field-merge, and required `ModelInfo` fields
have no fallback — an empty `base_instructions` reaches the wire as
`instructions: ""` and degrades the active model. Re-emitting Codex's own complete
entry is therefore the only faithful way to add a single field. The feature is
opt-in (`codex-catalog-enabled=false` by default) and every capability-affecting
overlay is separately opt-in, because the `ModelInfo` type is Codex-internal and
unstable and copilotd must never silently change a user's model behavior.

## Considered options

- **Synthesize `ModelInfo` from Copilot's data** — rejected: Codex requires ~18
  fields Copilot never returns (`base_instructions`, `truncation_policy`,
  `supported_reasoning_levels`, `model_messages`, …); fabricating them violates the
  no-fabrication rule, and an empty required field degrades the active model.
- **Payload-rewrite aliasing of `codex-auto-review`** — rejected: breaks under the
  Responses WebSocket transport (the rejected upstream PR); the catalog-native
  `auto_review_model_override` survives both HTTP and WSS. This is the same lever
  OpenAI's own Amazon Bedrock provider uses (routing auto-review to `gpt-5.4`).
- **Emit only the entries we inject into** (minimal blast radius) — rejected for
  simplicity: the whole intersection is emitted, but *only* when there is something
  to inject (a reviewer or the limits overlay), so prompt-pinning is never
  gratuitous — a bare `codex-catalog-enabled=true` emits nothing and Codex falls
  back to its own bundle.

## Consequences

The deliberate divergences (design §13, amended by ADR-0009): each served value
is release-tag-pinned; a future required-field addition fails the accept-gate and
holds the last-good release or `rust-v0.144.5` floor, so Codex retains complete
entries. Prompt/behavior values come from that accepted release; limits are
Codex's numbers unless the operator opts into the overlay; coverage is the
intersection of Copilot-forwardable and accepted Codex slugs; and auto-review
requires operator config. Recorded in
`docs/design/2026-07-19-phase-6b-codex-model-catalog-auto-review-design.md` §13.

The per-model routing extension deliberately changes the existing opt-in log
behavior: an unforwardable global reviewer now logs once per affected advertised
main model, and each warning names both the main model and the reviewer. This
change is confined to the off-by-default Codex catalog and has no wire-format,
catalog-content, or catalog-fidelity impact. Its rationale and boundary are
recorded in
`docs/design/2026-07-21-codex-per-model-auto-review-overrides-design.md` §6.
