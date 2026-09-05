# Token usage meter — Design

**Status:** proposed
**Date:** 2026-07-26

This proposal has not amended the [state-at-rest boundary](../../README.md#state-at-rest).
The proposed ADR-0015 and ADR-0016 numbers below are historical placeholders;
those numbers were later used for logging and Hook overrun decisions. Any
persistence exception would require a new accepted ADR.

A shim-hosted, opt-in meter that records one row per completed inference turn to a
local SQLite database, capturing each inbound Surface's **native** token-usage
fields verbatim. Single-user instance; multi-user is out of scope. In-process
query is out of scope — the database exists so external tooling (`sqlite3`,
Datasette, DuckDB) can read it.

---

## 1. Goal & outcome

Answer "what did I actually consume, on which model, when" without leaving the
proxy and without a second process.

_Outcome:_ with `--shim-usage-meter-enabled`, every completed turn on
`(Anthropic, /v1/messages)` and `(OpenAI, /responses)` — across all three
transports — lands as one row in `<os.UserConfigDir()>/copilotd/usage.db`. With
the flag off (the default) nothing is created, nothing is written, and the wire
is untouched.

Required fields, from the request:

| Asked for | Delivered as |
| --- | --- |
| (a) turn/session time | `at_ms` + generated `at_utc`; `turn_index` orders turns within a WebSocket session |
| (b) model id | `model`, as reported upstream |
| (c) input token | `input_tokens` — **per-Surface semantics, see §7.1** |
| (d) output token | `output_tokens` |
| (e) cache create | `cache_creation_input_tokens` (+ TTL split) / `cache_write_tokens` |
| (f) cache read | `cache_read_input_tokens` / `cached_tokens` |
| (g) provider by endpoint | the **table name** — one table per Surface |
| (h) anything else | `request_id`, `transport`, `turn_index`, `reasoning_tokens`, cache TTL split |

---

## 2. Problem

Three facts make this harder than "add a counter."

**The two Surfaces nest cache tokens in opposite directions.** Anthropic's
`input_tokens` is the *uncached remainder* — real input is
`input_tokens + cache_creation_input_tokens + cache_read_input_tokens`. OpenAI's
`input_tokens` is the *complete* count, with `input_tokens_details.cached_tokens`
a subset already inside it. Two turns with identical real consumption therefore
write `12` and `8012` into a naively-shared `input_tokens` column.

**Streaming usage is cumulative and optional.** The Messages API spec states the
`usage` on `message_delta` is *cumulative*, and that there may be **one or more**
`message_delta` events. Summing them double-counts. Separately, `usage` can be
absent entirely — the spec's thinking example shows a `message_start` with no
`usage` object at all — so absence must be tolerated, not assumed impossible.

**Shim hooks may not do I/O.** `internal/shim`'s package contract binds SSE and
WebSocket hooks to "prompt and non-blocking: CPU-bound transformation only, with
no I/O or waiting." A hook that writes to SQLite would violate it and could stall
the SSE pump.

---

## 3. Decisions & non-goals

### Decisions

1. **Persist to SQLite** via pure-Go `modernc.org/sqlite`. Would amend the
   [state-at-rest boundary](../../README.md#state-at-rest) if this proposal is
   accepted (see the proposed persistence ADR in §13).
2. **Only completed turns are recorded.** A turn that never delivers a
   usage-bearing terminal event writes nothing.
3. **One table per Surface, fields verbatim.** No unified columns, no derived
   totals. Interpretation lives in Go source (ADR-0016).
4. **Capture every token-count field the Surface's spec defines**, including
   subsets. Dropping a spec field is itself an interpretation.
5. **The meter is a pure observer.** Every hook returns its input unchanged;
   wire output is byte-identical.
6. **Opt-in, off by default**, like every other shim.
7. **Versioned schema** via `PRAGMA user_version`, forward-only migrations.

### Non-goals

- **Query.** No API, no CLI subcommand, no aggregation. External tools read the file.
- **Cost/pricing.** No rates, no currency. Copilot bills in *premium requests*,
  not tokens; a per-model row count (`SELECT model, COUNT(*) …`) is the closer
  proxy for billing and falls out of the schema for free.
- **Retention.** No pruning. A single user generates thousands of rows a year.
- **Multi-user / per-key attribution.** Explicitly out of scope.
- **Metering `count_tokens`.** `(Anthropic, /v1/messages/count_tokens)` reports an
  estimate, not consumption. Out of scope.
- **Non-token usage fields.** `service_tier` (a billing-tier label) and
  `server_tool_use.web_search_requests` (a request count) are in the Messages
  API `usage` object but are not token counts. Excluded by decision 4's wording.
  A migration adds them cheaply if Copilot turns out to emit them.

---

## 4. Placement: an observer shim

### 4.1 Why the shim seam

Two alternatives were considered and rejected:

- **A tap inside `sse.Pump`.** Rejected: ADR-0002 makes the SSE engine
  deliberately payload-opaque. Teaching it to read `usage` breaks that.
- **HTTP middleware.** Rejected: it cannot see inside an SSE stream without
  re-parsing bytes that have already been written downstream.

The shim seam already spans all three transports (buffered, SSE, WebSocket)
through one registry, and is already the sanctioned home for typed payload logic.

### 4.2 It is an observer, and that is new

Every existing shim exists to change something. The meter changes nothing: each
hook returns its input unchanged and `emit=true`. This is safe by construction on
the SSE path because `sse.Pump` writes `frame.Raw` verbatim (`pump.go:136`) and
`Frame.Raw` is documented as authoritative — a transformer that returns the frame
it was given produces byte-identical output.

Consequently the meter earns **no divergence-ledger entry**. This was checked
deliberately: `docs/divergence-ledger.md` enumerates departures from verbatim
forwarding, and there is none here. The ledger is left unchanged.

### 4.3 Hooks

Three, not four. `StreamFinalizer` is unnecessary: `Chain.StreamAdapter()`
includes an instance that implements `EventTransformer` *or*
`StreamFinalizer`, and the meter holds no frames and records only completed
turns, so the terminal event itself is the completeness signal.

| Transport | Hook | Record fires |
| --- | --- | --- |
| Buffered JSON | `BufferedTransformer` | once, on the single call |
| SSE | `EventTransformer` | on `message_stop` / `response.completed` |
| WebSocket | `ServerMessageTransformer` | on each `response.completed` |

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

- **`internal/usage`** — types only, **zero dependencies**.
- **`internal/usage/sqlitestore`** — writer goroutine, schema, migrations.
  Imports `usage` and `modernc.org/sqlite`.

So `shim` → `usage` only. **The SQLite dependency never enters the shim package
or its test binary.**

```go
// internal/usage — no dependencies.
package usage

// Sink receives one completed turn. Record must not block: it is called from
// shim hooks, including those running inside the SSE pump and the WebSocket
// server pump, which the shim contract binds to a prompt, non-blocking
// obligation.
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
	At        time.Time
	RequestID string
	Model     string    // as reported upstream, never the client's requested name
	Transport Transport
	TurnIndex int       // flush ordinal within the shim instance
	Usage     Usage
}

// Usage is a closed sum: only the two Surface-native records satisfy it.
type Usage interface{ isUsage() }

// AnthropicUsage mirrors Messages API usage verbatim.
//
// InputTokens is the UNCACHED REMAINDER. Real input is
// InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// The Ephemeral* fields are the TTL split *inside* CacheCreationInputTokens.
// A nil pointer means upstream did not report the field; a zero value means
// upstream reported zero.
type AnthropicUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
	Ephemeral5mInputTokens   *int64
	Ephemeral1hInputTokens   *int64
}

// OpenAIUsage mirrors Responses API usage verbatim.
//
// InputTokens is the COMPLETE count; CachedTokens and CacheWriteTokens are
// subsets already inside it. OutputTokens is the COMPLETE count;
// ReasoningTokens is a subset already inside it. Real input is InputTokens.
type OpenAIUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CachedTokens     *int64 // input_tokens_details.cached_tokens
	CacheWriteTokens *int64 // input_tokens_details.cache_write_tokens (newer models)
	ReasoningTokens  *int64 // output_tokens_details.reasoning_tokens
	TotalTokens      *int64
}
```

Those two doc comments are where the nesting asymmetry is recorded — stated once,
in Go, adjacent to the fields they govern. That is the whole of "interpretation
lives in Go source."

---

## 6. Record boundary and instance lifecycle

**Row cadence is uniform: one row per completed turn, on every transport.**
Instance lifetime is not, and that difference is a framework fact:

- `forward.go:137` builds a chain **per request** — an HTTP instance sees one turn.
- `wsforward/proxy.go:204` builds one **per session** — a WS instance sees many.
- `shim.go:21` states it: "per-request on the HTTP path but per-session on the
  long-lived, multi-turn WebSocket path."

**Flush-then-reset is the adapter between those lifetimes.** Without it a session
would produce one bloated row with summed counters instead of one row per turn.
It also satisfies `shim.go:23`'s obligation on any shim spanning both transports
("must not assume a request-scoped lifetime and must bound any per-turn
accumulation") — meter state is fixed-size (a few `int64`s and two strings) and
zeroed at every flush, so memory is flat regardless of session length.

`TurnIndex` is simply the flush ordinal within the instance: normally `0` on
HTTP, counting up on WebSocket.

**`RequestID` is captured once at construction**, so on WebSocket every turn in a
session shares one id. `(request_id, turn_index)` is what identifies a WS turn.

### 6.1 Two meters, not one

`Registration.New` already receives the Surface, so it selects a type rather than
branching inside one:

```go
{
	Name: "usage-meter", Enabled: false,
	Scope: func(s endpoint.Surface, r endpoint.Route) bool {
		return (s == endpoint.Anthropic && r == endpoint.RouteAnthropicMessages) ||
			(s == endpoint.OpenAI && r == endpoint.RouteOpenAIResponses)
	},
	New: func(ctx context.Context, s endpoint.Surface, _ endpoint.Route) any {
		if s == endpoint.Anthropic {
			return newAnthropicUsageMeter(ctx, deps)
		}
		return newOpenAIUsageMeter(ctx, deps)
	},
},
```

Each owns one spec's parser and nothing else. The Anthropic SSE half is the whole
shape:

```go
func (m *anthropicUsageMeter) TransformEvent(_ context.Context, f sse.Frame) []sse.Frame {
	switch f.Type {
	case "message_start": m.absorbStart(f.Raw)  // input + cache fields, and the message id
	case "message_delta": m.absorbDelta(f.Raw)  // CUMULATIVE: last writer wins, never summed
	case "message_stop":  m.flush()             // Record, then reset
	}
	return []sse.Frame{f} // unchanged on every path
}
```

`absorb*` are the only places that unmarshal and both are gated on `f.Type`, so a
text-delta-heavy stream costs one string compare per frame. A parse failure sets a
poison flag that suppresses the flush: a malformed turn is dropped, never
half-recorded, and no error reaches the wire.

### 6.2 Sequential-turn assumption, and its guard

The single-slot state assumes turns are strictly sequential within an instance.
That is **guaranteed** on HTTP (one request, one turn) and **assumed** on
WebSocket, where Copilot's behavior for a second `response.create` issued before
the first completes is unverified.

The assumption is guarded rather than trusted. The slot holds the turn's upstream
id (`message.id` / `response.id`); before absorbing any id-bearing event:

```go
if m.open && id != m.id {
	m.warnOnce(ctx, "usage meter: overlapping turns on one instance",
		slog.String("slot_id", m.id), slog.String("event_id", id))
	m.poisoned = true
	return
}
```

- **Drop, don't guess.** A poisoned slot records nothing, preserving the invariant
  that every stored row is trustworthy. A missing row is recoverable through the
  access log; a row silently merging two responses is not.
- **Ids only, never payload.** Bodies carry prompt content; CONTEXT.md's rule is
  that logs never echo secrets.
- **`WarnContext`, once per instance.** `Warn` matches `accessLog`'s treatment of
  non-clean outcomes; `WarnContext` lets the logging handler attach `request_id`
  automatically. Once-per-instance keeps a pathological session from flooding —
  this is a discovery signal, not a measurement.
- **Covers HTTP for free.** Overlap is structurally impossible on a per-request
  instance, so a fire there means Copilot emitted something genuinely unexpected.

Because `turn_index` is the flush ordinal, an anomalous second HTTP flush still
produces a unique `(request_id, turn_index)` rather than a duplicate.

---

## 7. Schema

Two independent tables. **The table name is the Surface column** — there is no
`surface` column, and no `route` column, since each Surface has exactly one
usage-bearing Route.

### 7.1 DDL (migration 1)

```sql
CREATE TABLE anthropic_turn (
  id                          INTEGER PRIMARY KEY,
  at_ms                       INTEGER NOT NULL,
  at_utc                      TEXT GENERATED ALWAYS AS (
                                strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                              ) VIRTUAL,
  request_id                  TEXT    NOT NULL,
  turn_index                  INTEGER NOT NULL,
  model                       TEXT    NOT NULL,
  transport                   TEXT    NOT NULL CHECK (transport IN ('buffered','sse')),
  input_tokens                INTEGER NOT NULL,  -- UNCACHED REMAINDER
  output_tokens               INTEGER NOT NULL,
  cache_creation_input_tokens INTEGER,           -- additive to input_tokens
  cache_read_input_tokens     INTEGER,           -- additive to input_tokens
  ephemeral_5m_input_tokens   INTEGER,           -- subset of cache_creation_input_tokens
  ephemeral_1h_input_tokens   INTEGER            -- subset of cache_creation_input_tokens
) STRICT;
CREATE INDEX anthropic_turn_at ON anthropic_turn(at_ms);

CREATE TABLE openai_turn (
  id                 INTEGER PRIMARY KEY,
  at_ms              INTEGER NOT NULL,
  at_utc             TEXT GENERATED ALWAYS AS (
                       strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')
                     ) VIRTUAL,
  request_id         TEXT    NOT NULL,
  turn_index         INTEGER NOT NULL,
  model              TEXT    NOT NULL,
  transport          TEXT    NOT NULL CHECK (transport IN ('buffered','sse','websocket')),
  input_tokens       INTEGER NOT NULL,  -- COMPLETE count
  cached_tokens      INTEGER,           -- subset of input_tokens
  cache_write_tokens INTEGER,           -- subset of input_tokens; newer models only
  output_tokens      INTEGER NOT NULL,  -- COMPLETE count
  reasoning_tokens   INTEGER,           -- subset of output_tokens
  total_tokens       INTEGER
) STRICT;
CREATE INDEX openai_turn_at ON openai_turn(at_ms);
```

### 7.2 Why each choice

- **`STRICT`.** SQLite enforces declared column types instead of coercing. For a
  verbatim ledger that is exactly right: a bug writing a string into a token
  column fails loudly.
- **Divergent `transport` CHECKs.** WebSocket is OpenAI `/responses`-only
  (ADR-0006), so the Anthropic table forbids it structurally, not by convention.
- **Nullable detail columns, NOT NULL core columns.** `NULL` means *upstream did
  not report the field*; `0` means *upstream reported zero*. That distinction
  answers a live question — does Copilot's Anthropic passthrough report cache
  fields at all? `DEFAULT 0` would destroy the answer permanently. Core token
  columns stay NOT NULL because a turn missing them is malformed and dropped.
- **`at_ms` integer + `at_utc` virtual.** Integer is canonical, 8 bytes, needs no
  format discipline, and supports direct arithmetic. Its one footgun is that
  `datetime(x,'unixepoch')` assumes *seconds*, so a millisecond column silently
  renders as year 55000; the unit in the column name removes the ambiguity, and
  the `VIRTUAL` generated column costs zero bytes at rest while making
  `SELECT at_utc, model FROM openai_turn` readable with no conversion to
  remember. Millisecond precision because WebSocket turns can share a second.

  _To verify during implementation:_ the exact `strftime` formulation above
  passes a REAL to the `unixepoch` modifier to keep sub-second precision, and
  support for fractional `unixepoch` values varies by SQLite version. Pin it
  against the chosen `modernc.org/sqlite` release. If fractional values are not
  supported there, render at second precision
  (`datetime(at_ms/1000, 'unixepoch')`); `at_ms` retains full precision either
  way, since `at_utc` is a convenience projection and never the stored truth.
- **No `UNIQUE(request_id, turn_index)`, despite appearances.**
  `logging.ResolveRequestID` **honors a well-formed inbound `X-Request-Id`**
  (`middleware.go:23`), so request-ids are client-influenceable and are not a key.
  A unique constraint would silently drop legitimate rows whenever a client reused
  an id. Index, not constraint.

### 7.3 The subset rule

Every token-count field a Surface's spec defines is captured, subsets included,
because omitting one is an interpretation and interpretation belongs in Go. This
supersedes an earlier draft rule ("track a subset only when priced differently")
that was a workaround for a unified schema — split tables removed the ambiguity
that rule existed to manage.

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

Open sequence: read `user_version`, apply every migration beyond it, each in its
own transaction, forward-only. **No down migrations** — this is a single-user local
file; rollback is restoring a copy.

**`user_version > len(migrations)`** means an older copilotd opened a file a newer
one wrote. Fail closed with an error naming both numbers. Never write against a
schema the binary does not understand.

### 8.2 Failure policy, split by phase

Follows CONTEXT.md's readiness rule ("the real serve lifecycle fails before
binding when a local prerequisite is missing"):

- **Startup** — meter enabled but the database cannot be opened or migrated →
  **fail before binding**, stating the reason. Metering was explicitly requested;
  silently not metering is worse than not starting.
- **Runtime** — write failure (disk full, I/O error) → **log and keep serving**.
  The meter must never take down the proxy.

---

## 9. Writer and durability

Buffered channel (1024) → one writer goroutine → batched insert in a single
transaction, flushed on a ~1s timer, on batch fill, and on shutdown.
`journal_mode=WAL`, `synchronous=NORMAL`.

**Channel full → drop the record and count the drop. Never block.** The SSE pump
stalling on a slow disk would be a far worse defect than a missing meter row.

Two honest costs:

- **WAL means three files at rest, not one** — `usage.db`, `-wal`, `-shm`. ADR-0015
  states this. Mitigation follows `identity/tokenfile.go`: the parent directory is
  `0700`, so sidecars SQLite creates under the process umask are protected by the
  directory regardless of their own mode.
- **Up to ~1s of records lost on hard kill.** The correct trade for a meter; the
  alternative is an fsync in the path of every turn.

---

## 10. Config surface and wiring

Two settings, declared as typed descriptors in the `config.go` table per ADR-0012,
named to match the existing shim toggle:

| Flag | Field | Default |
| --- | --- | --- |
| `--shim-usage-meter-enabled` | `ShimUsageMeterEnabled` | `false` |
| `--usage-db-path` | `UsageDBPath` | `<os.UserConfigDir()>/copilotd/usage.db` |

The default path mirrors `defaultGitHubOAuthTokenFile` (`config.go:377`),
including its fallback when `os.UserConfigDir()` fails, so the database lands
beside the token file in the same owner-only directory. Neither value is secret;
both log normally through the descriptor's `logAttr`.

**No knobs for flush interval or channel depth.** They stay unexported constants —
a setting with no tuning story is a liability, and a later descriptor makes it
cheap to add if a real need appears.

Validation splits by phase (§8.2): the descriptor's `check` does syntactic path
validation at resolution time; open/migrate happens at startup.

### 10.1 `main.go`

Currently `:397–403`. Becomes:

```go
store, err := sqlitestore.Open(cfg.UsageDBPath)   // only when enabled; fails before bind
registry := shim.CanonicalRegistry(shim.Deps{Usage: store, Logger: logger})
// the existing name-based Enabled loop gains a "usage-meter" case
defer store.Close()                                // drains the channel, final flush
```

```go
// internal/shim
type Deps struct {
	Usage  usage.Sink   // nil leaves the meter permanently disabled
	Logger *slog.Logger
}
func CanonicalRegistry(deps Deps) Registry
```

A `nil` sink leaves the registration present but disabled, so `login` and every
test construct nothing and never link a database.

### 10.2 Call sites this breaks

Fixed as part of the change, not discovered later:

- `internal/config/config_test.go` hardens descriptor invariants (commit
  `8591114`) — two new descriptors need entries.
- Every `CanonicalRegistry()` caller takes the new signature, including
  `wsforward/session_test.go:1092`, plus any test asserting registry contents or
  ordering.

---

## 11. Costs and known assumptions

### 11.1 Enabling the meter changes non-streaming buffering

Implementing `BufferedTransformer` opts the response into whole-body buffering:
`forward.go:406` streams the body with `io.Copy` unless a buffered hook exists,
and otherwise reads it under `maxBufferedResponseBytes`. So a non-streaming
response **above the cap passes through with the meter off and is rejected with it
on**. Messages and Responses non-stream bodies are small and the meter is opt-in,
so this is accepted — but it is a real, user-visible consequence, and it gets a
regression test (§12).

The rejection status depends on which design lands first. Today `forward`'s
buffered branch returns 413; the
[upstream call concentration](2026-07-26-upstream-call-concentration-design.md)
changes it to 502 (its behaviour change 3, on the grounds that 413 describes the
inbound request entity, not an upstream response), and that design is ordered
**before** this one. So the expected status here is **502**.

The same line also skips the buffered hook when the response is not
identity-encoded, so a compressed non-stream response is not metered. The outbound
client sets `DisableCompression: true` (`forward.go:115`), making this rare rather
than impossible.

### 11.2 The table under-reports real consumption

By decision, only completed turns are stored, so cancelled, stalled, and
upstream-errored turns burn tokens upstream and appear nowhere in the database.
This is acceptable because copilotd already records those off-band: `accessLog`
emits outcome and frame counts per request, and `StreamOutcomeCounter` counts them
by surface. The meter is the **clean-turn ledger**; dropped turns stay recoverable
by `request_id`. Both halves are needed for a complete picture, and the design doc
says so rather than letting a reader assume the table is exhaustive.

### 11.3 Sequential turns per WebSocket session

Stated and guarded in §6.2. If the warning ever fires, the fix is to key in-flight
state by `response.id` instead of a single slot.

### 11.4 `cache_write_tokens` may not exist on the Responses shape

The field is documented in OpenAI's prompt-caching guide under the Chat
Completions `prompt_tokens_details` for newer models; its presence in the
Responses `input_tokens_details` is unverified against Copilot. The column is
nullable, so a permanent `NULL` is a valid and informative answer.

---

## 12. Testing strategy

### `internal/usage/sqlitestore`

- Migration ladder: empty → v1 sets `user_version`; reopen is a no-op;
  `user_version > len(migrations)` refuses to open and names both numbers.
- Round-trip per table asserting **`NULL` survives as `NULL` and does not collapse
  to `0`**.
- `STRICT` rejects a wrong-typed write.
- `at_utc` renders a known `at_ms` correctly.
- Batching flushes on timer, on fill, and on `Close`.
- **A full channel drops rather than blocks** — wedge the writer, push past
  capacity, assert `Record` returns promptly and the drop is counted. A meter that
  can stall the SSE pump is worse than no meter.

### `internal/shim`

Table-driven over recorded frames in `testdata/`, mirroring
`responses_item_id_test.go`:

- **Multiple `message_delta` events → last wins, never summed.**
- `message_start` carrying no `usage` at all → no record, no error.
- Stream dies before its terminal → no record.
- WebSocket multi-turn session → N records, `turn_index` 0..N-1, state
  verifiably reset between turns.
- Overlap guard: conflicting id → warn **once**, no record; a second conflict
  logs nothing.
- Malformed JSON in a usage event → poisoned, no record, nothing on the wire.
- **Byte identity**: every hook returns its input unchanged; assert emitted `Raw`
  bytes are identical. The no-divergence claim rests on this, so it is asserted
  rather than assumed.

### Integration

`server_integration_test.go` shape, against a fake Copilot upstream:

- Meter off (default) creates no file and changes no behavior.
- Meter on lands exactly one row per turn on both Surfaces, all three transports.
- Client disconnect mid-stream produces **no row but a normal access-log
  outcome**, proving §11.2's recoverability claim rather than asserting it.
- **Regression:** a non-stream response above `maxBufferedResponseBytes` passes
  through with the meter off and 502s with it on (§11.1).

---

## 13. Docs, ADRs, and dependency

### ADRs

- **ADR-0015 — Persist token usage in a local SQLite database.** Would amend the
  [state-at-rest boundary](../../README.md#state-at-rest) if this proposal is
  accepted. Records: SQLite over JSONL or in-memory; why pure-Go
  `modernc.org/sqlite` is required (cgo breaks the single-static-binary,
  four-target story); WAL meaning three files at rest; opt-in and off by default.
- **ADR-0016 — Per-Surface usage tables with verbatim fields.** The policy a
  future contributor needs: capture each Surface's native token fields verbatim,
  no unified or derived columns, interpretation in Go. Motivated by the cache
  nesting asymmetry, with both worked examples recorded so it is not re-litigated.

### Docs

- **`CONTEXT.md`** — add **Usage meter** and **Turn**; amend **Shim** (see §14).
- **[README state-at-rest boundary](../../README.md#state-at-rest)** — currently
  asserts "no database"; amend only if a persistence ADR for this proposal is
  accepted.
- **`CONFIGURATION.md`** — both settings, per the `c8c373f` precedent.
- **`docs/divergence-ledger.md`** — deliberately **unchanged** (§4.2).

### Dependency

`modernc.org/sqlite` pinned to an explicit version per project practice; the Nix
build's vendor hash updates with it.

---

## 14. Open question

**`CONTEXT.md`'s `Shim` entry defines a shim as "a composable middleware layer
that closes one specific parity gap."** The usage meter closes no parity gap — it
observes. Two ways to resolve, and this is left for the reviewer:

1. **Amend the `Shim` entry** with one sentence admitting read-only observers, and
   add a `Usage meter` entry beneath it. Smallest change; keeps one mechanism with
   one name.
2. **Introduce `Observer shim`** as a named subtype in the glossary, defined as a
   shim that implements hooks but returns every input unchanged. More precise, and
   it gives future observers a term — at the cost of another glossary entry.

Option 1 is the lighter change; option 2 pays off only if more observers follow.
