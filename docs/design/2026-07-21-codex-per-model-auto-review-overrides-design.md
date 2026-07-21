# Per-model `auto_review_model_override` reviewer routing (`ningw42/copilotd#54`) — Design

Status: approved (polished via brainstorming + grilling)
Date: 2026-07-21
Tracking issue: `ningw42/copilotd#54` → closed and superseded by epic `ningw42/copilotd#88`
Builds on: `docs/design/2026-07-19-phase-6b-codex-model-catalog-auto-review-design.md`
(the global-only reviewer; this design was split out of that one during grilling — Phase 6b §2.2).

## 1. Goal and outcome

Phase 6b ships **global-only** reviewer routing: a single `codex-auto-review-model`
slug is injected as `auto_review_model_override` on every advertised Codex-catalog
model. This design adds the deferred **per-main-model** override — routing
different *main* models to different reviewer models (e.g. a cheap reviewer for a
cheap main model, a stronger one for a flagship).

A new config key `codex-auto-review-model-overrides` carries a `map[string]string`
(`main-slug → reviewer-slug`). Reviewer resolution becomes **per advertised
model** inside `RenderCodex`: `overrides[slug] ?? global ?? ""`. Everything else
about the Codex catalog — the vendored snapshot, the live-forwardable intersection,
verbatim field emission, the limits overlay, and the `?client_version=` content
negotiation — is unchanged.

**Outcome:** an operator who already runs the Codex catalog can point specific
main models at specific reviewers, with a small friendly config string, while the
common case (one reviewer for everything) keeps working exactly as before. The
feature is off by default and inert unless the catalog is enabled and there is
something to inject.

This key is copilotd's **first map-valued configuration**. The design keeps that
novelty contained: the value travels through the existing scalar precedence
machinery as a plain string and is parsed **once** into a map at `Resolve`, so the
precedence engine is untouched and malformed input fails fast at serve startup.

### 1.1 Grounding

The Codex-side facts were verified against `openai/codex` at tag `rust-v0.144.5` in
the Phase 6b design (§1.1) and are unchanged here; the load-bearing ones for this
design:

- **Reviewer resolution lives on the main model's `ModelInfo`.** Codex computes
  `review_model_id = turn.model_info.auto_review_model_override
  .unwrap_or(provider.approval_review_preferred_model())`
  (`core/src/guardian/review.rs`). The per-model override we inject is therefore
  read off the *main* model's entry — exactly what a per-main-model map targets.
- **Any forwardable model is a valid reviewer.** The guardian review runs Codex's
  *own* policy (`guardian_policy_config` → reviewer's
  `model_messages.auto_review.policy` → compiled-in default), not the reviewer's
  `base_instructions` (`core/src/guardian/review_session.rs`). So the reviewer
  choice is a cost/latency matter, not a correctness one — no "reviewer-capable"
  check is needed, only forwardability (Phase 6b §5).

The only new fact this design relies on is copilotd-internal: that reviewer
resolution in `RenderCodex` is currently a single global decision computed once
before the emission loop, and must move **inside** the loop to become per-model.

## 2. Scope

### 2.1 In scope

- One new configuration key, `codex-auto-review-model-overrides`, resolved through
  the existing flag/env/TOML precedence (flags > env > file > default), **parsed
  once at `Resolve`** into `map[string]string` with fail-fast validation, and
  enumerated non-secret in `ServeConfig.LogValue`.
- Per-advertised-model reviewer resolution in `internal/catalog`
  (`overrides[slug] ?? global ?? ""`), replacing the single global decision.
- Widening the Codex-shape emission gate so a non-empty overrides map is, on its
  own, "something to inject".
- **Uniform per-model** skip-with-warning: any advertised model whose resolved
  reviewer is set but not forwardable has its *reviewer injection* skipped — the
  model is still emitted, just without an `auto_review_model_override` key — and is
  logged once, naming both the model and the reviewer.
- Documentation: the config reference table plus a worked example.
- Unit, boundary, and config tests covering precedence, malformed parsing,
  resolution precedence, and skip-with-warning.

### 2.2 Out of scope

- Everything already delivered by Phase 6b (the snapshot, intersection, verbatim
  emission, limits overlay, content negotiation, command-auth provider docs).
- **Snapshot freshness automation** — tracked separately in `ningw42/copilotd#53`.
- **A per-model way to *suppress* the global** for one slug. An empty reviewer
  value is a validation error, not an opt-out; per-model disabling is YAGNI until
  an operator asks for it. If exemption is ever wanted, the forward-compatible shape
  is a **reserved sentinel value** (a literal token), never empty-string — so
  empty-string stays unambiguously "typo" and the future door is pre-planned.
- **Config-time validation of reviewer forwardability.** The live catalog is
  unknown until a request; forwardability stays a render-time skip + warning, as in
  Phase 6b.
- **Config-time validation that override keys name real/advertised models.** The
  advertised set is request-time and Copilot-dependent; an override for a slug
  Copilot never forwards is simply never consulted (§5.3).
- **Cross-layer map merging.** Precedence is wholesale replacement per layer, like
  every other key (§4.2). Merging keys across layers would be novel and surprising.
- **The Anthropic surface.** Auto-review routing is a Codex/OpenAI-surface concern.

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Config value shape | A single flat string `"main=reviewer,…"` parsed to `map[string]string`, **not** a native TOML table | Keeps "flag name = TOML key" and one flat-string shape across all layers, so the existing precedence machinery is reused unchanged. `fftoml` flattens tables to `key.subkey` string pairs, so a table would take a different shape per layer and fork the precedence engine. |
| Where the map is parsed | Once, in `Resolve`, after precedence layering yields the final string | The issue mandates fail-fast at serve startup. Parsing at resolve catches malformed input before binding, gives the renderer a ready map, and keeps the pure renderer free of parsing/validation. |
| Precedence semantics | Wholesale replacement per layer (highest-set layer's whole string wins); no cross-layer merge | Identical to every other key; a merge would be copilotd's only key with special precedence semantics. |
| Reviewer resolution | Per advertised model `M`: `overrides[M.slug] ?? global ?? ""` | A per-main-model map is exactly what routes different main models to different reviewers; the global remains the fallback for slugs with no explicit override. |
| Present-but-unforwardable override | Skips (no silent fall-back to the global) | A present override is the operator's explicit choice for that slug; silently substituting the global would mask a typo. A bad global still does not taint a model that has a good override. |
| Skip-with-warning granularity | **Uniform per-model**: one warning per advertised model whose resolved reviewer is unforwardable, naming model + reviewer | The acceptance criterion asks the warning to name the reviewer *and model*; per-model resolution makes the model the natural unit. A single global misconfiguration now logs once per affected model (a deliberate change from Phase 6b — §6). |
| Emission gate | Widen Phase 6b's `#2` from `global != ""` to `global != "" OR overrides non-empty` | A deployment configured **only** with per-model overrides must still emit the Codex shape; otherwise the overrides would never take effect. |
| Override for a non-advertised slug | Silently inert; no warning | The key is never consulted (that model isn't emitted); a warning would be request-time noise driven by Copilot's live lineup, not operator error. |
| `CodexConfig` field type | First non-scalar field (`map[string]string`) | The map is the resolved, validated representation the renderer consumes; storing the raw string would re-parse per request and push validation downstream. |

## 4. Configuration schema

One new flat key, following copilotd's ff/kebab conventions (flag = TOML key;
env = `COPILOTD_` + upper-snake; precedence flags > env > file > default). Non-secret
and enumerated in `ServeConfig.LogValue`.

| Key (flag / TOML) | Env | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `codex-auto-review-model-overrides` | `COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES` | string `"main=reviewer,…"` → `map[string]string` | `""` | Per-main-model reviewer; wins over the global `codex-auto-review-model` for its slug. |

### 4.1 Parsing and validation (fail-fast at `Resolve`)

The value travels through the flag/env/file layers as a plain string (a
`StringLong`, like `codex-auto-review-model`). After precedence layering yields the
final string, `Resolve` parses it once:

- Split on `,`. Trim each segment; a segment that is **empty after trimming** is
  skipped, tolerating a trailing or doubled comma (so `""`, `"m=r,"`, and `"m=r"`
  all parse). Every **non-empty** segment must be a valid `key=value` pair.
- Each pair splits on the **first** `=`; both sides are trimmed of surrounding
  whitespace.
- A malformed pair — empty key, empty value, missing `=`, or a duplicate key —
  makes `Resolve` return an error, so `serve` fails before binding a listener
  (house fail-fast posture, consistent with the other invalid-value errors).
- The empty string yields an empty map (the default); an empty map injects no
  per-model overrides.

### 4.2 Precedence, logging, and inertness

- **Precedence** is wholesale replacement per layer: whichever of flag/env/file is
  set at the highest precedence supplies the entire string, which is then parsed.
  No key is merged across layers.
- **`LogValue`** gains `slog.String("codex-auto-review-model-overrides", …)` whose
  value is the parsed map re-rendered as a **normalized, sorted** `k=v,…` string,
  so log output is deterministic and independent of input ordering/whitespace. The
  value is non-secret (model slugs only).
- **Inert when the catalog is disabled.** A non-empty overrides map with
  `codex-catalog-enabled=false` is valid and consumed by nothing — it is not an
  error, so operators can stage config ahead of enabling. Its presence is visible
  at startup because the whole resolved `ServeConfig` is logged through `LogValue`
  (the `config` package is pure resolution with no logger of its own), exactly like
  every other inert-when-disabled key.

## 5. Reviewer resolution and rendering

### 5.1 Per-model resolution

For each advertised model `M` (by slug), emitted in Copilot's live `data[]` order:

```
reviewer = overrides[M.slug]   // if the key is present, authoritative
        ?? global              // codex-auto-review-model, if non-empty
        ?? ""                  // none

inject "auto_review_model_override" = reviewer on M
    iff  reviewer != ""  AND  reviewer ∈ emitted-intersection
```

- A **present** override is authoritative for its slug: if its reviewer is not in
  the emitted intersection, `M`'s *reviewer injection* is **skipped** — `M` is still
  emitted in the catalog, just carrying no `auto_review_model_override` key — and it
  does **not** fall back to the global. A model with no override key uses the global; a
  model with neither gets no override key at all.
- Injection still overwrites any `auto_review_model_override` present in the
  snapshot entry (the overlapping slugs carry none today, but overwrite is the
  defined behavior, as in Phase 6b §7.3).

Requiring the resolved reviewer to be in the emitted intersection continues to
guarantee (a) copilotd can forward the review call and (b) Codex resolves the
reviewer's real `ModelInfo` rather than fallback metadata. Any intersection member
is a valid reviewer (§1.1).

### 5.2 Skip-with-warning (uniform per-model)

A **skip event** is recorded for every advertised model `M` whose resolved reviewer
is non-empty but not in the emitted intersection. The renderer returns these as a
deterministic slice; the handler logs one warning per event naming the model and
the reviewer. A single unforwardable global reviewer therefore logs once for each
advertised model that falls back to it; an unforwardable per-model override logs
once for its model.

A skip event suppresses only the *reviewer injection*: `M` is still emitted in the
catalog, byte-identical to the snapshot. **A skip never removes a model from
`data[]`** — the sole visible effect is the absence of an `auto_review_model_override`
key on that model.

### 5.3 Non-advertised override keys

An override whose *key* (main-model slug) is not in the emitted intersection is never
consulted — that model is not emitted — and produces no warning. The map is a
lookup table indexed by the models actually being emitted; unused entries are inert.

Slug matching is **verbatim and case-sensitive** (a plain `map` lookup and set
membership, exactly as the global reviewer already matches). No normalization is
applied, so a miscased key like `GPT-5` is simply a special case of a non-advertised
key — inert, no warning. The operator owns the exactness of what they write.

### 5.4 Data-structure changes (`internal/catalog`)

- `CodexRenderConfig` gains `AutoReviewModelOverrides map[string]string` alongside
  the existing `AutoReviewModel` and `OverrideLimits`.
- `CodexRenderOutcome` changes from a single `SkippedReviewer string` to
  `SkippedReviewers []SkippedReviewer`, where
  `SkippedReviewer struct { Model, Reviewer string }`, in emission order. This is
  the one shape change rippling into the handler.
- `RenderCodex` moves reviewer resolution **inside** the per-model emission loop:
  for each entry it resolves `reviewer` per §5.1, decides inject-vs-skip, and
  appends a skip event when applicable. The verbatim copy, limits overlay, and
  envelope marshalling are unchanged.

## 6. Behavior-change note

Phase 6b logged an unforwardable **global** reviewer **once** (a single
`SkippedReviewer`). With per-model resolution and uniform per-model warnings, an
unforwardable global now logs **once per advertised model that falls back to it**.
This is a deliberate, recorded change:

- it is confined to an **opt-in, off-by-default** feature;
- each line is now more informative (it names the affected main model); and
- there is **no wire-format, catalog-content, or fidelity change** — only the log
  volume and the presence of a `model` attribute on the warning.

It is noted here (and belongs in the ADR 0005 divergence lineage the Phase 6b
design established) so the shift in log cardinality is not mistaken for a
regression.

## 7. Architecture and package boundaries

All changes are additive and mirror the Phase 6b seams; the raw forwarder stays
dumb and the transform stays in the pure `internal/catalog` package.

```
copilotd/
└── internal/
    ├── config/       [CHG] register the flag; parse+validate the map in Resolve;
    │                        add CodexConfig.AutoReviewModelOverrides; LogValue entry.
    ├── server/       [CHG] thread codexConfig.AutoReviewModelOverrides into
    │                        catalog.CodexRenderConfig at newHandler.
    ├── catalog/
    │   ├── codex_render.go  [CHG] per-model resolution; SkippedReviewers slice.
    │   └── handler.go       [CHG] widen servesCodexShape gate; emit one warning
    │                              per skip event (model + reviewer).
    └── forward/      [UNCHANGED]
```

- **`internal/config`.** Registers `codex-auto-review-model-overrides` as a
  `StringLong`; carries the raw string through the existing overlay/precedence
  machinery via an **unexported staging field on `CodexConfig`**, co-located with
  the `AutoReviewModelOverrides` map it feeds (only the final layered string should
  be parsed, so `overlay` writes the raw string like any other scalar key and no
  per-layer parsing occurs); after layering, parses+validates it once into
  `CodexConfig.AutoReviewModelOverrides`; adds the normalized `LogValue` entry. The
  staging field is unexported, so it never reaches `LogValue` and no request-time
  consumer can read the un-parsed string.
- **`internal/server`.** Threads `codexConfig.AutoReviewModelOverrides` into the
  `CodexRenderConfig` it already builds at `newHandler`. No route changes.
- **`internal/catalog`.** `RenderCodex` resolves the reviewer per emitted model and
  returns the skip-event slice; `handler.go`'s `servesCodexShape` gains the
  `len(AutoReviewModelOverrides) > 0` disjunct and its warning loop names the model.
- **`internal/forward`.** Unchanged.

## 8. Errors, observability, security

- A malformed overrides string is a **startup** error from `Resolve` (fail fast),
  never a request-time failure.
- A resolved reviewer not in the emitted catalog is **not** an error: the override
  is skipped and a per-model warning is logged; the catalog is still served.
- The overrides value is non-secret (model slugs) and appears in `LogValue`
  normalized; no secret is introduced or logged. Model bodies and query values are
  still not logged (Phase 6b §12 preserved).
- No new metric, background task, cache, or durable state.

## 9. Test design

All tests use in-process fixtures (the real Copilot `/models` capture and the
embedded snapshot); none need a GitHub account or network.

### 9.1 `internal/config`

- The key resolves through flag, env, and TOML with correct precedence and the
  `""` default; a higher-precedence layer replaces the whole map.
- Malformed strings fail `Resolve`: empty key (`=r`), empty value (`m=`), missing
  `=` (`mr`), and duplicate key (`m=a,m=b`).
- `""` → empty map; `LogValue` includes the normalized sorted `k=v,…` string.
- A non-empty map with `codex-catalog-enabled=false` resolves cleanly (no error)
  and appears in the logged config; nothing consumes it.

### 9.2 `internal/catalog` (pure)

- **Precedence:** a slug present in the map is routed to its override; a slug absent
  from the map uses the global; a slug with neither carries no override key.
- **Single-hop resolution:** with `{A=B, B=C}` and `A` advertised, `A` is injected
  with reviewer `B` (never `C`) — the reviewer slug is never itself re-resolved
  through the map.
- **Present-but-unforwardable override:** the reviewer injection is skipped (the
  model is still emitted, without the key) and the model appears as a
  `SkippedReviewer{Model, Reviewer}`; it does **not** fall back to a valid global.
- **Unforwardable global:** every advertised model without an override that falls
  back to it produces its own skip event.
- **Non-advertised override key:** inert — no key change, no skip event.
- **Fidelity:** all non-injected fields remain byte-identical to the snapshot
  (round-trip through `map[string]json.RawMessage`), as in Phase 6b.

### 9.3 Server boundary / real listener

- The emission gate fires when **only** the overrides map is set (no global, no
  limits): `?client_version=` + enabled + non-empty map → `{"models":[…]}`.
- Warnings name model + reviewer for a skipped reviewer.
- An advertised main model with an override carries that reviewer in its
  `auto_review_model_override`, and that reviewer is itself present in the emitted
  catalog.
- **Regression:** Phase 6b paths are unchanged — no `client_version`, the OpenAI
  list, `/anthropic/v1/models`, the raw `/models` passthrough; the global-only
  reviewer still injects on every model when the map is empty. `go test ./...` and
  `go test -race ./...` pass.

## 10. Acceptance criteria

This work is complete when all hold:

1. `codex-auto-review-model-overrides` resolves through flags/env/TOML with the
   documented type, `""` default, and wholesale-replacement precedence; malformed
   pairs (empty key/value, missing `=`, duplicate key) fail `Resolve`; the value is
   logged non-secret and normalized in `LogValue`.
2. Reviewer resolution per advertised model is `overrides[slug] ?? global ?? ""`; a
   per-model override wins over the global for its slug; a present-but-unforwardable
   override skips its reviewer injection (the model is still emitted, without the
   key) without falling back to the global; a slug with neither gets no
   `auto_review_model_override` key.
3. An unforwardable resolved reviewer has its injection skipped (the model stays in
   the catalog) and is logged with a **uniform per-model** warning naming the model
   and the reviewer.
4. The emission gate treats a non-empty overrides map as "something to inject"
   (`global != "" OR overrides non-empty OR limits overlay`).
5. An override key naming a non-advertised model is inert (no key change, no
   warning).
6. Unit, boundary, and config tests cover precedence, malformed parsing, resolution
   precedence, single-hop resolution, present-but-unforwardable skip, and
   gate-widening; the suite and race detector pass.
7. Docs updated: the configuration reference gains the new key with a worked
   `main=reviewer,…` example; the §6 log-cardinality change is recorded.
