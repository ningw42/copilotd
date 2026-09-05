# Token usage meter — Design

**Status:** accepted architecture — implementation staged
**Date:** 2026-07-26
**Accepted:** 2026-09-05
**Repository baseline:** reviewed against `91e635c` plus the #194–#195 evidence commits

The state-at-rest and Shim-observer exceptions are explicitly accepted in
[ADR-0017](../adr/0017-persist-usage-in-local-sqlite.md) and
[ADR-0018](../adr/0018-store-per-surface-native-usage.md). This remains the full
implementation target. Issues #197–#200 implement the settings, private SQLite
store, **buffered OpenAI Responses plus buffered Anthropic Messages**, and OpenAI
SSE and WebSocket observers end to end. Only Anthropic SSE remains staged; target
language below does not advertise that hook as currently active.

The Upstream call concentration, infallible post-commit hooks, structured logging,
terminal request summary, Hook overrun monitoring, and shared SSE data-payload
handling have landed. The design below uses those current contracts, not the
rollout order or source line numbers from the original draft. The supported
usage projection and driver evidence gates are closed with the explicit evidence
limitations in §11.3 and §13.

A shim-hosted, opt-in meter that submits one row per observed successful inference
completion to a local SQLite database, capturing the two inference Surfaces'
**native** token-count values without normalization. Single-user instance;
multi-user is out of scope. In-process query is out of scope — the database exists
so external tooling (`sqlite3`, Datasette, DuckDB) can read it.

---

## 1. Goal & outcome

Answer "what did I actually consume, on which model, when" without leaving the
proxy and without a second process.

_Final outcome:_ with `--shim-usage-meter-enabled`, an eligible completion on
the Anthropic `/v1/messages` or OpenAI `/responses` Route submits one row to the
configured local usage database (OS-specific default in §10). Both final Routes
support buffered JSON and SSE; only OpenAI Responses supports WebSocket. The
GitHub Copilot Surface and the Catalogs are not metered.

_Current checkpoint (#200):_ qualifying buffered Anthropic Messages, buffered
OpenAI Responses objects, self-contained OpenAI `response.completed` SSE events,
and qualifying OpenAI WebSocket server Messages submit rows. The store carries
the frozen two-table migration unchanged. Only the Anthropic SSE usage observer
is not active at this checkpoint.

An eligible completion contains the required usage fields, identity, and model
reported upstream (§6). This is best-effort observation, not an exactly-once
delivery or durability guarantee: parsing, queue, and storage failures can lose
rows (§8–9).
With the flag off (the default), the meter creates nothing, writes nothing, and
adds no hooks. With it on, hook payloads stay unchanged, but buffered forwarding
has observable costs (§11.1).

Requested information:

| Asked for | Delivered as |
| --- | --- |
| (a) completion observation time | `at_ms` + generated `at_utc`; not request start time or duration; `turn_index` orders submissions within a WebSocket session |
| (b) model id | `model`, as reported upstream |
| (c) input token | `input_tokens` — **per-Surface semantics, see §7.1** |
| (d) output token | `output_tokens` |
| (e) cache create | `cache_creation_input_tokens` (+ TTL split) / `cache_write_tokens`, with native nesting (§5) |
| (f) cache read | `cache_read_input_tokens` / `cached_tokens` |
| (g) inference Surface | the **table name** — one table per metered Surface |
| (h) anything else | `request_id`, upstream `message_id` / `response_id`, `transport`, `turn_index`, `reasoning_tokens`, `thinking_tokens`, cache TTL split |

---

## 2. Problem

Three facts make this harder than "add a counter."

**The two Surfaces nest cache tokens in opposite directions.** Anthropic's
`input_tokens` is the *uncached remainder* — real input is
`input_tokens + cache_creation_input_tokens + cache_read_input_tokens`. OpenAI's
`input_tokens` is the *complete* count, with `input_tokens_details.cached_tokens`
a subset already inside it. Two turns with identical real consumption therefore
write `12` and `8012` into a naively-shared `input_tokens` column.

**Streaming usage is cumulative and can be absent.** Anthropic usage spans
`message_start` and cumulative `message_delta` updates; summing repeated updates
double-counts. Missing usage must be tolerated rather than converted into zero.
The parser tracks reported fields until completion. The accepted shape is grounded
in the exact official Messages Create, streaming, and cache sources plus generated
fixtures; live Copilot Anthropic compatibility remains unverified (§11.3).

**Shim hooks may not do I/O.** `internal/shim`'s package contract binds SSE and
WebSocket hooks to "prompt and non-blocking: CPU-bound transformation only, with
no I/O or waiting." A hook that writes to SQLite would violate it and could stall
the SSE pump.

---

## 3. Decisions & non-goals

### Decisions

1. **Persist to SQLite** via the accepted pure-Go `modernc.org/sqlite v1.58.0`
   exception in [ADR-0017](../adr/0017-persist-usage-in-local-sqlite.md). The
   [state-at-rest boundary](../../README.md#state-at-rest) admits only this
   opt-in usage database exception; unrelated state remains memory-only.
2. **Only observed successful completions are eligible.** Anthropic SSE
   accumulates usage before `message_stop`; OpenAI uses `response.completed`.
   A hook runs before the downstream write, so this is not proof of client
   delivery or a clean transport outcome (§6).
3. **One table per metered Surface, values verbatim.** No unified columns, no
   derived totals. Interpretation lives beside the Go types (§5).
4. **Capture the approved token-count fields of the supported usage shapes**,
   including subsets. Every newly supported count requires a forward migration
   and adjacent Go semantic documentation; historical absence stays `NULL`, not
   fabricated zero. Beta Anthropic `usage.iterations[]` is excluded pending a
   separate schema/cardinality review (§11.3).
5. **The meter observes without rewriting payloads.** Every hook returns its
   input unchanged. This does not make enabling buffered hooks wire-neutral
   (§11.1).
6. **Opt-in, off by default**, like every other shim.
7. **Versioned schema** via `PRAGMA user_version`, forward-only migrations.

### Non-goals

- **Query.** No API, no CLI subcommand, no aggregation. External tools read the file.
- **Cost/pricing.** No rates, currency, or billing reconciliation. Neither native
  token counts nor per-model row counts establish Copilot charges; do not bake a
  subscription's billing model into this design.
- **Retention.** No automatic pruning. Growth depends on client activity, including
  automated turns; operators own retention and backups.
- **Multi-user / per-key attribution.** Explicitly out of scope.
- **Metering `count_tokens`.** `(Anthropic, /v1/messages/count_tokens)` reports an
  estimate, not consumption. Out of scope.
- **Non-token usage fields.** Labels such as `service_tier` and request counts
  such as `server_tool_use.web_search_requests` are not token counts. Persisting
  them would be a scope change, not merely a parser update.
- **Model-name mapping.** Store the model reported by the inference response,
  not a Catalog's display ID or metadata source. Catalog normalization and Codex
  catalog aliases do not rewrite inference responses.

---

## 4. Placement: an observer shim

### 4.1 Why the shim seam

Two alternatives were considered and rejected:

- **A tap inside `sse.Pump`.** Rejected: ADR-0002 makes the SSE engine
  deliberately payload-opaque. Teaching it to read `usage` breaks that.
- **HTTP middleware.** Rejected: it cannot see inside an SSE stream without
  re-parsing bytes that have already been written downstream.

The shim seam already spans all three transports (buffered, SSE, WebSocket)
through one registry. Typed inference transforms already live there. ADR-0018
admits read-only observers through that same mechanism without introducing a
named observer subtype.

### 4.2 An observer, not a payload transform

Existing hook interfaces permit unchanged returns, so observation does not require
a new hook mechanism (`NopShim` itself implements no hooks). The accepted Shim
policy includes read-only observers. The meter returns the same buffered body,
the same SSE frame, and the same WebSocket Message with `emit=true`; it neither
holds nor drops output.
`Frame.Raw` remains authoritative, and `sse.Pump` writes it verbatim.

That proves payload identity through the hooks, not whole-response wire identity.
Enabling a `BufferedTransformer` activates bounded reading and header handling
(§11.1). There is no new usage-rewriting Alteration to list, but implementation
must review the existing copilotd-originated error entry in
`docs/divergence-ledger.md` for the meter-triggered buffering failure path.
Those failures reuse already-governed Fabrications rather than requiring a new
Alteration row. The old rationale that there is no wire departure does not hold.

### 4.3 Hooks

Three hooks. `StreamFinalizer` is unnecessary: `Chain.StreamAdapter` includes
an instance that implements `EventTransformer` *or* `StreamFinalizer`, and the
meter holds no frames. Finalization must not turn an interrupted response or a
synthesized terminal into a successful usage row.

| Transport | Hook | Record fires |
| --- | --- | --- |
| Buffered JSON | `BufferedTransformer` | once, if the body is a successful inference response with valid usage |
| SSE | `EventTransformer` | on `message_stop` / `response.completed` |
| WebSocket | `ServerMessageTransformer` | on each qualifying `response.completed` |

Eligibility is **payload-based**, not an HTTP status/content-type filter:
`BufferedTransformer` receives only `Body.Bytes`, not the response Prelude. It
must recognize the Messages response shape or a Responses object with completed
status and valid usage; an error object or incomplete response is not eligible.
A separate requirement to gate on HTTP status/headers would need an unchanged
`PreludeTransformer` as a fourth hook. This design does not add that policy.

The current adapters are `Chain.StreamAdapter(logCtx, monitor)` and
`Chain.WSServerAdapter(logCtx, monitor)`. Their transport-owned monitor will
observe the meter automatically under
[ADR-0016](../adr/0016-observe-shim-hook-overruns-without-controlling-execution.md).
There is no meter-specific watchdog or new registry-monitor interface. Monitoring
reports Hook overruns; it neither interrupts a hook nor makes a blocking sink
safe. Buffered hooks remain unmonitored.

### 4.4 Onion position: registered last

Registration order is onion order, and response-side folds run in reverse —
`sseAdapter.Transform` iterates `len-1 → 0`, documented as "folds one frame from
the innermost shim to the outermost." **Last-registered is innermost**, closest to
upstream. The meter belongs there: it records what Copilot reported, not what an
outer shim reshaped. No shipped shim touches usage fields, so this is
unobservable today; the ordering encodes the intent before it can become a bug.

---

## 5. Package split and the sink contract

Two packages, following the precedent CONTEXT.md sets for `endpoint` ("the
`Surface` type lives in `internal/endpoint`; consumers depend on it, not the
reverse"):

- **`internal/usage`** — types and sink contract only; standard-library imports
  (`time`), no repository or third-party dependencies.
- **`internal/usage/sqlitestore`** — writer goroutine, schema, migrations.
  Imports `usage` and `modernc.org/sqlite`.

The new dependency from `shim` is `usage`, not `usage/sqlitestore`. **The SQLite
dependency never enters the shim package or its test binary.**

```go
// internal/usage — standard-library dependencies only.
package usage

import "time"

// Sink receives one observed successful completion. Record must be safe for
// concurrent callers and must not block, do I/O, or emit synchronous logs: it
// is called from hooks inside the SSE pump and WebSocket server pump. A call
// attempts enqueueing; it does not acknowledge persistence. The supplied Turn,
// including pointed-to optional values, is an immutable snapshot.
type Sink interface{ Record(Turn) }

// Transport names the path that served a turn.
type Transport string

const (
	TransportBuffered  Transport = "buffered"
	TransportSSE       Transport = "sse"
	TransportWebSocket Transport = "websocket"
)

// Turn is the Surface-independent envelope. Usage carries the verbatim,
// Surface-native token fields and selects the destination table.
type Turn struct {
	At         time.Time
	RequestID  string    // inbound HTTP correlation; empty if unavailable
	ResponseID string    // upstream message.id / response.id, not an HTTP request ID
	Model      string    // as reported upstream, never the client's requested name
	Transport  Transport
	TurnIndex  int       // submission ordinal within the shim instance
	Usage      Usage
}

// Usage is a closed sum: only the two Surface-native records satisfy it.
type Usage interface{ isUsage() }

// AnthropicUsage mirrors Messages API usage verbatim.
//
// InputTokens is the UNCACHED REMAINDER. Real input is
// InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// The Ephemeral* fields are the TTL split *inside* CacheCreationInputTokens.
// OutputTokens is the COMPLETE count; ThinkingTokens is a re-tokenized subset
// already inside it, including delimiters, and subtraction only approximates
// non-reasoning output. A nil pointer means upstream did not report a numeric
// value; a pointer to zero means upstream reported zero.
type AnthropicUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
	Ephemeral5mInputTokens   *int64 // usage.cache_creation.ephemeral_5m_input_tokens
	Ephemeral1hInputTokens   *int64 // usage.cache_creation.ephemeral_1h_input_tokens
	ThinkingTokens           *int64 // usage.output_tokens_details.thinking_tokens; <= OutputTokens
}

// OpenAIUsage mirrors Responses API usage verbatim.
//
// InputTokens is the COMPLETE count; CachedTokens and CacheWriteTokens are
// subsets already inside it. OutputTokens is the COMPLETE count;
// ReasoningTokens is a subset already inside it. TotalTokens is stored only as
// reported, never recalculated. A nil pointer means no numeric report; a
// pointer to zero means upstream reported zero.
type OpenAIUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CachedTokens     *int64 // input_tokens_details.cached_tokens
	CacheWriteTokens *int64 // input_tokens_details.cache_write_tokens
	ReasoningTokens  *int64 // output_tokens_details.reasoning_tokens
	TotalTokens      *int64
}

func (AnthropicUsage) isUsage() {}
func (OpenAIUsage) isUsage()    {}
```

Those doc comments carry the nesting asymmetry alongside the fields it governs.
Parsers distinguish missing counts from genuine zero. Required counts must be
present and valid in the **final candidate**: after accumulation for Anthropic
SSE, or within the buffered/completion object elsewhere. Missing/null cumulative
updates preserve earlier numbers (§6.2); they are not early rejection signals.
Negative, out-of-range, or wrong-typed reported counts invalidate the candidate,
including optional counts rather than silently erasing them. An optional field
with no reported numeric value maps to nil. Resetting a meter must not mutate
values already submitted to the writer.

---

## 6. Record boundary and instance lifecycle

**One submission per qualifying observed completion, not per successful write to
the client.** `internal/forward` builds a Chain per HTTP request;
`internal/wsforward` builds one per WebSocket session. An HTTP request normally
has one completion, but the SSE engine does not enforce that event cardinality.
A WebSocket session can contain many inference turns.

Hooks run before downstream writes. A valid row can therefore be submitted before
a client disconnect, write failure, or outer-shim failure. No finalizer or access
summary retroactively acknowledges or withdraws it. `At` is the local time the
meter observes completion, not upstream creation time or downstream receipt time.

`TurnIndex` is the zero-based **submission-attempt ordinal** within an instance,
advanced when calling `Sink.Record`, even if the sink drops that record. It is
not a count of all attempted inference turns. Duplicate completion events are
not deduplicated; this is an observation ledger, not an exactly-once logical-turn
store. Each duplicate qualifying event can make another submission.

**`RequestID` is captured once at construction** via `logging.RequestIDFrom(ctx)`.
Every WebSocket row shares the handshake's ID; message hooks execute under a
session cancellation context, which is not a substitute for that correlated
construction context. If `RequestIDFrom` returns `ok=false`, keep `RequestID` empty
rather than fabricate correlation or discard otherwise valid usage. Production
inference handlers install it; tests and standalone internal callers may not.
`(request_id, turn_index)` is a correlation aid, not a globally unique key (§7.2).

Persist the upstream inference object's identity as well: `Turn.ResponseID` maps
to Anthropic `message_id` or OpenAI `response_id`. This is the message/response
object ID, **not** the upstream HTTP `X-Request-Id`. Repeated completion
observations can then be investigated without adding a uniqueness constraint or
assuming that ID scope is global.

### 6.1 Surface-specific parsers

`Registration.New` already receives the Surface, so it selects a parser rather
than branching inside one. The accepted sink is captured by the registration:

```go
{
	Name: "usage-meter", Enabled: false,
	Scope: func(s endpoint.Surface, r endpoint.Route) bool {
		return (s == endpoint.Anthropic && r == endpoint.RouteAnthropicMessages) ||
			(s == endpoint.OpenAI && r == endpoint.RouteOpenAIResponses)
	},
	New: func(ctx context.Context, s endpoint.Surface, _ endpoint.Route) any {
		if s == endpoint.Anthropic {
			return newAnthropicUsageMeter(ctx, sink)
		}
		return newOpenAIUsageMeter(ctx, sink)
	},
},
```

Each parser owns only its Surface's usage shape. Buffered hooks leave `Body.Bytes`
unchanged and return nil, including on malformed or irrelevant input. WebSocket
hooks leave Message kind/data unchanged and return `emit=true`. Malformed SSE
input also **declines by passthrough**; skipping a usage observation is not a drop
of forwarded content.

The buffered Anthropic validator requires `type:"message"`, non-empty `id` and
`model`, a non-empty string `stop_reason`, and valid required counts in `usage`.
It does not enumerate stop reasons: `tool_use`, `max_tokens`, and other reported
reasons still end an inference turn. Unknown extra fields remain untouched.
The buffered OpenAI validator is specified in §6.3.

### 6.2 Anthropic SSE: request-scoped accumulation

Anthropic reports input/cache usage at `message_start` and cumulative updates at
`message_delta`; the completion marker is `message_stop`. Keep one request-scoped
accumulator, including presence flags, message identity, model, and a poison flag.
Update **only numeric values actually reported**: the last reported number per
field wins, never a sum. An omitted field or explicit JSON `null` is no new
numeric report and preserves an earlier value, including zero. If no numeric
value for a required field arrives by completion, the turn is not eligible.

Schematic hook using the shared SSE data-payload interface:

```go
func (m *anthropicUsageMeter) TransformEvent(_ context.Context, f sse.Frame) []sse.Frame {
	switch f.Type {
	case "message_start", "message_delta", "message_stop", "error":
		payload, present := f.Data()
		m.observe(f.Type, payload, present) // validate, accumulate, or submit
	}
	return []sse.Frame{f} // unchanged on every path
}
```

`Frame.Raw` contains SSE framing, not JSON. `Frame.Data()` handles repeated data
fields and line endings; an observer never needs `WithData`. `Frame.Type` is
advisory routing information, not validation: require the decoded event's type to
agree before absorbing it. Malformed relevant events, conflicting starts, or an
upstream `error` poison this HTTP instance, suppressing subsequent submission.
A valid stop submits only if identity, model, and required counts are present,
then clears the accumulator. A duplicate stop without a new valid start submits
nothing. No terminal means no row.

An absent usage object is not itself malformed. If later updates provide all
required counts, completion may qualify; otherwise it does not. This distinction
must be fixture-tested. Text deltas do not need JSON decoding by the meter;
framework Hook overrun monitoring still has its own fixed per-invocation cost.

### 6.3 OpenAI: self-contained completion, no in-flight slot

For both SSE and WebSocket, parse each `response.completed.response` independently
and require that same response to contain its ID, model, completed status, and
valid required usage. SSE uses `Frame.Data()` after routing on `Frame.Type`, and
validates the decoded event type too; WebSocket decodes Message data directly.
A buffered Responses body uses the same response-object validator. Do not fill
missing values from an earlier event or the client request.

Keep only immutable instance metadata and the submission ordinal. There is no
OpenAI per-turn accumulator, overlap warning, `response.id` map, or client-message
hook. Interleaved responses cannot mix counters because no counters are shared
between completion envelopes. A malformed completion does not poison the next;
`response.failed`, `response.incomplete`, and `error` make no submission and need
no reset. Retained session state stays bounded independently of session length.

This replaces the old draft's sequential-turn assumption and single-slot guard.
The contract is supported by three dedicated recorded Copilot fixtures, one for
each OpenAI transport (§11.3); existing item-ID tests are not treated as usage
evidence. If Copilot omits required fields, observation is skipped; silently
reviving shared-slot accumulation is not the fallback.

---

## 7. Schema

Two independent tables. **The table name is the Surface column** — there is no
`surface` column, and no `route` column, because the design meters just one
inference Route on each of these two Surfaces. This is a scope decision, not a
claim that the repository has only two Surfaces or two Routes.

### 7.1 DDL (migration 1)

This is the accepted exact initial schema. The only projection change from the
reviewed draft is the approved nullable Anthropic `thinking_tokens` field.

```sql
CREATE TABLE anthropic_turn (
  id                          INTEGER PRIMARY KEY,
  at_ms                       INTEGER NOT NULL,
  at_utc                      TEXT GENERATED ALWAYS AS (
                                strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                              ) VIRTUAL,
  request_id                  TEXT    NOT NULL,
  message_id                  TEXT    NOT NULL,
  turn_index                  INTEGER NOT NULL,
  model                       TEXT    NOT NULL,
  transport                   TEXT    NOT NULL CHECK (transport IN ('buffered','sse')),
  input_tokens                INTEGER NOT NULL,  -- UNCACHED REMAINDER
  output_tokens               INTEGER NOT NULL,
  cache_creation_input_tokens INTEGER,           -- additive to input_tokens
  cache_read_input_tokens     INTEGER,           -- additive to input_tokens
  ephemeral_5m_input_tokens   INTEGER,           -- subset of cache_creation_input_tokens
  ephemeral_1h_input_tokens   INTEGER,           -- subset of cache_creation_input_tokens
  thinking_tokens             INTEGER            -- subset of output_tokens; re-tokenized
) STRICT;
CREATE INDEX anthropic_turn_at ON anthropic_turn(at_ms);

CREATE TABLE openai_turn (
  id                 INTEGER PRIMARY KEY,
  at_ms              INTEGER NOT NULL,
  at_utc             TEXT GENERATED ALWAYS AS (
                       strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                     ) VIRTUAL,
  request_id         TEXT    NOT NULL,
  response_id        TEXT    NOT NULL,
  turn_index         INTEGER NOT NULL,
  model              TEXT    NOT NULL,
  transport          TEXT    NOT NULL CHECK (transport IN ('buffered','sse','websocket')),
  input_tokens       INTEGER NOT NULL,  -- COMPLETE count
  cached_tokens      INTEGER,           -- subset of input_tokens
  cache_write_tokens INTEGER,           -- subset of input_tokens
  output_tokens      INTEGER NOT NULL,  -- COMPLETE count
  reasoning_tokens   INTEGER,           -- subset of output_tokens
  total_tokens       INTEGER
) STRICT;
CREATE INDEX openai_turn_at ON openai_turn(at_ms);
```

### 7.2 Why each choice

- **`STRICT`.** SQLite rejects values that cannot be losslessly converted to the
  declared type; it still accepts some coercions, such as numeric text to an
  integer. Parser validation and typed inserts, not `STRICT` alone, preserve the
  upstream value contract.
- **Divergent `transport` CHECKs.** WebSocket is OpenAI `/responses`-only
  (ADR-0006), so the Anthropic table forbids it structurally, not by convention.
- **Nullable detail columns, NOT NULL core columns.** `NULL` means *upstream did
  not report a numeric value*; `0` means *upstream reported zero*. That distinction
  answers a live question — does Copilot's Anthropic passthrough report cache
  fields at all? `DEFAULT 0` would destroy the answer permanently. Core token
  columns stay NOT NULL because a completion missing them is not eligible.
- **`at_ms` integer + `at_utc` virtual.** A 64-bit integer is canonical, needs no
  format discipline, and supports direct arithmetic. Its one footgun is that
  `datetime(x,'unixepoch')` assumes *seconds*, so passing milliseconds gives an
  incorrect or out-of-range result; the unit in the column name helps avoid it, and
  the `VIRTUAL` generated column costs zero bytes at rest while making
  `SELECT at_utc, model FROM openai_turn` readable with no conversion to
  remember. Millisecond precision because WebSocket turns can share a second.

  Pin the expression with a known millisecond timestamp against the SQLite
  version bundled by the chosen driver. `at_utc` is a convenience projection;
  `at_ms` is always the stored truth.
- **No `UNIQUE(request_id, turn_index)`, despite appearances.**
  `logging.ResolveRequestID` **honors a well-formed inbound `X-Request-Id`**
  in `internal/server.requestID`, so request IDs are client-influenceable and are
  not a key. A unique constraint would reject legitimate rows whenever a client
  reused an ID. The integer `id` is the row key; add a non-unique correlation
  index only if external query use justifies it.

### 7.3 The subset rule

Verified token-count subsets are retained rather than selected by pricing. Split
tables remove the normalization ambiguity; they do not eliminate provider-schema
drift. Every newly supported token-count field requires a forward migration and
an update to the adjacent Go semantic documentation. Historical rows without a
reported value remain `NULL`; migrations never invent zero.

---

## 8. Migrations and failure policy

### 8.1 Mechanism

`PRAGMA user_version` — SQLite's built-in schema-version integer. No
`schema_migrations` table, and the version bump commits in the same transaction as
the DDL, so a half-applied migration is impossible.

```go
// Ordered, append-only, embedded in the binary. Index+1 == user_version.
var migrations = []string{ /* 1: initial schema */ }
```

Open sequence on the configured connection (§9): acquire `BEGIN IMMEDIATE`, then
read `user_version` **inside that write transaction**, apply every migration beyond
it, set the final version, and commit. All pending migrations and their version
bump are atomic together. A second simultaneous opener waits or retries within the
startup contention budget (§8.3), then re-reads the committed version instead of
acting on a stale pre-lock value.

**No down migrations** — this is a single-user local file; rollback means stopping
all writers and restoring a consistent backup, not copying just a live WAL
database's main file.

**`user_version > len(migrations)`** means an older copilotd opened a file a newer
one wrote. Fail closed with an error naming both numbers. Never write against a
schema the binary does not understand.

### 8.2 Failure policy, split by phase

Follows CONTEXT.md's readiness rule ("the real serve lifecycle fails before
binding when a local prerequisite is missing"):

- **Startup** — meter enabled but the database cannot be opened or migrated →
  **fail before binding**, stating the reason. Metering was explicitly requested;
  silently not metering is worse than not starting.
- **Runtime** — write failure (disk full, I/O error) → **count the loss, report
  it off the hook path, and keep serving**. The meter must never take down the
  proxy. Loss reporting is bounded, not one synchronous log per rejected row.

Logs follow [ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md):
the writer receives a required positional `*slog.Logger` derived for
`internal/usage/sqlitestore`; new top-level keys live in `internal/logging`.
Startup failure belongs to `cmd/copilotd`. Level follows current consequence: a
contained transient loss or queue pressure is Warn; consecutive current write
failure that indicates persistent inability and requires operator action is
Error; a confirmed later commit clears that live failure state. Cumulative loss
counters remain cumulative but do not keep recovered runtime or final records at
Error forever. No prompt bodies or credential values enter rows or logs.

### 8.3 Concurrent access

Multiple same-version `serve` processes for one user may share a **local** database;
SQLite serializes their writes. Give startup lock contention one overall
five-second budget, including connection setup, WAL activation, and transaction
acquisition. `PRAGMA busy_timeout` alone is insufficient: concurrent fresh opens
can return `SQLITE_BUSY` immediately while switching `journal_mode` to WAL.

On `SQLITE_BUSY` **before the schema transaction is acquired**, close the attempted
connection and retry setup from a clean connection with a short bounded backoff,
recomputing the remaining monotonic budget and capping the native busy timeout
**before each potentially blocking setup/acquisition operation**. Do not reset
the budget on each attempt or retry non-contention errors. Read/check the actual
journal mode result; inability to establish WAL is a startup failure, not a
silent fallback. Once configured and admitted, use `busy_timeout=5000` on the
store's dedicated connection for runtime writes.

These waits occur only during startup or in the writer, never in hooks. Exhausting
the startup budget fails before bind; an exhausted runtime write attempt
loses/counts that batch and keeps serving. Do not automatically replay failed or
ambiguously committed runtime batches. A migration error after acquisition rolls
back and fails startup; it is not part of the pre-transaction contention retry.
The budget bounds contention waits/retries, not arbitrary filesystem I/O or
post-acquisition migration execution.

This is not an online mixed-version migration protocol. Stop existing writers
before starting a binary that upgrades the schema; concurrent fresh opens by the
same binary are supported. External tools may read while serving, but external
schema mutation and long-lived external write transactions are unsupported.
The WAL database and sidecars must stay together on a local filesystem; network
shares and roaming/synchronized copies of a live database are not supported.

---

## 9. Writer and durability

Buffered channel (1024) → one writer goroutine → bounded batches inserted in one
transaction, flushed on a ~1s timer, on batch fill, and on shutdown.
After startup succeeds, use one dedicated SQLite connection per store for its
lifetime. Configure the busy timeout, verify `journal_mode=WAL`, and set
`synchronous=NORMAL` before migration, with setup retries confined to §8.3. The
writer uses the admitted connection with its full runtime busy timeout. Do not
silently open a replacement or pooled connection without repeating setup.

**Channel full → drop the record and count the drop. Never block.** `Record` also
must not synchronously log a drop. The writer can report aggregated losses from
bounded counters. Failed batches must not grow an unbounded retry backlog.

Costs and lifecycle obligations:

- **WAL can leave three files at rest** — `usage.db`, `usage.db-wal`, and
  `usage.db-shm`. ADR-0017 accounts for all of them. On Unix,
  create a private parent directory (`0700`), then pre-create a missing database
  with `os.OpenFile` using `O_CREATE|O_EXCL|O_RDWR` and mode `0600`, closing that
  handle before SQLite opens it. Do not delegate main-file creation to SQLite's
  umask-dependent defaults or truncate an existing database. If another opener
  wins creation, follow the existing-file validation path.
  Validate existing main-file modes and the private parent before opening SQLite;
  never follow an unexpected non-regular main-file destination. `os.MkdirAll`
  does **not** tighten an existing directory. Refuse an unsafe shared parent
  rather than silently chmod it; the private parent protects SQLite-created
  sidecars independently of their mode. Windows permissions
  need the same explicit best-effort caveat as the GitHub OAuth token file;
  Unix modes are not a Windows ACL guarantee.
- **The ~1s timer is a flush target, not a loss bound.** A hard process kill loses
  whatever is still queued or uncommitted, which can exceed one second under
  backlog. WAL with `synchronous=NORMAL` also does not guarantee that recent
  committed transactions survive an OS crash or power loss.
- **Shutdown stops admission and tolerates late producers.** Drain HTTP and
  WebSocket work before the final flush when possible, but forced shutdown may
  leave a hook in flight. `Record` racing with or following the atomic cutoff
  safely drops/counts and never sends on a closed channel. Finalization follows
  the accepted bounded protocol below, never an unqualified `defer store.Close()`.

### 9.1 Accepted shutdown and final-loss protocol

1. `Server.Run` first completes its existing HTTP/WebSocket drain or force-close
   sequence under `ShutdownTimeout`. Meter admission remains open throughout so
   completing hooks can still submit Turns.
2. Immediately after `Server.Run` returns, the composition root atomically cuts
   off admission. A racing or later `Record` returns promptly, increments the
   `late_after_cutoff` loss count, and cannot send to a closed channel.
3. Finalization receives a **fresh**
   `context.WithTimeout(context.Background(), cfg.ShutdownTimeout)`. The current
   default therefore permits about twenty seconds of coordinator wait in total:
   up to ten seconds for server drain plus up to ten seconds for finalization.
4. The single writer owns final queue draining, bounded batches, and native
   cleanup. Before every potentially blocking native stage—including transaction
   acquisition, write, and commit—it recomputes the remaining monotonic budget
   and caps native `busy_timeout` to that remainder in addition to passing the
   context. `ExecContext` alone does not preempt the driver's busy wait.
5. A deadline, storage failure, or ambiguous batch result is not replayed. Every
   queued or not-confirmed row is conservatively counted as lost; only a batch
   confirmed committed is excluded from loss.
6. After the bounded native-cleanup wait, serialize one terminal logging
   handoff: an earlier runtime record finishes before the final record, or is
   suppressed if final publication seals logging first. Snapshot counters
   immediately before emitting the final aggregate, while the logger remains
   alive, including at least queue-full drops, runtime write losses,
   late-after-cutoff drops, final-flush losses, and native-cleanup completion
   status. Calls completed while waiting for native cleanup or the logging gate
   are included; post-snapshot producers cannot be promised inclusion.
7. The coordinator abandons its SQL/native-cleanup wait at the fresh bound.
   Native cleanup may finish in the writer worker; report it unconfirmed if it
   has not completed. Final `slog.Handler` publication is deliberately
   synchronous and is not made deadline-aware by the SQLite context, so a stuck
   configured log destination can extend finalizer return beyond that native
   bound. Arbitrary stuck filesystem or log I/O and operating-system exit cannot
   be guaranteed by a Go deadline. Apply this same finalizer on bind or serve
   failure after opening the store.

---

## 10. Config surface and wiring

Both serve-only settings are available as of #197 and are declared in
`internal/config.serveSpecs` per ADR-0012, using the existing shim-toggle
convention. At the #200 checkpoint they activate buffered recording for both
inference Surfaces and OpenAI SSE/WebSocket recording described in §1; the
remaining Anthropic SSE slice reuses the same settings and store:

| Flag | Field | Default |
| --- | --- | --- |
| `--shim-usage-meter-enabled` | `ShimUsageMeterEnabled` | `false` |
| `--usage-db-path` | `UsageDBPath` | OS-local path below |

A new `defaultUsageDBPath` keeps the database in local application storage:

- Unix: `<os.UserConfigDir()>/copilotd/usage.db`.
- Windows: `%LOCALAPPDATA%\\copilotd\\usage.db`, deliberately **not** the roaming
  `%AppData%` returned by `os.UserConfigDir()` there.
- If the corresponding base directory cannot be resolved, use relative
  `copilotd/usage.db` so flag registration remains usable; enabled store startup
  still validates the destination.

This no longer blindly mirrors `defaultOAuthTokenFile`. Overriding
`--github-oauth-token-file` must not implicitly move the usage database. An operator
whose default directory is network-mounted or synchronized must select a local
`--usage-db-path` (§8.3); the default helper cannot prove filesystem locality.
Directory safety is checked by store startup (§9), not assumed from the path. Neither setting is secret; both log normally through the
descriptor's `logAttr`. Environment and TOML forms derive from the same rows;
`login` gains neither setting.

**No knobs for flush interval or channel depth.** They stay unexported constants —
a setting with no tuning story is a liability, and a later descriptor makes it
cheap to add if a real need appears.

Validation splits by phase (§8.2): the descriptor's `check` does syntactic path
validation at resolution time; open/migrate happens at startup. The store
resolves that validated filesystem destination once to an absolute path and
passes an escaped SQLite file URI with only store-owned driver parameters, so
filename bytes such as `?`, `%`, or `file:` are never reinterpreted as a DSN.

### 10.1 Composition root

`cmd/copilotd/main.go` now builds `configuredShimRegistry(cfg)` once in
`runServe`, logs it, and passes that same registry into `runBoundServe`. Meter
wiring preserves that single-construction boundary:

1. In `runServe`, after configuration/logger and local credential resolution but
   **before `net.Listen`**, open/migrate the store only when enabled. Pass
   `logging.ForComponent(base, "internal/usage/sqlitestore")` as the required
   positional logger to `sqlitestore.Open`; report startup failure through the
   existing `cmd/copilotd` logger and return `errServeFailed`.
2. Build the configured registry once with that sink, log it, and pass that
   registry into `runBoundServe`. Both forwarders receive it; neither opens a
   store. Store initialization must not move into the post-bind startup work.
3. Run the §9.1 finalizer on bind/serve failure as well as normal shutdown,
   keeping the logger alive through final aggregate publication.

Accepted minimal shim interface target:

```go
// A nil sink leaves the usage-meter registration disabled.
func CanonicalRegistry(sink usage.Sink) Registry
```

`configuredShimRegistry` gains a sink argument and a `usage-meter` enable case;
that case requires both the flag and a non-nil sink. Production must fail rather
than silently supply nil when metering was requested. No new logger-bearing
`Deps` struct is needed: parsers do no logging and the store owns loss reporting.
An added logger-bearing seam would need a positional logger under ADR-0015,
not the old draft's optional `Deps.Logger`.

Use a nil **interface**, not an interface holding a typed-nil store, when disabled.
Unit tests can inject an in-memory sink without importing SQLite. `login` and
meter-off `serve` do not open a database, but the single `copilotd` binary (and
composition-root test binary) still links the driver's dependency graph. Runtime
flags cannot remove linked code.

### 10.2 Wiring and test gates

- `internal/config/config_test.go`: default oracle, exact non-secret log-key
  set, flag/env/TOML precedence, validation, and serve/login separation.
- Every `CanonicalRegistry()` and `configuredShimRegistry()` caller, including
  `internal/shim`, `internal/forward`, `internal/wsforward`, and the command
  capstone tests; assertions on registration names, scope, and innermost order.
- `runBoundServe` callers in `serve_e2e_test.go` and
  `impersonation_lifecycle_test.go`: supply the already-configured registry.
- `internal/logging/structure_test.go`: closed inventories for new log keys,
  Component sinks, and any changed base-logger propagation. The writer's logger
  belongs to `internal/usage/sqlitestore`, not `cmd/copilotd` or `internal/shim`.

---

## 11. Costs and known assumptions

### 11.1 Enabling the meter changes non-streaming buffering

In `internal/forward`, a `BufferedTransformer` opts **every non-SSE,
identity-encoded response** into whole-body buffering, including errors and
non-JSON bodies. Parsing cannot opt out of the read that already happened.
`upstream.Caller.ReadBounded` owns the limit from `MaxBufferedResponseBytes`.

With no other buffered shim enabled, an over-cap response passes through with the
meter off and returns **502** with it on. The authoritative over-cap branch is
`internal/upstream.ReadBounded`; its ownership is governed by
[ADR-0013](../adr/0013-govern-authenticated-upstream-calls-in-internal-upstream.md).
There is no remaining 413-versus-502 rollout dependency. Buffered read failure is
502, timeout is 504, and client cancellation produces no new error response.
Buffering also delays response commitment until the read finishes and recomputes
`Content-Length`, even though the meter leaves the payload unchanged. These costs
are accepted by this design, not excused by assuming responses are small.

The Upstream call explicitly requests `Accept-Encoding: identity`, and
`forward.NewClient` disables automatic compression handling. If Copilot still
returns unsupported encoding, a non-SSE body bypasses the buffered hooks and is
not metered; an SSE response is rejected with 502 before any event hook. The
current identity predicate accepts an absent encoding header or one trimmed,
case-insensitive `identity` value, not arbitrary lists/repetitions.

HTTP streaming is selected by the Endpoint's SSE capability and the upstream
response Content-Type, not merely the inbound `stream` field. A JSON error in
answer to a streaming request therefore takes the non-SSE path.

### 11.2 Neither exhaustive consumption nor a clean-transport ledger

Failed, incomplete, cancelled, or unparseable responses may consume tokens without
a qualifying completion. Queue/storage failure can also lose a valid observation.
Conversely, a completion can be recorded before downstream delivery fails, and
duplicate qualifying events can produce duplicate observations (§6).

A clean SSE transport outcome means a recognized terminal was delivered; that
terminal can be `response.failed`, `response.incomplete`, or `error`, not just
`response.completed`. A WebSocket session can fail after many earlier successful
completions. Application success, usage availability, and transport outcome are
three different facts.

`internal/requestsummary` now collects terminal facts, and
`internal/server.accessLog` emits the sole terminal request summary after the
handler returns. SSE outcomes also feed `StreamOutcomeCounter`; WebSocket facts
summarize the session, not each inference turn. These observations aid diagnosis
but **cannot recover missing token counts or identify every omitted WebSocket
turn**. A handler that never returns has no terminal access record. Do not treat
logs plus the database as a complete accounting system.

### 11.3 Accepted usage-schema evidence and limitations

The dated [native usage evidence](../research/2026-09-05-native-usage-shapes.md)
contains three sanitized recorded Copilot fixtures: buffered, SSE, and WebSocket
OpenAI Responses completions. Each completion carries its own ID, returned model,
completed status, and usage. All six approved scalar counts occur on every path.
Primary Responses sources establish that `cached_tokens` and
`cache_write_tokens` are subsets of complete `input_tokens`; the recorded zero
cache writes prove field presence on those requests, not non-zero Copilot cache
accounting. The older item-ID stabilizer fixtures remain non-evidence for usage.

The 2026-09-05 account had no working Anthropic Messages model, so no live
Copilot Anthropic fixture exists. The explicitly approved replacement evidence is
the exact official [Messages Create](https://platform.claude.com/docs/en/api/messages/create)
reference, related primary streaming/cache documentation, and clearly labeled
generated fixtures. They support the seven scalar fields in §5 and the cumulative
last-value parser contract. Anthropic documents `thinking_tokens` as a re-tokenized
output subset, including delimiters, whose reported value is at most
`output_tokens`; subtraction only approximates non-reasoning output. This states
the field's native meaning, not an additional cross-field parser validation rule.
Live Copilot Anthropic compatibility remains unverified.

Beta variable-cardinality `usage.iterations[]` is explicitly excluded from
migration 1 pending a separate schema/cardinality review. Compaction iteration
counts are not included in top-level usage, so this meter does not claim exhaustive
consumption. An independent schema/accounting review found no blocking issue with
the six-plus-seven projection. Subject to the recorded limitations, ADR-0018
freezes migration 1; no provider billing behavior is inferred from these native
counts.

---

## 12. Testing strategy

### `internal/usage/sqlitestore`

- Migration ladder: empty → v1 sets `user_version`; reopen is a no-op;
  `user_version > len(migrations)` refuses to open and names both numbers.
  Concurrent same-version fresh openers handle WAL-activation contention as well
  as serializing the version check and migrations. Exercise native immediate
  `SQLITE_BUSY`, sequential setup/acquisition stages sharing one shrinking budget,
  budget exhaustion, and non-contention errors.
  A failed migration rolls back all pending changes and the version bump.
- Round-trip per table asserting **`NULL` survives as `NULL` and does not collapse
  to `0`**; inbound correlation and upstream message/response IDs stay distinct;
  reused request IDs and duplicate upstream IDs do not prevent another row.
- `STRICT` rejects non-convertible text in an integer column; parser tests enforce
  the stronger native-number contract. `at_utc` renders a known `at_ms` correctly.
- Batching flushes on timer, fill, and shutdown; write failures count loss without
  killing the writer or growing an unbounded backlog. Contending writers wait
  only up to the busy timeout; exhausted contention follows the phase-specific
  failure policy, while a simultaneous external reader remains supported.
- **A full channel drops rather than blocks**: hold the writer, exceed capacity,
  assert prompt `Record` return and counted drops without hook-side logging.
- Concurrent producers, immutable optional values across meter reset, and
  `Record` racing with/following close are race-safe. Final flush obeys the §9.1
  shutdown bound, including a slow/failing database.
- Pre-creation is private even under a permissive Unix umask and never truncates
  existing data; concurrent creation, unsafe existing destinations, and sidecars
  follow §9. Test Windows behavior separately rather than asserting Unix modes
  there. Test OS-specific defaults and the unresolved-base fallback.

### `internal/shim`

Use dedicated usage fixtures (§11.3) and an in-memory sink:

- Multiple Anthropic deltas use the **last reported value per field**, never a
  sum; omitted fields and explicit null updates preserve start values. Missing
  core counts differ from reported zero. Missing start usage may be completed by
  later updates.
- No terminal, upstream error, malformed relevant payload, type disagreement, or
  conflicting Anthropic start → no submission. Validate stop payloads too.
- SSE data extraction covers multiline data, CRLF, absent/empty fields, and
  advisory event names. Every emitted `Raw` is byte-identical to its input.
- OpenAI completions are self-contained: interleaved event sequences cannot mix
  counters; failed/incomplete/malformed responses do not affect a later valid
  completion. Repeated qualifying completions have the documented non-deduplicated
  behavior and successive submission ordinals.
- WebSocket sessions use captured handshake correlation and bounded state; a
  missing construction request ID produces an empty correlation field, not a
  fabricated one. Assert Message kind/data identity as well as `emit=true`.
- Buffered bodies use the payload acceptance predicate (§4.3), skip error or
  incomplete shapes, return nil, and never mutate input bytes.
- The registration stays last/innermost, scoped to the two inference Routes,
  disabled without a sink, and covered by the existing post-commit monitor.

### Integration and composition root

Use the existing server integration and `cmd/copilotd` serve-test seams with a
fake Copilot upstream and a temporary database:

- Meter off creates no files or writer and adds no behavior. Meter on persists one
  row per valid fixture completion across buffered/SSE on both inference Surfaces
  and WebSocket on OpenAI only. Catalogs and `count_tokens` remain excluded.
- Startup open/migration failure happens before bind; the same injected sink
  reaches both forwarders. Clean shutdown flushes accepted rows.
- Disconnect **before** completion reaches the meter → no row. Completion observed
  before a downstream write failure or outer-shim rejection → a row may remain.
  Assert the returning handler's single access summary without claiming recovery
  of absent token counts.
- Failed/incomplete SSE terminals can produce a clean transport outcome with no
  usage row. A WebSocket session error preserves earlier completion rows.
- With other buffered hooks disabled, an over-cap response passes through with
  the meter off and 502s with it on. Cover delayed commit, recomputed length,
  non-SSE error bodies, classified read failures/timeouts, and cancellation.
- Non-identity buffered responses bypass metering; unsupported-encoding SSE is
  rejected before hooks. Exercise metering with the item-id stabilizer enabled
  to prove innermost observation and unchanged payload behavior compose.

Run Go checks through `nix develop` per `AGENTS.md`, including the race suite;
`nix flake check` verifies the complete local build/check set. Separately verify
all four cgo-free release targets with the chosen SQLite driver (§13).

---

## 13. Docs, ADRs, and dependency

### Accepted ADRs

- [ADR-0017](../adr/0017-persist-usage-in-local-sqlite.md) accepts the
  usage-specific private SQLite exception, rejected alternatives, cgo-free
  driver, platform evidence limits, file permissions, WAL sidecars,
  best-effort durability, and bounded finalization.
- [ADR-0018](../adr/0018-store-per-surface-native-usage.md) freezes the
  per-Surface verbatim projection, both cache-nesting examples, schema evolution
  rule, and read-only observer admission without a new Shim subtype.

### Reconciled and staged docs

`CONTEXT.md` defines Shim, Usage meter, and Turn without embedding this
implementation plan. As of #200, README and `CONFIGURATION.md` describe the
available opt-in database and settings, explicitly limit implemented coverage to
buffered Anthropic Messages plus buffered, SSE, and WebSocket OpenAI Responses,
and state durability, filesystem, buffering, retention, backup, external-query,
and shutdown consequences. The existing
`docs/divergence-ledger.md` copilotd-originated error row already covers the
meter-activated bounded-read `BadGateway`/`GatewayTimeout` Fabrications;
observation itself adds no usage-rewriting Alteration (§4.2).

### Dependency

ADR-0017 selects `modernc.org/sqlite v1.58.0`, embedding SQLite 3.53.4 with the
required matching `modernc.org/libc v1.75.6`. The retained feasibility module
builds a reachable driver path with `CGO_ENABLED=0` for `linux/amd64`,
`windows/amd64`, `windows/arm64`, and `darwin/arm64`; only Linux has runtime,
filesystem, contention, and race evidence. Windows/Darwin runtime behavior and
Windows ACLs remain accepted limitations, not certification. Issue #197 pins the
dependency in root `go.mod`/`go.sum` and updates the Nix vendor hash. Its
completion gate builds the actual feature-bearing `copilotd` binary—not only the
disposable probe—with `CGO_ENABLED=0` for all four
current release targets, and `flake.nix`'s `vendorHash` must match the root module
dependency graph. The preceding #196 architecture checkpoint added neither
production code nor a root dependency; #197 does.

---

## 14. Approved architecture gates

The four pre-implementation gates are explicitly closed:

1. **Persistence:** the narrow opt-in SQLite exception and its best-effort
   durability, local-filesystem, sidecar, permission, and platform limitations
   are accepted by ADR-0017.
2. **Shim language:** the existing Shim definition admits read-only observers;
   no observer subtype is introduced. README and package policy use the same
   broader wording.
3. **Projection:** migration 1 has six OpenAI and seven Anthropic scalar counts,
   including inclusive `cache_write_tokens` and nullable `thinking_tokens`.
   Native values, nil/zero, required core fields, schema evolution, and the
   `usage.iterations[]` exclusion follow ADR-0018 and §11.3.
4. **Shutdown:** one fresh `ShutdownTimeout` finalization extension follows the
   existing drain/force-close sequence, with atomic cutoff, late-loss counting,
   no ambiguous-batch replay, writer-owned cleanup, bounded coordinator wait,
   and observed-through-publication final reporting (§9.1).

These approvals authorized staged implementation. Issue #197 supplies the shared
contract, production SQLite dependency and writer, both settings, and the
buffered OpenAI hook; issue #198 adds the buffered Anthropic hook without changing
migration 1; issue #199 adds self-contained OpenAI SSE completion observation;
and issue #200 adds the same self-contained observation to OpenAI WebSocket
server Messages. Anthropic SSE parsing remains the later stage and must not be
inferred from the frozen schema or final-target sections.
