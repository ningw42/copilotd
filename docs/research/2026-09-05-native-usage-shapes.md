# Native usage shapes across Copilot transports

**Observed:** 2026-09-05

**Issue:** #194 (epic #192)

**Result:** OpenAI Responses is verified on all three transports; current
Anthropic-through-Copilot evidence is blocked, so the five-path schema gate is
**not closed**.

## Executive answer

The design's OpenAI parsers can observe today's real Copilot completions. One
successful buffered response, one SSE `response.completed` event, and one
WebSocket `response.completed` Message each carried their own non-empty response
ID, returned model, `status:"completed"`, and complete `usage` object. No
session accumulator or fallback to an earlier event is needed.

The same conclusion cannot be made for Anthropic today. The available Copilot
account's raw catalog had zero models advertising `/v1/messages`, the
provider-shaped Anthropic catalog was empty, and one bounded probe of the
previously working `claude-opus-4.8` model returned HTTP 400. No live Anthropic
completion existed to sanitize. The Anthropic fixtures in this change are
therefore explicitly **synthetic contract variants**, not recorded Copilot
evidence.

For migration 1, the OpenAI projection should retain the design's six token
counts, including `input_tokens_details.cache_write_tokens`. Current OpenAI
primary sources define cache-write tokens at exactly that Responses nesting,
and all three Copilot captures contained the field as a genuine numeric zero.
For Anthropic, current primary sources add
`output_tokens_details.thinking_tokens`, a subset of `output_tokens`, which is
missing from the design. That addition and the unresolved live-capture gap need
#196 review before the complete projection can be frozen.

## Capture method, privacy, and request count

Capture used an already-running local copilotd endpoint. The daemon was neither
changed nor restarted. Its configured `responses-item-id-stabilizer` was on;
the no-op shim was off. The exact daemon build version was not exposed and is a
recorded limitation. The shipped stabilizer rewrites only per-output-item IDs,
including IDs under `response.output`; it does not touch top-level response
`id`, `model`, `status`, or `usage`. The recorded projections retain only those
unaffected fields and omit `output` completely. Buffered Responses does not run
that SSE/WebSocket-only transform. See
[`responses_item_id.go`](../../internal/shim/responses_item_id.go).

The successful requests asked for `gpt-5.6-sol-fast`; every completion reported
`gpt-5.6-sol`. Fixtures retain the returned model because the meter must not use
the requested model as a fallback.

Exactly **seven inference requests** were made:

- three successful Responses requests, one per buffered, SSE, and WebSocket
  path; these are the recorded fixtures;
- three preliminary buffered Responses requests with a too-small output cap;
  all failed the completed-candidate check, and a transient diagnostic
  projection for one showed `status:"incomplete"` with numeric zero usage;
- one Anthropic buffered availability probe, which returned HTTP 400. No
  Anthropic SSE request followed the catalog and buffered-path failures.

Discovery also made ten small catalog GETs while selecting and diagnosing
models. There was no bulk inference, prompt-cache warming, repeated cache-hit
traffic, or daemon lifecycle operation.

Raw response material lived only in a mode-0700 directory under `/tmp`, with
mode-0600 files. The capture program never printed credentials, request text, or
raw responses. Raw files and the temporary programs were deleted after the
sanitized projections were validated. Published fixtures replace response IDs,
omit all request/generated content and headers, and preserve every nested
`usage` field and value. Details are in
[`internal/shim/testdata/usage/README.md`](../../internal/shim/testdata/usage/README.md)
and `recorded-capture-metadata.json` beside the fixtures.

## Capture and transport matrix

| Surface | Transport and terminal | 2026-09-05 result | Identity/model/completed/usage together? | Fixture |
| --- | --- | --- | --- | --- |
| Anthropic Messages | buffered JSON; non-empty `stop_reason` | blocked: no catalog model; known model probe returned 400 | not observed | synthetic eligibility shape only |
| Anthropic Messages | SSE; `message_stop` after cumulative state | not requested after the bounded availability failure | not observed | synthetic cumulative and late-usage streams only |
| OpenAI Responses | buffered JSON; response `status:"completed"` | recorded, returned `gpt-5.6-sol` | yes, in the same response object | `openai-responses-buffered.recorded.json` |
| OpenAI Responses | SSE `response.completed` | recorded, sequence 11 | yes, in that event's `response` | `openai-responses-sse.recorded.sse` |
| OpenAI Responses | WebSocket `response.completed` Message | recorded, sequence 11 | yes, in that Message's `response` | `openai-responses-websocket.recorded.jsonl` |

The SSE fixture keeps the retained event's original `event:`/`data:` lines and
blank-frame separator. The WebSocket fixture keeps one complete server Message
per JSONL record. Neither is inferred from the older item-ID fixture.

## Verified native token-count inventory

“Provider schema” below means the dated primary source defines the field.
“Copilot capture” means the field occurred in each applicable recorded fixture;
absence from one minimal request is never used as proof that a field does not
exist.

### OpenAI Responses

The official Go SDK's `ResponseUsage` at package version 3.56.0 contains exactly
these Responses token-count paths. Its `ResponseCompletedEvent` embeds a full
`Response`, matching both recorded streaming transports.

| Path | Native meaning | Provider schema | Copilot buffered/SSE/WS |
| --- | --- | --- | --- |
| `usage.input_tokens` | complete input count | required | `12` / `12` / `12` |
| `usage.input_tokens_details.cached_tokens` | input tokens retrieved from cache; subset of input | required | `0` / `0` / `0` |
| `usage.input_tokens_details.cache_write_tokens` | input tokens written to cache; subset of input | required | `0` / `0` / `0` |
| `usage.output_tokens` | complete output count | required | `6` / `20` / `20` |
| `usage.output_tokens_details.reasoning_tokens` | reasoning subset of output | required | `0` / `12` / `12` |
| `usage.total_tokens` | provider-reported input plus output total | required | `18` / `32` / `32` |

The official prompt-caching guide's Responses calculation subtracts both
`cached_tokens` and `cache_write_tokens` from `input_tokens` to obtain ordinary
input tokens. This establishes inclusive nesting without borrowing Chat
Completions fields. The current official SDK independently names
`cache_write_tokens` “the number of input tokens that were written to the
cache.” The recorded zeros prove Copilot preserved the field on these three
paths; they do not demonstrate a non-zero cache write.

No Chat Completions-only audio or prediction counters are imported into this
inventory. They are absent from the pinned `responses.ResponseUsage` shape.

### Anthropic Messages

Current stable Anthropic sources define the following scalar completion usage
fields. None has current Copilot capture confirmation because no Messages model
was available through the observed account.

| Path | Native meaning | Projection consequence |
| --- | --- | --- |
| `usage.input_tokens` | uncached remainder after the last cache breakpoint | required core column; additive, not a unified total |
| `usage.cache_creation_input_tokens` | tokens written to cache | nullable detail column |
| `usage.cache_read_input_tokens` | tokens read from cache | nullable detail column |
| `usage.cache_creation.ephemeral_5m_input_tokens` | 5-minute subset of cache creation | nullable detail column |
| `usage.cache_creation.ephemeral_1h_input_tokens` | 1-hour subset of cache creation | nullable detail column |
| `usage.output_tokens` | inclusive output total | required core column |
| `usage.output_tokens_details.thinking_tokens` | re-tokenized internal-reasoning subset, always at most `output_tokens` | **missing from design; propose nullable `thinking_tokens`** |

Anthropic documents that the two TTL fields sum to
`cache_creation_input_tokens`, and that real input is
`input_tokens + cache_creation_input_tokens + cache_read_input_tokens`.
Streaming documentation says `message_delta.usage` values are cumulative.

The official **beta** SDK additionally defines variable-cardinality
`usage.iterations[]`. Each `message`, `advisor_message`, `fallback_message`, or
`compaction` entry can carry `input_tokens`, `output_tokens`,
`cache_creation_input_tokens`, `cache_read_input_tokens`, and the two
`cache_creation` TTL counts. A compaction iteration's counts are explicitly not
included in top-level usage. These paths are inventoried, not silently dropped,
but they should be explicitly excluded from migration 1: they are beta,
variable-cardinality, absent from the stable `Usage` type, and unobserved from
Copilot. Supporting them later needs a reviewed child-table/schema design, not
one more scalar column.

## Worked native semantics

These are arithmetic examples grounded in provider documentation and stored as
**synthetic** fixtures. They are not billing assertions and do not normalize the
two Surfaces.

### Anthropic is additive

`anthropic-messages-buffered.synthetic.json` reports:

```text
uncached input       12
cache creation     2000  (= 750 five-minute + 1250 one-hour)
cache read         6000
real input         8012  (= 12 + 2000 + 6000)
output                9  (thinking_tokens 4 is already inside 9)
```

### OpenAI input is inclusive

`openai-responses-inclusive.synthetic.json` reports:

```text
complete input     8012
  ordinary input     12  (= 8012 - 2000 - 6000)
  cache write      2000  (subset already inside 8012)
  cache read       6000  (subset already inside 8012)
complete output       9  (reasoning_tokens 4 is already inside 9)
total              8021  (= 8012 + 9, reported rather than synthesized)
```

Persisting both examples into one normalized input column would erase their
native meanings. Migration 1 should store the provider-reported values, not
these explanatory calculations.

## Recorded versus synthetic behavior matrix

| Concern | Evidence artifact | Classification and expected parser contract |
| --- | --- | --- |
| self-contained OpenAI completion | three `*.recorded.*` fixtures | recorded Copilot: validate each own response ID/model/completed status/usage; never use session accumulation |
| genuine numeric zero | all recorded OpenAI detail fields | recorded Copilot: zero is present data, not NULL |
| cumulative last value | `anthropic-messages-sse-cumulative.synthetic.sse` | synthetic: final output is `9`, not `0+5+9` or `5+9` |
| explicit null preservation | first synthetic Anthropic delta | synthetic: null input/cache updates preserve `12` and `2000` |
| absent start usage, later complete | `anthropic-messages-sse-late-usage.synthetic.sse` | synthetic: later numeric zeros satisfy required presence before `message_stop` |
| optional null in a self-contained response | `openai-responses-null-details.synthetic.json` | synthetic design contract: map optional nulls to absent values, not zero; not current provider behavior |
| wrong count type | `invalid-count-cases.synthetic.json` | synthetic: reject candidate and pass wire payload through unchanged |
| negative count | same | synthetic: reject required or optional count rather than erase it |
| int64 overflow | same | synthetic: reject candidate rather than clamp/coerce |
| completion markers | recorded OpenAI status/event types; synthetic Anthropic stop reason/`message_stop` | provider markers are documented; Anthropic Copilot occurrence remains unverified |

The null, missing, malformed, negative, and overflow cases are deliberately not
called provider behavior. They specify defensive parser behavior approved by
the design/TDD seam.

## Migration 1 recommendation and unresolved gate

Proposed projection for #196 review:

- **OpenAI:** keep `input_tokens`, `cached_tokens`, `cache_write_tokens`,
  `output_tokens`, `reasoning_tokens`, and `total_tokens`. Keep only
  `input_tokens` and `output_tokens` as parser-required core fields under the
  current design; nullable detail columns preserve unsupported/older shapes.
  `cache_write_tokens` is no longer semantically tentative and should stay,
  documented as an input subset.
- **Anthropic:** keep `input_tokens`, `output_tokens`,
  `cache_creation_input_tokens`, `cache_read_input_tokens`,
  `ephemeral_5m_input_tokens`, and `ephemeral_1h_input_tokens`; add nullable
  `thinking_tokens` as an output subset. Explicitly exclude beta
  `usage.iterations[]` from migration 1 pending a separate cardinality/scope
  decision.

This is a recommendation, not an approved schema change. The complete projection
must not be called frozen until maintainers review the newly exposed Anthropic
field and obtain a real buffered and SSE Copilot Messages capture. No production
metering, persistence, ADR acceptance, or glossary policy is changed here.

## Primary sources

Accessed 2026-09-05 unless a source pin states otherwise.

1. OpenAI, [Responses API object reference](https://platform.openai.com/docs/api-reference/responses/object).
2. OpenAI, [Responses WebSocket events](https://developers.openai.com/api/reference/resources/responses/websocket-events).
3. OpenAI, [Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching) — Responses nesting and inclusive cache-subset calculation.
4. OpenAI official Go SDK, package 3.56.0 development head
   [`65785ca`](https://github.com/openai/openai-go/blob/65785ca59ffea26f592920b5aae7bbe302cf30cc/responses/response.go#L23694-L23764) — `ResponseUsage` and both detail objects; cache-write support entered the official Responses model in
   [`e2d7068`](https://github.com/openai/openai-go/commit/e2d7068792c9ad65593b39a5f46a86188bcebbba) on 2026-07-09.
5. OpenAI official Go SDK,
   [`ResponseCompletedEvent`](https://github.com/openai/openai-go/blob/65785ca59ffea26f592920b5aae7bbe302cf30cc/responses/response.go#L5646-L5668).
6. Anthropic, [Messages API reference](https://platform.claude.com/docs/en/api/messages).
7. Anthropic, [Streaming Messages](https://platform.claude.com/docs/en/build-with-claude/streaming) — cumulative delta usage and `message_stop`.
8. Anthropic, [Prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) — additive input and cache TTL breakdown.
9. Anthropic official Go SDK v1.71.0 at
   [`de6914c`](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/message.go#L12149-L12196) — stable `Usage`; see also
   [`CacheCreation`](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/message.go#L1567-L1584),
   [`OutputTokensDetails`](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/message.go#L6959-L6979), and
   [`MessageDeltaUsage`](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/message.go#L6788-L6824).
10. Anthropic official Go SDK v1.71.0,
    [beta iteration usage](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/betamessage.go#L14624-L14712) and
    [iteration variants](https://github.com/anthropics/anthropic-sdk-go/blob/de6914c544629b14a67c0695ce147edae6a291e0/betamessage.go#L7074-L7166).
