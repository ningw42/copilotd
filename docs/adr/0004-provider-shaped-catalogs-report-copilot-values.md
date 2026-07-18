# Provider-shaped catalogs carry the provider's schema with Copilot's values

**Status:** accepted

The `/anthropic/models` and `/openai/models` catalogs (Phase 6a) render each model
in the genuine provider's `GET /v1/models` **envelope and per-model schema**, but
populate it with **Copilot's own values** — not the provider's published data.
copilotd only knows Copilot's view of a model, and that view diverges from the
real provider's in ways copilotd cannot reconcile without inventing data. We chose
schema fidelity plus honest Copilot values over value-level provider parity,
because fabricating the provider's numbers would violate the no-fabrication rule
(Phase 3 §10.2) that governs everything copilotd puts on the wire.

## Considered options

- **Provider-identical values** — map token limits from the context window, emit
  the full capability tree with best-effort defaults, reshape `enabled` thinking
  to match Anthropic. Rejected: Copilot does not supply the provider's real
  numbers or its full capability tree, so matching them means inventing values the
  upstream never sent.
- **Schema-shaped, Copilot's values** (chosen) — the provider's schema, Copilot's
  values, and omission (not fabrication) where Copilot gives no basis.

## Consequences

The accepted, enumerated divergences from the genuine provider (design §5.5):

- Token limits are Copilot's forwardable budgets, which can be lower than the
  provider's published ceilings (e.g. `claude-opus-4.6`: 936K/64K vs 1M/128K).
- The Anthropic `capabilities` tree is a **subset** — leaves Copilot gives no
  basis for (`batch`, `citations`, `code_execution`, `context_management`) are
  omitted. A client doing unguarded bracket access into an omitted leaf (which
  Anthropic's own SDK guidance encourages) will not find it.
- `capabilities.thinking.types.enabled.supported` is inferred from Copilot's
  advertised budget and reads `true` for adaptive-only models (`opus-4.7/4.8`,
  `sonnet-5`) where the genuine API returns `false`; the discriminating fact is
  absent from Copilot's data, so no mapping can reproduce it.
- `owned_by` is Copilot's `vendor` verbatim (`"Azure OpenAI"`, `"Microsoft"`), not
  OpenAI's `"openai"`/`"system"` convention.
- List order is Copilot's `data[]` order, not "most recently released first."

Catalog membership is keyed on the wire-Surface, not vendor: `/openai/models`
lists every model forwardable on the Responses Surface, including non-OpenAI-vendor
models, because vendor-gating would make the catalog under-report what a client can
actually call.

The `created_at` / `created` epoch stub is **not** a divergence for Anthropic —
the live docs sanction an epoch value for an unknown release date; for OpenAI it is
a genuine stub kept so strict SDKs still deserialize.

See `docs/design/2026-07-18-phase-6a-provider-shaped-model-catalogs-design.md`
§§1, 5.5.
