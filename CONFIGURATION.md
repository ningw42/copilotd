# Configuration

Configuration precedence is shown from left to right in the table below: an
explicit command-line flag overrides an environment variable, which overrides
the selected TOML file, which overrides the built-in default.

Flags must follow `copilotd serve` or `copilotd login`. No configuration file is
loaded automatically; select one with `--config` or `COPILOTD_CONFIG`. The file
uses the flat TOML keys shown below. Durations use Go duration syntax such as
`500ms`, `30s`, or `24h`; quote duration and other string values in TOML.

| CLI flag (highest precedence) | Environment variable | TOML key | Default (lowest precedence) | Command |
| --- | --- | --- | --- | --- |
| [`--config <PATH>`](#--config) | `COPILOTD_CONFIG` | — | No file | `serve`, `login` |
| [`--log-level <LEVEL>`](#--log-level) | `COPILOTD_LOG_LEVEL` | `log-level` | `info` | `serve`, `login` |
| [`--log-format <FORMAT>`](#--log-format) | `COPILOTD_LOG_FORMAT` | `log-format` | `text` | `serve`, `login` |
| [`--log-file <PATH>`](#--log-file) | `COPILOTD_LOG_FILE` | `log-file` | Empty (stderr) | `serve`, `login` |
| [`--github-oauth-token-file <PATH>`](#--github-oauth-token-file) | `COPILOTD_GITHUB_OAUTH_TOKEN_FILE` | `github-oauth-token-file` | `<user config dir>/copilotd/github-oauth-token` | `serve`, `login` |
| [`--addr <HOST:PORT>`](#--addr) | `COPILOTD_ADDR` | `addr` | `127.0.0.1:8080` | `serve` |
| [`--shutdown-timeout <DURATION>`](#--shutdown-timeout) | `COPILOTD_SHUTDOWN_TIMEOUT` | `shutdown-timeout` | `10s` | `serve` |
| [`--apikey <KEY>`](#--apikey) | `COPILOTD_APIKEY` | `apikey` | Required | `serve` |
| [`--outbound-timeout <DURATION>`](#--outbound-timeout) | `COPILOTD_OUTBOUND_TIMEOUT` | `outbound-timeout` | `600s` | `serve` |
| [`--stream-idle-timeout <DURATION>`](#--stream-idle-timeout) | `COPILOTD_STREAM_IDLE_TIMEOUT` | `stream-idle-timeout` | `5m` | `serve` |
| [`--stream-keepalive-interval <DURATION>`](#--stream-keepalive-interval) | `COPILOTD_STREAM_KEEPALIVE_INTERVAL` | `stream-keepalive-interval` | `15s` | `serve` |
| [`--write-timeout <DURATION>`](#--write-timeout) | `COPILOTD_WRITE_TIMEOUT` | `write-timeout` | `90s` | `serve` |
| [`--response-header-timeout <DURATION>`](#--response-header-timeout) | `COPILOTD_RESPONSE_HEADER_TIMEOUT` | `response-header-timeout` | `600s` | `serve` |
| [`--ws-handshake-timeout <DURATION>`](#--ws-handshake-timeout) | `COPILOTD_WS_HANDSHAKE_TIMEOUT` | `ws-handshake-timeout` | `10s` | `serve` |
| [`--max-request-bytes <BYTES>`](#--max-request-bytes) | `COPILOTD_MAX_REQUEST_BYTES` | `max-request-bytes` | `33554432` (32 MiB) | `serve` |
| [`--max-buffered-response-bytes <BYTES>`](#--max-buffered-response-bytes) | `COPILOTD_MAX_BUFFERED_RESPONSE_BYTES` | `max-buffered-response-bytes` | `33554432` (32 MiB) | `serve` |
| [`--shim-nop-enabled=<BOOL>`](#--shim-nop-enabled) | `COPILOTD_SHIM_NOP_ENABLED` | `shim-nop-enabled` | `false` | `serve` |
| [`--codex-catalog-enabled=<BOOL>`](#--codex-catalog-enabled) | `COPILOTD_CODEX_CATALOG_ENABLED` | `codex-catalog-enabled` | `false` | `serve` |
| [`--codex-auto-review-model <SLUG>`](#--codex-auto-review-model) | `COPILOTD_CODEX_AUTO_REVIEW_MODEL` | `codex-auto-review-model` | Empty | `serve` |
| [`--codex-auto-review-model-overrides <MAP>`](#--codex-auto-review-model-overrides) | `COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES` | `codex-auto-review-model-overrides` | Empty | `serve` |
| [`--codex-catalog-override-limits=<BOOL>`](#--codex-catalog-override-limits) | `COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS` | `codex-catalog-override-limits` | `false` | `serve` |
| [`--github-oauth-token <TOKEN>`](#--github-oauth-token) | `COPILOTD_GITHUB_OAUTH_TOKEN` | `github-oauth-token` | Empty | `serve` |
| [`--startup-mint-retries <COUNT>`](#--startup-mint-retries) | `COPILOTD_STARTUP_MINT_RETRIES` | `startup-mint-retries` | `3` | `serve` |
| [`--vscode-version <VERSION>`](#--vscode-version) | `COPILOTD_VSCODE_VERSION` | `vscode-version` | `1.104.1` | `serve` |
| [`--plugin-version <VERSION>`](#--plugin-version) | `COPILOTD_PLUGIN_VERSION` | `plugin-version` | `0.26.7` | `serve` |
| [`--copilot-integration-id <ID>`](#--copilot-integration-id) | `COPILOTD_COPILOT_INTEGRATION_ID` | `copilot-integration-id` | `vscode-chat` | `serve` |
| [`--github-api-version <VERSION>`](#--github-api-version) | `COPILOTD_GITHUB_API_VERSION` | `github-api-version` | `2025-04-01` | `serve` |
| [`--impersonation-refresh-interval <DURATION>`](#--impersonation-refresh-interval) | `COPILOTD_IMPERSONATION_REFRESH_INTERVAL` | `impersonation-refresh-interval` | `24h` | `serve` |
| [`--github-client-id <ID>`](#--github-client-id) | `COPILOTD_GITHUB_CLIENT_ID` | `github-client-id` | `Iv1.b507a08c87ecfe98` | `login` |
| [`--github-scope <SCOPE>`](#--github-scope) | `COPILOTD_GITHUB_SCOPE` | `github-scope` | `read:user` | `login` |

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

Limits genuine upstream silence on a streaming response.

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

### `--shim-nop-enabled`

Enables the canonical no-op response shim.

### `--codex-catalog-enabled`

Allows a Codex-shaped model catalog when the request has `client_version` and an
auto-review model or live-limit override is configured.

### `--codex-auto-review-model`

Injects the model slug as Codex's auto-review model when it is present in both
the vendored Codex catalog and the live Copilot catalog.

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
default. The highest-precedence layer supplies the complete string; maps are
replaced wholesale rather than merged across layers. The environment variable is
`COPILOTD_CODEX_AUTO_REVIEW_MODEL_OVERRIDES`, and the flat TOML string key is
`codex-auto-review-model-overrides`.

Surrounding whitespace is ignored, and empty comma-separated segments are
tolerated, including a trailing or doubled comma. Any non-empty segment with a
missing `=`, empty main-model slug, or empty reviewer slug fails configuration
resolution before the server binds; duplicate main-model slugs also fail fast.

### `--codex-catalog-override-limits`

Reports live Copilot prompt and context limits in the Codex catalog instead of
the vendored Codex limits.

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
