# copilotd

Run Anthropic Messages and OpenAI Responses APIs on a GitHub Copilot
subscription.

## Configuration

Configuration precedence is shown from left to right in the table below: an
explicit command-line flag overrides an environment variable, which overrides
the selected TOML file, which overrides the built-in default.

Flags must follow `copilotd serve` or `copilotd login`. No configuration file is
loaded automatically; select one with `--config` or `COPILOTD_CONFIG`. The file
uses the flat TOML keys shown below. Durations use Go duration syntax such as
`500ms`, `30s`, or `24h`; quote duration and other string values in TOML.

| CLI flag (highest precedence) | Environment variable | TOML key | Default (lowest precedence) | Command | What it does |
| --- | --- | --- | --- | --- | --- |
| `--config <PATH>` | `COPILOTD_CONFIG` | — | No file | `serve`, `login` | Selects the optional TOML configuration file. The flag path overrides the environment path. |
| `--log-level <LEVEL>` | `COPILOTD_LOG_LEVEL` | `log-level` | `info` | `serve`, `login` | Sets the minimum log level: `debug`, `info`, `warn`, or `error`. |
| `--log-format <FORMAT>` | `COPILOTD_LOG_FORMAT` | `log-format` | `text` | `serve`, `login` | Selects `text` or `json` structured logs. |
| `--log-file <PATH>` | `COPILOTD_LOG_FILE` | `log-file` | Empty (stderr) | `serve`, `login` | Writes logs to this file; an empty value writes to standard error. |
| `--github-oauth-token-file <PATH>` | `COPILOTD_GITHUB_OAUTH_TOKEN_FILE` | `github-oauth-token-file` | `<user config dir>/copilotd/github-oauth-token` | `serve`, `login` | Sets the raw GitHub OAuth token file that `login` writes and `serve` reads. |
| `--addr <HOST:PORT>` | `COPILOTD_ADDR` | `addr` | `127.0.0.1:8080` | `serve` | Sets the proxy listen address. Port `0` requests an automatically assigned port. |
| `--shutdown-timeout <DURATION>` | `COPILOTD_SHUTDOWN_TIMEOUT` | `shutdown-timeout` | `10s` | `serve` | Sets the positive grace period for HTTP server shutdown before a forced close. |
| `--apikey <KEY>` | `COPILOTD_APIKEY` | `apikey` | Required | `serve` | Sets the secret clients must send as `Authorization: Bearer <KEY>` or `x-api-key: <KEY>`. |
| `--outbound-timeout <DURATION>` | `COPILOTD_OUTBOUND_TIMEOUT` | `outbound-timeout` | `600s` | `serve` | Sets the positive total backstop for buffered upstream responses and model-catalog fetches. |
| `--stream-idle-timeout <DURATION>` | `COPILOTD_STREAM_IDLE_TIMEOUT` | `stream-idle-timeout` | `5m` | `serve` | Limits genuine upstream silence on a streaming response. |
| `--stream-keepalive-interval <DURATION>` | `COPILOTD_STREAM_KEEPALIVE_INTERVAL` | `stream-keepalive-interval` | `15s` | `serve` | Sets the idle interval before copilotd emits an OpenAI stream keepalive. |
| `--write-timeout <DURATION>` | `COPILOTD_WRITE_TIMEOUT` | `write-timeout` | `90s` | `serve` | Limits each downstream HTTP write and each WebSocket write in either direction. |
| `--response-header-timeout <DURATION>` | `COPILOTD_RESPONSE_HEADER_TIMEOUT` | `response-header-timeout` | `600s` | `serve` | Limits how long HTTP forwarding waits for upstream response headers. |
| `--ws-handshake-timeout <DURATION>` | `COPILOTD_WS_HANDSHAKE_TIMEOUT` | `ws-handshake-timeout` | `10s` | `serve` | Limits the upstream WebSocket handshake. |
| `--max-request-bytes <BYTES>` | `COPILOTD_MAX_REQUEST_BYTES` | `max-request-bytes` | `33554432` (32 MiB) | `serve` | Caps inbound HTTP request bodies and each WebSocket message in either direction; values must be positive. |
| `--max-buffered-response-bytes <BYTES>` | `COPILOTD_MAX_BUFFERED_RESPONSE_BYTES` | `max-buffered-response-bytes` | `33554432` (32 MiB) | `serve` | Caps model-catalog response bodies and upstream response bodies processed by a buffered-response shim; values must be positive. |
| `--shim-nop-enabled=<BOOL>` | `COPILOTD_SHIM_NOP_ENABLED` | `shim-nop-enabled` | `false` | `serve` | Enables the canonical no-op response shim. |
| `--codex-catalog-enabled=<BOOL>` | `COPILOTD_CODEX_CATALOG_ENABLED` | `codex-catalog-enabled` | `false` | `serve` | Allows a Codex-shaped model catalog when the request has `client_version` and an auto-review model or live-limit override is configured. |
| `--codex-auto-review-model <SLUG>` | `COPILOTD_CODEX_AUTO_REVIEW_MODEL` | `codex-auto-review-model` | Empty | `serve` | Injects the model slug as Codex's auto-review model when it is present in both the vendored Codex catalog and the live Copilot catalog. |
| `--codex-catalog-override-limits=<BOOL>` | `COPILOTD_CODEX_CATALOG_OVERRIDE_LIMITS` | `codex-catalog-override-limits` | `false` | `serve` | Reports live Copilot prompt and context limits in the Codex catalog instead of the vendored Codex limits. |
| `--github-oauth-token <TOKEN>` | `COPILOTD_GITHUB_OAUTH_TOKEN` | `github-oauth-token` | Empty | `serve` | Supplies the GitHub OAuth token inline. This secret takes precedence over the token file. |
| `--startup-mint-retries <COUNT>` | `COPILOTD_STARTUP_MINT_RETRIES` | `startup-mint-retries` | `3` | `serve` | Sets retries after the initial Copilot-token mint attempt for transient failures; `0` disables retries. |
| `--vscode-version <VERSION>` | `COPILOTD_VSCODE_VERSION` | `vscode-version` | `1.104.1` | `serve` | Sets the bare VS Code version fallback used for Copilot client impersonation when runtime discovery has no value. |
| `--plugin-version <VERSION>` | `COPILOTD_PLUGIN_VERSION` | `plugin-version` | `0.26.7` | `serve` | Sets the bare Copilot Chat extension version fallback used for impersonation when runtime discovery has no value. |
| `--copilot-integration-id <ID>` | `COPILOTD_COPILOT_INTEGRATION_ID` | `copilot-integration-id` | `vscode-chat` | `serve` | Sets the upstream `Copilot-Integration-Id` header. |
| `--github-api-version <VERSION>` | `COPILOTD_GITHUB_API_VERSION` | `github-api-version` | `2025-04-01` | `serve` | Sets the upstream `X-GitHub-Api-Version` header. |
| `--impersonation-refresh-interval <DURATION>` | `COPILOTD_IMPERSONATION_REFRESH_INTERVAL` | `impersonation-refresh-interval` | `24h` | `serve` | Sets the runtime VS Code and Copilot Chat version rediscovery cadence; `0` disables discovery. |
| `--github-client-id <ID>` | `COPILOTD_GITHUB_CLIENT_ID` | `github-client-id` | `Iv1.b507a08c87ecfe98` | `login` | Sets the GitHub device-flow OAuth application client ID, typically overridden for GitHub Enterprise Server. |
| `--github-scope <SCOPE>` | `COPILOTD_GITHUB_SCOPE` | `github-scope` | `read:user` | `login` | Sets the non-empty OAuth scope requested during GitHub device flow. |
