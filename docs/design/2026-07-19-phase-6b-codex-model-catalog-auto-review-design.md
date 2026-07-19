# Phase 6b — Codex model catalog + auto-review routing (`auto_review_model_override`) — Design

Status: proposed design (polished via brainstorming + grilling), pending final written-spec review
Date: 2026-07-19
Roadmap reference: `ROADMAP.md` §4.2 and §7 "Phase 6 — Provider/client-shaped support endpoints"; the **client-shaped** half deferred by the Phase 6a design (§2.2, §12).
Builds on: `docs/design/2026-07-18-phase-6a-provider-shaped-model-catalogs-design.md`,
`docs/design/2026-07-18-phase-4-github-copilot-support-endpoint-design.md`.
Follow-on: snapshot freshness automation is split to `ningw42/copilotd#53` (§16);
per-model reviewer overrides to `ningw42/copilotd#54` (§2.2).

## 1. Goal and outcome

Phase 6b makes GitHub Copilot-backed models usable with the OpenAI **Codex CLI**'s
"approve-for-me" auto-approval. Today, with `approval_policy = "on-request"` and
`approvals_reviewer = "auto_review"`, Codex's guardian issues a review request for
the model `codex-auto-review`, which Copilot does not expose, so approval **fails
closed**. Phase 6b closes that gap without rewriting request payloads, using
Codex's own catalog-native mechanism: the per-model field
`ModelInfo.auto_review_model_override`.

copilotd's existing `GET/HEAD /openai/v1/models` endpoint gains **content
negotiation**: when a request carries Codex's `?client_version=` query parameter
(and the feature is enabled), copilotd returns the **Codex model catalog** shape
`{"models":[ModelInfo, …]}` instead of the Phase 6a OpenAI list. Each advertised
entry carries `auto_review_model_override` pointing at a real, forwardable
reviewer model configured by the operator. Codex then routes the guardian review
to that model, copilotd forwards it to Copilot normally, and auto-approval
succeeds.

**Outcome:** a Codex user who runs copilotd as a **command-auth provider** gets
working "approve-for-me" auto-approval against Copilot-backed models, driven by a
small, friendly configuration surface, with the model catalog bodies sourced
verbatim from Codex's own bundled `models.json` so the active model's behavior is
unchanged.

### 1.1 Grounding (verified against `openai/codex` at tag `rust-v0.144.5`)

Every claim below was read from `openai/codex` source at tag `rust-v0.144.5`
(Rust under `codex-rs/`). The load-bearing facts:

- **The catalog fetch is gated by command auth.** Codex only fetches/honors a
  server model catalog when
  `should_refresh_models() = uses_codex_backend() || has_command_auth()`
  (`codex-rs/models-manager/src/manager.rs:390`). A self-hosted proxy is not the
  ChatGPT/Codex backend, so `uses_codex_backend()` is false; the only lever is
  `has_command_auth()`, which is literally `self.auth.is_some()`
  (`codex-rs/model-provider-info/src/lib.rs:418`). "Command auth" = a
  `[model_providers.NAME.auth]` block whose `command` prints a bearer token to
  stdout. This is why the operator must configure copilotd as a command-auth
  provider (§4).
- **The request lands on our existing route.** With `base_url = ".../openai/v1"`,
  Codex issues `GET {base_url}/models?client_version=<MAJOR.MINOR.PATCH>`
  (`codex-rs/codex-api/src/endpoint/models.rs`, path `"models"` + `client_version`
  query). That is exactly copilotd's Phase 6a `GET /openai/v1/models`.
- **The wanted response shape differs from OpenAI's.** Codex deserializes
  `ModelsResponse { models: Vec<ModelInfo> }`
  (`codex-rs/protocol/src/openai_models.rs:568`), i.e. `{"models":[…]}` — **not**
  OpenAI's `{"object":"list","data":[…]}`.
- **`auto_review_model_override` does the routing.** It is
  `Option<String>` on `ModelInfo`
  (`codex-rs/protocol/src/openai_models.rs:420`). In
  `guardian_review_session_config` (`codex-rs/core/src/guardian/review.rs`,
  ~703–747) Codex reads `turn.model_info.auto_review_model_override`; if set it
  routes the guardian to that reviewer model instead of the default
  `codex-auto-review`, then resolves that reviewer's own `ModelInfo` before
  starting the review session.
- **The merge is wholesale per slug — no field merge.** Under command auth,
  `apply_remote_models` reloads Codex's bundled `models.json` and merges the
  fetched list over it, replacing the entire entry per slug
  (`codex-rs/models-manager/src/manager.rs:416`–`427`,
  `existing_models[existing_index] = model;`). To add `auto_review_model_override`
  to a model we must resend that model's **complete** `ModelInfo`.
- **`base_instructions` is load-bearing and has no fallback.** For a matched
  slug, the active model's system prompt resolves to
  `model_info.get_model_instructions(...)`
  (`codex-rs/core/src/session/mod.rs:618`), which returns
  `self.base_instructions.clone()` verbatim
  (`codex-rs/protocol/src/openai_models.rs:477`) and is sent as the request
  `instructions` verbatim (`codex-rs/core/src/client.rs:867`). An empty
  `base_instructions` reaches the wire as `instructions: ""` (degraded model). The
  compiled-in default prompt is used **only** for a completely unknown slug, never
  to rescue a matched-but-empty one.
- **Codex bundles the source of truth we vendor.** Codex embeds
  `codex-rs/models-manager/models.json` via `include_str!`
  (`codex-rs/models-manager/src/lib.rs:15`). At this tag it holds 8 models:
  `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `gpt-5.4`,
  `gpt-5.4-mini`, `gpt-5.2`, `codex-auto-review`, each with its real
  `base_instructions` (≈11–21 K chars) and `model_messages`.
- **License.** `openai/codex` is Apache-2.0; a snapshot of the bundled data may be
  vendored with attribution and inclusion of `LICENSE`/`NOTICE`.

**Consequence that shapes the whole design:** we cannot patch a single field, and
we cannot send empty required fields. The only faithful way to add
`auto_review_model_override` is to **re-emit Codex's own complete `ModelInfo`
entries** (from a vendored snapshot) with the override injected.

## 2. Scope

### 2.1 In scope

- **Content negotiation on `GET/HEAD /openai/v1/models`**: a request carrying
  `?client_version=` receives the Codex `{"models":[…]}` catalog **iff** the
  catalog is enabled **and** there is something to inject — a global reviewer or
  the limits overlay (§6); otherwise the Phase 6a OpenAI list is served unchanged.
- A **vendored snapshot** of Codex's `models.json` (pinned to `rust-v0.144.5`),
  embedded via Go `embed`, carried with provenance and Apache-2.0 attribution.
- The emitted Codex catalog = **intersection** of the live Copilot
  Responses-forwardable set (Phase 6a filter) and the snapshot's slugs, each entry
  re-emitted **field-for-field except** for an injected `auto_review_model_override`
  (per config) and, optionally, `context_window`/`max_context_window` overlaid from
  Copilot's live limits.
- A **three-key configuration surface** (§8): `codex-catalog-enabled`,
  `codex-auto-review-model`, `codex-catalog-override-limits` (all scalar).
  Per-model reviewer overrides are deferred to `ningw42/copilotd#54`.
- Operator documentation of the required Codex **command-auth provider** config
  (§4). No new copilotd inbound auth: the catalog request is authenticated by the
  existing managed API key.
- Reuse of the Phase 6a seams: `forward.FetchModels`, `catalog.Decode`,
  `catalog.Filter`. A new `RenderCodex`. All transform logic stays in the pure
  `internal/catalog` package; the raw forwarder stays dumb.
- Automated unit, boundary, and real-listener tests seeded from the real Copilot
  `/models` capture plus the vendored snapshot.

### 2.2 Out of scope

- **Snapshot freshness automation** (commit-based caching / TTL / upstream-change
  detection). Deferred to `ningw42/copilotd#53` (§16). Phase 6b ships the static
  embedded snapshot as the correct baseline.
- **Per-model reviewer overrides** (routing different active models to different
  reviewers). v1 ships a single global reviewer; per-model routing is deferred to
  `ningw42/copilotd#54`. Any forwardable model is a valid reviewer (§5), so one
  global reviewer covers the common case, and a per-model map would be copilotd's
  only non-scalar config key.
- **Advertising Copilot-only Responses models that are absent from the snapshot**
  (`mai-code-1-flash-picker`, `gpt-5-mini`, `gpt-5.3-codex`). We have no faithful
  `base_instructions` for them; they are not advertised to Codex in v1 (they
  remain callable on the raw OpenAI surface and appear in the Phase 6a list).
- **Payload-rewrite aliasing** of `codex-auto-review` (the rejected PR approach:
  breaks under the WebSocket transport). We use the catalog-native override only.
- **A separate Codex-only route or a distinct base URL.** Codex hardcodes
  `{base_url}/models`; we content-negotiate on the existing route.
- **`ModelInfo.model_messages` synthesis, review-policy authoring, WebSocket
  transport.** We emit Codex's own `model_messages` verbatim; the guardian review
  policy is Codex's concern.
- **Caching, refresh jobs, or state at rest** for the upstream `/models` fetch —
  unchanged from Phase 6a: one fetch per request.
- **The Anthropic Surface.** Auto-review routing is a Codex/OpenAI-Surface
  concern; `/anthropic/v1/models` is untouched.

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| How to enable auto-approval | Patch `auto_review_model_override` (Codex catalog-native), not payload aliasing | The field is Codex's designed mechanism; it survives both HTTP and WebSocket transports (`guardian/review.rs`). Aliasing the request breaks under WSS (the rejected PR). OpenAI's own Amazon Bedrock provider solves the same "no `codex-auto-review` on a non-ChatGPT backend" problem by overriding `approval_review_preferred_model()` to a real model (`gpt-5.4`) — prior art validating this approach. |
| Precondition on the operator | Configure copilotd as a Codex **command-auth provider** | The catalog fetch gate is `uses_codex_backend() || has_command_auth()` (`manager.rs:390`); command auth is the only path a self-hosted proxy can turn on. The auth command simply prints copilotd's managed API key. |
| Endpoint behavior | Content-negotiate `GET/HEAD /openai/v1/models` on `?client_version=` + the enable toggle | Codex hardcodes `{base_url}/models`; the standard list and the Codex catalog must share the route. `client_version` is Codex's own signal; standard OpenAI SDKs never send it. |
| When the Codex shape is emitted | Only when enabled **and** there is something to inject — a global reviewer **or** the limits overlay | Emitting the intersection re-sends every advertised model's complete `ModelInfo`, pinning its snapshot `base_instructions` (§13.2); doing that with nothing to inject would degrade prompts for no benefit, so a bare `codex-catalog-enabled=true` emits nothing and Codex falls back to its own bundle (fails safe). |
| Where entry bodies come from | Vendored Codex `models.json` (`rust-v0.144.5`), emitted **verbatim** + injected override | The merge is wholesale per slug (`manager.rs:422`) and empty `base_instructions` degrades the active model with no fallback (`client.rs:867`). Re-emitting Codex's own entries is the only faithful option. |
| Catalog coverage | Intersection of live Copilot-forwardable and snapshot slugs | Advertise only models copilotd can actually forward **and** for which we have a faithful body. Currently the 6 overlapping slugs. |
| Copilot-only non-snapshot models | Not advertised to Codex in v1 | No faithful `base_instructions`; degrading a user's active model is worse than omitting it. |
| Reviewer configuration | A single global `codex-auto-review-model` (per-model overrides deferred to #54) | Any forwardable model is a valid reviewer (the guardian review runs Codex's own policy, not the reviewer's instructions — §5), so one global knob covers the common case; a per-model map would be copilotd's only non-scalar config, so it waits for real demand. |
| Reviewer validity | Patch only when the resolved reviewer is itself in the emitted intersection; else skip + warn | A reviewer Copilot can't forward would fail the review call; failing safe to Codex's native default is cleaner than advertising a broken reviewer. Intersection membership also guarantees Codex resolves the reviewer's real `ModelInfo`, not fallback metadata. |
| Limits fidelity | Snapshot limits by default; `codex-catalog-override-limits=true` overlays `context_window ← max_prompt_tokens` and `max_context_window ← max_context_window_tokens` | Snapshot is the faithful Codex representation; the overlay reports Copilot's honest forwardable budget (Phase 6a §5.5). Off by default because it is a capability-affecting change that must be opted into, never silent. |
| Rendering technique | Decode each snapshot entry to `map[string]json.RawMessage`, inject/overwrite specific keys, re-marshal | Preserves every `ModelInfo` field verbatim without modelling the ~40-field struct; insulates us from fields we don't read (JSON object key order is insignificant to Codex's serde). |
| Master toggle | `codex-catalog-enabled` (default **false**) | The Codex `ModelInfo` shape is a Codex-internal, non-stable type pinned to one version; serving it is opt-in so drift can't silently break clients that didn't ask for it. This reflects a project tenet: copilotd never silently changes a model's behavior — the catalog, the reviewer routing, and the limits overlay are each independently opt-in. |
| Snapshot load failure | Fail fast at **startup** | The snapshot is embedded; a decode failure is a build/packaging defect, not a runtime condition. |

## 4. The enabling precondition — a command-auth provider

Auto-review routing only takes effect if Codex actually fetches copilotd's
catalog, which requires the provider to satisfy `has_command_auth()`
(`manager.rs:390`, `model-provider-info/src/lib.rs:418`). The operator therefore
configures copilotd as a **command-auth provider** in `~/.codex/config.toml`:

```toml
model_provider = "copilotd"

[model_providers.copilotd]
name     = "copilotd"
base_url = "http://127.0.0.1:8080/openai/v1"   # -> GET {base_url}/models?client_version=...
wire_api = "responses"                          # only "responses" is accepted at this tag
# NOTE: env_key and requires_openai_auth MUST be absent — they conflict with [.auth].

[model_providers.copilotd.auth]
command = "printf"                 # prints copilotd's managed API key to stdout
args    = ["sk-your-copilotd-key"] # (or a script / secret-file reader)
# timeout_ms / refresh_interval_ms / cwd are optional (defaults 5000 / 300000 / ".")
```

The presence of `[model_providers.copilotd.auth]` is exactly what flips
`has_command_auth()` true. The catalog request then carries
`Authorization: Bearer sk-your-copilotd-key`, which is copilotd's existing managed
API key — so **no new inbound auth is introduced**; the route keeps its existing
`guard` (auth → readiness). This is operator documentation, not copilotd code.

## 5. The mechanism — `auto_review_model_override` end to end

1. The user runs Codex with an active model `M` that copilotd advertises (e.g.
   `gpt-5.6-sol`), `approval_policy = "on-request"`, `approvals_reviewer =
   "auto_review"`.
2. Codex fetches `GET /openai/v1/models?client_version=…`; copilotd returns the
   Codex catalog in which `M`'s entry carries
   `"auto_review_model_override": "<reviewer>"`.
3. On a guarded action, `guardian_review_session_config`
   (`guardian/review.rs`) reads `turn.model_info.auto_review_model_override`,
   selects `<reviewer>` (instead of the unforwardable `codex-auto-review`), and
   resolves `<reviewer>`'s own `ModelInfo` from the merged catalog.
4. Codex issues the guardian review as `<reviewer>` — a normal Responses request
   to copilotd — which forwards it to Copilot. The review completes and the action
   auto-approves.

The reviewer must therefore be **a real model copilotd can forward** and **present
in the emitted catalog** (so Codex resolves its metadata). Both hold for any slug
in the intersection (§7).

The review itself runs Codex's own **guardian policy** — resolved as the operator's
`guardian_policy_config`, else the reviewer's `model_messages.auto_review.policy`,
else a compiled-in default (`build_guardian_review_session_config` in
`guardian/review_session.rs`) — with the reviewer model only as the engine. It does
**not** use the reviewer's `base_instructions`. So any forwardable model is a valid
reviewer and the choice is a cost/latency matter, not a correctness one (each
guarded action spends Copilot quota on a real review call). Codex's default reviewer
is the hardcoded, unforwardable `codex-auto-review` (`approval_review_preferred_model()`
returns `DEFAULT_APPROVAL_REVIEW_PREFERRED_MODEL`, not operator-settable), so the
override is the only lever a self-hosted provider has — the same lever OpenAI's
Bedrock provider uses to route to `gpt-5.4`.

## 6. When the Codex catalog is served — content negotiation

`GET/HEAD /openai/v1/models` decides its body shape as:

| `?client_version=` | `codex-catalog-enabled` | reviewer set **or** limits overlay on | Response |
| --- | --- | --- | --- |
| no | any | any | Phase 6a OpenAI list `{"object":"list","data":[…]}` (unchanged) |
| yes | `false` | any | Phase 6a OpenAI list |
| yes | `true` | no | Phase 6a OpenAI list (nothing to inject — see below) |
| yes | `true` | yes | Codex catalog `{"models":[…]}` |

The Codex shape is emitted only when the feature is enabled **and** there is
something to inject — a global `codex-auto-review-model` **or**
`codex-catalog-override-limits=true`. In every other case copilotd serves the
OpenAI list; Codex's fetch fails to parse it and **falls back to its own bundle**
(fails **safe**, no auto-review, exactly as today). This is deliberate: emitting
the intersection re-sends every advertised model's complete `ModelInfo` — pinning
its snapshot `base_instructions` (§13.2) — so a bare `codex-catalog-enabled=true`
with nothing to inject would degrade prompts for no benefit, and is treated as
"serve the OpenAI list."

Presence of `client_version` is detected by key, not value; copilotd does not
parse or echo it (Codex uses it only as a client-side cache key). `HEAD` computes
the same body for an accurate `Content-Length` and suppresses the write, as in
Phase 6a. The raw `GET/HEAD /models` passthrough and `/anthropic/v1/models` are
untouched.

## 7. Catalog contents

### 7.1 The vendored snapshot

`openai/codex`'s `codex-rs/models-manager/models.json` at `rust-v0.144.5` is
copied into copilotd (e.g. `internal/catalog/codexdata/models.json`) and embedded
with Go `embed`. It is carried with:

- the upstream Apache-2.0 `LICENSE` and `NOTICE`, and
- a short `PROVENANCE.md` recording the source repo, tag, commit SHA, and path.

The snapshot is decoded once at startup into per-entry
`map[string]json.RawMessage` keyed by `slug`; a decode failure is a **startup**
error (fail fast). Each entry's other fields are retained as raw values and
re-emitted verbatim.

**The snapshot carries no self-describing version.** Its top level is just
`{"models":[…]}` — there is no `version`/`tag`/`commit` key, and the wire
`client_version` is the *running Codex CLI's* crate version, never anything read
from this file. The per-entry `minimal_client_version` field is a per-model
minimum-client floor (and is not even in the `ModelInfo` struct at this tag, so
Codex drops it on decode) — it is **not** an origin marker. The binding "this
snapshot is Codex `rust-v0.144.5`" therefore lives **only out-of-band**, in
`PROVENANCE.md` and the vendoring commit; neither copilotd nor Codex can detect at
runtime that our served entries and the client's version differ. This is exactly
why the §13.1 deserialize-failure fail-safe is the only backstop against
required-field drift.

### 7.2 Intersection & filter

For each request that resolves to the Codex shape:

1. `forward.FetchModels(ctx)` → `catalog.Decode(body)` → `catalog.Filter(models,
   OpenAIResponsesRoute)` yields the **live Copilot Responses-forwardable** set
   (the Phase 6a predicate: `model_picker_enabled == true` **and**
   `supported_endpoints` contains `/responses`).
2. Intersect that set (by `slug`/`id`) with the snapshot. Only slugs present in
   **both** are advertised. Order follows the live upstream `data[]` order (as
   Phase 6a), so `priority` in the snapshot still governs Codex-side sorting but
   our wire order is deterministic and Copilot-derived.

Against the current capture the intersection is the 6 slugs
`gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`, `gpt-5.4`,
`gpt-5.4-mini`. Snapshot-only slugs (`gpt-5.2`, `codex-auto-review`) and
Copilot-only slugs (`mai-code-1-flash-picker`, `gpt-5-mini`, `gpt-5.3-codex`) drop
out. The set is computed per request, so it tracks Copilot's live lineup.

### 7.3 Reviewer resolution & patching

For each advertised model `M` (by slug), the reviewer is the single global
`codex-auto-review-model` (per-model overrides are deferred to `ningw42/copilotd#54`):

```
reviewer = codex-auto-review-model      // global, if non-empty
        ?? ""                           // none
```

The `auto_review_model_override` key is injected into `M`'s entry **iff**
`reviewer != ""` **and** `reviewer` is itself in the emitted (intersection)
catalog. Otherwise the key is omitted:

- reviewer empty → no model carries an override → Codex uses its native default
  (`codex-auto-review`), so auto-review stays broken, by choice (§13.5);
- reviewer configured but not in the intersection → **skip + log a warning** naming
  the reviewer (a bad config never advertises a reviewer that would fail the review
  call).

Requiring the reviewer to be in the intersection is what guarantees (a) copilotd
can forward the review call and (b) Codex resolves the reviewer's real `ModelInfo`
(with its `model_messages`) rather than fallback metadata. Any intersection member
is a valid reviewer — the guardian review runs Codex's own policy with the reviewer
as the engine (§5), so no "reviewer-capable" check is needed. Injection overwrites
any `auto_review_model_override` present in the snapshot entry (the overlapping
slugs carry none today, but overwrite is the defined behavior).

### 7.4 Limits overlay (`codex-catalog-override-limits`)

- **false (default):** emit the snapshot's `context_window` /
  `max_context_window` verbatim (Codex's own numbers).
- **true:** overlay each field from its semantically-matching Copilot limit —
  `context_window ← max_prompt_tokens` (the field Codex actually packs and
  auto-compacts against; `resolved_context_window()` prefers it) and
  `max_context_window ← max_context_window_tokens` (the ceiling Codex clamps a
  user's manual context override to). Each falls back to the snapshot value when
  Copilot does not supply it (no-fabrication: the implementing agent must confirm
  `max_context_window_tokens` is present in a live capture; Phase 6a consumed only
  `max_prompt_tokens`/`max_output_tokens`). This reports Copilot's honest
  forwardable budget (Phase 6a §5.5) so Codex never packs past what Copilot
  accepts. Two consequences (§13.3): it diverges from the numbers Codex ships, and
  because Codex reserves output headroom *inside* `context_window`, the effective
  input lands slightly under Copilot's true prompt budget — safe, mildly
  conservative. `context_window` must take `max_prompt_tokens`, not the larger
  total window: Codex packs input up to ~95% of `context_window`, so seeding it
  with the total window would let Codex exceed Copilot's prompt budget and earn a
  `400`.

No other fields are ever modified. `model_messages`, `base_instructions`,
`shell_type`, `truncation_policy`, `supported_reasoning_levels`, `visibility`,
`supported_in_api`, `priority`, etc. are all emitted verbatim from the snapshot.

## 8. Configuration schema

Three new flat keys, following copilotd's existing ff/kebab conventions
(flag = TOML key; env = `COPILOTD_` + upper-snake; precedence flags > env > file >
default). All are non-secret and enumerated in `ServeConfig.LogValue`.

| Key (flag / TOML) | Env | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `codex-catalog-enabled` | `COPILOTD_CODEX_CATALOG_ENABLED` | bool | `false` | Master opt-in. When `?client_version=` is present **and** there is something to inject (reviewer set or limits overlay on), emit the Codex `{"models":[…]}` shape (§6). |
| `codex-auto-review-model` | `COPILOTD_CODEX_AUTO_REVIEW_MODEL` | string | `""` | Reviewer slug, injected as `auto_review_model_override` on every advertised model. Empty = no auto-review routing. Example: `gpt-5.6-luna`. |
| `codex-catalog-override-limits` | `COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS` | bool | `false` | Overlay `context_window ← max_prompt_tokens` and `max_context_window ← max_context_window_tokens` from Copilot's live limits (§7.4). |

### 8.1 Parsing and validation

- **Per-model reviewer overrides** (a parsed `active=reviewer` map) are **not** in
  v1; the reviewer is the single scalar `codex-auto-review-model`. The map is
  tracked in `ningw42/copilotd#54` and would be copilotd's first non-scalar config
  key.
- No cross-field errors: the reviewer key and the limits toggle are simply
  **inert** when `codex-catalog-enabled=false`, so operators can stage config. A
  non-empty reviewer with the catalog disabled emits an informational log line at
  startup (not an error).
- **Reviewer-forwardability is not validated at config time** (the live catalog is
  unknown until a request): it is enforced at render time as a skip + warning
  (§7.3).
- Values are non-secret and appear in `ServeConfig.LogValue`.

## 9. Architecture and package boundaries

The raw forwarder stays dumb; the typed transform stays in the pure
`internal/catalog` package. New work is additive.

```
copilotd/
└── internal/
    ├── catalog/
    │   ├── codexdata/         [NEW] embedded models.json (rust-v0.144.5) + LICENSE/NOTICE/PROVENANCE
    │   ├── codex.go           [NEW] embed loader (map[string]json.RawMessage by slug);
    │   │                            RenderCodex(forwardable []Model, cfg CodexConfig) ([]byte,error):
    │   │                            intersect -> verbatim copy -> optional limits overlay ->
    │   │                            override injection -> {"models":[...]} marshal.
    │   └── handler.go         [CHG] OpenAI descriptor branches on client_version + enabled:
    │                                RenderCodex vs the existing RenderOpenAI.
    ├── config/                [CHG] three scalar keys; validation; LogValue.
    ├── forward/               [UNCHANGED] FetchModels reused as-is.
    └── server/                [CHG] resolve CodexConfig; pass it into the OpenAI catalog descriptor.
```

- **`internal/catalog` (pure).** `codex.go` owns the embedded snapshot and
  `RenderCodex`. It reuses the existing `Decode`/`Filter` for the live-forwardable
  set. `RenderCodex` reads each snapshot entry as `map[string]json.RawMessage`,
  intersects by slug, injects/overwrites `auto_review_model_override` (and limits
  when toggled), and marshals `{"models":[…]}`. No network, no credentials; fully
  unit-testable off the embedded snapshot + a decode fixture.
- **`internal/catalog/handler.go`.** The OpenAI `Descriptor` gains the Codex
  branch: if `r.URL.Query().Has("client_version")` and `cfg.Enabled`, render the
  Codex shape; else the Phase 6a OpenAI list. The Anthropic descriptor is
  unaffected. `HEAD` handling and error dialect are unchanged.
- **`internal/config`.** Registers the three flags, applies them in the existing
  overlay/precedence machinery, validates, and exposes a resolved `CodexConfig`
  (enabled, global reviewer, override-limits) plus `LogValue` entries.
- **`internal/server`.** Threads the resolved `CodexConfig` into the OpenAI
  catalog `Descriptor` at `newHandler`. No route changes; the existing
  `GET/HEAD /openai/v1/models` registrations carry the new behavior.
- **`internal/forward`.** Unchanged — `FetchModels` already provides the one
  credentialed `/models` fetch.

This is consistent with Phase 6a §6.4: reshaping is a first-party support-endpoint
concern, not a shim; the onion principle governs forwarded inference routes, not
support endpoints.

## 10. End-to-end flow (Codex branch)

For `GET/HEAD /openai/v1/models?client_version=…` with `codex-catalog-enabled=true`:

1. Request-ID, access-log, recovery middleware; then `guard` (API-key auth →
   readiness), exactly as every provider route. The command-auth bearer token is
   validated as the managed API key.
2. `fetcher.FetchModels(ctx)` — one credentialed `GET /models` to Copilot.
3. On a typed fetch error, short-circuit client cancellation (return without
   writing if the request context is canceled), else render the OpenAI dialect:
   `ErrNoCredential` → 503; timeout → 504; others → 502 (identical to Phase 6a).
4. On upstream `status != 200`, render an OpenAI-shaped 502; Copilot's body is not
   forwarded.
5. `catalog.Decode(body)` (decode error → 502) → `catalog.Filter(_,
   OpenAIResponsesRoute)` → the live-forwardable set.
6. `catalog.RenderCodex(forwardable, cfg)`: intersect with the snapshot, copy each
   entry verbatim, overlay limits if toggled, inject `auto_review_model_override`
   per §7.3, marshal `{"models":[…]}`.
7. Write `200`, `Content-Type: application/json`, `Content-Length`. For `GET`
   write the body; for `HEAD` suppress the body but keep the length.

No cache, no single-flight: two client calls cause two upstream fetches (as Phase
6a). The non-Codex branch is byte-for-byte the Phase 6a path.

## 11. Errors, timeout, and cancellation

- Every copilotd-originated failure renders in the **OpenAI** dialect
  (`apierror.OpenAI`), as this is the OpenAI Surface — unchanged from Phase 6a.
- Upstream non-2xx and undecodable 2xx → OpenAI-shaped 502; Copilot's error shape
  is never leaked.
- A snapshot embed/decode failure is caught at **startup** (fail fast), never at
  request time — the snapshot is compiled in.
- A resolved reviewer that is not in the emitted catalog is **not** an error: the
  override is skipped and a warning is logged; the catalog is still served.
- `ResponseHeaderTimeout`/`OutboundTimeout` bound the one fetch; no SSE timers
  (not a stream). `HEAD` returns GET-equivalent status/headers with no body.
- Router 404/405 and panic recovery remain generic, as elsewhere.

## 12. Observability and security

- One access-log line per request with the route template, status, byte count,
  duration, and correlation ID. The Codex-vs-OpenAI branch is recorded (e.g. a
  `catalog_shape` attribute) so operators can see which shape was served.
- A misconfigured reviewer logs a single warning at render naming the reviewer and
  model; startup logs whether the Codex catalog is enabled and (redaction-safe)
  the resolved config via `LogValue`.
- Secrets are never logged: the managed API key, GitHub OAuth token, and Copilot
  token are untouched; the command-auth token is just the managed API key and is
  handled by the existing auth path. Model bodies and query values are not logged.
- The reshape emits only snapshot fields plus the injected keys, so no Copilot
  billing/policy metadata is exposed (Phase 6a §5.4 property is preserved — the
  Codex catalog is built from the snapshot, not from Copilot's per-model
  metadata).
- No new metrics, background work, cache, or durable state.

## 13. Fidelity contract — named divergences (ADR 0005)

These are deliberate, recorded consequences of the wholesale-merge and
no-fabrication constraints (mirroring Phase 6a §5.5 / ADR 0004):

1. **Snapshot is version-pinned.** If a future Codex adds a **required**
   `ModelInfo` field, our entries fail to deserialize and Codex **retains its own
   bundled/last-cached catalog** (Codex applies a fetched list only on success —
   `apply_remote_models` runs after a clean parse; a failed refresh leaves the
   prior catalog in place). This **fails safe**: no crash, auto-review simply
   unavailable until we resync. The fallback is **all-or-nothing per fetch** — a
   single undecodable entry sinks the whole response for that client, not just the
   drifted model. A documented resync procedure plus the best-effort freshness
   automation in `ningw42/copilotd#53` (§16) are the mitigations.
2. **Prompt/behavior values are Codex `rust-v0.144.5`'s — for every advertised
   model.** Whenever the Codex shape is emitted, each intersection member is
   re-sent whole, so its `base_instructions`/`model_messages` are pinned to our
   snapshot (our fetched entry wins per slug under command auth); a user on a
   different Codex version silently gets our pinned values. This pinning is
   co-extensive with what you opted into: the emission gate (§6) only fires when a
   reviewer or the limits overlay is set, so a model's prompt is only ever pinned
   when you are also giving it auto-review routing or an honest limits budget —
   never gratuitously. Functional, but not that Codex version's native values.
3. **Limits are Codex's numbers by default;** `codex-catalog-override-limits=true`
   overlays `context_window ← max_prompt_tokens` and `max_context_window ←
   max_context_window_tokens` (§7.4) — more forward-safe, but diverging from
   Codex's shipped numbers, and mildly conservative (Codex reserves output headroom
   inside `context_window`). Off by default: a capability-affecting change is opted
   into, never silent.
4. **Coverage is the intersection.** Copilot-only Responses models absent from the
   snapshot do not appear in the Codex picker in v1.
5. **Auto-review requires operator config.** With no reviewer configured (global
   or per-model), a model carries no override and Codex's native default
   (`codex-auto-review`) still fails for it — the feature is opt-in per model.

## 14. Test design

All tests use `httptest` upstreams and a stubbed identity; none need a GitHub
account or network. Fixtures: the real Copilot `/models` capture (as Phase 6a) and
the embedded snapshot.

### 14.1 `internal/catalog` unit tests (pure)

- **Snapshot embed:** loads at init; every entry decodes; the 8 expected slugs are
  present; the entry map keys on `slug`.
- **Intersection:** the 6 overlapping slugs are advertised; snapshot-only
  (`gpt-5.2`, `codex-auto-review`) and Copilot-only (`mai-code-1-flash-picker`,
  `gpt-5-mini`, `gpt-5.3-codex`) are excluded; a snapshot slug Copilot stops
  forwarding drops out.
- **Verbatim fidelity:** for an advertised slug, every snapshot field except the
  injected keys is byte-identical to the source (round-trip through
  `map[string]json.RawMessage`), and `base_instructions`/`model_messages` are
  non-empty.
- **Override injection:** a configured global reviewer is injected on every
  advertised model; empty reviewer → no `auto_review_model_override` key on any; a
  reviewer not in the intersection is skipped with a warning and no key.
- **Limits overlay:** off → snapshot `context_window`/`max_context_window`
  verbatim; on → `context_window`←`max_prompt_tokens` and
  `max_context_window`←`max_context_window_tokens` where present, snapshot value
  where absent.
- **Envelope:** `{"models":[…]}`; every emitted entry deserializes into Codex's
  required-field set (a local mirror of the required keys guards drift).

### 14.2 `internal/config` unit tests

- The three keys resolve through flags/env/TOML with correct precedence and
  defaults; `LogValue` includes them.
- A non-empty reviewer with the catalog disabled logs the informational staging
  line (not an error).

### 14.3 Server boundary and real-listener tests

- `?client_version` + enabled + reviewer set → `{"models":[…]}`; `?client_version`
  + enabled but nothing to inject → OpenAI list; absent `client_version` → OpenAI
  list; `client_version` + disabled → OpenAI list.
- Both API-key forms authorize; invalid auth → 401 before readiness; not-ready →
  OpenAI-shaped 503; upstream non-2xx / malformed → OpenAI-shaped 502.
- `HEAD` returns headers (incl. `Content-Length`) with no body over a real
  listener; two calls → two upstream fetches; single-correlation-ID invariant
  holds.
- End-to-end sanity: an advertised active model's entry carries the configured
  `auto_review_model_override`, and that reviewer is itself present in the emitted
  catalog.

### 14.4 Regression

- Phase 6a `/openai/v1/models` (no `client_version`), `/anthropic/v1/models`, the
  raw `/models` passthrough, and inference routes are unchanged.
- `go test ./...` and `go test -race ./...`.

## 15. Acceptance criteria

Phase 6b is complete when all hold:

1. `GET/HEAD /openai/v1/models` content-negotiates: `?client_version=` + enabled +
   something-to-inject → `{"models":[…]}`; otherwise the Phase 6a OpenAI list,
   unchanged (§6).
2. The Codex catalog advertises exactly the intersection of live
   Copilot-forwardable Responses models and the vendored snapshot, entries emitted
   verbatim except for the injected keys.
3. `auto_review_model_override` is injected per §7.3 (single global reviewer,
   skip-with-warning when the reviewer is not in the emitted intersection).
4. `codex-catalog-override-limits` overlays `context_window ← max_prompt_tokens`
   and `max_context_window ← max_context_window_tokens` when true (snapshot fallback
   where absent) and emits snapshot limits when false; no other field is modified.
5. The three config keys resolve with the documented types/defaults/precedence and
   validation, and are logged non-secret.
6. The vendored snapshot is embedded with Apache-2.0 `LICENSE`/`NOTICE` and a
   `PROVENANCE` record; a decode failure fails startup.
7. copilotd-originated failures render in the OpenAI dialect; upstream non-2xx and
   unparseable bodies render an OpenAI-shaped 502; the non-Codex path is unchanged.
8. The raw forwarder stays dumb; all transform logic lives in `internal/catalog`;
   no reshape runs as a shim.
9. The feature is inert by default (`codex-catalog-enabled=false`); no new metric,
   background task, cache, or durable state is introduced.
10. ADR 0005 records the §13 divergences; operator docs cover the command-auth
    provider setup (§4).
11. The automated suite and race detector pass.

## 16. Deferred — snapshot freshness automation (`ningw42/copilotd#53`)

Keeping the vendored snapshot current (commit-based caching, TTL, upstream-change
detection, and required-field-drift handling) is a distinct, larger effort with a
"no state at rest" tension and a new outbound dependency. It is tracked as
`ningw42/copilotd#53` (`needs-triage`) and explicitly out of Phase 6b, which lands
the static embedded snapshot as the correct, simple baseline that the freshness
work builds on.

Serving the **latest** upstream `models.json` from such a cache is the
**best-effort ceiling** this approach can reach, and it is acceptable precisely
because of two properties already established above:

- **Bounded drift.** Within the TTL, the cached copy is served directly; after the
  TTL, an upstream change is detected and pulled (else the cache is refreshed in
  place). Staleness relative to upstream is therefore bounded by the TTL rather
  than by manual resync cadence.
- **Safe degradation both ways.** By §13.1, a client that cannot deserialize a
  served entry retains its own bundled/last-cached catalog (auto-review simply
  unavailable, no crash), and by the no-`deny_unknown_fields` rule a newer served
  file's extra fields are ignored by an older client — so a fresher snapshot is
  usually still usable across client versions, and incompatibility fails safe.

Because neither the file nor the wire protocol carries the snapshot's origin
version (§7.1), no runtime mismatch signal exists; the cache narrows the drift
window but does not eliminate the §13.1 fail-safe's role. Two questions remain for
#53 triage: the all-or-nothing-per-fetch fallback granularity, and the definition
of "latest" (a release tag vs. `main`), which trades freshness against the risk of
serving data newer than any released client.
