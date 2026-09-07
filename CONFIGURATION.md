# Configuration

Configuration precedence is shown from left to right in the tables below: an
explicit command-line flag overrides an environment variable, which overrides
the selected TOML file, which overrides the built-in default.

Flags must follow `copilotd serve`, `copilotd login`, or `copilotd usage`. No configuration file is
loaded automatically; select one with `--config` or `COPILOTD_CONFIG`. The file
uses the flat TOML keys shown below. Durations use Go duration syntax such as
`500ms`, `30s`, or `24h`; quote duration and other string values in TOML.

Each duration default below is stated in that setting's canonical unit —
seconds for timeouts and monitoring thresholds, hours for refresh intervals —
and `copilotd serve --help` prints the same string. The canonical unit is
presentation only: any Go duration
form is accepted as input, so `--stream-idle-timeout 5m` and
`--stream-idle-timeout 300s` are equivalent.

## `login`

| CLI flag (highest precedence) | Environment variable | TOML key | Default (lowest precedence) |
| --- | --- | --- | --- |
| [`--config <PATH>`](#--config) | `COPILOTD_CONFIG` | — | No file |
| [`--log-level <LEVEL>`](#--log-level) | `COPILOTD_LOG_LEVEL` | `log-level` | `info` |
| [`--log-format <FORMAT>`](#--log-format) | `COPILOTD_LOG_FORMAT` | `log-format` | `text` |
| [`--log-file <PATH>`](#--log-file) | `COPILOTD_LOG_FILE` | `log-file` | Empty (stderr) |
| [`--github-oauth-token-file <PATH>`](#--github-oauth-token-file) | `COPILOTD_GITHUB_OAUTH_TOKEN_FILE` | `github-oauth-token-file` | `<user config dir>/copilotd/github-oauth-token` |
| [`--github-client-id <ID>`](#--github-client-id) | `COPILOTD_GITHUB_CLIENT_ID` | `github-client-id` | `Iv1.b507a08c87ecfe98` |
| [`--github-scope <SCOPE>`](#--github-scope) | `COPILOTD_GITHUB_SCOPE` | `github-scope` | `read:user` |

## `usage`

This one-shot HTTP client has an isolated flag set: no API key, GitHub OAuth
token, database-path, logging, or inference settings. Shared TOML files may have
unrelated keys; only these descriptors are resolved. Help/root/version do not
load a configuration file, discover a timezone, open SQLite, or use the network.
No positional operands are accepted. Errors, cancellation, and output failures
exit 1; complete reports (including empty reports) exit 0.

| CLI flag | Environment variable | TOML key | Default |
| --- | --- | --- | --- |
| `--endpoint <URL>` | `COPILOTD_ENDPOINT` | `endpoint` | `http://127.0.0.1:8080` |
| `--period <PERIOD>` | `COPILOTD_PERIOD` | `period` | `day` |
| `--since <DATE>` | `COPILOTD_SINCE` | `since` | Omitted |
| `--until <DATE>` | `COPILOTD_UNTIL` | `until` | Omitted |
| `--timezone <NAME>` | `COPILOTD_TIMEZONE` | `timezone` | Omitted |
| `--surface <SURFACE>` | `COPILOTD_SURFACE` | `surface` | `all` |
| `--model <MODEL>` | `COPILOTD_MODEL` | `model` | Omitted |
| `--details=<BOOL>` | `COPILOTD_DETAILS` | `details` | `false` |
| `--json=<BOOL>` | `COPILOTD_JSON` | `json` | `false` |
| `--timeout <DURATION>` | `COPILOTD_TIMEOUT` | `timeout` | `15s` |
| `--config <PATH>` | `COPILOTD_CONFIG` | — | No file |

**Currently supported:** `--surface all` (the default), `anthropic`, or `openai`,
with `--period day|week|month|year` and a named timezone. Omitted `--timezone`
selects supported terminal-local configuration on Linux/macOS; native Windows
requires an explicit override.
`--since` (inclusive) and `--until` (exclusive) are strict `YYYY-MM-DD`, between
`1970-01-01` and `9999-01-01`, with since before until. Each omitted bound
**independently** selects the current month's first day or the next month's first
day from one daemon clock capture expressed in the requested zone. Period changes
grouping only. A historical until alone can precede the default since and is
invalid: provide both bounds, rather than expecting a rolling range.

Accept `UTC` or loadable slash-style names/aliases, for example `Europe/Berlin`,
`US/Eastern`, `Etc/UTC`, and `Etc/GMT+5`. Slash segments must be nonempty ASCII
letters/digits/`_`/`-`/`+`/`.` and cannot be `.` or `..`. Empty overrides, `Local`,
bare abbreviations/legacy names, offsets, absolute/backslash paths, unknown names,
and POSIX rule expressions are invalid and never silently become UTC. Client and
daemon share name validation. The executable embeds fallback timezone data,
including with CGO disabled; no companion asset is required. Go can prefer
operator `ZONEINFO` or platform data. The daemon's loaded rules are authoritative;
equal names do not certify pristine IANA data or equal tzdata revisions.

Weeks start Monday and use that Monday's date even across December/January;
months and years use first-of-month and January 1 labels. Edges use the earliest
instant of a date: first repeated midnight, or first valid instant after a skipped
midnight. A skipped whole date is invalid as an explicit bound; internal skipped
edges advance, omitting coincident daily buckets while retaining larger-period
nominal labels. Unsupported/non-monotonic transition behavior fails rather than
fabricating an interval; calendar traversal is context-checked and bounded for
custom operator data. UTC half-open intervals govern both selection and grouping,
even if a historical clock reversal later displays yesterday. The terminal uses
server-computed `[clipped]` and `[in progress]` flags; the latter tests the captured
instant against the unclipped interval. Empty/future buckets invent no model rows,
and future-dated stored Turns are not filtered out by generation time.

**Terminal-local timezone:** the configuration visible to the CLI process,
including inside SSH, containers, and WSL, not the remote daemon or physical
workstation outside that environment. Flags > environment > selected TOML >
defaults still apply; explicitly empty report-timezone/model settings are errors,
not omission. Valid explicit timezone overrides bypass discovery but still use
shared name validation. Discovery never rereads `COPILOTD_TIMEZONE` to invent a
second precedence order.

- On Linux/macOS, nonempty OS `TZDIR` or `ZONEINFO` makes automatic discovery
  unsupported. Set an explicit report timezone instead.
- With OS `TZ` present, exactly empty means configured `UTC`. Otherwise at most
  one leading colon is removed; a named value must validate/load unchanged, or
  an absolute zone-file path must establish a supported name. Invalid/rule/custom
  values error without falling back to `/etc/localtime`.
- With OS `TZ` absent, inspect `/etc/localtime`. A name-bearing symlink must lead
  to a readable TZif file. Relative links and directory/root aliases share a
  fixed 40-hop traversal bound. Linux recognizes `/usr/share/zoneinfo`,
  `/usr/share/lib/zoneinfo`, `/usr/lib/locale/TZ`, `/etc/zoneinfo`, and their
  verified resolved roots (including NixOS store layouts), not arbitrary paths
  containing `zoneinfo`. macOS additionally recognizes Apple's final `zoneinfo`
  directory component in versioned/resolved layouts. Preserve the name suffix;
  distinct candidate names are ambiguous rather than silently canonicalized.
- Reject `right/` and `posix/` subtrees, loops, missing/unreadable targets, ordinary
  copied/custom files, and changed path observations. Read at most 1 MiB of TZif
  evidence and recheck path/file identity; do not reverse-match bytes, current
  offsets/abbreviations, `time.Local`, or stale `/etc/timezone` metadata.
- Native Windows always requires `--timezone`, `COPILOTD_TIMEZONE`, or selected
  TOML `timezone`. No registry/CLDR mapping or Windows `TZ` inference is used.
  WSL follows Linux. Explicit named loading retains the embedded fallback on
  every supported target; no generated name allowlist is introduced.

Failure exits 1 before HTTP with bounded guidance such as
`cannot determine a named local timezone (unidentifiable or ambiguous zone file); pass --timezone Area/City (or --timezone UTC)`.
There is no silent UTC or daemon-local fallback. Every successful invocation
transmits the accepted name; raw HTTP still requires `timezone` (missing is
400). Traversal/read bounds cover normal filesystem work, not arbitrary stuck
I/O or an atomic guarantee against concurrent OS reconfiguration.
Native Linux static-executable isolation covers discovery and embedded loading;
macOS/Windows policy fixtures are not native runtime certification, which remains
pending for release verification. `--model`, `--details`, and `--json` remain
reserved for later slices and fail clearly today.

The endpoint is an absolute HTTP(S) base URL. Its optional path prefix is
preserved: `https://host/copilotd/` becomes
`https://host/copilotd/usage/v1/report`. Set it explicitly when `serve --addr`
changes. Userinfo, query strings, fragments, non-HTTP schemes, and invalid ports
are rejected. TLS verification and standard HTTP proxies remain enabled. No
credentials, cookies, redirects, retries, daemon discovery, or local-file
fallback are used. `--timeout` must be positive and bounds the request/read.

The schema-version-1 HTTP response contains all seven Anthropic and/or six
OpenAI native metrics for the selected Surfaces, period/model rows, per-model
and separate section totals, and effective selections. An omitted Surface
selects both. Unselected sections are omitted; selected empty sections contain
empty arrays, required zeros, and optional NULL sums. Anthropic input stays the
uncached remainder, while OpenAI input stays complete input. Cache TTL and
thinking/reasoning counts remain subsets of their native parent counts, never
extra input/output or an inferred total. No cross-Surface token total is added.
Counts/coverage are exact decimal strings (or null for an absent sum), never
floating point. The client validates case-sensitive required fields, duplicate
names, Unicode, integer ranges, and metric coverage before any output, while
allowing additive fields. It does not reaggregate totals or reconstruct calendar
rules. Text uses comma-separated exact counts, ASCII-escaped model identities,
`—` for unreported metrics, and `*` plus coverage for partial optional metrics.
Anthropic renders first with Turns, Uncached input, Output, Cache create, and
Cache read; OpenAI follows with Turns, Input, Output, Cache write, and Cache read.
A reported zero stays zero; an empty selection is explicitly labeled, not
represented as proof of no consumption.

The same listener serves exactly `GET`/`HEAD /usage/v1/report` without inference
authentication or readiness/upstream work. A disabled meter returns
`503 usage_meter_disabled`; an enabled empty report is successful, not disabled.
Failures include `400 invalid_query`, `405 method_not_allowed`,
`422 report_too_large`/`aggregation_overflow`, `429 report_busy`,
`503 usage_unavailable`, and `504 report_timeout`. Responses are JSON with
`Cache-Control: no-store`, `nosniff`, and the ordinary request ID. HEAD follows
the same work/validation contract without a response body.

Fixed safeguards are not flags: 16 KiB raw query; 3,660 selected dates; 4,096
buckets; two admitted handlers with no queue; five seconds of report/encoding
work; native lock waits capped at 100 ms and remaining work; 1,000,000 examined
Turns; 10,000 period/Surface/model groups; 1 MiB per model before transfer and
1 MiB retained distinct model bytes; 8 MiB encoded response/client read; five
seconds for route-local response writing. Admission remains held through writes,
but SQLite is released first. These caps do not isolate inference from shared
CPU/disk or an internet flood, and cannot preempt arbitrary stuck filesystem I/O.
Both native sections share one committed snapshot and all row/group/model/body
budgets apply across the whole request. A failure in either selected section
fails the report, never returning the other as complete. Reports describe
committed best-effort database history (including other writers/process runs),
not a freshness or completeness watermark. No writer flush, migration, extra
index, or read worker is introduced. Live file/schema replacement remains unsupported.

## `serve`

| CLI flag (highest precedence) | Environment variable | TOML key | Default (lowest precedence) |
| --- | --- | --- | --- |
| [`--config <PATH>`](#--config) | `COPILOTD_CONFIG` | — | No file |
| [`--shim-responses-item-id-stabilizer-enabled=<BOOL>`](#--shim-responses-item-id-stabilizer-enabled) | `COPILOTD_SHIM_RESPONSES_ITEM_ID_STABILIZER_ENABLED` | `shim-responses-item-id-stabilizer-enabled` | `false` |
| [`--shim-usage-meter-enabled=<BOOL>`](#--shim-usage-meter-enabled) | `COPILOTD_SHIM_USAGE_METER_ENABLED` | `shim-usage-meter-enabled` | `false` |
| [`--usage-db-path <PATH>`](#--usage-db-path) | `COPILOTD_USAGE_DB_PATH` | `usage-db-path` | Unix: `<user config dir>/copilotd/usage.db`; Windows: `%LOCALAPPDATA%\\copilotd\\usage.db` |
| [`--shim-nop-enabled=<BOOL>`](#--shim-nop-enabled) | `COPILOTD_SHIM_NOP_ENABLED` | `shim-nop-enabled` | `false` |
| [`--shim-hook-overrun-threshold <DURATION>`](#--shim-hook-overrun-threshold) | `COPILOTD_SHIM_HOOK_OVERRUN_THRESHOLD` | `shim-hook-overrun-threshold` | `1s` |
| [`--anthropic-catalog-model-id-normalization-enabled=<BOOL>`](#--anthropic-catalog-model-id-normalization-enabled) | `COPILOTD_ANTHROPIC_CATALOG_MODEL_ID_NORMALIZATION_ENABLED` | `anthropic-catalog-model-id-normalization-enabled` | `false` |
| [`--codex-catalog-enabled=<BOOL>`](#--codex-catalog-enabled) | `COPILOTD_CODEX_CATALOG_ENABLED` | `codex-catalog-enabled` | `false` |
| [`--codex-catalog-model-aliases <MAP>`](#--codex-catalog-model-aliases) | `COPILOTD_CODEX_CATALOG_MODEL_ALIASES` | `codex-catalog-model-aliases` | Empty |
| [`--codex-auto-review-model <SLUG>`](#--codex-auto-review-model) | `COPILOTD_CODEX_AUTO_REVIEW_MODEL` | `codex-auto-review-model` | Empty |
| [`--codex-auto-review-model-overrides <MAP>`](#--codex-auto-review-model-overrides) | `COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES` | `codex-auto-review-model-overrides` | Empty |
| [`--codex-catalog-override-limits=<BOOL>`](#--codex-catalog-override-limits) | `COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS` | `codex-catalog-override-limits` | `false` |
| [`--codex-catalog-refresh-interval <DURATION>`](#--codex-catalog-refresh-interval) | `COPILOTD_CODEX_CATALOG_REFRESH_INTERVAL` | `codex-catalog-refresh-interval` | `24h` |
| [`--log-level <LEVEL>`](#--log-level) | `COPILOTD_LOG_LEVEL` | `log-level` | `info` |
| [`--log-format <FORMAT>`](#--log-format) | `COPILOTD_LOG_FORMAT` | `log-format` | `text` |
| [`--log-file <PATH>`](#--log-file) | `COPILOTD_LOG_FILE` | `log-file` | Empty (stderr) |
| [`--github-oauth-token-file <PATH>`](#--github-oauth-token-file) | `COPILOTD_GITHUB_OAUTH_TOKEN_FILE` | `github-oauth-token-file` | `<user config dir>/copilotd/github-oauth-token` |
| [`--addr <HOST:PORT>`](#--addr) | `COPILOTD_ADDR` | `addr` | `127.0.0.1:8080` |
| [`--shutdown-timeout <DURATION>`](#--shutdown-timeout) | `COPILOTD_SHUTDOWN_TIMEOUT` | `shutdown-timeout` | `10s` |
| [`--apikey <KEY>`](#--apikey) | `COPILOTD_APIKEY` | `apikey` | Required |
| [`--outbound-timeout <DURATION>`](#--outbound-timeout) | `COPILOTD_OUTBOUND_TIMEOUT` | `outbound-timeout` | `600s` |
| [`--stream-idle-timeout <DURATION>`](#--stream-idle-timeout) | `COPILOTD_STREAM_IDLE_TIMEOUT` | `stream-idle-timeout` | `600s` |
| [`--stream-keepalive-interval <DURATION>`](#--stream-keepalive-interval) | `COPILOTD_STREAM_KEEPALIVE_INTERVAL` | `stream-keepalive-interval` | `15s` |
| [`--write-timeout <DURATION>`](#--write-timeout) | `COPILOTD_WRITE_TIMEOUT` | `write-timeout` | `90s` |
| [`--response-header-timeout <DURATION>`](#--response-header-timeout) | `COPILOTD_RESPONSE_HEADER_TIMEOUT` | `response-header-timeout` | `600s` |
| [`--ws-handshake-timeout <DURATION>`](#--ws-handshake-timeout) | `COPILOTD_WS_HANDSHAKE_TIMEOUT` | `ws-handshake-timeout` | `10s` |
| [`--max-request-bytes <BYTES>`](#--max-request-bytes) | `COPILOTD_MAX_REQUEST_BYTES` | `max-request-bytes` | `33554432` (32 MiB) |
| [`--max-buffered-response-bytes <BYTES>`](#--max-buffered-response-bytes) | `COPILOTD_MAX_BUFFERED_RESPONSE_BYTES` | `max-buffered-response-bytes` | `33554432` (32 MiB) |
| [`--github-oauth-token <TOKEN>`](#--github-oauth-token) | `COPILOTD_GITHUB_OAUTH_TOKEN` | `github-oauth-token` | Empty |
| [`--startup-mint-retries <COUNT>`](#--startup-mint-retries) | `COPILOTD_STARTUP_MINT_RETRIES` | `startup-mint-retries` | `3` |
| [`--vscode-version <VERSION>`](#--vscode-version) | `COPILOTD_VSCODE_VERSION` | `vscode-version` | `1.136.1` |
| [`--plugin-version <VERSION>`](#--plugin-version) | `COPILOTD_PLUGIN_VERSION` | `plugin-version` | `0.48.1` |
| [`--copilot-integration-id <ID>`](#--copilot-integration-id) | `COPILOTD_COPILOT_INTEGRATION_ID` | `copilot-integration-id` | `vscode-chat` |
| [`--github-api-version <VERSION>`](#--github-api-version) | `COPILOTD_GITHUB_API_VERSION` | `github-api-version` | `2025-04-01` |
| [`--impersonation-refresh-interval <DURATION>`](#--impersonation-refresh-interval) | `COPILOTD_IMPERSONATION_REFRESH_INTERVAL` | `impersonation-refresh-interval` | `24h` |

## Options

### `--config`

Selects the optional TOML configuration file. The flag path overrides the
environment path.

### `--log-level`

Sets the minimum log level: `debug`, `info`, `warn`, or `error`.

### `--log-format`

Selects `text` or `json` structured logs.

### `--log-file`

Writes logs to this file; an empty value writes to standard error.

### `--github-oauth-token-file`

Sets the raw GitHub OAuth token file that `login` writes and `serve` reads.

### `--addr`

Sets the proxy listen address. Port `0` requests an automatically assigned
port.

### `--shutdown-timeout`

Sets the positive grace period for HTTP/WebSocket drain before a forced close.
When the Usage meter is enabled, the store receives one fresh timeout of the
same length only after server drain returns. The default can therefore allow
about `20s` of HTTP/WebSocket plus SQLite coordinator wait in total: up to `10s`
for drain and then up to `10s` for final usage flush and native cleanup. The
terminal structured log write is synchronous; a stuck configured log destination
can extend return beyond this native-work bound.

### `--apikey`

Sets the secret clients must send as `Authorization: Bearer <KEY>` or
`x-api-key: <KEY>`.

### `--outbound-timeout`

Sets the positive total backstop for buffered upstream responses and
model-catalog fetches.

### `--stream-idle-timeout`

Limits genuine upstream silence on a streaming response. The default is `600s`,
matching the other two upstream bounds.

### `--stream-keepalive-interval`

Sets the idle interval before copilotd emits an OpenAI stream keepalive.

### `--write-timeout`

Limits each downstream HTTP write and each WebSocket write in either direction.

### `--response-header-timeout`

Limits how long HTTP forwarding waits for upstream response headers.

### `--ws-handshake-timeout`

Limits the upstream WebSocket handshake.

### `--max-request-bytes`

Caps inbound HTTP request bodies and each WebSocket message in either direction;
values must be positive.

### `--max-buffered-response-bytes`

Caps model-catalog response bodies and upstream response bodies processed by a
buffered-response shim; values must be positive. Enabling the Usage meter adds a
buffered hook to Anthropic Messages and OpenAI Responses, so an over-cap
qualifying or non-qualifying non-SSE identity-encoded response on either Route
returns `502` before commitment. With no other buffered hook and the meter
disabled, the same response remains on the unbuffered passthrough path.

### `--anthropic-catalog-model-id-normalization-enabled`

Replaces dots with hyphens in provider-shaped Anthropic catalog IDs when Copilot
reports `vendor:"Anthropic"` and the ID begins `claude-`; for example,
`claude-opus-4.8` becomes `claude-opus-4-8`. Other vendors and non-Claude IDs
remain verbatim. The catalog's `first_id` and `last_id` use the same normalized
IDs. This setting affects only `/anthropic/v1/models`; inference requests and
responses remain unchanged. If normalization would produce duplicate model IDs,
the catalog fails closed with HTTP `502` instead of emitting ambiguous IDs.
Disabled by default, preserving Copilot's IDs.

### `--shim-nop-enabled`

Enables the canonical no-op response shim.

### `--shim-responses-item-id-stabilizer-enabled`

Enables the opt-in OpenAI Responses item-id stabilizer shim. When enabled, it
pins one genuine upstream item `id` per `output_index` on the `/responses` SSE
and WebSocket transports and rewrites the later id-bearing events on that index
to it; no id is minted, and any payload it cannot confidently rewrite is
forwarded verbatim. Scoped to the OpenAI `/responses` endpoint, so every other
surface is untouched. Disabled by default, leaving both transports
byte-for-byte verbatim.

### `--shim-usage-meter-enabled`

Enables best-effort recording of native token counts to the SQLite file selected
by [`--usage-db-path`](#--usage-db-path). It is off by default: disabled `serve`
and `login` do not open a database, create usage files, start a usage writer, or
install a metering hook.

**Enabling this flag also exposes unauthenticated aggregate Usage reports on
`serve --addr`, including non-loopback/public bindings.** Anyone with network
reachability can read models and activity history at `/usage/v1/report`.
Inference API keys do not protect this local path, and neither missing CORS nor
private SQLite files are HTTP access controls. Other local users, DNS rebinding,
and reachable browser/local-network actors remain part of this deliberate
exposure. Protect the listener/path with binding, firewall, or reverse-proxy
policy; inference-only reverse-proxy authentication does not automatically
protect reports. See [`usage`](#usage) for the first supported selection.

**Implemented coverage is all five supported paths: buffered and SSE Anthropic
Messages plus buffered, SSE, and WebSocket OpenAI Responses.** Requested-model
attribution covers only the four HTTP buffered/SSE paths; WebSocket rows always
have `requested_model IS NULL`.

Both native tables keep `model` as the unchanged **Reported model** from the
upstream completion. The nullable `requested_model` is the **Requested model**:
the exact decoded string in the top-level, case-sensitive `model` property of
the final upstream-bound HTTP request, after earlier request-side Shims. It is
not necessarily the original client selection and is not proof of upstream
receipt or acceptance. For example, a request for `gpt-5.6-sol-fast` can complete
with reported `model = 'gpt-5.6-sol'`; both names are retained independently.

Strings are not trimmed, case-folded, normalized, or resolved through Catalogs,
Codex aliases, or metadata sources. An explicit `""` is stored as an empty string,
not SQL `NULL`. Missing, null, wrong-typed, malformed/non-object requests
(including invalid UTF-8), and ambiguous duplicate top-level `model` members yield
`NULL`; nested or differently
cased keys are not sources. Unknown attribution does not reject the request,
backfill a required response field, or prevent an otherwise-eligible row. The
meter observes once and retains only the optional model string, never the prompt
or full request body. Every qualifying observation for that HTTP request shares
that metadata, without inheriting from another request.

A buffered Anthropic row requires one self-contained object whose `type` is
`"message"`, with its own non-empty `id` and reported `model`, a non-empty string
`stop_reason`, and valid nonnegative integer `usage.input_tokens` and
`usage.output_tokens`. Stop reasons are not enumerated: `tool_use`, `max_tokens`,
and unfamiliar future non-empty reasons remain eligible. The meter preserves
these Anthropic-native values without normalization:

- `input_tokens`, the uncached remainder;
- `cache_creation_input_tokens` and `cache_read_input_tokens`, which are additive
  to that remainder;
- `cache_creation.ephemeral_5m_input_tokens` and
  `cache_creation.ephemeral_1h_input_tokens`, TTL subsets inside cache creation;
- `output_tokens`, the complete output count; and
- `output_tokens_details.thinking_tokens`, a re-tokenized subset already inside
  output.

Anthropic SSE keeps one accumulator per HTTP Shim instance. It routes only
`message_start`, `message_delta`, `message_stop`, and `error` frames by advisory
frame type, decodes their joined `data:` payload, and requires the decoded type
to agree. The start supplies the upstream message identity and reported model;
usage may be absent there and completed by later deltas. Each field is cumulative:
the last numeric report wins, while omission or explicit `null` preserves an
earlier report including zero. A valid `message_stop` submits only an active,
unpoisoned candidate with both required counts and then clears that candidate;
a duplicate stop or a stream without a stop submits nothing. Malformed relevant
events, invalid reported counts, conflicting starts, and upstream errors
permanently poison only that HTTP instance. They do not fault or rewrite the
stream, and no stream finalizer manufactures a completion.

Live Anthropic-through-Copilot compatibility remains unverified because the
evidence account lacked Anthropic model access. This parser is grounded in the
exact official Messages Create contract and explicitly generated fixtures; those
fixtures are not recorded Copilot responses. Beta variable-cardinality
`usage.iterations[]` remains excluded pending a separate schema/cardinality
review.

An OpenAI row requires a self-contained response object with its own non-empty
`id` and reported `model`, `status: "completed"`, and valid nonnegative integer
`usage.input_tokens` and `usage.output_tokens`. Buffered JSON validates that
object directly. SSE observes only `response.completed` frames, extracts their
joined `data:` payload (including repeated fields and CRLF framing), requires the
decoded event type to agree, and validates the nested `response` with the same
predicate. WebSocket observes each upstream server Message directly, validates
the same self-contained `response.completed` envelope without using the SSE pump,
and installs no client-message hook. Neither streaming observer fills fields
from an earlier event or the client request.
The meter also preserves these OpenAI-native fields when reported:

- `input_tokens_details.cached_tokens` and
  `input_tokens_details.cache_write_tokens`, both subsets already inside complete
  `input_tokens`;
- `output_tokens_details.reasoning_tokens`, a subset already inside complete
  `output_tokens`; and
- provider-reported `total_tokens`, without recalculation.

For both Surfaces, optional numeric reports remain nullable: omitted or `null`
values become SQL `NULL`, while a reported zero stays zero. Eligibility validates
JSON types, required presence, nonnegative values, and the signed 64-bit range;
it does not add cross-field arithmetic consistency checks. Malformed, incomplete,
error, or irrelevant payloads simply produce no row. The reported response model
is stored verbatim; requested model names, Catalog normalization, Codex aliases,
and metadata sources are never used as fallback or mapping inputs.

Each row also records local completion-observation time as canonical `at_ms` plus
generated millisecond UTC `at_utc`, inbound `request_id` (empty only when
unavailable), upstream object identity as `message_id` or `response_id`, the
applicable `transport` (`buffered` or `sse` for Anthropic; `buffered`, `sse`, or
`websocket` for OpenAI), and a zero-based submission-attempt `turn_index`. One
WebSocket shim instance captures the handshake `request_id` once and uses it for
every qualifying Turn even though
message execution uses a session-rooted context; unavailable construction
correlation stays empty. It does not store prompts, generated content, API keys,
GitHub OAuth tokens, or Copilot tokens.

The GitHub Copilot Surface, raw `/models`, provider/Codex Catalogs, and
`/v1/messages/count_tokens` are not metered. Built-in calendar native-Surface aggregation
is available through [`usage`](#usage); pricing/billing reconciliation, automatic
pruning, per-key attribution, and non-token usage projection remain out of scope.
External SQLite tooling still supports either native table, for example:

```sh
sqlite3 "$USAGE_DB" \
  'SELECT at_utc, quote(requested_model) AS requested_model, model, input_tokens, output_tokens FROM anthropic_turn ORDER BY at_ms DESC;
   SELECT at_utc, quote(requested_model) AS requested_model, model, input_tokens, output_tokens FROM openai_turn ORDER BY at_ms DESC;'
```

`quote(requested_model)` makes SQL `NULL` visibly different from the empty string
`''` in SQLite's text output.

Enabling this setting changes non-SSE forwarding even when a body is an error or
is not JSON: every non-SSE response with no `Content-Encoding`, or exactly one
trimmed case-insensitive `identity` value, is read in full under
`--max-buffered-response-bytes` before the parser can decline it. This delays
response commitment and recomputes `Content-Length`. An ordinary buffered read
failure returns `502`, timeout returns `504`, and client cancellation adds no new
response. Unsupported, repeated, list-valued, or explicitly empty encodings
bypass the buffered meter and remain opaque. Payload bytes are unchanged when a
hook runs; whole-wire neutrality is not promised.

For both SSE paths, transport selection follows the typed Endpoint and upstream
`Content-Type`, not the inbound `stream` field. An unsupported SSE
`Content-Encoding` still returns the existing pre-hook `502`. Each observer
returns every frame with its exact original `Raw` bytes and structure; neither
holds, drops, coalesces, rewrites, or finalizes frames. The OpenAI WebSocket
observer likewise always emits each original server Message with the same kind
and data. It retains only captured correlation plus a submission ordinal: no
response map, usage accumulator, overlap guard, or per-session sum. Interleaved,
malformed, failed, incomplete, and error Messages cannot contaminate a later
valid completion.

Observation happens before downstream writing. A row can remain after a client
write failure or an outer Shim failure, and duplicate qualifying observations
are not deduplicated. A clean SSE terminal may instead be `response.failed`,
`response.incomplete`, or `error` and produce no row; a WebSocket session can fail
after preserving earlier successful-completion rows. Conversely, failed,
incomplete, cancelled, malformed, or unparseable responses and queue/storage
loss can omit real consumption. Application completion, usage availability, and
downstream delivery are independent; WebSocket summaries describe a session, not
every inference Turn, and the database and logs are not exhaustive or
billing-grade accounting.

### `--usage-db-path`

Selects the Usage meter's private local SQLite main file. On Unix the default is
`<os.UserConfigDir()>/copilotd/usage.db`; on Windows it is
`%LOCALAPPDATA%\\copilotd\\usage.db`, deliberately not roaming AppData. If the
OS base cannot be resolved, the default is relative `copilotd/usage.db`.
Changing `--github-oauth-token-file` does not move the usage database. If the
default is network-mounted or synchronized, override it with a genuinely local
path: network shares and live roaming/synchronized copies are unsupported. The
resolved path is treated as a literal filename, not a driver DSN; punctuation
such as `?`, `%`, or `file:` cannot introduce SQLite query parameters.

When metering is enabled, startup validates the destination and migrates before
binding. Failure is fatal rather than silently disabling requested recording.
Schema version 2 appends nullable `requested_model TEXT` to both existing STRICT
native tables without changing their earlier columns or indexes. Historical rows
keep all values and acquire `NULL`; fresh databases reach the same schema as
upgraded ones, and reopening is a no-op. All pending DDL and the version bump
commit atomically or roll back together. **Stop all existing writers before
starting a binary that upgrades the schema**; mixed-version online upgrades are
not supported. An older binary refuses a newer schema rather than writing to it.
On Unix, a missing final parent is created at `0700`, a missing main file is
exclusively pre-created at `0600`, and existing shared parents, permissive main
files, symlinks, and non-regular destinations are refused without chmod or
truncation. The private parent protects SQLite-created `usage.db-wal` and
`usage.db-shm` sidecars. Windows uses best-effort exclusive creation and regular
file checks; Go mode bits do not establish or certify Windows ACLs, sidecar ACL
inheritance, reparse-point handling, or native runtime behavior.

The store uses one dedicated connection, WAL, and `synchronous=NORMAL`. External
readers are supported while serving; stop every writer for upgrades, retention,
or backup. Back up a consistent stopped database or use SQLite-aware backup
tooling—never copy a live main file without its WAL state. No down migration or
automatic retention exists; operators own growth, pruning, backups, and restore.
Keep the main, WAL, and SHM artifacts together.

A 1024-record in-memory queue, bounded transaction batches, and an approximately
one-second timer keep SQLite work outside hooks. The timer is a flush target, not
a one-second loss bound: backlog or an uncommitted transaction can exceed it,
and WAL/NORMAL does not guarantee recent commits survive process kill, OS crash,
or power loss. Full queues, runtime write failure, forced shutdown, and stuck
storage can lose rows. Failed or ambiguously committed batches are counted and
not replayed; the writer continues with later batches. Runtime levels describe
live consequence rather than cumulative history: contained loss/queue pressure
is `Warn`, consecutive current write failure is `Error`, and a later confirmed
commit records recovery instead of leaving future reports at `Error`. Shutdown
cuts off
admission after server drain, attempts a bounded final flush under the fresh
`--shutdown-timeout`, and publishes aggregate queue, write, late, final-flush,
and cleanup status while logging remains alive. Runtime and final store records
are serialized so the final aggregate is terminal. Its counters are snapped
immediately before publication; calls completed while waiting for native cleanup
or an earlier runtime log are included, while later calls are outside that
snapshot. The SQLite/native wait is bounded, but synchronous `slog.Handler` I/O
is not deadline-aware. There are no public queue-depth or flush-interval tuning
settings.

### `--shim-hook-overrun-threshold`

Sets the one global duration after which a still-running post-commit Shim hook
becomes a **Hook overrun**. The default is `1s`; `0` disables monitoring, and
negative values are rejected before the server binds. This setting applies to
all monitored SSE and WebSocket hook roles — there are no per-role or
per-registration overrides. Monitoring reports threshold crossings but never
bounds, interrupts, or cancels hook execution.

### `--codex-catalog-enabled`

Allows a Codex-shaped model catalog when the request has `client_version` and a
catalog alias, global auto-review model, per-main-model reviewer override, or
live-limit override is configured.

### `--codex-catalog-model-aliases`

Sets explicit Codex catalog aliases as a comma-separated string of
`LIVE_COPILOT_MODEL_ID=OFFICIAL_CODEX_METADATA_SOURCE` pairs. The default is an
empty map. For example:

```sh
copilotd serve \
  --codex-catalog-enabled \
  --codex-catalog-model-aliases 'gpt-example-alias=gpt-example' \
  --codex-auto-review-model-overrides \
    'gpt-example-alias=gpt-example-alias'
```

The left side is a real Copilot model ID that Codex selects and sends unchanged
to `/responses`; this setting never rewrites an inference request. The right
side is used only as a metadata source. It must name a complete entry in the
current accepted official Codex catalog, but it need not be live in Copilot.
The example's per-main-model override also demonstrates valid self-review by
the served alias.

The same map can be supplied through
`COPILOTD_CODEX_CATALOG_MODEL_ALIASES` or the flat TOML string key:

```toml
codex-catalog-model-aliases = "gpt-example-alias=gpt-example"
```

Matching is exact and case-sensitive. Surrounding whitespace and empty comma
segments are ignored, and each non-empty segment splits on its first `=`. A
missing `=`, empty alias, empty source, duplicate alias, or alias-to-itself map
fails configuration resolution before the server binds. Every supplied TOML,
environment, and flag layer is parsed eagerly, so a malformed lower-precedence
value remains an error even when a higher layer is valid. Among valid layers,
flag > environment > TOML > default precedence replaces the complete map;
layers are never merged. A non-empty map is valid but inert while
`--codex-catalog-enabled=false`, allowing staged rollout.

An alias is emitted only while Copilot reports it as picker-visible and
Responses-forwardable and its metadata source exists in the accepted Codex
catalog. Exact official metadata wins if Codex later publishes the alias slug.
Every configured mapping that is not applied produces one `Warn` record per
Codex catalog request with `model`, `metadata_source`, and `skip_reason`:

- `alias_not_forwardable`: the live Copilot-forwardable set lacks the alias;
- `metadata_source_missing`: the accepted Codex catalog lacks its source; or
- `shadowed_by_official`: Codex now has an exact entry for the alias, so that
  official entry is still served with ordinary reviewer/live-limit mutations.

A `shadowed_by_official` warning repeats on every Codex catalog request until
the superseded mapping is removed. The other two conditions omit only the
affected alias. An accepted source's own client gates and behavior remain
authoritative, including `minimal_client_version`, visibility, priority,
reasoning presets, service tiers, prompts, tool policy, and model messages.
The operator owns the compatibility assertion between those values and the
real alias model; copilotd validates source existence and completeness, not
behavioral suitability. Because those gates come from the source, they can
leave an alias hidden or unsuitable for a particular Codex client even though
the mapping applied successfully; that condition produces no `skip_reason`
warning.

Complete-source fidelity can give the source and alias duplicate picker labels
and ranking. Under client/catalog version skew, an alias can therefore change
the default selected model. Operators should choose compatible sources and
account for both effects during rollout. The alias also disappears safely if
live eligibility or source availability is later lost; no official cached
Codex bytes are edited or persisted.

### `--codex-auto-review-model`

Injects the model slug as Codex's auto-review model when its served slug belongs
to the complete emitted Codex membership, including resolved exact official
entries and Codex catalog aliases. The injected value takes precedence over
Codex's provider default. As of Codex
0.153.4, command-auth providers default to `gpt-5.6-luna`; an explicit value
remains useful for stable routing across Codex versions and changing Copilot
lineups.

### `--codex-auto-review-model-overrides`

Sets per-main-model reviewers as a comma-separated string of `MAIN=REVIEWER`
pairs. The default is the empty string. For example:

```sh
copilotd serve --codex-auto-review-model-overrides \
  'gpt-5.4=gpt-5.4-mini,gpt-5.6-sol=gpt-5.4'
```

For each advertised main model, its per-model override wins; models without an
override fall back to `--codex-auto-review-model`. A configured per-model entry
is authoritative: if its reviewer cannot be advertised, copilotd skips that
injection and warns instead of silently using the global reviewer.

The exact configuration precedence is flag > environment variable > TOML file >
default. Every supplied layer must contain a valid override string; a malformed
TOML or environment value fails configuration resolution even when a valid
higher-precedence value is also supplied. Among valid layers, the
highest-precedence layer supplies the complete map, which is replaced wholesale
rather than merged across layers. The environment variable is
`COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES`, and the flat TOML string key is
`codex-auto-review-model-overrides`.

Surrounding whitespace is ignored, and empty comma-separated segments are
tolerated, including a trailing or doubled comma. Any non-empty segment with a
missing `=`, empty main-model slug, or empty reviewer slug fails configuration
resolution before the server binds; duplicate main-model slugs also fail fast.

### `--codex-catalog-override-limits`

Reports live Copilot prompt and context limits in the Codex catalog instead of
the vendored Codex limits.

### `--codex-catalog-refresh-interval`

Sets the best-effort cadence for checking the latest stable `openai/codex`
release, resolving its tag to a commit, and refreshing Codex's `models.json`
cached value from that commit. The default is `24h`;
`0` disables outbound refresh and pins the enabled catalog to its embedded
fallback. Negative values are rejected. When `--codex-catalog-enabled=false`,
the cached value is not registered and no Codex release request is made.

### `--github-oauth-token`

Supplies the GitHub OAuth token inline. This secret takes precedence over the
token file.

### `--startup-mint-retries`

Sets retries after the initial Copilot-token mint attempt for transient
failures; `0` disables retries.

### `--vscode-version`

Sets the bare VS Code version fallback used for Copilot client impersonation
when runtime discovery has no value.

### `--plugin-version`

Sets the bare Copilot Chat extension version fallback used for impersonation
when runtime discovery has no value.

### `--copilot-integration-id`

Sets the upstream `Copilot-Integration-Id` header.

### `--github-api-version`

Sets the upstream `X-GitHub-Api-Version` header.

### `--impersonation-refresh-interval`

Sets the runtime VS Code and Copilot Chat version rediscovery cadence; `0`
disables discovery.

### `--github-client-id`

Sets the GitHub device-flow OAuth application client ID, typically overridden
for GitHub Enterprise Server.

### `--github-scope`

Sets the non-empty OAuth scope requested during GitHub device flow.
