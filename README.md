# copilotd

A single-binary proxy for the Anthropic Messages and OpenAI Responses APIs,
backed by one GitHub Copilot subscription and protected by an operator-managed
API key. Requests stay within their API family; copilotd does not translate
between Anthropic and OpenAI.

## Quick start

Download a [release](https://github.com/ningw42/copilotd/releases) for Linux
x86-64, macOS arm64, or Windows x86-64/arm64 and put `copilotd` on your `PATH`.
Then, in a POSIX shell:

```sh
copilotd login
export COPILOTD_APIKEY='replace-with-a-long-random-secret'
copilotd serve
```

`login` uses GitHub's device flow and saves a GitHub OAuth token locally.
`serve` listens on `127.0.0.1:8080` by default. Clients send your API key as
`Authorization: Bearer <KEY>` or `x-api-key: <KEY>`; it is never sent upstream.

See [Configuration](CONFIGURATION.md) for flags, environment variables, TOML
settings, and supplying a GitHub OAuth token without login.

## API surfaces

| Method | Path | Behavior |
| --- | --- | --- |
| POST | `/anthropic/v1/messages` | Anthropic Messages, JSON or SSE |
| POST | `/anthropic/v1/messages/count_tokens` | Anthropic token counting |
| POST | `/openai/v1/responses` | OpenAI Responses, JSON or SSE |
| GET (WebSocket upgrade) | `/openai/v1/responses` | OpenAI Responses over WebSocket |
| GET / HEAD | `/models` | Raw GitHub Copilot model data |
| GET / HEAD | `/anthropic/v1/models` | Provider-shaped Anthropic catalog |
| GET / HEAD | `/openai/v1/models` | Provider-shaped OpenAI catalog, or opt-in Codex catalog |

Provider-shaped catalogs use provider schemas with Copilot's values, not a
promise of direct-provider capabilities. The [Codex catalog](CONFIGURATION.md#--codex-catalog-enabled)
is selected by `?client_version=` when enabled and configured.

Chat Completions, embeddings, multi-tenant keys/quotas/billing, and multi-account
pooling are not supported. Responses retrieve, delete, cancel, and input-item
operations are [out of scope](.out-of-scope/responses-management-operations.md);
HTTP `background: true` requests are rejected.

`GET /healthz` reports liveness. `GET /readyz` checks local serving prerequisites,
not GitHub or Copilot reachability.

## Usage reports

The opt-in Usage meter records best-effort completion history in local SQLite
for Anthropic Messages (JSON/SSE) and OpenAI Responses (JSON/SSE/WebSocket).
It stores native token counts and completion metadata, not prompts or generated
content.

**Enabling metering exposes `/usage/v1/report` without authentication on the
same listener.** Anyone who can reach it can read model identities, activity
history, estimated valuation, and pricing coverage. Inference API keys, missing
CORS headers, and private database permissions do not protect this HTTP path.
Restrict access with binding, firewall, or reverse-proxy policy.

Add the flag when starting the daemon, then query it from another terminal:

```sh
copilotd serve --shim-usage-meter-enabled

copilotd usage                         # Current month, daily groups, local timezone
copilotd usage --timezone Europe/Berlin --surface openai --details
copilotd usage --timezone UTC --period month --since 2026-01-01 --until 2027-01-01 --json
```

The CLI reads from the running daemon, not directly from SQLite. Use `--endpoint`
to override `http://127.0.0.1:8080`, and an explicit `--timezone Area/City` or
`--timezone UTC` if local timezone discovery fails. Reports support daily,
Monday-weekly, monthly, and yearly groups, with separate Anthropic and OpenAI
counts. `--model` filters an exact Reported model; `--details` adds native metrics
and pricing matches; `--json` emits the validated report.

Estimated cost uses current original-provider rates from the daemon's models.dev
pricing snapshot. Reports are neither a Copilot bill nor proof of complete
consumption; coverage describes only stored Turns. Disabled metering returns an error, not empty history.
See [report options and limits](CONFIGURATION.md#usage) and
[meter configuration](CONFIGURATION.md#--shim-usage-meter-enabled) for details.

## Design principles

- **Raw passthrough first.** Preserve request/response bodies and unknown fields
  with minimal interpretation; no cross-family translation.
- **Opt-in inference shims.** Ordered shims transform or observe the forward path
  without accessing Copilot or driving retries. Support catalogs own their
  representations outside the shim layers.
- **Transform without fabrication.** Shims must use upstream-basis information,
  make buffering costs explicit, and keep post-commit hooks prompt and
  non-blocking. Departures from verbatim forwarding belong in the
  [divergence ledger](docs/divergence-ledger.md).
- **Observability without secrets.** Structured, request-correlated logs and
  bounded counters accompany each component; API keys, GitHub OAuth tokens, and
  Copilot tokens stay out of logs. Counters are not a full exported metrics system.

See the [shim design](docs/design/2026-07-16-phase-3-middleware-framework-design.md)
and [architecture decisions](docs/adr/) for implementation contracts.

### State at rest

No database or companion service is required by default. The owner-only GitHub
OAuth token file is the only persisted application state unless the Usage meter
is enabled; an injected GitHub OAuth token needs no file. Metering adds a private
local SQLite database and WAL/SHM sidecars. Copilot tokens and refreshed cached
values stay in memory. See [database operations](CONFIGURATION.md#--usage-db-path)
for upgrades, backups, retention, and Windows permission caveats.

## Limitations and risks

- **Unofficial integration.** copilotd impersonates the VS Code Copilot client.
  GitHub's terms and abuse controls apply; automated or bulk use may trigger
  restrictions.
- **Upstream drift.** Copilot endpoints, headers, models, and streaming behavior
  can change. Shims and catalogs do not guarantee parity with direct provider APIs.
- **Plain HTTP listener.** Use a TLS reverse proxy or tunnel when needed; network
  access policy is especially important when usage reporting is enabled.

## Documentation and development

- [Configuration](CONFIGURATION.md): operator settings and usage-report contract.
- [Domain glossary](CONTEXT.md): canonical terminology.
- [Architecture decisions](docs/adr/) and [design documents](docs/design/):
  rationale and implementation details; historical plans are not a completion checklist.
- [Verification guide](docs/verification/usage-reporting.md): platform evidence
  and revision-specific release gates, not guarantees from cross-builds alone.
- [GitHub Issues](https://github.com/ningw42/copilotd/issues): proposed and planned work.

The Go toolchain comes from the Nix development shell (Linux x86-64 and macOS arm64):

```sh
nix develop -c go test ./...
nix develop -c go test -race ./... -count=1
nix fmt
nix flake check
```
