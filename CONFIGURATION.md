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

Sets the positive grace period for HTTP server shutdown before a forced close.

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
buffered-response shim; values must be positive.

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
