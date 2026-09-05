# Store per-Surface native token counts verbatim

**Status:** accepted

The Usage meter stores one Turn in the table for its inference Surface,
preserving supported provider-reported token counts as signed 64-bit values
without normalization. There is no unified usage table, unified token column,
derived ordinary/real-input column, or inferred total. The table name identifies
the Surface; required `input_tokens` and `output_tokens` use `int64`, while every
other projected count uses a nullable pointer so an absent report remains SQL
`NULL` and a reported zero remains zero.

## Frozen initial projection

The initial OpenAI Responses projection has six scalar counts: `input_tokens`,
`input_tokens_details.cached_tokens`,
`input_tokens_details.cache_write_tokens`, `output_tokens`,
`output_tokens_details.reasoning_tokens`, and `total_tokens`. `input_tokens` is
complete input; both cached and cache-write tokens are subsets already inside it.
`output_tokens` is complete output, with reasoning tokens inside it. The three
recorded OpenAI transport fixtures and primary Responses sources support this
projection.

The initial Anthropic Messages projection has seven scalar counts:
`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
`cache_read_input_tokens`, the two `cache_creation` TTL counts, and nullable
`output_tokens_details.thinking_tokens`. Anthropic `input_tokens` is the uncached
remainder; cache creation and cache read are additive. Thinking tokens are a
re-tokenized output subset including delimiters and are at most `output_tokens`;
subtracting them only approximates non-reasoning output. This projection is
grounded in the exact official
[Messages Create reference](https://platform.claude.com/docs/en/api/messages/create),
related primary streaming/cache sources, and honestly labeled generated
fixtures. Live Copilot Anthropic compatibility remains unverified.

Beta variable-cardinality `usage.iterations[]` is deliberately excluded.
Compaction iteration counts are not included in top-level usage, and supporting
iteration variants later requires a separate schema/cardinality review rather
than flattening them into migration 1.

## Native nesting examples

For an Anthropic report with `input_tokens=12`,
`cache_creation_input_tokens=2000` (750 five-minute plus 1250 one-hour), and
`cache_read_input_tokens=6000`, explanatory real input is
`12 + 2000 + 6000 = 8012`; the database stores the three native values, not a
synthesized `8012`. If `output_tokens=9` and `thinking_tokens=4`, the four
thinking tokens are already inside nine.

For an OpenAI report with `input_tokens=8012`, `cache_write_tokens=2000`, and
`cached_tokens=6000`, explanatory ordinary input is
`8012 - 2000 - 6000 = 12`; the database stores the complete input and its two
subsets, not a synthesized `12`. If `output_tokens=9`, `reasoning_tokens=4`, and
reported `total_tokens=8021`, reasoning is already inside output and the total is
stored as reported rather than recalculated.

## Evolution policy

Every newly supported token-count field requires both a forward schema migration
and updated semantic documentation immediately beside the corresponding Go
field. Historical rows that did not report the new field remain `NULL`; a
migration must never fabricate zero. Values retain their Surface-native meaning
even when providers choose asymmetric nesting, and interpretation stays adjacent
to the Go types rather than being hidden in query-time normalization.

## Observer policy

The existing Shim mechanism admits both parity transforms and read-only
observers; no `Observer shim` subtype is introduced. A Usage meter Shim observes
at the existing buffered, SSE, and WebSocket hook seams and returns every payload
unchanged. This admission does not relax the hook contract: it performs no I/O
or waiting, never drives an upstream retry, and submits only to a prompt
non-blocking sink.

The [native usage evidence](../research/2026-09-05-native-usage-shapes.md) and
[Usage meter design](../design/2026-07-26-token-usage-meter-design.md) record the
provider semantics and evidence limitations behind this decision. Issue #197
lands the frozen persistence shape and buffered OpenAI parser; Anthropic and
streaming transport parsers remain staged implementation work.
