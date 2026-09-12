# OpenAI reported service-tier valuation — Design

**Status:** complete design proposal awaiting full-design approval. Valuation direction is agreed; implementation and feature verification have not started.
**Issue:** [#248](https://github.com/ningw42/copilotd/issues/248), part of [#242](https://github.com/ningw42/copilotd/issues/242).
**Depends on:** implemented [#247 capture design](2026-09-12-openai-service-tier-capture-design.md).
**Extends:** [Estimated-cost reporting](2026-09-11-usage-cost-reporting-design.md).

## Agreed direction

The maintainer selected **“Where data suffices”** for Fast/context derivation:

- Use each Turn's complete native input to select its context band, as existing valuation does.
- Use the completed response's recorded service-tier evidence, never request intent, Requested model, Reported-model `-fast`, Catalog metadata, or daemon configuration, to select service mode.
- Derive Fast/context prices from the same Pricing model's explicit models.dev base, Fast, and selected context rates whenever the necessary data permits it. Do not require a maintained model/context allowlist.
- Accept that applying this formula to combinations without published OpenAI Fast/context prices is a **deliberate estimation policy**, not a claim that those combinations have confirmed provider prices or are offered by OpenAI.
- Do not add service-tier provenance to valuation results, reports, or CLI output. The implementation and accepted models.dev data define how rates are selected and calculated; no additional classification, counters, or policy marker is needed.
- Isolate the Fast/context composition rule in one centralized place so future changes do not spread through reporting callers.

There are two pricing dimensions, not an additional charge or a provenance taxonomy:

| Context band | Normal pricing | Fast pricing |
| --- | --- | --- |
| Base band | Explicit normal base rates | Explicit Fast base rates |
| Selected context tier | Explicit normal context rates | Fast base × normal context / normal base, per rate category |

Unavailable or unrecognized response-tier evidence selects normal pricing in whichever context band applies. This fallback is a calculation rule, not a new report category.

The source remains the single current accepted models.dev snapshot; this direction does not add a second maintained dollar-price table, report-time pricing network work, or stored prices/costs. Existing pricing-source/version fields and priced/unpriced coverage remain unchanged.

### Explicit amendment to the originating issue

The current #248 acceptance criteria prohibit deriving a multiplier and require explicit mode/context rates. The agreed direction above **replaces that prohibition** with a governed same-model/same-snapshot derivation. The maintainer has also explicitly dropped the requested service-tier provenance/coverage extension and related terminal details. No new HTTP fields or client-validation extension are required. These are scope amendments, not claims that the original requirements are satisfied unchanged.

The GitHub issue has not yet been amended. Align it and the affected current design/ADR documentation when this focused design is approved. Approval of the agreed direction is not blanket approval of the concrete implementation contracts below.

## Evidence and limits

### Provider and source contracts

OpenAI's [Responses reference](https://developers.openai.com/api/reference/resources/responses/methods/create) defines response `service_tier` as the processing tier actually used, which may differ from the request. The [Fast mode guide](https://developers.openai.com/api/docs/guides/fast-mode) documents `priority` and `fast` as equivalent requests on supported models, with responses returning `priority` for GPT-5.6 and earlier. Its ramp-rate downgrade example returns `default` and uses Standard pricing. Do not assume an unverified response spelling for newer models or infer execution from request intent.

The models.dev [mode schema](https://github.com/anomalyco/models.dev/blob/b8244af8bd1ca8c0e60103f058854d5510a2534d/packages/core/src/schema.ts#L274-L288) stores an optional cost object and provider request metadata under `experimental.modes.*`. The inspected Fast entries use `experimental.modes.fast.cost` and map `experimental.modes.fast.provider.body.service_tier` to `priority`. The [cost shapes at that same audited commit](https://github.com/anomalyco/models.dev/blob/b8244af8bd1ca8c0e60103f058854d5510a2534d/packages/core/src/schema.ts#L72-L111) distinguish basic mode costs from the broader model cost shape with structured context tiers. None of the inspected Fast cost objects contains its own context tiers. This schema audit identifies the authored contract, not the source commit of the separately fetched live artifact.

The [OpenAI pricing page](https://developers.openai.com/api/docs/pricing?latest-pricing=fast) identifies short context as at most 272K input tokens and long context as more than 272K for the priced splits below. Complete input includes cached and cache-write subsets; it is not the uncached remainder and does not include output. The [prompt-caching guide](https://developers.openai.com/api/docs/guides/prompt-caching) documents the corresponding ordinary-input calculation as `input_tokens - cached_tokens - cache_write_tokens`. Context selection remains data-driven from the accepted model's exact `tier.size`, not a hardcoded 272K condition. In the inspected data, the generated `context_over_200k` copy must not replace the authoritative structured 272,000 threshold.

### Inspected artifact identities

The comparison was performed on **2026-09-12**, using exact decimal decoding and rational multiplication/division without intermediate rounding. Official Standard/Fast rows were extracted independently and checked against the literal published cells before comparison.

| Source | Bytes | SHA-256 |
| --- | ---: | --- |
| Vendored `internal/usage/pricing/modelsdevdata/api.json`, originally fetched `2026-09-11T11:57:10Z` | 4,584,629 | `db685655368231dce789b060e493a21899cb861c9930cc52521795776ab6e3b5` |
| Live [models.dev artifact](https://models.dev/api.json), fetched 2026-09-12 | 4,621,537 | `07617fe08e8038b5fa2d5018dfe896a7478d6f48f2e78af2bc308d32914c5560` |
| Official [complete pricing matrix](https://developers.openai.com/api/docs/pricing.md), retrieved `2026-09-12T13:40:01Z` | 23,005 | `63878741d55c9dc3c928cd88ac8f55399198daedfb49456f816d7d2cbab96768` |

These timestamps identify observations, not price-effective dates. The two models.dev artifacts differ globally but contain identical relevant OpenAI Fast identities, wire mappings, base vectors, and context vectors. The comparison uses only the eight original-provider `openai` entries, not similarly named reseller or Copilot entries.

### Rate vectors sufficient to reproduce the comparison

All values are **USD per million tokens**. Vector order is **input / cache read / cache write / output**. `—` means absent evidence, never zero. These are dated verification examples, not a runtime price table, model allowlist, or promise about future refreshes.

| Pricing model | Standard base vector | Explicit Fast base vector | Observed factor for each populated category |
| --- | --- | --- | --- |
| `gpt-5.4` | 2.5 / 0.25 / — / 15 | 5 / 0.5 / — / 30 | 2 |
| `gpt-5.4-mini` | 0.75 / 0.075 / — / 4.5 | 1.5 / 0.15 / — / 9 | 2 |
| `gpt-5.5` | 5 / 0.5 / — / 30 | 12.5 / 1.25 / — / 75 | 2.5 |
| `gpt-5.6` | 4 / 0.4 / 5 / 20 | 8 / 0.8 / 10 / 40 | 2 |
| `gpt-5.6-luna` | 0.2 / 0.02 / 0.25 / 1.2 | 0.4 / 0.04 / 0.5 / 2.4 | 2 |
| `gpt-5.6-sol` | 4 / 0.4 / 5 / 20 | 8 / 0.8 / 10 / 40 | 2 |
| `gpt-5.6-terra` | 2 / 0.2 / 2.5 / 12 | 4 / 0.4 / 5 / 24 | 2 |
| `gpt-6-astra` | 10 / 1 / 12.5 / 50 | 20 / 2 / 25 / 100 | 2 |

All 29 populated base/Fast category pairs have nonzero denominators and agree within their model. The three absent cache-write pairs remain absent; agreement on other categories does not supply a missing rate. GPT-5.5 is an explicit counterexample to a universal 2x factor.

Seven entries have a structured Standard tier at complete input strictly above 272,000. Applying `Fast base × Standard context / Standard base` independently to each populated category gives:

| Pricing model | Standard >272K vector | Derived Fast >272K vector | Official Fast-long comparison |
| --- | --- | --- | --- |
| `gpt-5.4` | 5 / 0.5 / — / 22.5 | 10 / 1 / — / 45 | No published comparator |
| `gpt-5.5` | 10 / 1 / — / 45 | 25 / 2.5 / — / 112.5 | No published comparator |
| `gpt-5.6` | 8 / 0.8 / 10 / 30 | 16 / 1.6 / 20 / 60 | All four match through documented Sol alias |
| `gpt-5.6-luna` | 0.4 / 0.04 / 0.5 / 1.8 | 0.8 / 0.08 / 1 / 3.6 | All four match exactly |
| `gpt-5.6-sol` | 8 / 0.8 / 10 / 30 | 16 / 1.6 / 20 / 60 | All four match exactly |
| `gpt-5.6-terra` | 4 / 0.4 / 5 / 18 | 8 / 0.8 / 10 / 36 | All four match exactly |
| `gpt-6-astra` | 20 / 2 / 25 / 75 | 40 / 4 / 50 / 150 | All four match exactly |

For example, Sol long input is `8 × 8 / 4 = 16`; long output is `40 × 30 / 20 = 60`. Both match the independently published Fast-long cells. GPT-5.4-mini has no structured context tier to combine; no new threshold or surcharge is inferred for it.

Bare `gpt-5.6` has no separate pricing-table row. The [official Sol model page](https://developers.openai.com/api/docs/models/gpt-5.6-sol.md) explicitly states, “The `gpt-5.6` alias routes requests to GPT-5.6 Sol.” That page was retrieved through the bare model URL at `2026-09-12T13:40:09Z`, SHA-256 `23f4ff2c236486d42145e21693f450bfcac272c37ee839a5ee6f9455cd1d8b7b`. The comparison follows this documented identity relationship, not an inference from similar names or equal rates; it does not change copilotd's model matcher.

The result per snapshot is **16 directly published matching Fast-long cells, four additional matches through the alias, six cells without a comparator, and zero numerical mismatches**. The alias does not create four independent published cells, and the identical live/vendored results are not independent confirmations. The base, explicit Fast, and Standard-context operands also match the corresponding published values after resolving the documented alias.

### What the evidence does not establish

The [GPT-5.4 context guide](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.4#1m-context-window) says Fast **requests** with prompts above 272K are automatically processed at Standard rates. Its documented Standard-long vector is therefore `5 / 0.5 / — / 22.5`, not the hypothetical derived Fast vector `10 / 1 / — / 45`. This request-handling statement does not specify the returned tier spelling and is not permission to rewrite recorded response evidence. Under the agreed valuation policy, a returned `default` selects base/context pricing; a returned recognized Fast value selects the derived-estimate path where the operands suffice.

GPT-5.5's Fast-long cells are also unpublished. Its [model page](https://developers.openai.com/api/docs/models/gpt-5.5.md) names Standard, Batch, and Flex for long-context pricing; it does not independently establish GPT-5.5-specific automatic downgrade behavior. The derived `25 / 2.5 / — / 112.5` vector is consequently an agreed estimate, not verified OpenAI pricing. Neither missing comparator is counted as a successful numerical verification or as proof that the combination is offered.

The newer model pages describe applying the selected context rates to the full request. Older GPT-5.4/GPT-5.5 pages use “full session” for Standard/Batch/Flex without precisely defining cross-request persistence. This design retains the existing per-Turn benchmark; it does not introduce session accumulation or claim exact invoice reconstruction from that wording.

A derivable factor also depends on dataset coverage: 13 of the official matrix's 20 explicit Fast model rows have no Fast mode cost in either inspected models.dev OpenAI projection. A published OpenAI price outside the accepted projection is not permission to borrow another model's factor. [Codex credit multipliers](https://developers.openai.com/codex/speed), advertised speed improvements, regional uplifts, and fixed tool fees are not inputs to this token-rate derivation.

These observations support the formula for the inspected combinations, not a guarantee that every future model or refreshed dataset has separable service-mode/context pricing. The agreed general rule remains an estimation policy, documented here and isolated in one implementation location; it does not add runtime provenance output.

## Inherited evidence and reporting invariants

Reuse the capture contract without another migration or observer change. The stored nullable `openai_turn.service_tier` is separate from the frozen numeric OpenAI metric projection. Preserve raw stored spelling; any normalization is lookup-only.

Keep:

- the completed response as the authority, including upstream downgrades;
- normal base/context estimates for unavailable, explicitly empty, or unrecognized tier evidence, without treating that calculation as proof of Standard execution;
- distinct model matching and mode selection; the Reported model still determines grouping and Pricing-model resolution, not execution tier;
- one immutable report-local pricing snapshot and one database read snapshot;
- native counts, exact money arithmetic, checked coverage sums, cancellation, and existing work/response limits;
- per-Turn valuation before period/model/range aggregation, including mixed-tier Turns in one model row;
- estimated original-provider valuation, not a Copilot bill or exact OpenAI invoice.

## Accepted projection and mode lookup

### Scope and source admission

This version supports **OpenAI Fast only**, under the existing original-provider `openai` namespace. It does not automatically support Flex, Scale, Ultrafast, Batch, Anthropic speed modes, or reseller mode prices. A later mode needs its own explicit policy rather than inheriting the Fast composition rule. Model matching remains independent of Surface; only OpenAI-native Turns consume OpenAI service-tier evidence.

Extend `ParseSnapshot` rather than introduce another source parser or cache. Keep the existing whole-document duplicate-member/Unicode validation, provider/model identity checks, selected-provider allowlist, decimal validation, and base/context projection. For OpenAI models:

1. Optional `experimental` and `experimental.modes` must be objects when present; explicit null or another type rejects the snapshot. Every mode entry must be an object with a nonempty valid UTF-8 name of at most 1 KiB.
2. Optional mode `provider` and `provider.body` must be objects when present. A present `provider.body.service_tier` must be a nonempty valid UTF-8 string of at most 1 KiB; null, empty, and wrong types reject. Unrelated metadata fields remain ignored.
3. Recognize a Fast candidate when its mode name, or its explicit wire tier, equals `fast` or `priority` after **ASCII-only case folding**. A named `fast`/`priority` mode may omit provider metadata: its name supplies the supported mode identity and both known response aliases remain accepted.
4. A named Fast candidate with an explicit wire value other than `fast`/`priority` is contradictory and rejects the snapshot. A mode named `default`, `auto`, `flex`, `scale`, or `ultrafast` claiming a Fast wire value also rejects rather than overriding those names' meanings. A differently named entry such as `accelerated` with wire `priority` may supply Fast prices, but its name does not become an additional response-tier alias.
5. At most one Fast candidate is allowed per Pricing model. Two entries claiming either Fast alias reject even if their costs are identical. This includes case-colliding names and different names mapping to the same wire tier. Never choose by object order or overwrite a map entry.
6. A present mode `cost` must be an object. Validate the existing recognized numeric rate fields in every OpenAI mode cost (`input`, `output`, `cache_read`, `cache_write`, `reasoning`, `input_audio`, `output_audio`) using the existing exact source-rate bounds; unknown fields are ignored. Retain chargeable rates only for the supported Fast candidate. A missing Fast `cost`, an empty object, or missing individual rates is valid but does not supply a price.
7. The supported Fast cost shape is currently flat. Treat `tiers` or `context_over_200k` inside a Fast cost as a recognized **unsupported shape** and reject it, even if well formed. Do not silently ignore future explicit mode/context prices and continue estimating over them. Supporting that future shape requires a deliberate projection/selection amendment; last-good remains available meanwhile. Other modes' unrelated fields remain outside this pricing projection.

All recognized structural/type errors and collisions reject a fetched snapshot and retain last-good. They are not report-level `missing_rate` outcomes. Conversely, missing source rates and non-derivable rates in an otherwise valid snapshot do not reject the entire dataset.

Parse Fast candidates even when the model has no top-level `cost`; the current early `continue` on absent base cost must not discard independently usable Fast prices. Keep every model identity in the matcher whether or not any prices are present.

### Immutable retained shape and resource accounting

Extend the existing private `Tariff` implementation with an optional Fast declaration containing its explicit vector and derived context vectors. Absence of the declaration is different from a declaration with no usable rates. Source mode names and wire mappings are consumed during admission to identify and validate that declaration; they do not become extra origin metadata on the retained pricing value.

Retain only the Fast declaration and rates needed for calculation, not arbitrary mode-name strings, provider metadata, or raw mode objects. The supported response alias set is always `{fast, priority}`; aliases are activated only when this Pricing model has an accepted Fast declaration. Provider/model identity retention and `Snapshot.IdentityBytes()` therefore keep their existing meaning and request-wide accounting. The original raw dataset remains identified by its existing content hash. Stored Turn evidence is never normalized in place.

At most one Fast declaration is retained per model, with at most one derived vector for each already-retained Standard context tier. Projection growth is linear, not a modes-by-tiers cross-product. Keep the 8 MiB artifact cap and existing rate bounds. Check the projection context during mode traversal and each derived-tier construction; a canceled or over-budget build publishes no partial projection. Diagnostics must not retain remote mode bodies or unbounded member names.

`Source.Current` still captures once per report and reuses the content-version-keyed immutable projection. Derived-rate construction belongs to that projection, including enabled floor construction and context-aware cache misses; it does not add per-Turn parsing, divisions, network calls, or a second cache lifecycle. A report-local budget failure does not reject accepted cached bytes. Model/mode deletions in a new accepted snapshot are authoritative; never merge old mode prices into the replacement.

### Response-tier classification

Normalize only for lookup: ASCII letters are case-insensitive; no whitespace trimming, Unicode folding, punctuation changes, or suffix interpretation. The result for a matched Pricing model is:

| Recorded evidence | Accepted Fast declaration | Selection |
| --- | --- | --- |
| `default`, in any ASCII case | Either | Normal base/context rates |
| `fast` or `priority`, in any ASCII case | Present | Fast rates, even if required rates are missing |
| `fast` or `priority` | Absent | Base/context fallback |
| SQL NULL, empty, or any other string | Either | Base/context fallback |

Thus `FAST` is recognized, but ` fast`, `priority `, `fast\u0000`, a Unicode lookalike, and a source-only mode name such as `accelerated` are not. Flex/Scale/Ultrafast remain fallback estimates in this version; fallback is not proof of Standard execution. A recognized declaration with missing prices instead produces `missing_rate` when a required rate is unavailable. This distinction also governs refreshes that remove a mode versus retain the mode and remove its cost.

## Pricing-module interface and locality

Keep mode interpretation and rate-vector construction inside `internal/usage/pricing`. The reporter supplies tier lookup evidence and native counts; HTTP and CLI adapters only transport and present supplied results. Do not create a second pricing module, a new provider-plugin interface, or an exported ratio operation.

Extend the existing `Tariff`, rather than add a pass-through model-pricing wrapper. `Tariff` is the existing Go type holding rates; it does not represent an extra charge.

```go
func (t Tariff) CalculateOpenAI(
    native usage.OpenAIUsage, reportedTier *string,
) (Contribution, error)

// Unchanged parameters; always uses normal base/context rates.
func (t Tariff) CalculateAnthropic(native usage.AnthropicUsage) (Contribution, error)

// Unchanged result: exact amount or an existing exclusion reason only.
type Contribution struct {
    Amount Amount
    Reason ExclusionReason
}
```

`Tariff.CalculateOpenAI` replaces its existing one-argument form; update its internal-repository callers rather than retain two competing mode-selection entry points. `Tariff.Rates(input)` remains a detached normal/base-rate accessor, and the lower-level `CalculateOpenAI(native, Rates)` and `CalculateAnthropic(native, Rates)` functions remain explicit-vector calculators. Rate selection and derivation stay private to pricing. `Contribution` remains unchanged: callers receive the amount or exclusion reason, without any pricing-origin metadata.

`Snapshot.Tariff(identity)` returns an immutable value when a base cost or Fast declaration exists; its boolean is false only when neither is projected. Zero `Tariff` still supports the existing matched-but-unpriced path. A Fast declaration without base cost can price a base-band Fast Turn from explicit rates; a default/fallback Turn without base rates remains unpriced. No base context bands can be invented when none were supplied.

The composition rule lives in **one private function** in `internal/usage/pricing/mode_context.go`:

```go
func deriveFastContextRates(base, fast, selectedContext Rates) Rates
```

It returns a possibly incomplete immutable vector. Supporting helpers for exact arithmetic stay in the same file; callers receive neither a factor nor an instruction to finish the calculation. Build each derived vector through this function exactly once per projection construction. A future change in how Fast/context prices compose has locality here, behind the same valuation interface.

The computation is an in-process dependency and needs no adapter. Reuse the existing pricing-source seam for literal test snapshots, the local HTTP adapter fixture for refreshes, and real temporary SQLite databases for report integration. Tests cross those existing interfaces, not a new mock-only seam.

Rejected alternatives: calculating ratios in reporting/CLI spreads pricing knowledge; a separate exported Fast pricer adds an unnecessary interface; a model allowlist conflicts with the agreed data-sufficient estimate; one scalar reused across categories assumes future ratios remain equal. None provides more leverage than extending the existing pricing module.

### Exact calculation and failure contract

For each native chargeable category `r` independently:

```text
derived_fast_context[r]
    = explicit_fast_base[r] × selected_standard_context[r] / standard_base[r]
```

- If context selection uses the base band, use the explicit Fast vector directly. Do not divide merely to reproduce rates already supplied by the source.
- For a selected context tier, use only that tier's rate, the same model's base rate, and the same model's explicit Fast rate. Never use another model/mode/category to fill an absent operand.
- Calculate categories independently. Current ratios agree, but the representation need not assume future input/output/cache premiums stay equal.
- Evaluate the final expression exactly, without rounding an intermediate factor. A non-terminating intermediate ratio can still yield an exact terminating final rate.
- A missing operand or zero base denominator makes that derived category unavailable. An absent category is not an invented zero, and zero-valued valid source rates do not make an entire fetched snapshot malformed.
- Reduce the final rational exactly. A finite decimal is representable only when its reduced denominator contains no prime factors other than 2 and 5, and its canonical expansion fits the existing `Rate` bounds: at most 18 integer and 18 fractional digits. Otherwise that derived rate is unavailable rather than rounded. Do not round or bound the intermediate ratio as though it were a source `Rate`.
- With a nonzero denominator and all operands present, a zero Fast numerator or zero context rate produces an explicit zero rate. Missing operands and `0/0` remain unavailable even if another operand is zero. Each operation uses newly owned big integers; it must not mutate source rates shared across reports.
- The existing Turn formula requires input/output rates even for known-zero counts, and optional cache rates only for known-positive chargeable cache counts. Unavailable required rates produce `missing_rate`; an unavailable optional rate does not exclude a known-zero cache count.
- Existing monetary aggregation overflow remains a report error; this proposal does not convert arbitrary arithmetic failures into missing-price coverage.
- Build reusable immutable derived vectors with the pricing projection, not by dividing per scanned Turn. Per-Turn context/mode selection chooses from that captured projection.

For OpenAI, preserve the existing formula and exclusion precedence after mode/context selection: missing required rate, missing required usage, then inconsistent usage. Model resolution (`unknown_model` or `ambiguous_model`) remains earlier in the reporter. Persisted negative or missing required numeric counts still fail the existing stored-data validation rather than being silently repaired. For direct pricing calls, a negative complete input with retained context rows returns `inconsistent_usage` before attempting row selection; with no context rows, retain the explicit-vector calculator's existing precedence. This applies equally to Standard and derived Fast context rows. Anthropic's complete-input checks and its provable-missing-rate precedence are unchanged.

The source parser still rejects malformed recognized source fields; it does not reject an otherwise valid dataset because one derived category is unavailable. The helper's arithmetic is bounded by three already-bounded source decimals. Unexpected internal failures and cancellation must propagate as errors, not be hidden as absent rates.

### Illustrative outcomes under the agreed scope

| Recorded tier | Selected context band | Valuation |
| --- | --- | --- |
| `default` | Base or long | Selected base/context vector |
| Unavailable, empty, or unrecognized | Base or long | Selected normal base/context vector |
| Recognized Fast (`fast` / `priority`) | Base | Explicit Fast vector |
| Recognized Fast | Long, required operands available | Derived Fast/context vector |
| Recognized Fast | Long, required derived rate unavailable | `missing_rate`; no base-rate borrowing |

## Reading Turns and aggregating once

In `internal/usage/report/read.go`, add OpenAI service-tier metadata separately from `OpenAIMetrics()`. Append its scan destination after the unchanged numeric destinations; Anthropic SELECT/scan shape remains unchanged. Continue the exact schema-version check and both tables' column-presence probes. There is no new SQLite migration or reader-side schema repair.

Only `default`, `fast`, and `priority` can be recognized by this policy. The pricing module exposes `MaxServiceTierLookupBytes = 8`, the maximum byte length of that fixed alias set, as a lookup contract. Bound metadata transfer in SQL rather than allocating an arbitrarily large unknown string:

```sql
CASE WHEN typeof(service_tier) = 'text'
          AND octet_length(service_tier) <= ?
     THEN service_tier ELSE NULL END
```

Bind the pricing-owned limit; never interpolate evidence into SQL. This expression produces a **lookup candidate**, not a replacement observation: longer strings cannot match any supported alias and are intentionally omitted from lookup. They follow the same base-fallback valuation as any other unrecognized value, without failing or truncating the report. The exact original text remains in SQLite. In-range text, including explicit empty strings and ASCII casing, reaches pricing unchanged. SQL NULL and unexpected non-text metadata yield no lookup candidate. Invalid UTF-8/control bytes cannot match the ASCII aliases. Use byte length, not SQLite character length, so embedded NUL does not defeat the guard.

This bounded projection deliberately does not preserve the distinction between unavailable and overlong unrecognized evidence in the report: both use the applicable normal rates, and raw-tier export is out of scope. The pricing method also treats any overlong string passed directly as unrecognized. Future supported aliases must update the pricing-owned limit and its interface tests together.

Preserve indexed timestamp ordering, exact model filtering, reusable scan storage, and the existing row/group/model-byte budgets. No retained raw-tier map, tier grouping key, or per-tier query is added. `valueTurn` passes the candidate to the captured model's pricing value. Memoize model resolution and immutable pricing only—not a chosen mode—so consecutive Turns with the same model can use different tiers. Reuse the resulting amount and exclusion reason at the period/model, range/model, and section levels through the unchanged accumulation path.

## Reports and existing coverage

Keep `pricing.Contribution`, `report.Cost`, and the report's aggregate shapes unchanged. Valuation returns an exact amount or one of the existing exclusion reasons. Do not add rate-origin metadata, fallback/derivation counters, service-tier breakdowns, or an algorithm identifier.

At every existing aggregate, retain the checked partition:

```text
priced_turns + sum(existing five unpriced reasons) = turns
```

A fully priceable Turn increments `priced_turns`, including a zero-cost Turn. An unpriced Turn increments exactly one existing reason. Empty aggregates have amount zero; nonempty aggregates with no priceable Turns have amount null; priceable-free usage has amount zero with positive coverage. Normal fallback and derived Fast pricing use these same existing outcomes, without another classification.

The report retains one row per existing `(Surface, Reported model, period)`, even when different context bands and service tiers occur within it. `pricing_match` remains the model-resolution result, not a service-tier label. The existing `report.PricingProvenance` and its dataset/source/version fields remain unchanged; this design adds no new provenance to them and does not remove existing snapshot metadata.

## Unchanged version-1 protocol and client contract

Keep `/usage/v1/report`, `schema_version: 1`, and the entire existing JSON shape. Only calculated amounts and existing priced/unpriced coverage can change as a result of the new valuation. There is no new wire extension, policy marker, capability flag, or client-validation state.

`reporthttp/encode.go` and `validate.go` need no new fields or semantic rules for this feature. Preserve the trusted-`QueryFunc` contract: the reporter owns complete, valid results; the encoder bounds and transports them; the client independently validates the existing protocol. Extend regression tests to exercise service-tier-valued reports through that unchanged mapping.

Old and new clients consume the same daemon-supplied amounts. Existing pricing-capable clients remain wire-compatible without knowing which valuation implementation produced them. Native-only legacy responses still follow the existing all-absent pricing-extension behavior; recognized partial shapes of that existing extension remain invalid. Unknown additive data and validated original-byte JSON remain supported as before. The protocol does not advertise whether a daemon applies service-tier valuation, and the CLI must not infer that from model names or amounts.

Keep the existing amount representation: canonical nonnegative decimal strings, at most 24 fractional digits and 128 bytes. The exact source/derived-rate bounds must not produce an incompatible amount format. Preserve existing checked coverage sums, duplicate-member/Unicode validation, model-identity limits, and no client reconstruction of server aggregates or prices.

Keep the 8 MiB response cap, bounded fragment encoding, two admission slots, five-second work/write budgets, GET/HEAD behavior, `MaxBodyBytes+1` client reads, and complete-before-success output. Source/query failures retain existing errors; exclusions remain successful report data. Overflow of counts, amounts, or existing coverage remains a whole-report failure. Nothing changes inference readiness, writer admission, or forwarded payloads.

## Terminal presentation

Keep the existing `Est. USD` column, three-decimal rounding, model rows, period totals, secondary native tables, model-resolution details, and exclusion notes. The CLI continues to sum supplied amounts and existing coverage with checked arithmetic; it never selects rates or applies a Fast factor.

Do not add provenance labels, counters, fallback/derivation notes, service-tier details, or new older-daemon capability messages, including under `--details`. The existing generic estimated-cost/non-billing caveat is sufficient. Replace only the hard-coded `standard + context` / `standard/context` wording in the pricing header and final caveat with neutral `models.dev rates` wording, so the same text remains accurate for both old and new daemons without exposing calculation provenance.

Empty-history and native-only Estimated-cost-unavailable messages remain unchanged. Keep existing terminal-safe model escaping and complete-before-stdout behavior. `--json` still emits validated original bytes, unaffected by text-only totals or wording changes.

## Verification and acceptance

The interface is the test surface. Use literal synthetic snapshots and temporary SQLite databases; mark all manufactured completion/pricing fixtures as synthetic. Do not modify recorded captures to add service tiers or contact live models.dev/OpenAI in tests.

| Area | Required cases |
| --- | --- |
| Projection/acceptance | Valid embedded floor; absent mode/base cost; independently usable Fast cost without base cost; `fast` and `priority` names and wire mappings; ASCII case; a differently named wire-mapped candidate; ignored non-Fast modes; missing/null/wrong-typed containers and rates; contradictory mappings; identical/conflicting/case-colliding duplicate candidates; unsupported Fast context shape; last-good after each malformed fetch |
| Tier lookup | `default`, `fast`, `priority`, ASCII variants, NULL, empty, unknown, `flex`, `scale`, whitespace, embedded NUL, Unicode lookalikes; no request/model-suffix/Catalog inference; source-only mode names do not become wire aliases; Fast absent versus declared-but-unpriced |
| Derivation | Self-contained matrices above; strict threshold equality and above; greatest of several thresholds; legacy tier only without structured tiers; unequal category factors; missing operands, `0/0`, positive denominator with zero numerator/context; no cross-category borrowing; missing optional rate with zero versus positive cache counts; directly usable Fast base rates despite unavailable derivation |
| Exactness/failures | A repeating intermediate ratio whose final rate terminates, a final repeating decimal, final expanded-digit limits, immutable operands/concurrent callers, existing model/formula exclusion precedence, actual amount/count overflow, cancellation during mode/derived-tier projection |
| Reporter | Real schema-v3 databases; unchanged numeric projection and report types; all periods/filters/Surfaces; one model row with normal, fallback, explicit Fast, derived Fast/context, and excluded Turns; exact amounts and existing coverage at every aggregate level; empty/free/all-unpriced; long unknown tier uses normal long-context rates; overlong tier stays fallback without materialization/failure; alternating tiers do not reuse prior selection |
| Refresh/resources | Fast rate changes revalue history; mode deletion changes recognition to base fallback; retained mode with deleted cost becomes missing-rate; one captured projection across mid-query refresh and both Surfaces; existing identity/row/group caps, schema checks, read cancellation, and no per-Turn/per-report network work |
| HTTP | Service-tier-valued results use the unchanged field set and amount/coverage formats; existing pricing-extension complete/absent/partial validation remains unchanged; existing malformed/canonical/overflow checks, unknown additive data, bounded fragments and 8 MiB cap, GET/HEAD, timeout and no partial success bytes; no new provenance fields |
| CLI | Exact supplied-amount and existing-coverage period sums; unchanged overflow/no-output behavior, model-resolution details, escaping, legacy priced/native-only output, rounding/layout, and original-byte JSON; neutral pricing-header wording; no added provenance output |
| Executable | Synthetic completed responses through buffered HTTP, SSE, and WebSocket to storage/report/CLI; response `default` despite Fast request intent; null/empty/unknown/fast/priority; mixed-tier model; prices pinned or locally injected; no external network calls; native totals and payload fidelity unchanged |

A concrete mixed-row fixture can use base I/O/C/W rates `1/2/0.1/1.25`, Fast rates `2/4/0.2/2.5`, and Standard context rates `2/3/0.2/2.5` strictly above 100 input tokens. Four Turns with output 10 and zero cache counts are: default/input 50 (`0.00007`), unknown/input 50 (`0.00007`), priority/input 50 (`0.00014`), and fast/input 150 (`0.00066`). Their exact subtotal is `0.00094`, with four priced Turns. A fifth Fast/input-150 Turn with missing cached-token evidence increments the existing `missing_usage` count once and adds no cost; native input/output totals still include all five Turns. The report contains no additional breakdown of how those four prices were obtained.

For exact derivation, synthetic `base=3, fast=1, context=6` must yield rate 2 despite the intermediate ratio 1/3. `base=3, fast=1, context=1` leaves that derived category unavailable. A base-band Fast vector must remain directly usable in both examples.

Extend the existing suites rather than layer an independent mock-based suite: `pricing/snapshot_test.go`, `source_test.go`, `openai_test.go`; `report/pricing_test.go` and resource tests; `reporthttp/protocol_test.go` and failure tests; `reportcli/cost_test.go`; and `cmd/copilotd/usage_executable_test.go`. Replace the staged `TestQueryIgnoresStoredOpenAIServiceTierEvidence` assertion with tier-dependent amount and existing-coverage expectations rather than leaving contradictory expectations. Preserve #247's observer/migration tests as regression coverage.

After implementation, run through Nix:

```sh
nix develop -c go test ./internal/usage/pricing ./internal/usage/report ./internal/usage/reporthttp ./internal/usage/reportcli ./cmd/copilotd -count=1
nix develop -c go test -race ./... -count=1
nix fmt
nix flake check
```

Update the native verification inventory when named executable cases change; native-platform certification remains revision-specific and is not inferred from this document. No feature test execution is claimed for the design-only change.

## Implementation and documentation map

1. **Pricing:** extend `snapshot.go`'s private rates projection and Fast admission; add private `modes.go` lookup/admission helpers and `mode_context.go` exact derivation; add tier input to `Tariff.CalculateOpenAI` while keeping `Contribution` and the native formula unchanged. No additional pricing source. Update source/projection tests and adjacent method semantics.
2. **Reporting:** add bounded metadata projection and pass each Turn's lookup candidate into pricing in `report/read.go`. Keep `turnContribution`, `report.Cost`, report metadata, existing coverage, grouping, and model memoization unchanged. Freeze the numeric metric lists; no report-type extension is needed in `report/report.go`.
3. **Transport and CLI:** retain the existing production encoder and client validator. Add regression coverage for changed valuations using the existing wire shape. In `reportcli/command.go`, only replace the hard-coded normal-pricing wording in the header/caveat with neutral rate wording; retain subtotal logic and output structure. No new fields, flags, configuration, route, authentication, or schema version.
4. **Documentation:** amend the cost-reporting design's mode exclusion, rate selection, arithmetic, and estimation policy while explicitly preserving its report/coverage/protocol shapes; amend the reporting design's staged metadata-ignored/read-projection statement; update the capture design's staging note without changing its evidence contract. Amend ADR-0018/0019 notes without changing persistence/exposure decisions. Update README, CONFIGURATION, the models.dev projection documentation, and adjacent domain vocabulary. Preserve dated artifact identities; no floor bump is required.
5. **Issue alignment and approval:** amend #248/#242 to allow the agreed same-model/same-snapshot derivation and remove the requirements for new service-tier provenance, added coverage fields, terminal details, and their additive HTTP extension. Retain numerical, existing-coverage, HTTP/CLI, and executable verification. Link this design at its approved revision. Do not claim approval, publish an approval record, or start feature implementation merely because this plan is complete.

No new ADR is needed: this extends the existing pricing/reporting seams and records the estimator trade-off here and in their amendments. The implementation scope remains the revised #248; new service-tier provenance, raw Turn export, broader service-mode support, invoicing, pricing overrides, additional listeners, and capture redesign are excluded.

## Design-only validation

Completed on 2026-09-12:

- Checked the proposed admission structure against both identified models.dev artifacts; each has eight accepted Fast candidates and no conflicting declaration.
- Rechecked all eight base/Fast and seven context-vector examples using exact arithmetic, plus the mixed-row subtotal and existing priced/unpriced partition, zero/missing/repeating-ratio cases, and final representation limits.
- Executed the bounded SQL candidate expression against NULL, empty, ASCII-case, embedded-NUL, and overlong values in temporary in-memory SQLite.
- Checked document links, absence of dependencies on investigation notes, and Markdown file hygiene.
- Inspected existing report/HTTP/CLI contracts and checked that the reduced reporting scope retains their result shapes, wire schema, validation rules, and subtotal logic. The earlier independent review covered pricing/read contracts plus a now-removed report extension; it is not treated as a review or approval of the revised scope.

These checks validate this proposal and its examples, not feature implementation. The Go acceptance/race/native-platform gates above remain to be run after code exists.

## Approval state

The maintainer has agreed to deriving wherever data suffices, centralizing the calculation, and omitting new service-tier provenance from this design. The remaining contracts above are concrete engineering proposals derived from the existing repository behavior, for review as one implementation-ready design. There are no intentionally deferred design choices or placeholders requiring an implementer to invent policy. Full-design approval and the subsequent implementation/verification remain separate steps.
