# OpenAI reported service-tier capture — Design

**Status:** approved design; implemented by #247.
**Approval:** 2026-09-12 — Ning Wang, following author/reviewer consensus.
**Issue:** [#247](https://github.com/ningw42/copilotd/issues/247), part of
[#242](https://github.com/ningw42/copilotd/issues/242).
**Repository baseline:** `33994c5`.

## Scope

Capture the top-level `service_tier` reported by each qualifying completed OpenAI
Response and persist it as nullable native Turn evidence. Buffered HTTP, SSE, and
WebSocket use the same completed-Response parser. This extends the existing Usage
meter rather than introducing a separate capture module, Shim registration, or hook.

This slice does not select service-mode rates, change Estimated cost, add report
fields, change CLI output, or introduce settings. It does not satisfy the parent
epic's separate requirement to approve the full service-mode pricing design before
finalizing cost-calculation implementation.

The response is authoritative. A request for priority/Fast, including a Requested
model ending in `-fast`, can complete with `service_tier: "default"`. Store
`"default"`; do not reconstruct evidence from the request, Requested model,
Reported model, Catalogs, aliases, or configuration.

## Existing seams and dependency strategy

- `internal/shim/usage_openai.go` already routes buffered bodies and the nested
  `response` in SSE/WebSocket `response.completed` events through
  `parseOpenAIResponse`. Keep qualification and optional evidence together there.
- `internal/shim/usage_turn_recorder.go` owns observation time, captured inbound
  correlation, HTTP Requested model, and the submission-attempt ordinal.
- `usage.Sink.Record(Turn)` in `internal/usage` is the existing interface between
  the Usage meter and persistence. It remains unchanged, including its immutable-snapshot,
  concurrent, prompt, non-blocking, no-I/O, no-synchronous-logging contract.
- `internal/usage/sqlitestore` owns migration, admission, WAL batching, loss
  accounting, and finalization. It does not deep-copy a Turn's optional pointers.
- `internal/usage/report/read.go` separately probes required schema columns and
  reads numeric metrics. Only the schema probe needs the new column in this slice.

Parsing is an in-process dependency. The injected sink already has two real
adapters: the SQLite writer and an in-memory test sink. Persistence is
local-substitutable using real SQLite in temporary private directories; a fake
SQL adapter would weaken migration and nullable-string verification. The provider
contract describes observed bytes, not a new remote dependency. No new port,
network call, separate capture module, or dependency is needed.

## Interface alternatives and decision

Three independent designs were compared:

1. **Flat Turn extension and keyed recorder input — chosen.** Add
   `Turn.OpenAIServiceTier *string` and change the private recorder to
   `record(turn usage.Turn)`. The private OpenAI parser returns the response-derived
   fields as a Turn after one Response-level decode and the existing required/numeric
   validation. This needs no new wrapper and keeps the change local. Its trade-off
   is an internal, not-yet-stamped Turn; the recorder explicitly owns and
   overwrites the common metadata before publication.
2. **Grouped OpenAI evidence.** Add `Turn.OpenAI *OpenAIResponseEvidence` with
   `ServiceTier *string` inside it and pass a grouped observation to the recorder.
   This gives future response metadata a convenient home, but introduces an
   extra type, nullable level, allocation, and equivalent absent states for one
   field. It still cannot prevent mismatched Surface/evidence combinations. No
   second supported field currently justifies that growth investment.
3. **Validated completion value.** Return a private `completion` wrapping a Turn
   from both parsers and the Anthropic accumulator; record only that value.
   This makes validate-then-publish ordering visible in the interface and can
   reduce assembly at callers. However, it changes more parser interfaces,
   introduces a zero-value construction convention, and remains discipline
   within the same Go package rather than a proof of validation. It is more
   restructuring than this issue needs.

The flat design wins on **depth** by hiding evidence rules once behind the
existing OpenAI observer and sink interfaces, rather than exposing new evidence
operations to each transport. It wins on **locality** by leaving Anthropic
parsing, shared JSON admission, report aggregation, and writer lifecycle alone.
The **seam** stays between immutable Turn observation and persistence. Grouping
alone does not add leverage, and deleting either proposed wrapper would remove
interface knowledge rather than force complex behavior back into callers.

## Evidence contract

Extend the immutable `usage.Turn` envelope with an explicitly OpenAI-scoped field:

```go
// OpenAIServiceTier is the exact decoded top-level service_tier in the
// qualifying completed OpenAI Response. Nil means unavailable evidence;
// a pointer to "" means explicitly empty. It is not request intent or
// a pricing classification. It is unset for Anthropic Turns.
OpenAIServiceTier *string
```

Keep `OpenAIUsage` unchanged: it mirrors `response.usage`, whereas `service_tier`
is a sibling of `usage` on the Response. Keep the new field out of
`OpenAIMetrics()`. Do not define a service-tier enum, recognized-value table,
normalizer, fallback tier, or pricing interpretation in the observation path.

The three OpenAI transports share these semantics:

| Completed Response evidence | Stored evidence | Otherwise qualifying Turn |
| --- | --- | --- |
| Missing member or JSON `null` | `nil` / SQL `NULL` | Recorded |
| `""` | Explicit empty string | Recorded |
| Known or future string | Exact decoded string | Recorded |
| Number, boolean, array, or object | `nil` / SQL `NULL` | Recorded |
| Duplicate top-level `service_tier`, even identical values | `nil` / SQL `NULL` | Recorded |
| Lossy Unicode decoding would be required for the tier | `nil` / SQL `NULL` | Recorded |
| Nested or differently cased member only | `nil` / SQL `NULL` | Recorded |
| Invalid required identity/status/usage or syntactically invalid Response | No evidence submission | Not recorded, as today |

`service_tier` contents are plain text, not embedded JSON. Only the enclosing
Response and the field's JSON string token (its quotes and escapes) are decoded.
The resulting Go string is stored as SQLite `TEXT`; it is never parsed as JSON
again.

Escaped member names that decode exactly to `service_tier` count as that member,
including for duplicate detection. Escapes in the string are decoded, but contents
are not trimmed, case-folded, normalized, truncated, or enum-validated.

An optional evidence failure is not a usage-parser failure. This does not relax
existing numeric validation: malformed reported numeric counts still invalidate
the candidate. Nor does it introduce a tolerant parser for syntactically broken
JSON; there must first be an otherwise qualifying Response under the existing
admission rules.

### Lossless Unicode policy

The extraction must reject invalid UTF-8 in the selected string and unpaired
UTF-16 surrogate escapes instead of persisting `encoding/json`'s replacement-derived
text. A real U+FFFD, whether literal or escaped as `\uFFFD`, is valid evidence.
A valid surrogate pair is decoded and retained normally. Testing the decoded
string for the presence of U+FFFD is therefore incorrect.

Keep this validation local to the selected optional string: malformed bytes or
surrogate escapes in unrelated content do not erase an independently valid tier
or change existing Turn qualification. This deliberately does not harden all
response text, identity parsing, or Requested-model extraction in this issue.
Escaped backslash text such as `"\\uD800"` remains literal text, not an invalid
surrogate escape.

Use the existing toolchain's `encoding/json/jsontext.AppendUnquote` on the
whitespace-trimmed raw member value. It accepts only a JSON string and reports
invalid UTF-8 and malformed surrogate escapes. **Discard all returned bytes on
any error**: this function can return replacement-derived output alongside its
error. Convert to an owned string only on success; empty successful output is a
present empty string, not missing evidence. This avoids a handcrafted Unicode
validator and adds no dependency: `jsontext` is already used in the repository.
The ordinary `encoding/json` decoder remains responsible for the permissive
outer traversal, so unrelated evidence is not subjected to whole-document strict
Unicode or duplicate-name validation.

### One duplicate-aware Response decode

`decodeJSONObject` alone loses duplicate occurrences. At the private OpenAI
Response level, replace its call with a small `json.Decoder` token loop that both
builds the same `map[string]json.RawMessage` and tracks top-level `service_tier`
occurrences:

1. Require an opening object delimiter and read each decoded member name.
2. Decode its value as `json.RawMessage` and assign it to `object[key]`. This
   preserves the existing last-wins behavior for unrelated duplicate members
   and does not interpret unrelated numbers or nested content.
3. Track occurrences of the decoded exact key `service_tier`. Only one
   occurrence can supply evidence; a duplicate makes the tier unavailable, not
   the Response invalid.
4. Require the closing object delimiter and end of input. Decode a unique tier
   string with `AppendUnquote`, mapping an absent/null/wrong-type or invalid
   string to nil.
5. Run the existing required identity, completed-status, and numeric validation
   against the collected map. Publish a Turn only if those checks pass.

This is one Response-level object traversal, not a new whole-Response pass after
validation. The existing event-envelope decode and nested numeric helpers keep
working as before. Shared `decodeJSONObject`, Anthropic parsing, and transport
routing remain unchanged. No generic JSON module or extra validation framework
is needed; a local private parser return adjustment carries the tier with the
validated response-derived fields.

The decode and string checks perform only CPU/memory work on already bounded
payloads. They retain no raw Response or generated content after observation.
A decoded tier value is separately owned and never mutated after submission;
later completions cannot overwrite earlier Turns' pointed-to evidence.

## Recording integration

Keep transport routing and the exported `Sink` interface unchanged. Adjust the
private OpenAI parser result and recorder input:

```go
func parseOpenAIResponse(raw []byte) (usage.Turn, bool)
func (r *turnRecorder) record(turn usage.Turn)
```

On success, the parser fills `ResponseID`, `Model`, `Usage`, and
`OpenAIServiceTier` from the same decoded Response after required/numeric
validation. Its boolean continues to mean completion eligibility; unavailable
tier evidence does not make it false. It leaves transport and recorder-owned
metadata for the caller and recorder.

The common OpenAI caller becomes:

```go
turn, ok := parseOpenAIResponse(raw)
if !ok {
    return
}
turn.Transport = transport
m.recorder.record(turn)
```

`parseOpenAIResponse` has one caller, so this result change stays local. The
Anthropic buffered and SSE submitters become keyed Turn literals containing only
their existing identity, transport, and native usage. They omit the new field;
Anthropic parser and accumulator return shapes remain unchanged.

The recorder receives the not-yet-stamped Turn by value. Callers supply
`ResponseID`, `Model`, `Transport`, `Usage`, and optional response evidence.
The recorder overwrites `At`, `RequestID`, `RequestedModel`, and `TurnIndex`
from its existing sources regardless of their input values, then advances the
submission-attempt ordinal and calls the sink. The partial-value convention
stays private; the sink always receives the completed immutable envelope.

The recorder must not retain `service_tier` as instance state: a WebSocket
session and repeated SSE completions can report different tiers, including
missing evidence immediately after a reported tier. Every qualifying completion
gets its own observation and ordinal; duplicate completion events retain the
existing non-deduplicated behavior. Anthropic callers leave `OpenAIServiceTier`
nil, and the writer's Anthropic insertion branch does not consume that field;
unsupported optional metadata must not become a new batch-failure condition.

WebSocket Requested model remains unavailable even though WebSocket service-tier
evidence can be present. Do not introduce a client-message hook, use an earlier
`response.created`, or correlate pending requests. The Usage meter stays
innermost in the existing registry and returns every buffered body, SSE frame,
and WebSocket Message unchanged, including malformed evidence.

## Persistence

Append `internal/usage/sqlitestore/migrations/003_openai_service_tier.sql`:

```sql
ALTER TABLE openai_turn ADD COLUMN service_tier TEXT;
```

Add its filename to the ordered `migrationNames` list in `store.go`; embedding
alone does not schedule it. Leave migrations 001 and 002 byte-unchanged.

- SQLite `user_version` becomes 3 through the existing migration transaction.
- Historical OpenAI rows acquire `NULL`, with every earlier value unchanged.
- Anthropic rows and their schema are unchanged by migration 3.
- No enum/check constraint, default, backfill, index, table rebuild, or money
  column is introduced. The existing STRICT table and indexes remain intact.
- Add `service_tier` to the explicit OpenAI INSERT column/placeholder/argument
  lists. Bind the `*string` as nullable text, as already done for Requested model.
- Keep admission, atomic all-pending migrations, startup contention, WAL,
  bounded batching, loss accounting, and finalization unchanged.
- Stop existing writers before upgrading. Mixed-version online migration is
  still unsupported; an older binary still refuses a newer schema.

Fresh, historical-v1, and historical-v2 databases converge on the same v3 schema.
Reopening v3 makes no schema or history change. Migration failure must roll back
all pending changes and the version bump together.

## Reports after the accepted staging interval

Add `service_tier` to the OpenAI `SELECT ... LIMIT 0` integrity probe in
`internal/usage/report/read.go`. The reporter must still require exact equality
with the writer-owned `sqlitestore.SchemaVersion()` and probe both native tables
even when only Anthropic history is selected. A file claiming v3 but lacking this
column is incompatible.

Do not describe this presence probe as full DDL validation: it checks the
explicit columns used by the contract, while store tests pin types, nullability,
constraints, and indexes. Do not expand that verification policy in this issue.

The later approved
[service-tier pricing design](2026-09-12-openai-service-tier-pricing-design.md)
ends the staging interval. The numeric SELECT and `OpenAIMetrics()` remain
unchanged, but the OpenAI reader appends a separately bounded lookup candidate
and passes it to per-Turn valuation. Recorded `fast`/`priority` evidence can now
change exact amounts and existing priced/unpriced coverage; aggregation, report
wire schema version 1, and CLI structure remain unchanged. Unknown, unavailable,
or overlong evidence follows normal base/context fallback without altering the
stored observation. This supersedes the former identical-report staging test;
the capture and response-authority contract in this document remains unchanged.

## Verification plan

### Usage meter interface

Use the existing Shim chain interfaces with an in-memory sink. Test a shared
behavior matrix through buffered, SSE, and WebSocket paths, including WebSocket
Text and Binary Message preservation:

- absent, null, empty, known, future, whitespace/control/Unicode, and escaped
  strings; exact-key casing/nesting and escaped-key behavior;
- wrong types, identical/conflicting/escaped-equivalent duplicates, invalid UTF-8,
  lone high/low surrogate escapes, valid surrogate pairs, genuine U+FFFD, and
  literal backslash-u text;
- unrelated malformed Unicode does not invalidate independently valid evidence;
- response `default` despite HTTP priority/Fast intent and a `-fast` Requested
  model; absent response evidence never falls back to request intent;
- earlier-event tier evidence is ignored; repeated/interleaved completions retain
  their own tiers and submission ordinals, including present-to-missing changes;
- every emitted body/frame/Message is identical to a separately copied input,
  and clearing/reusing input buffers or processing later completions cannot
  mutate an already submitted Turn;
- malformed tier preserves valid usage, while invalid core or optional numeric
  usage, incomplete/error events, and invalid JSON retain existing eligibility;
- no new client-message hook or retained completion state.

Use clearly marked synthetic fixtures, either in a new `.synthetic` fixture file
or explicitly labeled test literals. Do not augment any existing `.recorded`
usage fixture or imply that synthetic tiers were captured from live Copilot.
Retain the recorded fixtures as regression evidence, including unavailable tier
when the field is absent.

### SQLite and report interfaces

Through `Store.Open` / `Record` / finalization and external SQL inspection:

- Exact round trips for `NULL`, empty, `default`, unfamiliar strings, whitespace,
  decoded control characters (including NUL), and Unicode, independently of
  Requested and Reported model. Compare scanned strings, not SQLite's `quote()`
  display, which does not represent embedded NUL faithfully.
- Genuine v2 history built from migrations 001 and 002, then v2→v3: all existing
  values unchanged, OpenAI tier `NULL`, Anthropic schema unchanged.
- v1→v3 and fresh→v3 equivalence, exact ordered column metadata, preserved
  constraints/indexes, and reopen no-op. The existing v1 test now expects two
  appended NULLs for OpenAI but only one for Anthropic.
- A later pending migration failure rolls back earlier pending ALTERs and leaves
  historical schema, rows, and version unchanged.
- Updated admission, concurrent opener/process, literal-path, and startup/E2E
  version assertions. A future-schema test must use v4 or current+1, not v3.
- Hand-built current-schema fixtures in `report/snapshot_test.go` and
  `cmd/copilotd/usage_report_failures_e2e_test.go` include migration 003; otherwise
  encoding/damage tests could pass for the wrong reason (an obsolete version).
- v3 missing `service_tier` fails reporting, including an Anthropic-only query;
  exact schema-version mismatch still fails without reader-side migration.
- Changing only stored service-tier evidence leaves native and Estimated-cost
  reports unchanged during this slice.

Extend the real-listener-to-SQLite tests for all three OpenAI transports using
synthetic completion evidence and unchanged forwarded payloads. Retain queue
pressure, concurrency/race, loss, and finalization coverage rather than replacing
it with string-parser tests. Update the mandatory native acceptance inventory if
new named tests are added to its required evidence set.

During implementation run focused tests through `nix develop`, then
`nix develop -c go test -race ./... -count=1`, `nix fmt`, and `nix flake check`.
These are planned gates, not verification already performed for this design.

## Documentation changes when implemented

The accepted Usage meter design explicitly excludes non-token `service_tier`.
This issue intentionally changes that scope; do not silently leave contradictory
current claims in place or rewrite the historical migration-001 evidence.

Update:

- `internal/usage/usage.go`: adjacent field semantics and immutable ownership.
- `docs/design/2026-07-26-token-usage-meter-design.md`: the narrow non-token
  exception, shared completed-Response observation, Turn shape, migration 3,
  testing, and staged valuation caveat.
- `docs/adr/0018-store-per-surface-native-usage.md`: distinguish unchanged native
  token counts from newly admitted nullable response metadata.
- `CONFIGURATION.md` and the README state-at-rest summary: observed field,
  nullable semantics, v3 upgrade behavior, and unchanged reporting/pricing.
- `docs/design/2026-09-07-usage-reporting-design.md`: current schema version and
  unchanged report projection. Amend other current-version claims as necessary,
  without relabeling dated historical verification as newly rerun evidence.

No new divergence is introduced: this remains read-only observation. A new ADR
is unnecessary for a field extension within the approved persistence and observer
seams; revise the relevant native-evidence documentation when the change lands.

## Source evidence

The issue pins OpenAI's contract: `Response.service_tier` is optional, nullable,
and top-level rather than part of `usage`; `response.completed.response` carries
the same Response on SSE and WebSocket. It also cites response-tier authority
and priority/Fast downgrade behavior. Use its pinned sources and preserve the
distinction between provider contract evidence and live Copilot captures:

- [Response field and schema](https://github.com/openai/openai-openapi/blob/38170fdddbb6a1813eae6c6587ee17cf2987185b/openapi.yaml#L61067-L61075).
- [Completed-event envelope](https://github.com/openai/openai-openapi/blob/38170fdddbb6a1813eae6c6587ee17cf2987185b/openapi.yaml#L61631-L61654).
- [WebSocket server events](https://developers.openai.com/api/reference/resources/responses/websocket-events#server-events).
- [Fast-mode downgrade behavior](https://developers.openai.com/api/docs/guides/fast-mode#rate-limits-and-ramp-rate).
