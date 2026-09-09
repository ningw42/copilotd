# Usage reports over local HTTP with a terminal client

**Status:** agreed design; #207–#210 implement native Anthropic/OpenAI and combined
reports with all four calendar periods, named zones, independent month defaults,
baseline safeguards, and conservative Unix terminal-local timezone discovery
(native Windows explicit-only). #211 adds exact UTF-8 Reported-model filters,
detailed native tables, and validated original-byte CLI JSON. #212 retains
[concurrency/lifecycle evidence](../research/2026-09-08-usage-reporting-concurrency.md),
including the corrected test-client connection-ownership regression. #213 adds
real-executable acceptance and the native verification pipeline. Implementation
is complete; native release certification is a separate, revision-specific gate
tracked on [#213](https://github.com/ningw42/copilotd/issues/213), using the
[verification guide](../verification/usage-reporting.md). Pipeline definitions and
cross-builds do not establish that this gate has passed.
**Date:** 2026-09-07
**Related decision:** [ADR-0019](../adr/0019-serve-unauthenticated-usage-reports.md)
**Tracking epic:** [#206](https://github.com/ningw42/copilotd/issues/206); slice ownership and dependencies are in section 12.

## 1. Outcome and agreed scope

Expose the collected Usage meter history through one daemon-owned reporting
module. `copilotd usage` requests a Usage report over HTTP and renders terminal
tables. The command never opens SQLite, starts a daemon, or falls back to a local
file when the daemon is unavailable.

The maintainer explicitly chose:

1. An **unauthenticated report route on the existing listener**, following
   `serve --addr`, including when that address is publicly reachable.
2. Reads from the daemon's configured usage database. The report covers the
   **database**, including observations from other writers sharing it, not just
   the current process.
3. A `usage` subcommand with daily, weekly, monthly, and yearly grouping by model.
4. `--endpoint` as a **base URL**, defaulting to `http://127.0.0.1:8080`.
5. Daemon-owned aggregation; terminal rendering does not recalculate usage.
6. Distinct disabled, empty, unavailable, and unreachable outcomes. Reporting
   requires the Usage meter to be enabled; it does not force a writer flush.
7. Separate read-only SQLite access, consistent short-lived reads, and bounded
   queries and responses.
8. The **terminal host's local timezone** as the CLI default, with an explicit
   `--timezone` override. Detection failure must request an override, not silently
   select UTC or the daemon's timezone.

The protocol, limits, and layout below describe the implemented contract. The
implementation sequence records ownership; release/platform evidence must still
be checked against the exact revision rather than inferred from this design.

### Non-goals

No browser presentation, charts, watch mode, arbitrary SQL, raw-Turn export,
pricing, retention, new count projection, Requested-model grouping, multi-user
attribution, daemon-instance filtering, separate listener, authentication,
automatic daemon discovery, or offline CLI mode. Existing external SQLite tools
remain supported. HTML and charts can later consume the same report value, but
v1 introduces no extension framework for them.

## 2. Existing contracts and scope change

The [Usage meter design](2026-07-26-token-usage-meter-design.md) and
[ADR-0017](../adr/0017-persist-usage-in-local-sqlite.md) explicitly excluded
built-in querying and aggregation. This proposal deliberately replaces that
scope restriction, not the persistence policy. It does not change opt-in
collection, the private local files, the non-blocking Sink, WAL batching,
best-effort durability, or bounded writer finalization.

[ADR-0018](../adr/0018-store-per-surface-native-usage.md) remains authoritative:
store and aggregate each Surface's counts with their native meanings. Do not
create normalized input columns or infer a provider total. The current two
Turn tables, `at_ms` indexes, and schema version 2 are sufficient; **no migration
is required** for v1 reporting.

The served HTTP path is a local handler, **not an Endpoint, Route, or Surface**
in the project's domain vocabulary: it has no upstream dependency. Do not add a
fake identity to `internal/endpoint` or render errors through an inference
Surface. Like probes, local report responses are not alterations of a forwarded
Copilot response; they do not add a forwarding divergence.

Reporting performs no Exchange, startup/on-demand mint, Catalog lookup, Shim
hook, or readiness lookup. A bound daemon can answer reports during upstream
failure. This does **not** bypass `serve`'s existing pre-bind configuration and
GitHub OAuth token requirements or make offline history accessible without a
running daemon.

## 3. Module and seam

```text
Usage meter -> existing asynchronous SQLite writer
                                      |
                         committed local database
                                      |
                      Usage reporting module
                    (calendar + native aggregation)
                                      |
                            HTTP JSON adapter
                                      |
                   HTTP client -> terminal presentation
```

The external reporting seam returns a **report value**, not SQL rows, a database
handle, or formatted text. Its depth comes from hiding calendar construction,
consistent reads, missing-value accounting, checked arithmetic, ordering, and
query limits. One semantic fix has locality in the reporting module rather than
being repeated in a terminal renderer and a future chart renderer.

Illustrative Go interface; names may change without changing the contract:

```go
// internal/usage/report
func New(databasePath string) *Reporter
func (r *Reporter) Query(ctx context.Context, q Query) (Report, error)

type Query struct {
    Period   Period // day | week | month | year; empty selects day
    Since    string // optional inclusive YYYY-MM-DD
    Until    string // optional exclusive YYYY-MM-DD
    Timezone string // required named timezone; never "Local"
    Surface  string // empty/all | anthropic | openai
    Model    *string // nil = all; otherwise exact Reported model
}
```

`New` captures the absolute, daemon-selected path without opening, creating, or
migrating files. A private constructor accepts a clock for tests; production
uses `time.Now`. `Query` does not retain a transaction, mutable result, or file
handle for its caller to manage. The returned value is independent of later
writes. Errors are typed/report-specific rather than raw driver errors.

Suggested ownership:

| Location | Responsibility |
|---|---|
| `internal/usage/report` (new) | Query/report types; calendar policy; native aggregation; private read-only SQLite implementation; domain errors |
| `internal/usage/reporthttp` (new) | Route constants, handler/client adapters, wire encoding/validation, HTTP admission and limits |
| `internal/usage/reportcli` (new) | Local-timezone resolution, one-shot command orchestration, safe terminal formatting |
| `internal/config` | A command-local `UsageConfig` and declare-once descriptors |
| `cmd/copilotd/main.go` | Command registration, configuration projection, dependency/lifecycle wiring |
| `internal/server` | Same-listener mounting, ordinary request logging and recovery |

The HTTP handler consumes a query function with the illustrated `Query`/`Report`
shape; **a nil function explicitly means reporting is disabled**. Production
passes `Reporter.Query` only after the enabled writer opens successfully;
transport tests supply controlled results/errors or nil. The adapter checks nil
without invoking a query. It owns cheap HTTP syntax checks and admission, while
`Reporter.Query` owns semantic timezone/calendar/filter validation after
admission; the adapter does not duplicate that policy. Section 7 fixes the
resulting error precedence. The CLI consumes an injected HTTP client for
transport tests. Do not add a generic repository, mutable query builder,
formatter registry, or exported `*sql.DB` to the report interface.

Computation is **in-process**. SQLite is **local-substitutable**: use real temporary
databases, not a mock database interface. HTTP is **remote but owned**: exercise
its adapter against an in-process HTTP server. Implementation-only seams for
clock, filesystem timezone discovery, and native cleanup stay private.

## 4. CLI contract

```sh
copilotd usage
# Current calendar month, daily groups, terminal-local timezone.

copilotd usage --period week
copilotd usage --period month --since 2026-01-01
copilotd usage --period year --since 2024-01-01
copilotd usage --since 2026-08-01 --until 2026-09-01 --timezone Europe/Berlin
copilotd usage --surface openai --model gpt-example --details
copilotd usage --period month --since 2026-01-01 --json
copilotd usage --endpoint https://example.test/copilotd
```

These are supported commands; example model names do not assert Catalog contents.

| Flag | Default | Meaning |
|---|---|---|
| `--endpoint` | `http://127.0.0.1:8080` | Absolute HTTP(S) base URL |
| `--period` | `day` | `day`, `week`, `month`, or `year` |
| `--since` | omitted | Inclusive local calendar date; server supplies current month's first day |
| `--until` | omitted | Exclusive local calendar date; server supplies next month's first day |
| `--timezone` | omitted | Explicit named timezone; omitted means detect on the terminal host |
| `--surface` | `all` | `all`, `anthropic`, or `openai` |
| `--model` | omitted | Exact, non-empty Reported model; no alias expansion |
| `--details` | `false` | Include secondary native count tables |
| `--json` | `false` | Emit the validated report JSON, not terminal tables |
| `--timeout` | `15s` | Positive overall HTTP request/read timeout |
| `--config` | none | Existing explicit flat TOML selection mechanism |

Use the [ADR-0012](../adr/0012-declare-config-settings-once-via-typed-descriptor-table.md)
engine: flags > environment > selected TOML > defaults; eager overlay parse
errors retain their existing policy. Environment names follow the existing
mechanical convention (`COPILOTD_ENDPOINT`, `COPILOTD_PERIOD`,
`COPILOTD_TIMEZONE`, etc.). Do not invent another precedence engine or implicitly
load `serve` settings. Shared TOML files may contain unrelated keys, as the
current engine permits, but `usage` registers/resolves only its own descriptors.

Optional descriptor values retain presence: an omitted timezone/model is not
an explicitly empty override. An explicitly empty timezone or model is invalid;
it must not trigger autodetection or remove a filter. Keep any typed optional
field constructor inside the existing descriptor engine, not a second overlay
pass in the command.

A supplied model filter must be non-empty valid UTF-8. Preserve its exact bytes,
including whitespace, case, and valid Unicode; do not trim or normalize it.
Reject an invalid resolved CLI filter before HTTP; the raw HTTP route rejects
invalid model UTF-8 during admitted semantic validation, before SQL, as
`invalid_query`. Never repair an identity with replacement characters.

No `--usage-db-path`, API key, GitHub OAuth token, logging-file settings, or
inference settings belong to `usage`. Do not pull in the existing shared
operational descriptor block if that would expose those irrelevant flags.
Root/help/version behavior remains filesystem- and network-free. In particular,
`usage --help` does not load TOML, detect timezone, or contact a daemon.

The command accepts no positional operands. Malformed flags, local timezone
failure, transport/protocol/report errors, cancellation, and output failure
exit 1; a successful report, including an empty one, exits 0. Errors go to
stderr through the existing CLI error path. A successful invocation writes only
its selected presentation to stdout. No progress messages contaminate JSON.

### Base URL and client behavior

Accept HTTP or HTTPS with a host and optional path prefix. Reject URL userinfo,
query strings, fragments, invalid ports, and non-HTTP schemes. A trailing slash
is immaterial. Append `usage/v1/report` **under** the prefix, rather than using a
leading-slash reference that discards it:

```text
http://127.0.0.1:8080      -> http://127.0.0.1:8080/usage/v1/report
https://host/copilotd/    -> https://host/copilotd/usage/v1/report
```

Changing `serve --addr` requires a matching client setting; no discovery occurs.
The client uses ordinary TLS certificate verification and standard HTTP proxy
configuration, never the authenticated Upstream call module or impersonation
headers. Send no API key, cookie, or GitHub/Copilot credential. Do not follow
redirects automatically; report their status as a base-URL/proxy configuration
problem instead of silently changing the destination.

Make one request, with no automatic retry. Bound the response body to 8 MiB
before decoding, including chunked or decompressed bodies, irrespective of
`Content-Length`. A success must have media type `application/json` (parameters
such as charset are permitted). Treat an invalid success schema, missing required fields,
unsupported `schema_version`, invalid decimal counts, or trailing JSON as a
protocol error. Ignore additive unknown object fields within schema version 1.
JSON member names are case-sensitive: case variants are unknown fields, not
substitutes for required names. Reject duplicate member names after JSON
unescaping, invalid UTF-8 JSON, and unpaired surrogate escapes rather than
silently choosing or repairing a value. Validate effective filters against
explicit request selections, selected-section and native-metric presence,
non-NULL arrays, int64 ranges, and the metric sum/coverage relationships in
section 6, including its empty-section exception. This is report validation,
not client-side reaggregation or calendar reconstruction.

On non-2xx, prefer the bounded structured error code/message; handle an older
daemon's HTML/plain 404 or a proxy's response without dumping its body into the
terminal. Include status and request ID when available. Escape and bound any
untrusted diagnostic text.

`--json` validates the bounded response before writing its original JSON bytes
plus a final newline; this preserves unknown additive fields and exact numbers.
Build/validate the whole report before any terminal output. A stdout write error
can still leave partial output; it never turns into success. Signal cancellation
uses the command context; it does not request daemon shutdown.

## 5. Timezone and calendar policy

### Named timezone resolution

The CLI resolves a named timezone and sends it on **every** request.
“Terminal-local” means configuration visible to the CLI process, including when
that process runs inside SSH, a container, or WSL; it does not discover the
physical workstation outside that environment. The daemon must never interpret
a transmitted `Local`, abbreviation, or today's fixed UTC offset as those
calendar rules.

Accept `UTC` or loadable slash-style timezone names/aliases such as
`Europe/Berlin`, `US/Eastern`, and `Etc/UTC`. Apart from the special `UTC`, require
at least two non-empty slash-separated segments, with only ASCII letters,
digits, `_`, `-`, `+`, or `.` within a segment. Reject empty explicit values,
`Local`, bare legacy names/abbreviations, absolute paths, backslashes, `.`/`..`
path components, fixed-offset strings, and POSIX rule expressions. An explicit
`Etc/GMT+5` name is allowed; never manufacture it by observing the current offset.
Both client and daemon validate names with this grammar and `time.LoadLocation`.
Keep that validator in `internal/usage/report` and reuse it from the CLI;
filesystem discovery remains private to `reportcli`, and calendar construction
remains daemon-owned. An unrecognized zone errors rather than selecting another
zone.

Import embedded Go `time/tzdata` in the executable so named-zone loading works
without an installed timezone database and with CGO disabled. This is a fallback,
not a mechanism that forces embedded-only rules: Go may first use `ZONEINFO`
and platform data. The daemon's loaded rules are authoritative, and the report
echoes the accepted name. Equal identifiers do not promise equal client/daemon
tzdata revisions or recovery of arbitrary custom OS rules.

Use this conservative v1 discovery procedure, only after command configuration
resolves and no explicit timezone override was supplied:

1. **Linux/macOS `TZ`:** distinguish presence from absence. An exactly empty
   OS `TZ` selects `UTC`, because that is its documented configuration meaning,
   not a fallback after failed discovery. Otherwise remove at most one leading
   colon, then accept a validated named value or an absolute zone-file path
   whose name can be established by the next step. A lone colon is invalid.
   Unsupported rules, invalid values, or a custom/unidentifiable file error
   without trying the OS default. A non-empty `TZDIR` or `ZONEINFO` override
   disables automatic discovery; require an explicit report timezone instead.
2. **Linux/macOS system setting:** with `TZ` absent, inspect `/etc/localtime`.
   Follow symlinks with a fixed 40-hop bound, handling relative targets and
   rejecting loops, missing/unreadable targets, or inconsistent results. On
   Linux recognize installed zoneinfo roots `/usr/share/zoneinfo`,
   `/usr/share/lib/zoneinfo`, `/usr/lib/locale/TZ`, `/etc/zoneinfo`, and their
   resolved roots (including NixOS store paths reached through those roots).
   On macOS recognize Apple's documented final `zoneinfo/` directory component,
   including `/var/db/timezone/zoneinfo`, `/usr/share/zoneinfo`, and their
   versioned/resolved forms. Preserve the suffix while resolving root aliases;
   do not split an arbitrary Linux pathname merely containing `zoneinfo`.
   Require exactly one validated candidate name and a readable TZif target.
   A `TZ` path directly under a recognized root can carry its own name;
   `/etc/localtime` requires a name-bearing symlink. Reject `right/` and `posix/`
   subtrees instead of stripping them, and reject ordinary copied/custom files.
3. **No metadata guesses:** never use `/etc/timezone` alone (it can be stale),
   compare the current offset/abbreviation, or reverse-match zone-file bytes.
   Missing/unidentifiable configuration fails with explicit-override guidance.
4. **Native Windows:** v1 requires `--timezone` or its configuration/environment
   equivalent. Windows dynamic timezone keys and CLDR default mappings do not
   establish one unambiguous IANA location; no mapping dependency is introduced.
   Do not treat Windows `TZ` as Unix discovery when Go's Windows initialization
   ignores it. WSL uses the Linux procedure.

An explicitly empty `--timezone`/`COPILOTD_TIMEZONE`/TOML value is still invalid;
it is not the OS `TZ` setting. Explicit overrides bypass discovery, not name
validation. Failure makes no HTTP request and says, for example,
`cannot determine a named local timezone (copied zone file); pass --timezone Area/City`.
The raw HTTP route requires `timezone`; absence is 400, not a daemon-local or UTC
default.

The [timezone research](../research/2026-09-07-usage-report-timezones.md) records
primary sources and the CGO-disabled loading probes. Its generated name-allowlist
option is **not selected for v1**: the interface promises a validated loadable
identifier and daemon-owned rules, not certification that operator-supplied
`ZONEINFO` data is pristine IANA data. A maintained name registry or embedded-only
rules snapshot would add build/data ownership without guaranteeing that a
terminal's custom rules match the daemon. Custom automatic-discovery inputs are
rejected instead. Newer-than-bundled names may require a newer daemon; never
mislabel that error as UTC. #210 implements the Unix discovery procedure with
40-hop component-aware traversal, bounded TZif verification (at most 1 MiB), and
path/file observation rechecks. Public-command fixtures cover Linux/macOS layouts
and Windows explicit-only behavior; Linux static-executable isolation also covers
system discovery and embedded loading. Native execution remains a revision-specific
release-verification obligation; deterministic fixtures are not certification.
See the [verification pipeline and evidence guide](../verification/usage-reporting.md).

### Range and buckets

`period` changes grouping, **not the date range**. Resolve the daemon clock once
per query and express it in the requested timezone when supplying omitted date
bounds. This avoids client-clock/daemon-clock disagreement over the default
month; the user's chosen timezone still governs it.

Each omitted bound independently uses the current-month default. Consequently,
a historical `until` alone can precede the default `since` and is invalid; supply
both bounds for a historical closed range. Never swap bounds, infer a rolling
window from `period`, or silently rewrite a date.

Dates are strict `YYYY-MM-DD`, from `1970-01-01` through `9999-01-01`, with
`since < until`. This leaves room to express a nominal bucket's ending date
without overflowing the four-digit response year. Convert their date starts to
UTC instants and select `at_ms >= start_ms AND at_ms < end_ms`. These are
completion-observation times, not request starts or database commit times.
Requests entirely in the future can succeed empty. Stored future-dated
observations caused by clock skew remain in their selected bucket; generation
time is not a filter or watermark.

Construct calendar **edges**, not `24h`, `7*24h`, or fixed-duration months:

- Day: the interval from one date's start to the next date's start.
- Week: Monday's start through the next Monday's start; label with the Monday's
  date, including across December/January, rather than an ambiguous week number.
- Month: first-of-month start through first-of-next-month start.
- Year: January 1 start through the next year's January 1 start.

Define a date start as the earliest instant of that local date. If midnight is
repeated, use the first occurrence; if midnight is skipped but the date exists,
use its first valid instant. A completely skipped date has no duration: reject
it as a user-supplied bound. For an internal nominal period edge on such a date,
advance to the next existing date; omit a daily bucket whose two edges
consequently coincide, while retaining a larger period's nominal label and
remaining dates. Reject timezone data that yields non-monotonic edges rather
than fabricating an interval. Do not accept Go's unspecified choice for an
ambiguous `time.Date` as the contract; resolve against the actual transitions.

**UTC interval membership is authoritative**, not a second per-row comparison
of the displayed local date. This keeps selection, aggregation, and period
status consistent with one half-open interval per bucket. A rare clock reversal
can move the displayed date backward after the earliest date start; those
instants stay in the already-started reporting interval. Strict local-date
membership would require disjoint intervals and is deliberately not v1 policy.

Literal regression case for `America/Goose_Bay`:

| Instant | Local clock | Day reporting interval |
|---|---|---|
| `1988-10-30T02:00:00Z` | October 30, `00:00 -02:00` | October 30 |
| `1988-10-30T02:01:00Z` | October 29, `22:01 -04:00` | October 30 |
| `1988-10-30T04:00:00Z` | October 30, `00:00 -04:00` | October 30 |

The October 30 interval is `[1988-10-30T02:00Z, 1988-10-31T04:00Z)`.
An exclusive `until=1988-10-30` ends at `02:00Z` and therefore excludes the
later-returning October 29 clock readings. At captured time `02:30Z`, October
30's interval is in-progress although the local clock displays October 29.
This surprising historical case is explicit policy, not an undocumented
side effect of the SQL predicate. The transition values were checked with the
pinned Go toolchain during design review; implementation tests must also prove
edge construction and membership, not merely timezone formatting.

Return chronological nominal buckets intersecting the range, with the clipped
UTC interval actually queried. `range_partial` means selection clips that
nominal UTC interval; `in_progress` means its **unclipped UTC interval** contains
the captured query time. Future buckets are not mislabeled in-progress. These
flags say nothing about collection completeness. Emit bucket metadata even for
empty buckets, but do not invent model rows where no Turns were stored.

## 6. Aggregation and report value

Group by `(Surface, Reported model, bucket)` with binary/case-sensitive model
identity. Combine transports within that group. Do not normalize model IDs,
consult the Catalog, substitute Requested model, or deduplicate by response ID:
the store is best-effort observation, not an exactly-once ledger.

Every native metric has a known-value sum and reporting coverage:

```go
type Metric struct {
    Sum           *int64 // nil when no stored Turn reports this metric
    ReportedTurns int64  // number of non-NULL reports in this group
}
```

For 10 stored Turns, 8 of which report cache reads, the metric records the sum of
those 8 values and coverage 8/10. It does not estimate the other 2. A reported
zero contributes zero and increments coverage. All-NULL is `Sum=nil`, coverage 0.
Required input/output metrics have coverage equal to the group's Turn count.
For a section with no Turns, required metrics are known sums of zero with zero
coverage; optional metrics remain NULL. The empty-report message prevents those
structural zeros from being represented as proof of no consumption.

| Native projection | Interpretation retained in reports |
|---|---|
| OpenAI `input_tokens` | Complete input |
| OpenAI `cached_tokens`, `cache_write_tokens` | Subsets already inside input |
| OpenAI `output_tokens` | Complete output |
| OpenAI `reasoning_tokens` | Subset already inside output |
| OpenAI `total_tokens` | Only the reported total; never inferred |
| Anthropic `input_tokens` | Uncached input remainder |
| Anthropic `cache_creation_input_tokens`, `cache_read_input_tokens` | Additive to uncached input |
| Anthropic `ephemeral_5m_input_tokens`, `ephemeral_1h_input_tokens` | Subsets inside cache creation |
| Anthropic `output_tokens` | Complete output |
| Anthropic `thinking_tokens` | Re-tokenized subset already inside output |

Keep the frozen native field documentation adjacent to `internal/usage/usage.go`
authoritative. Do not add a universal input/total count, stack subsets onto their
containing counts, or derive cache-hit percentages from partially reported data.

The report contains separate Anthropic and OpenAI sections. Each selected
section has period/model rows, per-model totals for the whole selected range,
and one whole-section total. Totals are computed in the reporting module from
the same read, not by the client. Unselected sections are omitted; selected empty
sections remain present. There is no cross-Surface token total.

Rows sort by bucket start, then model bytes. Model totals sort by model bytes;
section order in terminal output is Anthropic, then OpenAI. Arithmetic for sums,
Turn counts, and coverage is checked signed 64-bit integer arithmetic. Overflow
fails the **whole** report with a request-to-narrow-the-range error. Do not switch
to floating point, saturate, wrap, or return partially accumulated totals.
Negative counts or invalid stored model encoding produce an unavailable-data
error, not a repaired row. An excessively large model value counts against the
report's size limits, not a license to truncate its identity.

### JSON shape and exactness

The versioned route returns `schema_version: 1`. Token sums, Turn counts, and
coverage are **decimal strings**, or `null` for an absent sum; this preserves
int64 exactness for future browser consumers as well as the Go CLI. No exponent,
fraction, sign on nonnegative counts, or leading zeros except `"0"`. Internal Go
values remain integers. Booleans and `schema_version` retain their JSON types.

Illustrative fragment, not a complete response:

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-07T12:00:00Z",
  "timezone": "Europe/Berlin",
  "period": "week",
  "since": "2026-09-01",
  "until": "2026-10-01",
  "window_start": "2026-08-31T22:00:00Z",
  "window_end": "2026-09-30T22:00:00Z",
  "scope": "configured_database",
  "collection": "best_effort",
  "buckets": [
    {
      "start_date": "2026-08-31",
      "until_date": "2026-09-07",
      "range_start": "2026-08-31T22:00:00Z",
      "range_end": "2026-09-06T22:00:00Z",
      "range_partial": true,
      "in_progress": false
    }
  ],
  "openai": {
    "rows": [
      {
        "bucket_start": "2026-08-31",
        "model": "gpt-example",
        "turns": "10",
        "usage": {
          "input_tokens": {"sum": "15000", "reported_turns": "10"},
          "cached_tokens": {"sum": "12400", "reported_turns": "8"}
        }
      }
    ]
  }
}
```

Complete responses include every native metric in the table above for their
section, even when its sum is NULL, plus `models` and `total`. `models` entries
have `model`, `turns`, and `usage`; `total` has `turns` and `usage`. The effective
filters appear as `surface` (`all`/`anthropic`/`openai`) and `model` (string or
NULL) at the top level. All arrays, including empty ones, are JSON arrays rather
than NULL. UTC instants use RFC3339 with optional fractional seconds. Clients
must not derive freshness from `generated_at`; it is the captured query clock,
not a database commit time or completeness watermark.

`collection: "best_effort"` is always present. It does not contain a supposedly
complete lost-Turn count: existing loss counters are process-local and cannot
establish loss history for the shared database. Coverage is only coverage
**within stored Turns**.

## 7. HTTP contract, exposure, and failures

Register exactly `/usage/v1/report` on the existing mux. Support GET and HEAD;
HEAD follows the same validation/query/status contract and limits but emits no
body. Other methods get a local JSON 405 with `Allow: GET, HEAD`. Do not add a
trailing-slash alias, path-prefix query engine, or any caller-selectable file.

Query keys are `period`, `since`, `until`, `timezone`, `surface`, and `model`.
Omitted optional keys follow the defaults above; `timezone` is required.
`details`/`json`/`endpoint`/`timeout` are CLI-only. Validation never interpolates
values as SQL or SQLite URI options.

The route bypasses inference authentication and readiness middleware. Missing,
wrong, and valid API keys all have identical reporting behavior. It remains
registered while metering is disabled, returning an explicit error **without
opening the database or validating a timezone**. Error precedence is:

1. Method check: anything except GET/HEAD gets 405.
2. Nil query dependency: disabled reporting gets 503, even for malformed or
   missing query parameters. No query function is invoked.
3. Cheap transport parsing: enforce raw query size, URL decoding, allowed keys,
   scalar uniqueness, required timezone presence, and non-empty supplied values.
   A failure gets 400 even if report slots are occupied.
4. Try admission without waiting: saturation gets 429. In particular a
   syntactically decodable but invalid date/zone/period can get 429 here; it has
   not yet undergone semantic validation.
5. Start the work budget and invoke `Reporter.Query`, which validates enum
   values, model/zone grammar, zone loading, dates, calendar edges, and limits.
   These failures get their specified 400/422 without SQL. Only a valid semantic
   query proceeds to SQLite, then complete materialization/encoding.

This ordering requires no second calendar validator in the HTTP adapter and
makes disabled-plus-malformed and saturated-plus-invalid requests deterministic.

Normal report successes and errors set:

```text
Content-Type: application/json
Cache-Control: no-store
X-Content-Type-Options: nosniff
```

Retain the existing `X-Request-Id` middleware. Do not return database paths, SQL,
credentials, row-level request/response IDs, or stack traces. No CORS opt-in,
JSONP, cookies, or browser session mechanism is introduced. **Absence of CORS is
not authentication or protection against all browser/local-network attacks**;
DNS rebinding, other local users, and any actor with network reachability remain
part of the deliberate unauthenticated exposure. Existing HTTP serving is
plain TCP; HTTPS requires an operator-managed reverse proxy or tunnel.

Enabling `--shim-usage-meter-enabled` now also enables this disclosure. The
existing opt-in stays off by default; there is no separate report-enabled flag
in v1. Help and operator documentation must state the consequence prominently.
A non-loopback bind can expose it to everyone who can reach that listener.
Adding authentication to inference in a reverse proxy does not automatically
protect this separate path. Binding/firewall/reverse-proxy policy is the
operator's control; this design intentionally does not enforce loopback clients.

### Error envelope

```json
{"schema_version":1,"error":{"code":"usage_meter_disabled","message":"Usage meter is disabled on this daemon."}}
```

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_query` | Missing/invalid timezone, dates, period, filters, duplicates, unknown keys, or query too long |
| 405 | `method_not_allowed` | Neither GET nor HEAD |
| 422 | `report_too_large` | Calendar, scanned-Turn, group, retained-data, or encoded-size limit; narrow range/filter or coarsen period as appropriate |
| 422 | `aggregation_overflow` | Exact aggregate exceeds int64; narrow range/filter |
| 429 | `report_busy` | All report admission slots occupied; `Retry-After: 1` |
| 503 | `usage_meter_disabled` | Collection/reporting disabled; no database read |
| 503 | `usage_unavailable` | Read failure, contention, invalid stored data, missing database, or incompatible schema |
| 504 | `report_timeout` | Work budget expired before a complete report was available |

A deadline takes precedence over the driver error caused by that cancellation.
A disconnected client may receive no response. Once a body write fails, do not
try to append an error document to it. Unexpected panics remain covered by the
existing generic recovery middleware, whose 500 is intentionally not guaranteed
to use this JSON envelope; the CLI already handles non-JSON errors. These local
errors do not add cases to `internal/apierror.Kind`.

### Initial fixed limits

These are named implementation constants and test cases, not new operator flags
or caller-adjustable budgets:

| Limit | Initial value |
|---|---|
| Raw query input | At most 16 KiB before decoding |
| Selected calendar dates | At most 3,660 dates |
| Bucket metadata | At most 4,096 buckets |
| Concurrent report handlers | 2, no admission queue |
| Query work, including materialization/encoding | 5 seconds |
| Native SQLite busy wait | At most 100 ms and never more than remaining work budget |
| Stored Turns examined | At most 1,000,000 matching rows across selected Surfaces |
| Period/Surface/model groups | At most 10,000 |
| One model value, before identity transfer | At most 1 MiB UTF-8 |
| Retained distinct model UTF-8 bytes | At most 1 MiB |
| Encoded success body | At most 8 MiB |
| HTTP response write | At most 5 seconds after materialization |
| CLI overall request/read | 15 seconds by default |

Hold the handler admission slot through response writing so slow readers cannot
accumulate unbounded retained reports. Use a route-local write deadline through
`http.ResponseController`, not a global server write timeout that would break
SSE. Fully materialize and bounded-encode before committing success headers;
release SQLite before writing the response. Encode bounded fragments/rows into a
capped buffer, or reject an established encoded-size bound before whole-value
encoding: a limited writer alone does not bound `encoding/json.Encoder`'s
internal allocation for one enormous report. Invalid/disabled/busy errors are
small and do not perform SQL. Early errors also receive a bounded write deadline.

Limits bound normal CPU, memory, native lock waiting, and network waiting, not
arbitrary stuck filesystem I/O or the total cost of an internet flood. A shared
listener and shared disk do **not** provide inference isolation. Sustained
unauthenticated traffic can still consume resources; dedicated rate limiting
and broader overload policy are out of scope, not guarantees supplied by the
two-slot cap. No automatic report refresh or polling is introduced.

## 8. Read implementation and lifecycle

Construct the reporter only after the enabled writer has successfully opened
and migrated its configured database. Resolve the path once in the composition
root for both owners. A disabled meter passes a nil query function to the HTTP
handler factory; it does not construct a reporter or inspect historical data.
`server.New` receives that explicit handler, not the store's connection,
configuration file, or `usage.Sink`.

For each admitted query:

1. Validate calendar/filters, capture the clock once, and derive UTC bucket
   intervals before reading data. Enforce range/bucket limits.
2. Open the existing local database using an escaped SQLite file URI with
   `mode=ro`. Set one connection for this query, and configure connection-local
   `PRAGMA query_only=ON` and a bounded native `busy_timeout` before reading.
   Never call writer `sqlitestore.Open`, create the main database or its parent,
   migrate, activate WAL, checkpoint, or use `immutable=1` on a live WAL database. WAL shared-memory coordination is not a promise of zero filesystem
   activity: normal read-only WAL access may still use sidecars.
3. Begin one read transaction, check the supported schema and text encoding,
   and read both selected native tables under that same snapshot. v1 requires
   the current schema version 2 and UTF-8 encoding, as created by the existing
   writer; incompatible encoding/schema returns `usage_unavailable`. No lazy
   migration or partial-schema interpretation. Keep compatibility and literal
   path policy aligned with store-owned facts, not an independent schema.
4. Select only `at_ms`, a **size-guarded** Reported model plus its byte-length
   metadata, and the frozen native counts, with bound half-open timestamp and
   optional exact-model predicates. Guard the model in SQLite before its full
   identity is transferred, as specified below; do not first scan an unbounded
   string and then check its Go length. Use the existing timestamp indexes and
   stream in timestamp order. Do not fetch prompt data, correlation IDs,
   Requested model, or whole tables into memory. Map timestamps to the
   precomputed intervals in Go; SQLite `localtime`, string-prefix grouping, and
   offset-only bucketing do not implement this calendar-edge contract.
5. Accumulate rows, per-model totals, and section totals with checked integer
   arithmetic and coverage. Intern each model identity and reuse
   its string across groups/totals, so the retained-model-byte limit bounds retained data,
   not just a distinct-name statistic while duplicate large strings accumulate.
   Stop with an error when a row/group/retained-data limit is exceeded; never
   return a successful truncated report. Check context
   cancellation during iteration and check `Rows.Err`.
6. End the read transaction and close rows, connection, and database before
   terminal/HTTP rendering. Sort the independent result and bounded-encode it
   inside the work budget; publish only after the whole operation succeeds.

### Model length guard

Use SQLite's [`octet_length`](https://www.sqlite.org/lang_corefunc.html#octet_length)
on the stored TEXT column. SQLite documents that this form can determine byte
length from metadata without reading the full string. With the checked UTF-8
encoding, the length is the prospective UTF-8 identity size. Project a guarded
value in the **same transaction** as the report, for example:

```sql
SELECT at_ms,
       octet_length(model) AS model_bytes,
       CASE WHEN octet_length(model) <= ? THEN model ELSE NULL END AS safe_model,
       input_tokens, output_tokens /* plus the other native counts */
FROM openai_turn
WHERE at_ms >= ? AND at_ms < ?
ORDER BY at_ms;
```

SQLite's [lazy CASE evaluation](https://www.sqlite.org/lang_expr.html#the_case_expression)
keeps an oversized identity out of the returned value. A NULL marker with an
oversized byte count means `report_too_large`, not an unknown model or an omitted
row. Never truncate it or coalesce it to a placeholder identity.

An optional exact-model predicate must not defeat the guard by materializing an
oversized value to compare it first. The query input is already bounded; use a
lazy byte-length check before identity equality, for example
`CASE WHEN octet_length(model) = ? THEN model = ? COLLATE BINARY ELSE 0 END`,
with the supplied filter's UTF-8 byte length. Unrelated oversized values can then
be excluded without transferring their identity. Do not rely on the evaluation
order of ordinary `AND` predicates for this protection, or sort/group by an
unguarded model column.

A design-time CGO-disabled probe against the pinned SQLite 3.53.4 verified the
UTF-8 default, a 1,048,577-byte model yielding only its length and a NULL marker,
and the guarded exact filter. It did **not** certify peak native allocation or
all query plans. Retain pinned-driver characterization of the guard before
shipping, including evidence that rejection precedes full identity transfer;
an eventual HTTP 422 alone is insufficient.

### Shared policies and shutdown

The implementation may share literal-path/schema helpers with the existing
writer; these are private-to-usage implementation details, not a generic SQL
interface for callers. No new persistent tables, indexes, rollups, cached values,
or background workers are introduced. Measure the indexed streaming approach
before adding materialized summaries. Coarser grouping reduces result groups,
not the number of stored Turns examined; row-limit errors must say to narrow the
range or model/Surface filter, not promise that yearly grouping fixes scanning.

`mode=ro` and cancellation must be characterized against the pinned driver,
including first-open behavior and every connection's pragmas. Cap native busy
waiting before each potentially blocking SQL stage; the existing writer has
already demonstrated that Go contexts alone do not preempt this driver's native
busy wait. Do not retain a read transaction while sending output, which would
unnecessarily retain WAL pages.

The HTTP server's existing drain includes report handlers. On forced shutdown,
cancel active report work and close their response connections; per-request
read cleanup must not gain another unbounded composition-root wait. Preserve
`StopAdmission` immediately after `Server.Run` returns and the existing fresh
writer-finalization budget. Report setup/query/cleanup failures must not stop
inference, toggle `/readyz`, or change writer admission. There is no read-pool
finalizer or report-specific background goroutine to extend shutdown.

Live replacement/deletion of the database or its sidecars and schema upgrades
under running writers/readers remain unsupported. Stop daemons for those
operations. Read-only reporting does not make a main-file-only live backup safe.
Same-version concurrent writers still serialize writes; the report's snapshot
contains committed observations visible to that read, not queued Turns and not
all observations through any promised wall-clock instant.

## 9. Terminal presentation

Print the endpoint, effective timezone, selected date range, period, and query
time in a short header. Render separate native sections with these core columns:

```text
Anthropic
Period      Model              Turns  Uncached input  Output  Cache create  Cache read
2026-09-01 "claude-example"  ...    ...             ...     ...           ...
            "claude-other"    ...    ...             ...     ...           ...
──────────────────────────────────────────────────────────────────────────────────────
2026-09-08 "claude-example"  ...    ...             ...     ...           ...

OpenAI
Period      Model           Turns  Input  Output  Cache write  Cache read
2026-09-01 "gpt-example"  ...    ...    ...     ...          ...
            "gpt-other"    ...    ...    ...     ...          ...
─────────────────────────────────────────────────────────────────────────
2026-09-08 "gpt-example"  ...    ...    ...     ...          ...
```

Within each section, group one period's Reported-model values into multiline
cells and put a horizontal table rule between period groups. Show the period
label only on the group's first model line; do not add tree markers or an `All`
row. Do not render the whole-range per-model or section totals as duplicate
terminal rows; they remain in JSON. Do not sum unlike Surface inputs into a grand
total. Model strings are rendered verbatim in identity but escaped for terminal
safety: quote/escape controls, newlines, escape sequences, and non-ASCII
formatting characters so upstream text cannot inject terminal commands, bidi
reordering, or extra rows.
Use a deterministic ASCII-escaped representation for model cells in v1; JSON
preserves the original valid Unicode identity. No truncation or aliasing of
model names to make a row fit.

Render exact integer counts with deterministic thousands separators, never
rounded `k`/`M` values that obscure differences. `—` means unreported, not zero.
A partially covered optional metric gets `*`; a following compact coverage list
states its reported-Turn fraction, for example `cache read: 8/10 stored Turns`.
Static table period labels show only the bucket start date. The validated JSON
retains the server-provided `range_partial` and `in_progress` flags for callers
that need those distinctions; do not reuse the coverage marker for them.

`--details` adds the server-provided OpenAI reasoning/reported-total metrics and
Anthropic thinking/cache-TTL metrics to the same period-grouped model rows.
Input/output/cache fields remain the compact default. `--json` always includes
all fields; `--details` does not change the HTTP request or JSON representation.
Render the static tables with rounded Lip Gloss borders, without Bubble Tea or
another interactive event loop. Do not add colors, interactive terminal control,
charts, width-based identity truncation, or TTY-only behavior; piped output
remains deterministic text.

For an empty selection, print `No stored Turns in the selected range.` rather
than a page of invented model rows. Always include this caveat in text:

> Persisted successful Turns observed by the Usage meter; best-effort and
> potentially incomplete. Optional-count coverage refers only to stored Turns.

## 10. Logging and operator documentation

Mount report traffic through the existing request ID, access summary, and
recovery chain, with the registration's fixed `inbound` pattern and **no** fake
Surface attribute. It is not a probe: successful reports use ordinary request
summary levels, not probe Debug classification. Do not log raw query strings,
model selections, usage counts, response bodies, or credentials in access logs.

Expected client errors use the access record rather than new per-error logs.
Unexpected storage/encoding failures may add a request-correlated diagnostic
owned by the emitting report module, using ordinary `slog` and existing governed
keys; never relabel it as the writer's loss report. Public errors stay generic
and path-free. Any new structured key requires the existing key inventory and
structure-test review, not inline string literals. A module that emits these
diagnostics takes its `*slog.Logger` as a required positional constructor/factory
argument, derived with `logging.ForComponent` for that emitting package
(ADR-0015). Do not add unused logger dependencies to modules that only return
errors; the illustrative report constructor in section 3 is logger-free unless
that module becomes an emitter.

At implementation time update README, CONFIGURATION, generated/help assertions,
and relevant old design status notes together. State prominently that enabling
the meter exposes the report without authentication on `--addr`. Document the
base-URL prefix rule, client-local timezone limitations, disabled versus empty,
shared-database scope, fixed limits, exact JSON count strings, and the lack of a
freshness/completeness guarantee. Do not advertise a billing dashboard or imply
that v1 serves HTML.

## 11. Acceptance tests

The interface is the test surface. Use real SQLite for reporting behavior and
literal expected reports, not tests coupled to private SQL strings.

### Reporting module

- Both native Surfaces; mixed transports; multiple Reported models; independent
  Requested model values do not change grouping; exact case-sensitive filters.
- Period rows, model totals, and section totals agree without double-counting
  cache/reasoning subsets or manufacturing native totals.
- Optional all-NULL, reported zero, and mixed coverage; empty selection; one
  Surface selected while the other is empty/unselected; no cross-Surface total.
- Exact counts above JavaScript's safe integer range, int64 overflow, invalid
  stored counts/encoding, huge model identities, and every limit crossing.
  Prove oversized identities are rejected before full transfer/materialization,
  including with exact filters, rather than merely asserting the eventual 422.
- All four calendar periods; inclusive/exclusive edges; Monday week/year edges;
  leap days; ordinary DST gaps/folds; midnight changes and a skipped date;
  clipped, in-progress, future, and empty buckets; default bounds under an
  injected clock and a timezone different from the daemon's local setting.
  Include the literal Goose Bay 1988 reversal from section 5: earliest edge
  selection, interval membership, exclusive range end, and `in_progress` must
  follow that policy even when the displayed local date moves backward.
- Unknown schema, missing file, literal path punctuation, permissions, canceled
  contexts, native lock contention, failure during scan, and cleanup failure.
- Concurrent WAL reader/writer behavior: one consistent report across both
  tables while new commits occur; report reads don't request a flush, create or
  migrate a database, change writer pragmas, or wait on network output.

### HTTP adapter and daemon integration

- Same production mux/listener with no API key, wrong key, and correct key;
  inference Endpoints retain their existing authentication/readiness behavior.
- Explicit meter-disabled 503 without usage files/read attempts; enabled-empty
  200; query failure does not change inference or readiness.
- Strict query parsing, all status/error mappings, GET/HEAD/405 behavior,
  required headers, absent CORS, request-ID correlation, and non-probe logging.
  Pin error precedence: disabled plus malformed/missing query is 503; an invalid
  method is still 405; saturated plus duplicate key is 400; saturated plus an
  invalid calendar date is 429, whereas that date with admission available is
  400. None of these cases performs SQL.
- Two active report handlers, immediate excess-admission 429, slot release on
  all errors/cancellation, bounded encoding, and a slow writer deadline.
- No private path/SQL/credential/correlation-row data in successful reports or
  public errors. Ordinary generic panic recovery remains readable by the CLI.
- Existing shutdown drains reports, cancels forced-close reads, and preserves
  writer admission/finalization ordering and bounds.

### CLI/configuration

- Independent flag scope; help/root/version make no filesystem/network/timezone
  calls; exact flag/env/TOML precedence and eager malformed-overlay behavior.
- Default and prefixed base URL joins, invalid URLs, TLS verification, redirects,
  daemon-down/older-daemon errors, bounded bodies, timeout, and cancellation.
- Explicit timezone precedence, terminal-local named detection, ambiguous or
  missing detection errors, and no fallback to daemon local time or UTC.
- Exact HTTP query parameters; terminal and daemon hosts with different zones;
  no API key, GitHub OAuth token, database path, or Upstream call dependency.
- Both native period-grouped layouts, horizontal inter-period rules, no tree
  markers, details/coverage, omission of range rows and period annotations,
  exact numbers, malicious model/error strings, empty report,
  JSON-only stdout, stderr errors, protocol version skew, additive fields, and
  failing output writers.
- Exact case-sensitive JSON member matching, duplicate members (including
  escaped spellings), invalid UTF-8/unpaired surrogates, contradictory
  filter/section/metric coverage, and valid additive fields. Reject invalid
  model-filter UTF-8 before HTTP or SQL as applicable; preserve valid Unicode
  without normalization. The client validates but never recomputes totals.

## 12. Implementation sequence and release gates

The [epic](https://github.com/ningw42/copilotd/issues/206) anchors this design;
its seven native sub-issues are vertical implementation and verification slices,
not replacement specifications. The following ownership map replaces the initial
layer-by-layer rollout order. A section can have several consumers, but its
policy stays in the owning module, not duplicated across tickets or renderers.

| Slice | Requirements and acceptance ownership | Blocked by |
|---|---|---|
| [#207](https://github.com/ningw42/copilotd/issues/207) — daily OpenAI end to end | Sections 2–4 and 6–11 for explicit OpenAI/day/UTC/date bounds: report value, real schema-v2 reader, all baseline safeguards, report-specific errors, local HTTP handler, client validation, isolated config, compact output, and exposure docs | None |
| [#208](https://github.com/ningw42/copilotd/issues/208) — Anthropic and combined reports | Sections 6, 8, 9, 11: native Anthropic counts, combined snapshot, attribution, empty/unselected sections, and whole-request budgets | #207 |
| [#209](https://github.com/ningw42/copilotd/issues/209) — periods and explicit named zones | Sections 4–5 and 11: all periods, default bounds, named-zone validation/loading, calendar edges and regression cases | #207 |
| [#210](https://github.com/ningw42/copilotd/issues/210) — terminal-local timezone default | Sections 4–5 and 11: presence-aware overrides, conservative platform discovery, failure guidance, and resolver tests | #209 |
| [#211](https://github.com/ningw42/copilotd/issues/211) — filters, details, JSON | Sections 4, 6, 8–9, 11: exact UTF-8 model filters and guarded predicates, detailed native tables, validated original-byte JSON, and output-error tests | #208 |
| [#212](https://github.com/ningw42/copilotd/issues/212) — concurrent inference and shutdown | Sections 7–8, 10–11: integrated contention/admission/cancellation/slow-reader/shutdown evidence and pinned-driver characterization | #208 |
| [#213](https://github.com/ningw42/copilotd/issues/213) — release verification | Sections 10–12: complete executable acceptance, documentation/status agreement, revision-specific native-platform run evidence, and the full verification suite | #210, #211, #212 |

Every slice inherits section 1's scope and the preserved contracts in section 2.
All section 7 safeguards must land with the first exposed data route in #207;
#212 adds concurrent evidence, not deferred safety. No route claims to serve
history before its reader exists. Intermediate unsupported selections fail
clearly without inventing empty data, changing final defaults, or advertising
unfinished capabilities. #209 and #211 can proceed independently with explicit
selections; #213 verifies their combined behavior and the final default command.
No additional feature ticket or generic framework is needed.

Release verification runs focused tests and `go vet` through `nix develop`, then
`nix develop -c go test -race ./... -count=1`, `nix fmt`, and `nix flake check`.
Exercise the existing CGO-disabled four-target build matrix. For each covered
requirement, retain the implementation revision and concrete test/evidence
location on its owning child; references to the design alone are not evidence
that the feature has shipped.

Before shipping, retain evidence for timezone discovery on each claimed native
platform and for the pinned driver's read-only WAL/interrupt/cleanup behavior.
Compilation alone does not certify Windows/macOS timezone discovery, ACLs, or
filesystem behavior. The conservative explicit-timezone fallback is supported
where automatic discovery cannot establish a name. A failure of native work to
honor context/lock caps must not be disguised as a hard five-second guarantee;
retain the same honest stuck-I/O limitation as the writer.

**Remaining product decisions:** none. The numerical limits and wire/layout
choices above are implemented and ready for release review. The release gates
above are engineering verification obligations, not permission to silently
change the agreed listener, authentication, database ownership, or timezone
semantics.
