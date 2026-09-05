# Configuration

Configuration precedence is shown from left to right in the tables below: an
explicit command-line flag overrides an environment variable, which overrides
the selected TOML file, which overrides the built-in default.

Flags must follow `copilotd serve` or `copilotd login`. No configuration file is
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

**Implemented coverage is all five supported paths: buffered and SSE Anthropic
Messages plus buffered, SSE, and WebSocket OpenAI Responses.**

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
`/v1/messages/count_tokens` are not metered. There is no query API, CLI query
subcommand, aggregation, pricing or billing reconciliation, automatic pruning,
per-key attribution, or non-token usage projection. Query either native table
with external SQLite tooling, for example:

```sh
sqlite3 "$USAGE_DB" \
  'SELECT at_utc, model, input_tokens, output_tokens FROM anthropic_turn ORDER BY at_ms DESC;
   SELECT at_utc, model, input_tokens, output_tokens FROM openai_turn ORDER BY at_ms DESC;'
```

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
