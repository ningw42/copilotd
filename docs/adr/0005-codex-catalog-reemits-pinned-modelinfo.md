# Codex catalog re-emits Codex's own complete release `ModelInfo`, mutating only named fields, opt-in

**Status:** accepted. This text folds in the freshness change from
[ADR-0009](0009-refresh-codex-models-from-latest-release-in-memory.md) and the
enablement and reviewer-fidelity change approved in
[#287](https://github.com/ningw42/copilotd/issues/287).

The Codex client-shaped catalog (Phase 6b), served on
`GET /openai/v1/models?client_version=…`, re-emits an accepted release of Codex's
own `models.json` **field-for-field per source entry**. Every accepted entry
must be a complete Codex `ModelInfo`; copilotd changes only the three named
fields below, and only when the operator opts into each mutation.

We do this because Codex, under command auth, merges a fetched catalog
**wholesale per slug** (`apply_remote_models`: `existing_models[i] = model` for
a known slug and `existing_models.push(model)` for an unknown one) with no
field-merge, and required `ModelInfo` fields have no fallback — an empty
`base_instructions` reaches the wire as `instructions: ""` and degrades the
active model. Re-emitting Codex's own complete entry is therefore the only
faithful way to add a single field. Because the `ModelInfo` type is
Codex-internal and unstable, and copilotd must never silently change a user's
model behavior, the feature and every capability-affecting mutation are opt-in.

## Source

The served source is the latest stable Codex release, fetched commit-addressed
into a memory-only cached value, with the vendored
[snapshot](../../internal/catalog/codexdata/models.json) as its embedded floor.
ADR-0009 governs refresh, acceptance, fallback, and `/readyz`.
[release.json](../../internal/catalog/codexdata/release.json) is the single
authority for the floor's current identity; the floor advances independently as
newer stable releases are audited.

## Enablement and membership

`--codex-catalog-enabled` (default `false`) is the only configuration switch for
serving the Codex shape on the OpenAI Catalog when `client_version` is present.
Aliases, reviewer routing, and live-limit overlays are optional additions, not
prerequisites. Enabling the catalog explicitly opts into the accepted release's
complete prompt and behavior metadata, even when it differs from the client's
bundle; there is no client-version compatibility gate.

Without add-ons, membership is the exact intersection of picker-visible,
Responses-forwardable Copilot models and accepted official Codex entries, in
Copilot order. An empty intersection returns `200` with `{"models":[]}`, not a
provider-shaped fallback.

## Enumerated mutations

Serving and altering are independent opt-ins. Without add-ons, every selected
source entry is preserved field-for-field, including the presence, absence,
null value, or non-null value of `auto_review_model_override`. Each add-on
controls only its named mutation, applied in this order:

1. **Alias slug substitution.** An explicitly configured Codex catalog alias
   replaces `slug` with its real live Copilot model ID. Exact official metadata
   wins over a configured alias source for the same slug.
2. **Reviewer routing.** A successfully applied per-main-model
   `codex-auto-review-model-overrides` entry, or the global
   `codex-auto-review-model` fallback, replaces `auto_review_model_override`.
   A configured reviewer replaces the field only when its target is in the
   complete emitted membership. Otherwise the add-on is skipped, the official
   field stays untouched, and a per-main-model warning is logged. A present but
   unusable per-main-model override does not fall back to the global
   configuration, and a per-main-model configuration does not alter reviewer
   fields on unrelated entries.
3. **Live-limit overlay.** Under `codex-catalog-override-limits`,
   `context_window` / `max_context_window` may be overlaid from that served
   model's live Copilot facts.

Alias-only and limits-only configurations retain the official reviewer field. An
official reviewer value is not filtered against emitted membership when no
deployment reviewer applies; preserving accepted metadata is not a guarantee of
that reviewer's Copilot availability.

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
  simplicity: the whole resolved membership is emitted.
- **Require an add-on before serving the Codex shape** — the original rule, so
  prompt-pinning was never gratuitous and a bare enable emitted no Codex shape.
  Superseded by #287: explicit catalog enablement is itself consent to the
  accepted release's metadata.
- **Remove the reviewer field unconditionally** — the original rule, including
  removal after an unusable configured override. Superseded by #287 in favor of
  preserving the official field exactly.

## Consequences

The deliberate divergences (design §13): each served value is
release-tag-pinned; a future required-field addition fails the accept-gate and
holds the last-good release or the vendored floor, so Codex retains complete
entries. Prompt/behavior values come from that accepted release; limits are
Codex's numbers unless the operator opts into the overlay; coverage is exact
Copilot-forwardable/accepted Codex intersections plus explicitly configured live
aliases with accepted metadata sources; and overriding official reviewer
routing requires operator config. Alias slugs are real Copilot inference IDs;
catalog aliasing itself does not rewrite inference requests.
[ADR-0020](0020-request-only-model-overrides.md) separately approves an opt-in,
request-only Model override without changing this catalog contract. In its
absence, the selected ID continues to be forwarded unchanged. Recorded in
`docs/design/2026-07-19-phase-6b-codex-model-catalog-auto-review-design.md` §13
and `docs/design/2026-09-01-codex-catalog-model-aliases-design.md` §9.

An unforwardable global reviewer logs once per affected advertised main model,
and each warning names both the main model and the reviewer. This is confined to
the off-by-default Codex catalog and has no wire-format, catalog-content, or
catalog-fidelity impact; see
`docs/design/2026-07-21-codex-per-model-auto-review-overrides-design.md` §6.

**Compatibility note (2026-09-05, audited against `rust-v0.153.4`):** that Codex
release treats command auth as API-key auth and defaults that provider mode's
Guardian reviewer to `gpt-5.6-luna`; `codex-auto-review` remains the
ChatGPT-auth default. Configured `auto_review_model_override` values still take
precedence, so the explicit routing mechanism remains valid but is no longer the
only way those command-auth clients can avoid `codex-auto-review`. The note has
not been re-audited against later floors.
