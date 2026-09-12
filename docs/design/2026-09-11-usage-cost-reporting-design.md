# Estimated cost in Usage reports

**Status:** implemented through #238 and amended by #241: pricing/source,
matching, native calculators, per-Turn context-tier selection, daemon-owned
aggregation, additive HTTP transport, CLI presentation, and local synthetic
executable acceptance. Final full-suite/race/flake and same-revision
native-platform certification remain release gates, not inferred from this status.
**Date:** 2026-09-11
**Extends:** [Usage reporting](2026-09-07-usage-reporting-design.md)

**Snapshot-policy amendment:** The maintainer confirmed
[`8bb205a`](https://github.com/ningw42/copilotd/commit/8bb205a7dc2a158e250e3270ef7b5b90cf232d60)
as superseding the epic-linked `0d761b6` per-report parsing policy and associated
work-accounting/lifetime requirements. The source owns a reusable immutable
projection keyed by content hash, seeds the floor during enabled construction
outside report work, and may retain parsed state across reports and HTTP response
writing. Projection misses use the calling report's work context; each report
still applies its identity budget and cancellation.

## 1. Agreed direction

Add estimated cost to `copilotd usage` without turning the Usage report into a
billing ledger or changing the persisted native observations.

The maintainer chose:

1. **Original-provider prices from models.dev**, not the `github-copilot` rates
   or an arbitrary reseller's prices.
2. **The existing vendored cached-value practice:** an embedded, identified
   fallback; best-effort in-memory refresh; last-good retention; no runtime price
   files.
3. **One cache-write price:** multiply the aggregate cache-write count by the
   corresponding models.dev rate. Do not distinguish cache-write TTLs.
4. **Per-Turn context pricing:** retain base and structured context tariffs and
   select one rate vector from each Turn's complete Surface-native input. Use the
   base vector when no strict-above threshold matches and otherwise the greatest
   matching threshold. Selection is data-driven from models.dev, not special-cased
   by provider.
5. **Report-time valuation:** never persist prices or calculated costs. Historical
   Turns are always valued using the latest accepted pricing snapshot available
   to this daemon, not a historical tariff.
6. **Daemon-owned calculation:** the Usage reporting module calculates amounts;
   the HTTP adapter transports the report. The CLI renders and sums supplied
   amounts for its existing terminal-only period totals.
7. **A dedicated model-matching module within usage**, with constrained
   approximate/prefix matching rather than making each caller interpret names.
8. **Terminal layout and precision:** place the cost column immediately after
   `Model(s)` and before `Turns`; round displayed USD amounts to three digits
   after the decimal point. Calculate and sum exact amounts before rounding.

The remaining sections propose concrete details for approval. They do not claim
those details are already implemented or separately approved.

### Meaning of the number

**Estimated cost** is a USD valuation of priceable persisted Turns using the
selected original-provider rates. It is not money paid to GitHub, an invoice,
subscription savings, or proof of complete consumption. Current accepted rates,
per-Turn context selection, and a single cache-write rate do not guarantee that
the result is an upper bound on actual charges.

A **Pricing model** is the original-provider/model pair selected for that
valuation. It is separate from the Reported model and Requested model, and is
not a forwarding target or Catalog Metadata source. These are proposed glossary
additions for approval with this design.

### Non-goals

No historical rate storage, database migration, pricing in the Usage meter,
Copilot billing reconciliation, currency conversion, tax/discount accounting,
fast/flex/batch/priority reconstruction, cache-TTL pricing, new token-count
projection, configurable price overrides, live Catalog dependency, arbitrary
reseller fallback, or CLI-side token-to-money calculation.

## 2. Existing contracts retained

- [ADR-0017](../adr/0017-persist-usage-in-local-sqlite.md): collection remains
  opt-in, best-effort, and asynchronously persisted in the existing database.
- [ADR-0018](../adr/0018-store-per-surface-native-usage.md): native counts remain
  unchanged. Missing counts are not reported zeros. OpenAI input contains cache
  subsets; Anthropic uncached input excludes additive cache counts.
- [ADR-0019](../adr/0019-serve-unauthenticated-usage-reports.md): reports remain
  unauthenticated on the existing listener, bounded, database-scoped, and
  independent of inference readiness. Estimated costs inherit that disclosure.
- [ADR-0009](../adr/0009-refresh-codex-models-from-latest-release-in-memory.md):
  reuse the cached-value lifecycle, not Codex's release-tag selection or
  complete-ModelInfo fidelity policy.
- Report grouping/filtering remains exact `(Surface, Reported model, period)`.
  Matching must not merge rows, alter a model filter, replace the Reported model,
  or substitute Requested model.
- Existing native metrics, integer arithmetic, database snapshot, cancellation,
  work/response limits, and writer-finalization ordering remain authoritative.

This deliberately extends the earlier reporting design's pricing non-goal. It
also adds a best-effort background pricing dependency, but **not network work
inside a report request**.

## 3. Modules and seam placement

```text
models.dev -> embedded fallback / cached value
                            |
                    captured pricing snapshot
                            |
                  original-provider pricing
                  /                       \
      model matching                 rate selection
                  \                       /
committed native Turns -> Usage reporting module
                            |
                     HTTP JSON adapter
                            |
                    CLI render + subtotal
```

| Location | Responsibility |
| --- | --- |
| `internal/usage/modelmatch` (new) | Index candidate identities; resolve Reported models; govern approximation and ambiguity |
| `internal/usage/pricing` (new) | Embedded data and public-source adapter; parse/validate prices; select rates; value native counts |
| `internal/usage/report` | Capture prices once; read Turns; aggregate native metrics, monetary amounts, and pricing coverage |
| `internal/usage/reporthttp` | Additive wire fields, bounded encoding, validation, compatibility |
| `internal/usage/reportcli` | Safe formatting, exact addition of supplied amounts/coverage, explanatory notes |
| `internal/config`, `cmd/copilotd` | One refresh setting; conditional registration and existing lifecycle wiring |

`Reporter.Query` remains the external reporting seam. Its caller does not learn
matching rules, models.dev structure, price selection, or token nesting.

The reporter accepts a pricing source supplied by the composition root; it does
not create an HTTP client. `report.New(databasePath, pricing.Source)` requires
that dependency explicitly. The source retains one immutable parsed projection
for its effective content hash while the generic cached value remains the owner
of the authoritative raw bytes and refresh lifecycle. At Query entry, before
timezone/date resolution and bucket construction, Query calls
`Source.Current(ctx, ProjectionLimit)` exactly once to capture that local value
and the current status while applying the caller's provider/model identity-byte
projection limit. It defers a non-cancellation capture failure until
semantic/calendar validation completes, so invalid query input keeps its
established precedence; context cancellation remains authoritative. A
report-specific limit failure does not reject an accepted cached value. Query
then derives one immutable report-local matching snapshot for the whole report.
A refresh during later calendar or database work cannot change that report's
prices or model matches. Tests provide fixed data through the same source seam.

Matching and arithmetic are in-process modules. Test them through their
interfaces with literal data; no adapter abstraction is needed for pure
computation. Test the remote source adapter against an in-process HTTP server
and reporting against real temporary SQLite databases. Do not introduce a
generic pricing-provider plugin framework or expose SQL through these interfaces.

## 4. Pricing snapshot and refresh

### Source and original-provider scope

Use `https://models.dev/api.json`, whose entries are keyed by provider and model.
The provider-agnostic `models.json` is not suitable: it has no costs.

**Proposed initial source allowlist:** `openai`, `anthropic`, `google`, and `xai`.
These are original-provider namespaces for the initially targeted model families.
Adding another original provider is an explicit, tested extension of this list,
not a fallback to a reseller or inference Surface. Models outside this scope
remain visible in native usage and unpriced.

Include **all model identities** from the selected namespaces in the matcher,
even entries lacking prices. An exact unpriced model must not disappear from
candidate selection and cause a nearby priced model to win.

### Vendored artifact

Place unchanged `api.json` bytes, the applicable MIT license, an identity
manifest, and a short provenance/bump guide under
`internal/usage/pricing/modelsdevdata/`. The manifest records the artifact URL,
observation time, byte size, SHA-256, and separately audited source/license commit.
Tests verify the actual embedded bytes against that manifest.

The live artifact has no embedded source commit. Never label downloaded bytes
with whichever Git commit happens to be latest. Source audit identity and fetched
artifact identity are separate facts. The vendored data is the reliable floor;
a successful later refresh can replace the effective bytes in memory.

### Cached-value recipe

Use `internal/cache` with:

- Name `usage_prices`.
- Raw bytes as the cached value; content SHA-256 as both hash and version identity.
- The embedded bytes as fallback; validate that floor in tests.
- No separate `Version` peek initially: fetch the artifact, hash, validate, and
  hold/swap through the existing refresh ladder. ETag optimization can follow
  evidence of a need; it is not required for correctness.
- A dedicated credential-free HTTP client. No API key, GitHub OAuth token,
  Copilot token, or impersonation headers. Refuse redirects rather than silently
  changing the source. Each fetch has a five-second context and an 8 MiB decoded
  response limit.
- `--usage-pricing-refresh-interval`, default `24h`; `0` pins the embedded floor.
  It is serve-only and uses the existing declare-once configuration machinery.

Register the value only when the Usage meter/reporting is enabled, and **before**
starting registry priming. A disabled meter registers nothing and makes no
models.dev request. The existing registry owns startup, periodic refresh,
observation, and cancellation; no second background lifecycle is introduced.
Neither a fetch failure nor stale prices gate readiness, native reports, or
inference. A failed fetch/acceptance keeps last-good, or the floor when cold.

### Accept contract

Accept additive fields without requiring fidelity to unrelated catalog metadata.
Validate the selected providers' identity and price projection, not every
reseller's schema. Reject duplicate JSON members, invalid identity encoding,
wrong price types, negative prices, and excessive numeric exponents/precision.
A provider entry or model without `cost` is not by itself malformed; an explicitly
zero rate is valid. Require each configured provider section to remain structurally
present so a truncated artifact cannot silently erase an entire pricing source.

Rates are nonnegative base-10 decimals. Proposed numeric bounds are at most 18
integer and 18 fractional digits after expansion; reject an unsupported rate
rather than rounding it or allowing an exponent to cause unbounded allocation.
Unknown price fields are ignored; malformed recognized fields reject the fetch.

Parse accepted bytes into one immutable derived projection per effective content
hash, not once per report. Seed the embedded-floor projection only when an
enabled pricing source is constructed; after a changed accepted value is
published, the first `Current` observation builds and atomically replaces the
one-entry derived projection. Cache hits still enforce each report's identity
budget, and a limit or cancellation failure never publishes a partial
projection. Build the report-local matching index once and memoize each distinct
Reported model's resolution within that report. Do not parse or search the full
artifact per Turn, or memoize only by model name across snapshot revisions. The
current artifact is about 4.6 MB; do not assume Codex's small parsed model count
applies. Include cold/version-change projection in the initiating work context
and check cancellation during projection/index construction.

A valid newer dataset can remove a model or its rate. Do not merge deleted
entries from an older dataset into the new one: that would create a mixed,
unidentified tariff. Affected history becomes unpriced until a later accepted
snapshot supplies it again. A restart may return to the floor until refresh.

## 5. Model matching

### Small interface

Illustrative shape, not a commitment to exported Go spelling:

```go
type Identity struct { Provider, Model string }

func New(candidates []Identity) *Matcher
func (m *Matcher) Resolve(reportedModel string) Resolution
```

`Resolution` is either a matched identity plus method, `unknown`, or `ambiguous`.
The matcher owns its indexes and all selection rules; it does no I/O, pricing,
count interpretation, mutation, or Catalog lookup. The implemented constructor
also receives the report's remaining identity-byte limit, checks bytes for only
keys it actually retains (including shorter dated stems and duplicate/collision
behavior), and exposes that retained charge to the reporter's request-wide
balance. The reporter does not duplicate or estimate the matcher's private index
layout. The matcher is immutable after construction and safe to share. Candidate
order must not affect results.

Returned identities retain their source spelling. Approximation changes only
lookup keys, never stored observations, report identity, or inference requests.

### Source selection

An unqualified Reported model is searched only within the original-provider
candidate set. An explicit recognized prefix such as `openai/` restricts the
search to that namespace. Do not strip arbitrary prefixes, infer the original
provider from the Surface, or use provider ordering to break ties.

### Ordered matching policy

1. **Exact:** literal model ID match. Exact matches win even when unpriced.
2. **Normalized:** ASCII case folding; for Claude IDs only, normalize dotted
   numeric-version spelling to its hyphenated spelling (for example,
   `claude-haiku-4.5` to `claude-haiku-4-5`). Do not trim whitespace, reorder words,
   strip arbitrary punctuation, or normalize OpenAI version numbers away.
3. **Explicit alias:** a small reviewed table for identities not explained by
   the rules. Each entry needs source/observational evidence and an acceptance
   case. No speculative aliases or operator override setting in v1; the initial
   table may be empty.
4. **Constrained suffix/prefix:** permit a terminal `-fast` on the Reported model
   to fall back to its base model, and permit a valid terminal model-date suffix
   (`-YYYYMMDD` or `-YYYY-MM-DD`) to fall back to an existing undated base. Remove
   at most one of each recognized suffix. Rank candidates by: fewest suffix
   removals, then literal stem match before normalized stem match, then longest
   retained normalized stem. Dropping one `-fast` or one entire date suffix each
   counts as one removal; normalization is a separate lookup tier, not a removal.
   After removal, retry the remaining literal spelling before its normalized key.
   An unmatched suffix must be entirely explained by these rules; an arbitrary
   prefix is not enough.
5. **Undated-to-dated:** when an undated ID has no earlier match, consider dated
   candidates whose full undated stem is equal after the preceding permitted
   transformations. Select only a unique identity. Multiple dated candidates are
   ambiguous; do not select newest, most expensive, or first in the dataset.

A supplied date may fall back to an existing undated base, but must not jump
sideways to a different dated candidate. Date suffixes must parse as actual
calendar dates. Recognize terminal `-fast` case-insensitively while preserving
what remains of the literal spelling. Strip it only from the Reported model,
never from candidate identities to turn a base-model query into a fast-model
selection.

At every stage, more than one equally ranked identity means `ambiguous`; do not
continue to a weaker stage to manufacture a unique result. A literal match at a
stronger tier wins without consulting normalized alternatives. When a normalized
tier is reached, collisions among distinct identities are ambiguity, not map
overwrite. Duplicate identical candidate pairs may be deduplicated. Do not
allocate an unbounded candidate list in the report.

**Never discard model variants or generations.** `mini`, `nano`, `pro`, `codex`,
and unrecognized suffixes remain significant. Numeric changes do not become
suffixes. No Levenshtein distance, arbitrary substring matching, or cross-family
similarity ranking.

### Acceptance examples

Each row specifies its entire relevant candidate set. Except the cited observed
identities, candidate combinations are synthetic policy tests, not claims about
the current models.dev catalog. They become matcher-interface tests before code
is wired into reporting.

| Reported model | Candidate model IDs (one provider unless stated) | Expected |
| --- | --- | --- |
| `gpt-5.6-sol` | `gpt-5.6-sol`, `gpt-5` | Exact Sol |
| `gpt-5.6-sol-fast` | `gpt-5.6-sol`, `gpt-5` | Sol by permitted suffix |
| `gpt-5.6-sol-fast` | `gpt-5.6-sol-fast`, `gpt-5.6-sol` | Exact fast identity wins |
| `gpt-5.6-sol-fast` | `gpt-5.6-sol`, `GPT-5.6-SOL` | Lowercase literal stem wins after one suffix removal |
| `Gpt-5.6-sol-fast` | `gpt-5.6-sol`, `GPT-5.6-SOL` | Ambiguous; no literal stem match, and normalized keys collide |
| `gpt-5.6-sol-mini` | `gpt-5.6-sol` | Unknown; do not discard `mini` |
| `gpt-5.6-sol-unknown` | `gpt-5.6-sol` | Unknown; arbitrary suffix is not permission |
| `gpt-5.6` | `gpt-5` | Unknown; do not discard version `.6` |
| `claude-sonnet-4.6` | `claude-sonnet-4` | Unknown; do not discard version `6` |
| `claude-haiku-4.5` | `claude-haiku-4-5-20251001` | Unique normalized dated match |
| `claude-haiku-4-5-20251001` | same ID, `claude-haiku-4-5` | Exact dated identity wins |
| `claude-haiku-4-5-20251001` | `claude-haiku-4-5` | Undated base by date suffix |
| `claude-haiku-4-5` | same stem dated `20251001` and `20251101` | Ambiguous, not newest |
| `claude-haiku-4-5-20251001` | only same stem dated `20251101` | Unknown; no sideways date substitution |
| `gpt-5.6-sol-2026-02-30` | `gpt-5.6-sol` | Unknown; invalid date |
| `GPT-5.6-SOL` | `gpt-5.6-sol` | Normalized match; display remains uppercase |
| ` gpt-5.6-sol` | `gpt-5.6-sol` | Unknown; do not trim identity |
| `openai/gpt-5.6-sol` | OpenAI and another allowed provider with that exact ID | Exact within OpenAI |
| `shared-model` | two allowed providers with that exact ID | Ambiguous |
| `shared-model` | one allowed provider plus a reseller with that ID | Allowed original-provider identity only |

The Sol fast/base relationship is recorded in the
[OpenAI usage captures](../research/2026-09-05-native-usage-shapes.md); Haiku's
Reported-model spelling is documented in the
[Anthropic model-ID observations](../research/2026-07-26-anthropic-model-id-aliases.md).
Matching names does not establish the execution tier or recover an actual bill.

## 6. Rate selection and native valuation

### One tariff and one selected vector per Turn

Use standard `cost` values from the chosen provider/model. Retain its base rate
vector and every valid structured `cost.tiers` context row, ordered by exact
numeric `tier.size`. For each Turn, use the base vector when complete native
input is below or equal to every threshold. Otherwise select the matching tier
with the greatest threshold for which `complete input > tier.size`. At equality,
that tier does not apply, though a lower threshold may still match.

Structured tiers are authoritative and selection is provider-independent. Current
models.dev data exercises this under selected OpenAI, Google, and xAI Pricing
identities; the direct Anthropic catalog's current absence of tiers is not a
permanent exemption. If no structured tier exists but deprecated
`context_over_200k` rates do, treat that row as a strict `complete input > 200000`
compatibility tier. Never prefer the generated legacy name over a structured
threshold: current OpenAI rows demonstrate that it can discard an exact 272,000
boundary. A selected row is authoritative, so an omitted rate stays absent; do
not fill it from the base or another tier.

Reject malformed or duplicate structured thresholds rather than depending on
input order, and validate recognized base, structured, and legacy rows even when
one is not selected. Experimental mode rates are not selected. The optional
`reasoning` rate does not create an extra charge: this feature values the complete
native output count once at the output rate.

Derive complete input from the persisted native Surface, independently of the
Pricing model's provider. OpenAI uses complete `input_tokens`. Anthropic uses a
checked sum of uncached `input_tokens`, `cache_creation_input_tokens`, and
`cache_read_input_tokens`; missing, negative, or overflowing context evidence
cannot select a tier and excludes the Turn. Output, reasoning/thinking, TTL
subdivisions, and reported total tokens never select context rates.

All source rates are USD per million tokens. Selection does not depend on the
Reported model's apparent context limit, a live Catalog, Requested-model suffix,
or metadata used for Codex aliases.

### Formulas

Let `pI`, `pO`, `pR`, and `pW` be the selected input, output, cache-read, and
cache-write rates respectively.

For OpenAI native counts:

```text
ordinary input = input_tokens - cached_tokens - cache_write_tokens
cost = (ordinary input × pI + output_tokens × pO
        + cached_tokens × pR + cache_write_tokens × pW) / 1,000,000
```

For Anthropic native counts:

```text
cost = (input_tokens × pI + output_tokens × pO
        + cache_read_input_tokens × pR
        + cache_creation_input_tokens × pW) / 1,000,000
```

Ignore Anthropic's TTL subdivisions for pricing, whether present, missing, or
inconsistent with one another; the aggregate creation count is the chosen input.
Do not add reasoning/thinking to output or use reported `total_tokens` as another
chargeable count. All native fields remain visible exactly as before.

Select and value individual Turns during the existing scan. This preserves
mixed tiers, missing-count correlation, and pricing coverage; aggregated
optional-field coverage cannot establish which Turns were fully priceable.
Calculate one contribution per Turn and reuse it at period/model, whole-range
model, and whole-section aggregation levels.

### Missing information and coverage

**Proposed conservative rule: include only fully priceable Turns in the monetary
subtotal.** Do not fabricate missing counters, fill unknown prices with zero,
or estimate only some chargeable parts of an otherwise unpriceable Turn.

A Turn is priceable when:

- its model resolves uniquely and has the selected input/output rates;
- the cache counts required by its native formula are present; missing TTL,
  reasoning, thinking, and reported-total details do not matter;
- every positive chargeable cache count has a known rate; a known zero count
  contributes zero without requiring that optional rate;
- OpenAI cache subsets do not exceed complete input in their sum. Do not clamp a
  negative derived ordinary-input count.

Unpriceable Turns still contribute to all existing native metrics and Turn
counts. Distinguish `unknown_model`, `ambiguous_model`, `missing_rate`,
`missing_usage`, and `inconsistent_usage` in monetary coverage. Assign one reason
per Turn to keep reasons additive. Model resolution remains first, and a matched
model with no tariff is `missing_rate`. When a context tier must be selected,
missing or inconsistent complete-input evidence produces `missing_usage` or
`inconsistent_usage` before selected-row rate checks, because no authoritative
row is known. After selection, preserve the existing `missing_rate`,
`missing_usage`, then `inconsistent_usage` formula precedence. Within
`missing_rate`, check optional rates only for known positive counts; an absent
cache count is `missing_usage`, not evidence of a missing rate requirement.

This is stricter about **missing evidence** than about the agreed **tariff
approximations**. One cache-write rate and data-driven context-tier selection do
not themselves make a Turn unpriceable.

## 7. Amounts and report interface

### Exactness

Parse rates as exact decimals; calculate and accumulate without `float64` and
without per-Turn rounding. Standard-library scaled big integers or rationals are
sufficient; a new arithmetic dependency is not a design requirement. With the
rate bounds above and division by a million, amounts need at most 24 fractional
digits. Canonical JSON amounts are nonnegative decimal strings with no exponent,
leading zeros, or insignificant trailing fractional zeros; exact zero is `"0"`.
Bound amount strings to 128 bytes before parsing, and fail arithmetic exceeding
the supported representation rather than saturating or rounding. Existing
checked int64 Turn/count limits remain unchanged.

The CLI may parse/add these **amounts** exactly. It must not receive a token-price
calculation job disguised as rendering. Currency is always USD in this version.

### Additive JSON extension

Keep `/usage/v1/report` and `schema_version: 1`; preserve every existing field.
#236 exposed typed pricing provenance, cost coverage, and model resolution through
`Reporter.Query` while keeping the fields `json:"-"`. #237 now publishes the
complete extension atomically through explicit bounded HTTP adapter mappings;
direct domain-struct marshaling remains intentionally native-only. The mapping
adds a top-level `pricing` object identifying the benchmark and captured snapshot:

```json
{
  "pricing": {
    "dataset": "models.dev/api.json",
    "currency": "USD",
    "basis": "original_provider",
    "cache_write_policy": "single_rate",
    "version": "sha256:<full content digest>",
    "source": "fallback",
    "last_success": null
  }
}
```

#241 removes the presentation-only `pricing.context_policy` member without a
replacement. It never configured or drove valuation; report documentation and
terminal caveats describe the calculation. A new client treats that member from
an older daemon as unknown additive data and preserves it in original-byte JSON.

`last_success` is the cached value's successful content-fetch time, not a tariff
effective date, report watermark, or proof every entry is up to date. Full
refresh-attempt status remains available in the registry's `/readyz` observation.

Every existing `Total` (period/model row, range/model entry, and section total)
gains `cost`:

```json
{
  "cost": {
    "amount": "0.003157",
    "priced_turns": "1",
    "unpriced": {
      "unknown_model": "0",
      "ambiguous_model": "0",
      "missing_rate": "0",
      "missing_usage": "1",
      "inconsistent_usage": "0"
    }
  }
}
```

This fragment describes a group of two Turns: one fully valued, one excluded.
The amount is a **priced-Turn subtotal**, not a complete amount for both Turns.
All coverage counts are decimal int64 strings. Require:

```text
priced_turns + sum(unpriced reason counts) = turns
```

All-unpriceable nonempty groups have `amount: null`, not zero. A group with
priceable zero-cost Turns has `amount: "0"` and positive `priced_turns`. An empty
section has `amount: "0"`, zero priced Turns, and zero reason counts, accompanied
by the existing empty-history message. No invented model rows are added.

Rows and range/model entries additionally expose their model resolution:

```json
{
  "pricing_match": {
    "status": "matched",
    "provider": "openai",
    "model": "gpt-5.6-sol",
    "method": "suffix"
  }
}
```

Unresolved forms contain only `status: "unknown"` or `status: "ambiguous"`.
Matched methods are `exact`, `normalized`, `alias`, `suffix`, and `dated`;
combined normalization/suffix work is labeled by the stage that resolved it.
No unbounded candidate list is returned. Matched identity does not by itself
promise usable rates or complete cost coverage. Section totals have no single
model resolution.

### Version skew and validation

- An old CLI already ignores additive fields and preserves them in original-byte
  `--json`; its existing text output remains usable.
- A new CLI accepts an old daemon's response when **all** pricing fields are
  absent and renders `Estimated cost unavailable (daemon does not provide prices)`.
  Do not reinterpret absence as zero or silently calculate locally.
- When top-level `pricing` is present, require and validate the full extension on
  all selected aggregate levels. Partial extension presence is a protocol error.
- Validate decimal bounds/canonical form, the known cache-write policy/statuses, identity shape,
  amount/coverage relationships, and checked reason-count sums. Reuse existing
  duplicate-member/Unicode checks and bounded fragment encoding. Do not reconstruct
  pricing, matching, or cross-level server aggregates in the client.
- `--json` still emits the validated original bytes; text-only subtotals never
  modify them. New fields count against the existing 8 MiB response cap.

## 8. Terminal presentation

Add **Est. USD** immediately after **Model(s)** and before **Turns** in each
Surface's primary table, including its existing period `Total` row:

```text
Day | Model(s) | Est. USD | Turns | ...native token columns...
```

The first column still uses the selected period's label. Keep the current
grouping/layout and separate native Surfaces. Secondary native tables do not
repeat the monetary column. This feature does not add a cross-Surface token
total or a new grand-total layout.

The CLI accumulates unrounded amounts, priced-Turn counts, and reason counts from
validated model rows for the period total. It neither chooses rates nor resolves
models. Preserve checked arithmetic and complete-before-stdout behavior.

**Agreed display precision:** exactly three decimal places in USD, rounded half
up for nonnegative values. For example, `0.003157` renders as `0.003`, `0.0035`
as `0.004`, and `0.0004` as `0.000`. Both exact zero and sufficiently small
positive amounts render as `0.000`; JSON retains full exactness. Totals sum
exact amounts before display rounding, so they need not equal the sum of already
rounded cells.

- `—`: no priceable Turns in a nonempty group.
- `*`: a monetary subtotal with incomplete pricing coverage.
- A compact note identifies the period/model or `Total`, priced/total stored
  Turns, and nonzero exclusion reasons. Entirely unpriced groups also receive a
  note; unlike optional native metrics, zero monetary coverage needs explanation.
- A short header identifies original-provider/per-Turn context-tier pricing and
  whether the effective snapshot is fetched or fallback. Include the successful fetch
  time when available; do not call it the price's effective date.
- `--details` also lists distinct Reported-model → Pricing-model resolutions and
  methods; unknown/ambiguous matches are explicit. Escape these identities using
  the existing terminal-safe policy.

Always state that this is estimated original-provider cost for persisted,
best-effort observations; uses current accepted per-Turn context-tier selection
and a single cache-write rate; is not a Copilot bill; and may exclude unpriceable Turns. This should be
short explanatory text, not a new interactive presentation.

## 9. Failure and resource behavior

Price-source failures cannot interrupt inference, turn off readiness, or request
a writer flush. A report uses one captured immutable pricing projection and one
database read snapshot. Report-local matching/memo state and the database
snapshot are not retained while writing the HTTP response; the pricing source
may retain its current content-hash-keyed parsed projection as derived cache
state. Unpriced Turns are successful report data, not HTTP failures.

Malformed fetched data never replaces accepted bytes. An unexpected failure to
interpret the validated effective snapshot is an internal report failure, not
permission to serve invented zeros. Reuse report-unavailable/timeout errors with
safe public messages; no source bodies, credentials, or local paths are exposed.
Exact monetary overflow follows the report's existing whole-request overflow
policy. Source/index work and larger result fields must remain within existing
query, identity-retention, admission, and encoding limits. Apply the selected
source-identity budget during the initial strict JSON member walk, before a
complete selected-key map is allocated; the separate 8 MiB raw-artifact cap still
governs accepted bytes and this retained-identity limit is not a claim about all
transient allocations. Limit projected source provider/model identifiers to 1
KiB each and reject larger selected identities at acceptance, before they can
enlarge report fragments.

## 10. Acceptance and implementation order after approval

The **interface is the test surface**. Fixtures must say whether their identity
relationships are observed or synthetic; no live models.dev or paid inference
calls are required in normal tests.

1. **Matcher:** implement the acceptance table, ordering independence, scoped
   provider matching, normalization collisions, invalid dates, meaningful variant
   preservation, multiple suffix ordering, concurrent reads, and unpriced exact
   candidates. Document evidence for every explicit alias.
2. **Pricing data/module:** verify embedded identity/license, base/structured-
   context/legacy selection, strict thresholds, exact decimals, known-zero versus absent values,
   native formulas, TTL-independence, output-subset non-duplication, invalid native
   relationships, and numeric bounds. Exercise fetch size/deadline/redirect limits,
   malformed/duplicate data, last-good, floor, refresh disabling, and no credentials
   with a local HTTP server. Reuse existing cache-engine tests rather than cloning
   its concurrency test suite.
3. **Reporter and HTTP:** use literal expected reports from real temporary
   databases. Cover both Surfaces, every aggregate level, distinct Requested and
   Reported models, mixed priced/unpriced Turns, all reason categories, empty/free
   histories, and model filters. Change prices and matches between queries without
   changing the database; swap prices during a query and prove one snapshot is
   used throughout. Native metrics must remain identical. Test bounded encoding,
   canceled projection/scans, schema compatibility, and partial wire extensions.
4. **CLI and lifecycle:** cover the cost column immediately after `Model(s)` and
   before `Turns`, exact period addition and coverage, three-decimal rounding
   (including halfway cases and positive amounts rounded to zero), model-resolution
   details, malicious identities,
   original-byte JSON, older daemon behavior, enabled-empty and disabled-meter
   outcomes, no report-triggered networking, and production registration before
   priming. End-to-end tests inject a local price source or pin refresh to zero;
   enabling metering in a test must not accidentally contact the internet.
5. **Documentation and verification:** update README, CONFIGURATION, help/config
   assertions, domain definitions, and affected ADR/design scope notes together.
   Record the deliberate current-price/per-Turn-context approximation so it is
   not later "fixed" into historical billing. Run focused tests and the
   existing full verification through Nix (`nix develop -c go test ...`, race tests,
   `nix fmt`, `nix flake check`) only once implementation is authorized.

No implementation task starts merely because this sequence exists.

## 11. Review points before implementation

The original-provider benchmark, no persisted pricing, per-Turn context rates,
aggregate cache-write calculation, server/CLI split, dedicated constrained
matcher, cost-column position, and three-decimal USD display are settled
direction. Please review these **proposed defaults**, which make that direction
executable:

1. Initially support the four explicit original-provider namespaces above and
   use standard rates rather than experimental mode prices.
2. Adopt the exact matching table, including date handling and the treatment of
   ambiguous names as unpriced rather than selecting a newest/most-expensive
   candidate.
3. Include only fully priceable Turns in cost subtotals, with explicit reason
   coverage; do not treat absent cache counts as zero.
4. Use a 24-hour memory-only refresh with an off switch, additive v1 JSON fields,
   and matching explanations under `--details`.

These are proposals for this review, not hidden approvals inferred from the
instruction to write a design.
