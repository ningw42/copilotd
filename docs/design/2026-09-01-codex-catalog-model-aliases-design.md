# Explicit Codex catalog model aliases — Design

Status: approved
Date: 2026-09-01
Builds on:

- `docs/design/2026-07-19-phase-6b-codex-model-catalog-auto-review-design.md`
- `docs/design/2026-07-21-codex-per-model-auto-review-overrides-design.md`
- ADR-0005, "Codex catalog re-emits Codex's own complete release `ModelInfo`, mutating only named fields, opt-in"
- ADR-0009, "Refresh Codex models from the latest release in memory"
- ADR-0012, "Declare each config setting once via a typed descriptor table"

## 1. Goal and outcome

The official Codex catalog does not contain every model that GitHub Copilot can
forward. For example, Codex's `models.json` may contain `gpt-example` while
GitHub Copilot additionally advertises `gpt-example-alias` as its own
Responses-forwardable model ID. copilotd's Codex catalog currently emits only
the exact-slug intersection of those two catalogs, so Codex cannot discover the
Copilot model even though inference can forward it.

This design adds an explicit, operator-configured **Codex catalog alias** map. A
mapping has the direction:

```text
LIVE_COPILOT_MODEL_ID=OFFICIAL_CODEX_METADATA_SOURCE
```

For example:

```text
gpt-example-alias=gpt-example
```

When the alias is in the live Copilot-forwardable set and the source is a
complete entry in the current accepted Codex catalog, copilotd emits a clone of
the source `ModelInfo` under the alias slug. Every source field remains
byte-identical except the enumerated mutations already owned by the Codex
renderer:

1. `slug` is replaced with the alias;
2. `auto_review_model_override` is removed and resolved from copilotd's reviewer
   configuration; and
3. `context_window` / `max_context_window` may be replaced by the existing
   opt-in live-limit overlay.

The result is a real remote `ModelInfo` that Codex can list and resolve. Codex
then sends the alias slug as the inference request's `model`, and copilotd
forwards it unchanged to GitHub Copilot. This is **not** inference model-name
mapping: the alias is already the real Copilot model ID, while the source is
used only for Codex metadata.

**Outcome:** an operator can expose `gpt-example-alias` to Codex with the
complete behavioral metadata of `gpt-example`, customize its reviewer
independently, and retain copilotd's live-forwardability and complete-entry
safety guarantees.

### 1.1 Terms

**Codex catalog alias**: A live Copilot Responses model ID that is absent from
the accepted official Codex catalog and is emitted with a cloned official
`ModelInfo`. The alias is the slug Codex selects and sends to inference.

**Metadata source**: The exact slug of an entry in the current accepted official
Codex catalog whose complete `ModelInfo` is cloned for an alias. It is not an
inference routing target and need not itself be live Copilot-forwardable.

The implementation should add these terms to `CONTEXT.md` when the feature is
built. Until then, this design is their source of truth.

### 1.2 Grounding

The design depends on four facts verified during design. The fourth is not yet
repository evidence; §11.6 and §13 require promoting it into the pinned-binary
audit before implementation proceeds:

- `catalog.Handler` already has both inputs at request time: the current decoded
  official `CodexModels` and the live Copilot models filtered for picker
  visibility and the OpenAI Responses Route.
- `RenderCodex` is already the pure seam that joins those inputs, clones raw
  `ModelInfo` fields, injects reviewer routing, overlays optional limits, and
  reports safe skips.
- Codex command-auth clients merge each remote entry wholesale by slug. Current
  Codex also appends a remote entry whose slug is absent from its bundle
  (`apply_remote_models` uses `existing_models.push(model)` for an unknown slug).
- A black-box probe against Codex 0.151.0 accepted a cloned official entry under
  a slug absent from its bundle, selected that unbundled slug, and sent the same
  value as `model` to `/responses`.

The source entry must remain complete because remote merge is wholesale and
Codex's behavioral fields include prompts, truncation, tools, reasoning levels,
model messages, and other values GitHub Copilot does not provide. Cloning an
already accepted entry preserves that contract without inventing missing
fields.

## 2. Scope

### 2.1 In scope

- One explicit map-valued configuration setting,
  `codex-catalog-model-aliases`, with `alias=source,...` syntax.
- Exact, case-sensitive, single-hop alias resolution in `RenderCodex`.
- Emitting an alias only when:
  1. the alias is present in the live Copilot-forwardable set; and
  2. the metadata source exists in the current accepted Codex model map.
- An exact official entry for the alias taking precedence over configuration if
  a future Codex release adds it.
- Resolving the complete emitted membership, including aliases, before reviewer
  injection so aliases can be main models and Reviewer models.
- Per-main-model reviewer overrides keyed by the served alias slug, including a
  self-review mapping such as `gpt-example-alias=gpt-example-alias`.
- Applying the live-limit overlay from the alias's Copilot model facts, not from
  the metadata source.
- Widening Codex-shape negotiation so a non-empty alias map is independently
  "something to inject."
- Deterministic reporting for configured mappings that are not applied — whether
  because the alias is absent, shadowed by an exact official entry, or missing
  its source — using ADR-0015's governed logging keys.
- Configuration, pure-renderer, handler, server-boundary, compatibility, and
  regression tests.
- Updates to the configuration reference, domain glossary, ADR-0005 fidelity
  contract, and divergence ledger when implementation lands, including
  reconciliation of the existing reviewer and live-limit Alterations.

### 2.2 Out of scope

- Rewriting `model` in inbound inference requests or outbound Responses data.
  The served alias is already Copilot's real model ID.
- Advertising an operator-invented slug that GitHub Copilot does not itself
  advertise as picker-visible and Responses-forwardable.
- Built-in alias rows tied to today's Copilot lineup.
- Inferring metadata sources from alias naming conventions.
- Alias chaining. A metadata source is looked up only in the accepted official
  `CodexModels`; it is never recursively resolved through the aliases map.
- Requiring the metadata source to be live Copilot-forwardable.
- Editing, augmenting, or re-hashing the cached official Codex `models.json`
  bytes.
- Customizing cloned metadata fields such as `display_name`, prompts, tool mode,
  reasoning levels, or service tiers. Such customization would be a separate
  governed mutation.
- Applying aliases to the provider-shaped OpenAI or Anthropic catalogs, the raw
  GitHub Copilot `/models` passthrough, or any non-Codex request.
- Config-time checks against either live catalog. Both catalogs are request-time
  or cached-value inputs unavailable to pure configuration resolution.

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Alias definition | Explicit `alias=source` map | The operator makes the behavioral-compatibility assertion; no naming convention can establish it safely. |
| Configuration key | `codex-catalog-model-aliases` | The name makes its catalog-only effect explicit and avoids implying inference rewriting. |
| Alias identity | Alias must be a live Copilot model ID | Codex sends the emitted slug to inference, and copilotd must be able to forward it unchanged. |
| Metadata authority | Clone one complete accepted official entry | Preserves Codex's full `ModelInfo` contract without synthesizing missing fields. |
| Source forwardability | Not required | The source supplies metadata only; requiring it in Copilot would unnecessarily couple metadata availability to inference routing. |
| Resolution | Exact and single-hop | Keeps behavior auditable and prevents a configuration graph, cycles, or accidental transitive changes. |
| Official collision | Exact official alias entry wins, and the unapplied mapping is reported at Warn | Codex becomes authoritative as soon as it publishes first-party metadata for that slug. The operator's asserted metadata has been superseded, so the mapping is dead configuration the operator should see and delete. |
| Rendering order | Resolve entries and complete `emitted` membership, then apply reviewer and limit mutations | Reviewer safety depends on knowing every resolvable alias; this order permits alias self-review and alias reviewers. |
| Wire order | Preserve live Copilot order | The alias is a real Copilot model, so it occupies its existing live catalog position. |
| Picker metadata | Mirror `display_name`, `priority`, and `visibility` | Complete-source fidelity wins over distinguishable labels or controlled ranking; duplicate labels and default-selection changes under client/catalog skew are documented operator hazards (§5.3, §9.2). |
| Missing alias/source | Omit only that alias and report an unapplied mapping | A stale mapping must not fabricate metadata or fail the otherwise valid catalog. |
| Reporting level | Every unapplied mapping is a Warn, at one uniform cardinality | A configured mapping that does not take effect is one condition with one operator remedy — edit configuration. Splitting it across levels by cause would make the vocabulary harder to reason about than the noise it saves. |
| Empty aliases map | Exact current behavior | The feature is opt-in and introduces no output change at its zero value. |
| Cached value | Keep exact official bytes; synthesize per render | Preserves ADR-0009 hashes, provenance, refresh ladder, and last-good semantics while retaining live Copilot gating. |

## 4. Configuration interface

One new flat setting follows the repository's standard precedence and typed
descriptor machinery from ADR-0012.

| Flag / TOML key | Environment variable | Resolved type | Default |
| --- | --- | --- | --- |
| `codex-catalog-model-aliases` | `COPILOTD_CODEX_CATALOG_MODEL_ALIASES` | `map[string]string` (`alias -> source`) | empty map |

Example:

```sh
copilotd serve \
  --codex-catalog-enabled \
  --codex-catalog-model-aliases \
    'gpt-example-alias=gpt-example' \
  --codex-auto-review-model \
    'gpt-example-reviewer' \
  --codex-auto-review-model-overrides \
    'gpt-example-alias=gpt-example-alias'
```

The corresponding result is:

| Main model | Resolved Reviewer model |
| --- | --- |
| `gpt-example-alias` | `gpt-example-alias` (per-main-model override) |
| `gpt-example` | `gpt-example-reviewer` (global fallback) |
| `gpt-example-reviewer` | `gpt-example-reviewer` (global fallback) |
| Other emitted models | `gpt-example-reviewer` (global fallback) |

Self-review is valid: the override means that when the alias is the Main model,
Codex uses the same slug as the Reviewer model. Reviewer selection is single-hop;
the Reviewer model does not recursively acquire another Reviewer model.

### 4.1 Parsing and precedence

The descriptor should follow the existing
`codex-auto-review-model-overrides` map setting:

- Split on commas and ignore empty segments, including a trailing comma.
- Split each non-empty segment on the first `=` and trim surrounding whitespace.
- Reject a missing `=`, empty alias, empty source, duplicate alias, or
  `alias == source`. A self-map is locally detectable, never useful, and
  unambiguously signals operator confusion. By contrast, a non-self mapping
  shadowed by an exact official entry may have been valid when written and only
  become redundant after a later Codex release, so shadowing is not a startup
  error. It is instead detected and reported at render time, where the official
  catalog is actually known (§7.2).
- Match slugs verbatim and case-sensitively; perform no normalization.
- Parse every supplied flag, environment, and TOML layer eagerly. A malformed
  lower-precedence layer remains an error even if a higher-precedence layer is
  valid, as required by ADR-0012.
- Among valid layers, the highest-precedence layer replaces the complete map;
  maps are never merged across layers.
- Render the resolved map as a normalized, alias-sorted `alias=source,...`
  string in `ServeConfig.LogValue`. Model slugs are non-secret.
- Permit a non-empty map while `codex-catalog-enabled=false`. It is valid but
  inert, allowing staged configuration.

The implementation may deepen the config module by extracting a private
map-valued descriptor helper shared with reviewer overrides, but it must not
expose a generic transform pipeline or change existing precedence semantics.

### 4.2 Projected render configuration

`ServeConfig` gains:

```go
CodexCatalogModelAliases map[string]string
```

The composition root projects it into the catalog module without allowing a
`config.*` type to cross the seam:

```go
type CodexRenderConfig struct {
    AutoReviewModel          string
    AutoReviewModelOverrides map[string]string
    ModelAliases             map[string]string // live Copilot alias -> official Codex source
    OverrideLimits           bool
}
```

`RenderCodex` keeps its existing interface:

```go
func RenderCodex(
    codexModels CodexModels,
    forwardable []Model,
    cfg CodexRenderConfig,
) ([]byte, CodexRenderOutcome, error)
```

This keeps the module deep: callers supply facts and policy once, while exact
precedence, membership, cloning, mutation order, safety checks, deterministic
output, and outcomes remain hidden in the implementation.

## 5. Catalog alias resolution

### 5.1 Resolvable entry

For each live Copilot-forwardable model `M`, in live order, resolve one entry:

```text
if codexModels[M.ID] exists:
    resolve the exact official entry
else if source = ModelAliases[M.ID] exists
        and codexModels[source] exists:
    resolve an alias clone from codexModels[source]
else:
    M is not emitted in the Codex catalog
```

An exact official entry always wins, even when `ModelAliases[M.ID]` is present.
This makes a configured mapping automatically redundant when Codex gains
first-party metadata, without pinning older cloned behavior over the new entry.
The now-inert mapping is reported so the operator can remove it (§7.2).

The source lookup is directly against `codexModels`. Given:

```text
A=B,B=C
```

`A` can clone official `B` if and only if official `B` exists. `A` never follows
`B`'s configured mapping to `C`. Likewise, a mapping cannot create an entry for
an alias absent from `forwardable`; configuration only fills a Codex metadata
gap for a model Copilot already advertises.

### 5.2 Preserve and extend the two-pass renderer invariant

`RenderCodex` already works in two passes: it builds the complete `emitted` set,
then runs the mutation loop. Alias support preserves that structure and extends
the existing membership pass rather than introducing a new rendering
architecture.

**Pass 1 — validate configured aliases and resolve membership**

1. Index `forwardable` by live model ID.
2. Walk configured aliases in alias-sorted order and record at most one
   unapplied-mapping outcome for each, in this precedence:
   `alias_not_forwardable` when the alias is absent from that index — a loop
   over `forwardable` alone cannot observe this case; otherwise
   `shadowed_by_official` when `codexModels` already carries the alias as an
   exact entry, because first-party metadata supersedes the mapping.
3. Walk `forwardable` in live order. Resolve an exact official entry first,
   otherwise resolve the configured source directly from `codexModels`; record
   `metadata_source_missing` when a live alias has no accepted source.
4. Build the complete `emitted` set from each successfully resolved **served
   slug**, including aliases, and retain the source used by each served slug.

The three reasons are mutually exclusive by construction: a non-forwardable
alias never reaches step 3, and step 3 consults a configured source only when no
exact official entry exists. Every configured mapping therefore yields exactly
zero or one outcome.

Unapplied-mapping outcomes are sorted by alias after every path is collected.
Wire membership and later reviewer skips continue to follow live Copilot order.

**Pass 2 — clone and mutate**

For every resolved model in the same live order:

1. clone the exact or source entry with `copyCodexEntry`;
2. when aliased, encode the served alias into `fields["slug"]`;
3. delete the inherited `auto_review_model_override`;
4. resolve the Reviewer model by the served slug;
5. inject it only if it belongs to the complete `emitted` set;
6. apply optional limits from the live Copilot model `M`; and
7. append the entry for deterministic envelope marshalling.

In pseudocode:

```go
forwardableByID := indexForwardable(forwardable)
sorted := sortedAliases(cfg.ModelAliases)
mappingIssues := classifyAliasMappings(sorted, forwardableByID, codexModels)
resolved, emitted, sourceIssues := resolveCodexEntries(
    codexModels, forwardable, cfg.ModelAliases)
outcome.UnappliedAliases = sortByAlias(mappingIssues, sourceIssues)

for _, model := range forwardable {
    source, ok := resolved[model.ID]
    if !ok {
        continue
    }

    fields := copyCodexEntry(source.Fields)
    if source.Aliased {
        fields["slug"] = encodeString(model.ID)
    }

    delete(fields, "auto_review_model_override")
    reviewer := resolveReviewer(model.ID, cfg)
    if reviewer != "" && emitted.contains(reviewer) {
        fields["auto_review_model_override"] = encodeString(reviewer)
    } else if reviewer != "" {
        outcome.skipReviewer(model.ID, reviewer)
    }

    overlayLiveLimits(fields, model, cfg.OverrideLimits)
    entries = append(entries, fields)
}
```

The pseudocode names private implementation concepts, not new exported
interfaces. The implementation may keep them as local maps/helpers if that is
clearer.

Extending the complete membership pass is load-bearing. If reviewer mutation ran
while aliases were still being discovered, an alias used as its own reviewer—or
as another model's reviewer—could be incorrectly rejected as unadvertised.

### 5.3 Metadata fidelity

For an emitted mapping:

```text
gpt-example-alias=gpt-example
```

all fields originate from the accepted `gpt-example` entry. `slug` changes to
the live Copilot value. `display_name` remains the source value, such as
`GPT Example`; changing it to a fabricated alias-specific label is outside this
design.
Any future source fields remain copied automatically because the representation
is `map[string]json.RawMessage`, not a hand-authored partial struct.

The source has already crossed the Codex cached value's accept-gate (or is the
init-validated embedded fallback), so cloning does not require a second schema
implementation. Tests still verify that only the governed fields differ.

Mirroring `display_name`, `priority`, and `visibility` has a deliberate picker
cost. When the source is also emitted, source and alias can appear as
identically labelled and ranked rows. If the source is bundled in the client,
current pinned Codex behavior leaves the in-place source ahead of the appended
alias after a stable priority sort; §11.6 pins that mitigation. Across
client/catalog skew—especially when the source is absent from the client's
bundle—the alias can become the first picker-visible model and change the
session default. The renderer does not alter these fields because doing so would
trade complete-source fidelity for fabricated presentation or ranking policy.

### 5.4 Reviewer interaction

Reviewer resolution remains the existing per-main-model rule, but its key is the
**served slug**:

```text
reviewer = AutoReviewModelOverrides[servedSlug] if present
        ?? AutoReviewModel
        ?? ""
```

A present per-main-model override remains authoritative and does not fall back
to the global if its reviewer is unadvertised. Because Pass 1 includes aliases
in `emitted`, any emitted alias is a valid reviewer target. This supports:

```text
ModelAliases:
    gpt-example-alias -> gpt-example

AutoReviewModel:
    gpt-example-reviewer

AutoReviewModelOverrides:
    gpt-example-alias -> gpt-example-alias
```

The source entry's own reviewer value is never inherited. Alias cloning happens
first; the existing renderer then deletes and resolves
`auto_review_model_override` from deployment configuration.

### 5.5 Live limits

When `codex-catalog-override-limits` is enabled, the renderer overlays limits
from the live Copilot `Model` whose ID is the alias. It never borrows the
metadata source's Copilot limits. Missing live limit fields retain the cloned
official values under the existing independent-field fallback.

## 6. Content negotiation and affected surfaces

`servesCodexShape` gains one disjunct:

```text
Codex catalog enabled
AND client_version is present
AND (
    global reviewer is non-empty
    OR per-model reviewer map is non-empty
    OR model aliases map is non-empty
    OR live-limit override is enabled
)
```

A deployment using aliases alone therefore receives the Codex-shaped
`{"models":[...]}` response. Existing gates remain unchanged:

- no `client_version` -> provider-shaped OpenAI catalog;
- disabled Codex catalog -> provider-shaped OpenAI catalog;
- Anthropic Surface -> provider-shaped Anthropic catalog;
- raw GitHub Copilot `/models` -> raw passthrough; and
- inference requests -> unchanged forwarding.

HEAD behavior is preserved when `ModelAliases` is empty. When the map is
non-empty, HEAD keeps its existing contract rather than acquiring a new one: the
handler renders the same alias-inclusive representation it would return for GET,
reports that representation's length in `Content-Length`, and suppresses only the
body. The header therefore tracks the emitted catalog automatically, and no
separate aliased-HEAD policy exists to implement or test.

## 7. Failure and collision policy

### 7.1 Startup failures

Configuration resolution fails before binding for malformed syntax, empty
values, duplicate aliases, and self-mappings. These failures are independent of
live catalog state and belong in `internal/config`.

### 7.2 Request-time unapplied mappings

Live and cached catalog facts cannot be validated at startup. A syntactically
valid mapping is **not applied** — without failing the catalog — when:

- the alias is absent from the live Copilot-forwardable set
  (`alias_not_forwardable`);
- the alias already has an exact entry in the current accepted Codex model map,
  so first-party metadata supersedes it (`shadowed_by_official`); or
- the metadata source is absent from the current accepted Codex model map
  (`metadata_source_missing`).

`CodexRenderOutcome` should report these deterministically so the handler can
warn with the alias, source, and reason. One bad mapping affects only that
mapping; other exact and aliased entries still render. Outcomes are ordered by
alias slug because they originate partly from configuration-map entries that
have no live position; existing `SkippedReviewers` remain in live Copilot order.
The two sibling slices therefore have different, explicit deterministic
orderings.

The reason vocabulary names why the **configured mapping** was not applied, not
whether a slug reached the wire. Under `shadowed_by_official` the alias slug is
still served — from Codex's own entry — while the mapping that named it is inert.
`alias_not_forwardable` and `metadata_source_missing` omit the alias entirely;
all three mean the same thing to the operator, which is that a configured line
had no effect.

A shadowed mapping is not an error and never alters the official metadata
through alias cloning. Reviewer and live-limit mutations apply normally to that
served slug, exactly as they would for any exact entry.

This design deliberately chooses visibility over silent inertness for every
unapplied mapping: the configuration exists specifically to make that slug
resolve a particular way, and a Warn record explains entitlement drift, lineup
drift, or Codex catalog evolution as a contained abnormality. Shadowing warns at
the same level as the other two reasons rather than at info/debug, because all
three describe one condition with one operator remedy — edit the configuration —
and a level split by cause would cost more comprehension than the noise it saves.
The operator consequence of shadowing is also not merely cosmetic: the metadata
Codex now serves for that slug is first-party and may differ behaviorally from
the source the operator asserted was compatible.

Shadowing is the one reason that never clears on its own; it persists until the
operator deletes the mapping. That permanence is accepted deliberately as the
price of one uniform rule.

The warning is emitted once per unapplied mapping on every Codex catalog request.
The pinned client is observed to fetch the catalog at session start rather than
per turn, so practical cardinality is closer to sessions than inference turns;
that cadence is an observed compatibility fact, not a contract. Deliberate
staging and entitlement drift are indistinguishable to the renderer.
Process-wide deduplication is rejected because mutable cross-request state would
cost more than the noise and compromise the pure render seam. That trade is
least comfortable for `shadowed_by_official`, whose condition is permanent — but
per-request repetition is precisely the pressure that gets a dead mapping
deleted, and the remedy is a one-line configuration edit. Existing
unconfigured Copilot-only models remain silently absent as today.

A possible outcome shape is:

```go
type UnappliedCatalogAlias struct {
    Alias  string
    Source string
    Reason CatalogAliasSkipReason
}

type CodexRenderOutcome struct {
    UnappliedAliases []UnappliedCatalogAlias
    SkippedReviewers []SkippedReviewer
}
```

The exact private/exported split and the final Go names are implementation
details, but the type should not be called "skipped": under
`shadowed_by_official` the slug is served and only the mapping is inert. The
interface must let the handler distinguish `alias_not_forwardable`,
`shadowed_by_official`, and `metadata_source_missing` without parsing an error
string. The handler logs the alias with `logging.ModelKey`, the source with a
new `logging.MetadataSourceKey = "metadata_source"`, and the reason with a new
`logging.SkipReasonKey = "skip_reason"`. The wire key stays `skip_reason`
because what was skipped is the mapping. `SkipReasonKey` is intentionally
distinct from `FailureClassKey`: an unapplied mapping is a successful-render
outcome, not a request failure. Existing reviewer-skip warnings retain only
`model` and `reviewer` because they have one reason—an unemitted reviewer—and
need no reason vocabulary.

### 7.3 Catalog-level errors

Existing catalog-level failures are unchanged. Invalid upstream Copilot JSON,
an invalid current cached value, JSON encoding failure, or invalid raw field
still produces the existing `502`. Alias resolution itself is best-effort and
does not add a new request error class.

## 8. Architecture and file-level plan

All behavior remains behind the existing `internal/catalog` render seam. Both
dependencies are in-process values at render time, so no new adapter or port is
justified.

```text
cmd/copilotd/main.go
    project resolved aliases into CodexRenderConfig

internal/config/config.go
    declare codex-catalog-model-aliases once via a typed descriptor
    parse/validate alias=source pairs
    normalize LogValue output

internal/catalog/handler.go
    aliases-only Codex-shape gate
    turn unapplied alias-mapping outcomes into structured logs

internal/catalog/codex_render.go
    resolve exact entries and alias sources
    build complete emitted membership
    clone source and rewrite slug
    apply reviewer mutation after aliasing
    apply live alias limits

internal/logging/keys.go
internal/logging/structure_test.go
    add MetadataSourceKey and SkipReasonKey to the governed registry
    and its two-way inventory

internal/catalog/codex_models_cache.go
internal/catalog/codex_snapshot.go
    unchanged: retain exact accepted official bytes and validation

internal/forward
internal/wsforward
internal/shim
    unchanged: alias is already the real Copilot model ID
```

The configuration declaration follows ADR-0012: adding the setting requires a
flat `ServeConfig` field and one descriptor row; the descriptor engine derives
flag/environment/TOML precedence, logging, and validation behavior. If the
existing reviewer-map descriptor is generalized, the helper stays private and
specific to string maps rather than becoming a catalog-transformation
interface.

## 9. Divergence and compatibility

### 9.1 Divergence classification

A catalog alias is an opt-in **Alteration**, not a Fabrication:

- the served slug comes from the live Copilot catalog;
- every behavioral field comes from one complete accepted official Codex entry;
- reviewer and limits retain their existing governed sources; and
- no value is invented from a naming convention or default.

The implementation must amend ADR-0005's current "only reviewer and limits"
fidelity language to enumerate `slug` replacement for explicit aliases. The
same documentation change must reconcile three sibling Alterations in the
divergence ledger, each pointing to amended ADR-0005 as its authoritative
source: reviewer override, live-limit overlay, and explicit catalog alias. The
first two are a pre-existing ledger gap that adding only the alias row would
make more misleading. If that reconciliation proves too broad during
implementation, it becomes an explicit tracked blocker rather than leaving an
asymmetric ledger. The alias map's empty default and the parent
`codex-catalog-enabled` gate provide its opt-in.

### 9.2 Client compatibility

Codex 0.151.0 was black-box verified during design to accept and select an
unknown remote slug. At `rust-v0.152.0`, source inspection corroborates that
`apply_remote_models` still appends unknown remote models. Both unknown-slug
append and equal-priority default ordering are Codex-internal, so implementation
step 1 promotes them into the repository's release-audit test rather than
treating either as a permanent public contract.

The audit stays on the existing pinned Codex 0.151.0 binary and does not advance
the embedded floor, `codex-latest` fixture, or accept-gate hashes. It proves two
cases: an unknown alias can win the picker and reach `/responses` unchanged; and
an alias with the same picker metadata as its bundled source does not displace
that source as the default. Source inspection at 0.152.0 remains corroboration,
not a pin update.

The cloned source's own client gates remain authoritative. In particular,
`minimal_client_version`, visibility, priority, reasoning presets, service tiers,
and tool policy are intentionally mirrored. An operator should map an alias only
to a source whose behavior is compatible with the real Copilot model. The
operator must also accept duplicate picker labels and the possibility that
client/catalog skew changes the default model (§5.3).

## 10. Considered alternatives

### 10.1 Built-in explicit registry

A code-owned row such as
`gpt-example-alias -> gpt-example` would remove operator configuration and
misconfiguration. Rejected: Copilot lineups vary by entitlement and over time,
and each mapping is a deployment assertion about metadata compatibility. A
built-in table would freeze today's external fact into copilotd and require a
release to add, remove, or correct it.

### 10.2 Prefix or suffix inference

The renderer could strip an alias suffix or choose the longest official slug
prefix.
Rejected: naming does not prove behavioral equivalence. Automatic inference
would silently copy prompts, tools, reasoning levels, service tiers, and future
fields onto every matching Copilot-only model, broadening the blast radius
without operator intent.

### 10.3 Mutate the cached official catalog

Aliases could be inserted into the current `models.json` bytes during refresh.
Rejected: alias eligibility depends on the request's live Copilot-forwardable
set and deployment configuration, while the cached value is an exact,
commit-addressed official artifact with hash/provenance and last-good semantics.
Mixing those responsibilities would weaken the ADR-0009 seam and make cache
status misrepresent the served source.

### 10.4 Export a generic catalog-transform pipeline

Aliases, reviewers, and limits could become exported composable transforms.
Rejected: callers would have to understand ordering, complete membership,
mutation authority, and failure rules. Keeping policy inside `RenderCodex`
provides greater depth and locality through its existing small interface.

### 10.5 Rewrite inference model names

copilotd could advertise the official source slug and rewrite inference requests
to the Copilot alias. Rejected: it changes the feature from catalog metadata
completion into bidirectional transport mutation, must span HTTP and WebSocket,
and cannot expose source and alias as distinct selectable models. It is also
unnecessary because the alias is already the ID Copilot accepts.

## 11. Test design

### 11.1 Configuration

- Empty input resolves to a canonical nil/empty map.
- Flag, environment, and TOML values parse `alias=source,...` correctly.
- Every supplied layer is parsed eagerly; malformed lower-precedence input fails
  even when a higher-precedence value is valid.
- Valid layers use wholesale precedence, not cross-layer merge.
- Whitespace and empty comma segments are normalized.
- Missing `=`, empty alias, empty source, duplicate alias, and self-mapping fail.
- Matching is case-sensitive and values are not normalized.
- `LogValue` contains one deterministic alias-sorted non-secret string.
- A staged map is valid while the Codex catalog is disabled.

### 11.2 Pure renderer

- A live Copilot alias clones its official source; every raw field is
  byte-identical except `slug`, the inherited `auto_review_model_override`, and
  any separately enabled limit mutations. The inherited reviewer field is
  deleted unconditionally (§5.2 Pass 2 step 3, §5.4), so a source carrying that
  field differs from its clone even when no reviewer is configured.
- The source may be absent from the live Copilot set and still supply metadata.
- An alias absent from the live forwardable set is omitted and reported
  `alias_not_forwardable`.
- A live alias with a missing official source is omitted and reported
  `metadata_source_missing`.
- An alias with an exact official entry is served from that official entry,
  byte-identical to it apart from ordinary reviewer/limit mutations, and
  reported `shadowed_by_official`.
- A configured mapping produces at most one outcome, and an alias that is both
  non-forwardable and shadowed reports `alias_not_forwardable` only.
- Resolution is single-hop and never follows another configured alias.
- Emission order remains the live Copilot order regardless of map insertion
  order.
- Unapplied-mapping outcomes remain deterministic regardless of map insertion
  order.
- An alias-only configuration leaves unrelated exact entries unchanged.
- An empty alias map reproduces current bytes and outcomes.

### 11.3 Reviewer ordering

- A per-main-model reviewer override keyed by an alias replaces the global
  reviewer for that alias.
- An emitted alias may review itself.
- An emitted alias may review another model and another model may use the alias
  as its reviewer.
- A missing alias is not considered an emitted reviewer.
- A present-but-unemitted reviewer remains a normal `SkippedReviewer` and does
  not fall back to the global.
- The cloned source's original `auto_review_model_override` never leaks into the
  alias.

### 11.4 Limits

- With live limits disabled, an alias retains the source's official limits.
- With live limits enabled, each available field comes from the alias's live
  Copilot entry.
- A missing live prompt or maximum-context field independently retains the
  source field.

### 11.5 Handler and server boundary

- Enabled catalog + `client_version` + aliases only serves the Codex shape.
- Disabled catalog, missing `client_version`, and an empty mutations set preserve
  provider-shaped behavior.
- Alias warnings use governed `model`, `metadata_source`, and `skip_reason`
  keys; the logging registry's two-way inventory includes both new constants.
- Skip reasons are `alias_not_forwardable`, `shadowed_by_official`, and
  `metadata_source_missing`; reviewer skips remain unchanged without a
  `skip_reason`.
- Warning cardinality is one record per unapplied mapping per Codex catalog
  request, with no process-wide deduplication.
- An exact-official collision warns with `shadowed_by_official` while still
  serving the official entry unaltered, with ordinary reviewer and limit
  mutations applied.
- A configured mapping yields at most one outcome; a non-forwardable alias that
  also has an exact official entry reports `alias_not_forwardable` only.
- Provider-shaped OpenAI, Anthropic, and raw `/models` responses are unchanged.
- A direct `/responses` request using the alias reaches the Copilot stub with the
  same model ID, proving no inference rewrite was introduced.

### 11.6 Codex release audit

Before production implementation, extend and run the opt-in pinned-binary
contract test at its existing Codex 0.151.0 pin with two cases:

1. Return an unknown remote alias cloned from a complete accepted source, make
   it the picker winner within the test fixture, and assert that Codex sends the
   alias slug to `/responses`.
2. Return an unknown alias with the same `display_name`, `priority`, and
   `visibility` as its bundled source, and assert that adding it does not change
   the bundled source selected by default.

The first case pins unknown-slug append; the second pins the current stable-sort
plus first-picker-visible mitigation for the common bundled-source case. Neither
case advances the binary or catalog fixture pins. Keep the pure renderer fidelity
test responsible for proving that production alias metadata changes only
governed fields.

Run the focused packages, `go test ./...`, and `go test -race ./...`.

## 12. Documentation and rollout

Implementation updates:

1. `CONTEXT.md`: add Codex catalog alias and metadata source terminology; state
   that an alias is a real Copilot model ID, not an inference rewrite.
2. `CONFIGURATION.md`: document flag/env/TOML syntax, precedence, staging,
   source semantics, and the self-review example. Use placeholder model slugs
   for this setting rather than freezing a transient external lineup into docs.
3. ADR-0005: amend the fidelity contract to permit explicit alias `slug`
   substitution before reviewer/limit mutations.
4. `docs/divergence-ledger.md`: reconcile separate reviewer-override,
   live-limit-overlay, and catalog-alias Alteration rows against amended
   ADR-0005, or make that reconciliation an explicit tracked blocker.
5. Release notes: call out the dependency on live Copilot picker visibility and
   Responses forwardability, duplicate alias/source picker labels, possible
   default-model changes under client/catalog skew, and per-catalog-request
   warnings for every unapplied mapping — including the `shadowed_by_official`
   warning that persists until the operator removes a mapping Codex has
   superseded.

No migration is needed. The default map is empty, so existing configurations and
wire output remain unchanged. Operators enable the feature by adding the map to
an already enabled Codex catalog; aliases disappear safely if entitlement or
catalog compatibility later changes.

## 13. Implementation sequence

1. Extend and run the existing pinned Codex 0.151.0 binary audit for both
   unknown-alias forwarding and bundled-source default preservation. Stop the
   feature if either premise fails; do not advance snapshot or binary pins.
2. Add the typed config descriptor, parser, normalized logging, and focused
   config tests.
3. Extend `CodexRenderConfig` and the existing membership pass for exact/alias
   resolution, explicit alias-map skips, and complete `emitted` membership.
4. Apply existing reviewer and limit mutations after alias cloning; add the full
   pure test matrix, including self-review.
5. Widen content negotiation, add governed structured skip logging (including
   the key registry and two-way inventory), and cover handler, real-listener,
   warning-cardinality, and inference-passthrough seams.
6. Update domain, configuration, ADR, all three divergence-ledger rows, and
   release documentation; create an explicit blocker if ledger reconciliation
   cannot land atomically.
7. Run focused tests, the full suite, and the race detector.

## 14. Acceptance criteria

The implementation is complete when all of the following hold:

1. `codex-catalog-model-aliases` resolves as an explicit, case-sensitive,
   single-hop `alias -> official source` map through flag, environment, and TOML
   with ADR-0012 eager parsing and wholesale precedence.
2. A configured alias is emitted only when its slug is live Copilot
   picker-visible/Responses-forwardable and its source exists in the accepted
   official Codex map.
3. An alias entry is a complete clone of its source. Only `slug`, the
   unconditionally removed inherited `auto_review_model_override`, configured
   reviewer routing, and opt-in live limits may differ.
4. Exact official metadata wins if Codex later adds the alias slug, and the
   superseded mapping is reported as `shadowed_by_official` without altering
   that official entry.
5. Complete alias membership is built before reviewer injection; per-main-model
   overrides are keyed by the served alias and an emitted alias can review
   itself.
6. Live-limit overlays use the alias's Copilot facts, with the existing
   independent official fallback for missing fields.
7. Alias output preserves live Copilot order; unapplied-mapping outcomes are
   alias-sorted, reviewer skips retain live order, and normalized config logging
   is deterministic.
8. Every unapplied mapping — missing live alias, shadowed by an exact official
   entry, or missing official source — produces governed `model` /
   `metadata_source` / `skip_reason` Warn observability once per mapping per
   catalog request at one uniform level, without cross-request deduplication.
   The first and third omit only the affected alias; the second omits nothing.
   Malformed local syntax remains a startup error.
9. Aliases alone activate the enabled, `client_version`-negotiated Codex shape;
   all other catalog and inference paths retain current behavior.
10. The official cached bytes, refresh ladder, forwarder, WebSocket forwarder,
    and shim registry remain unchanged.
11. Before implementation proceeds, the existing pinned Codex 0.151.0 audit
    proves both unknown-alias forwarding and bundled-source default
    preservation without advancing binary or snapshot fixtures; configuration,
    pure-renderer, ordering, reviewer, limits, handler, server-boundary, and
    inference-passthrough tests cover the remaining behavior.
12. `CONTEXT.md`, `CONFIGURATION.md`, and ADR-0005 are updated when the feature
    lands; the divergence ledger contains separate reviewer, live-limit, and
    alias Alteration rows or an explicit blocker prevents completion.
13. Duplicate picker labels and possible default-model changes under
    client/catalog skew are documented operator hazards; the full suite and race
    detector pass.
