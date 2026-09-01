# Codex catalog re-emits Codex's own complete release `ModelInfo`, mutating only named fields, opt-in

**Status:** accepted; freshness/pinning amended by ADR-0009

**Amendment:** ADR-0009 changes the fixed `rust-v0.144.5` vendored snapshot
from the sole served source into the embedded fallback of a memory-only cached
value that follows the latest stable Codex release and fetches content at its
resolved commit. The embedded floor was advanced to `rust-v0.151.0` on
2026-08-31 after its current contract was audited. This ADR's fidelity
contract remains in force: every accepted entry must be a complete Codex
`ModelInfo`, re-emitted field-for-field except for the enumerated explicit-alias
slug substitution, reviewer routing, and live-limit overlay.

**Compatibility note (2026-08-31):** Codex `rust-v0.151.0` treats command auth
as API-key auth and defaults that provider mode's Guardian reviewer to
`gpt-5.6-luna`; `codex-auto-review` remains the ChatGPT-auth default. Configured
`auto_review_model_override` values still take precedence, so this ADR's
explicit routing mechanism remains valid but is no longer the only way current
command-auth clients can avoid `codex-auto-review`.

The Codex client-shaped catalog (Phase 6b), served on
`GET /openai/v1/models?client_version=…`, re-emits an accepted release of Codex's
own `models.json` (latest stable release, commit-addressed, with the vendored
`rust-v0.151.0` floor) **field-for-field per source entry**, applying only this
enumerated sequence of mutations: an explicitly configured Codex catalog alias
replaces `slug` with its real live Copilot model ID;
`auto_review_model_override` is removed and then optionally injected from a
per-main-model `codex-auto-review-model-overrides` entry or the global
`codex-auto-review-model` fallback; and, under the opt-in
`codex-catalog-override-limits`, `context_window` / `max_context_window` may be
overlaid from that served model's live Copilot facts. Exact official metadata
wins over a configured alias source for the same slug. We do this because Codex,
under command auth, merges a fetched catalog **wholesale per slug**
(`apply_remote_models`: `existing_models[i] = model` for a known slug and
`existing_models.push(model)` for an unknown one) with no field-merge, and
required `ModelInfo` fields
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
  simplicity: the whole resolved membership is emitted, but *only* when there is
  something to inject (an explicit alias, a reviewer, or the limits overlay), so
  prompt-pinning is never gratuitous — a bare `codex-catalog-enabled=true` emits
  nothing and Codex falls back to its own bundle.

## Consequences

The deliberate divergences (design §13, amended by ADR-0009): each served value
is release-tag-pinned; a future required-field addition fails the accept-gate and
holds the last-good release or `rust-v0.151.0` floor, so Codex retains complete
entries. Prompt/behavior values come from that accepted release; limits are
Codex's numbers unless the operator opts into the overlay; coverage is exact
Copilot-forwardable/accepted Codex intersections plus explicitly configured live
aliases with accepted metadata sources; and auto-review requires operator
config. Alias slugs are real Copilot inference IDs and are not rewritten on the
inference path. Recorded in
`docs/design/2026-07-19-phase-6b-codex-model-catalog-auto-review-design.md` §13
and `docs/design/2026-09-01-codex-catalog-model-aliases-design.md` §9.

The per-model routing extension deliberately changes the existing opt-in log
behavior: an unforwardable global reviewer now logs once per affected advertised
main model, and each warning names both the main model and the reviewer. This
change is confined to the off-by-default Codex catalog and has no wire-format,
catalog-content, or catalog-fidelity impact. Its rationale and boundary are
recorded in
`docs/design/2026-07-21-codex-per-model-auto-review-overrides-design.md` §6.
