# copilotd

Run Anthropic Messages and OpenAI Responses APIs on a GitHub Copilot
subscription, behind an operator-managed API key. copilotd is a single-binary
proxy, not a cross-family translation engine.

See [Configuration](CONFIGURATION.md) for the complete command-line flag,
environment variable, and TOML configuration reference.

## Scope

One GitHub Copilot account, one configured inbound API key. Clients present the
API key using `Authorization: Bearer` or `x-api-key`; it is never sent upstream.
`copilotd login` obtains a GitHub OAuth token through device flow, or an operator
can supply one directly. The identity manager exchanges it for a short-lived
Copilot token used only on authenticated upstream calls.

Explicit non-goals:

- Chat Completions, in either direction: copilotd neither accepts it nor calls it
  upstream.
- Cross-family inference translation: Anthropic stays Anthropic; Responses stays
  Responses.
- Multi-tenant API-key minting, per-key quotas, or billing.
- Multi-account pooling or rotation of GitHub Copilot subscriptions.
- Embeddings. They could be considered as a GitHub Copilot-native support Route
  later, but are not a current goal.

## API surfaces

Inference is mounted under provider-namespaced paths; GitHub Copilot-native
support data uses unprefixed paths.

| Method | Path | Behavior |
| --- | --- | --- |
| POST | `/anthropic/v1/messages` | Anthropic Messages, JSON or SSE |
| POST | `/anthropic/v1/messages/count_tokens` | Anthropic token counting |
| POST | `/openai/v1/responses` | OpenAI Responses, JSON or SSE |
| GET (WebSocket upgrade) | `/openai/v1/responses` | OpenAI Responses WebSocket transport |
| GET / HEAD | `/models` | Raw GitHub Copilot model data, without reshaping |
| GET / HEAD | `/anthropic/v1/models` | Provider-shaped Anthropic catalog |
| GET / HEAD | `/openai/v1/models` | Provider-shaped OpenAI catalog, or the opt-in Codex catalog |

The support data has two distinct representations: the GitHub Copilot Surface's
raw `/models` response and provider/client-shaped catalogs. Provider-shaped
catalogs use the providers' schemas with Copilot's values, not invented
provider-level capabilities ([ADR-0004](docs/adr/0004-provider-shaped-catalogs-report-copilot-values.md)).
The Codex catalog uses official Codex metadata with governed alias, reviewer, and
live-limit mutations; it is selected by `?client_version=` when enabled and
configured. See the [catalog configuration](CONFIGURATION.md#--codex-catalog-enabled).

Responses management subpaths (retrieve, delete, cancel, and input items) are
not served; see the [support-boundary decision](.out-of-scope/responses-management-operations.md).
HTTP `background:true` requests are rejected rather than creating responses that
clients cannot retrieve through copilotd.

`GET /healthz` reports liveness; `GET /readyz` reports local serving prerequisites
and non-secret runtime observations. Readiness is not a guarantee that GitHub or
Copilot is reachable.

## Design principles

- **Raw passthrough first.** Forward request and response bodies with minimal
  interpretation, preserving unknown fields. Strict SDK deserialization and
  reserialization do not belong on the raw path; typed handling belongs only in
  the specific transforms that need it.
- **No cross-family translation.** Each inference Surface forwards only to its
  matching upstream Surface and Route.
- **Inference extensions use the shim onion.** Ordered, individually toggleable
  shims wrap the forward path; they can close a parity gap by transforming
  requests or responses, or observe forwarded inference data without changing
  it. Hooks span response Preludes, buffered bodies, SSE streams, and opt-in
  WebSocket Messages. A shim never accesses Copilot or drives an upstream retry.
  First-party support catalogs own their representations outside the inference
  shim onion.
- **Transform without fabrication.** Shims may alter, drop, hold, or coalesce
  upstream-basis content, but cannot invent information without an upstream
  basis. Buffering costs must be explicit, and post-commit hooks must remain
  prompt and non-blocking. Stream transforms must compose with transport-owned
  terminal handling, keepalives, and cancellation. Governed departures from
  verbatim forwarding belong in the [divergence ledger](docs/divergence-ledger.md).
- **Observability is cross-cutting.** Structured, request-correlated logs and
  bounded metric scaffolding accompany each component, rather than being a
  finishing feature. Observe request outcomes and latency, upstream calls,
  token minting, and stream outcomes without logging API keys, GitHub OAuth
  tokens, or Copilot tokens. The current counters do not constitute a full
  exported metrics system; the logging contract is
  [ADR-0015](docs/adr/0015-govern-log-record-structure-with-ordinary-slog.md).

### State at rest

By default, there is no database or required companion service. The opt-in Usage
meter is the one narrow exception: `--shim-usage-meter-enabled` creates a private
local SQLite main/WAL/SHM file set for best-effort Turn history
([ADR-0017](docs/adr/0017-persist-usage-in-local-sqlite.md)). The current
implementation records qualifying **buffered Anthropic Messages plus buffered,
SSE, and WebSocket OpenAI Responses** completions. Only Anthropic SSE recording
remains staged. With the flag off,
`serve` creates no usage files or writer and installs no metering hook. See the
[complete meter configuration and operating contract](CONFIGURATION.md#--shim-usage-meter-enabled).

The owner-only GitHub OAuth token file remains the only other persisted
application state; an injected GitHub OAuth token needs no file. Copilot tokens
and best-effort cached values stay in memory, with embedded fallbacks and no
disk persistence
([ADR-0009](docs/adr/0009-refresh-codex-models-from-latest-release-in-memory.md)).
Optional configuration files and log destinations are operator inputs/outputs,
not additional application state stores.

## Architecture

The edge authenticates the API key, then dispatches by typed Endpoint contract.
On inference paths, the shim onion surrounds the forwarder; the shared Upstream
call component attaches the Copilot token and impersonation headers. Responses
return through the applicable body, SSE, or WebSocket transforms. Catalog
handlers instead fetch support data and render their own representations.

| Component | Owner | Responsibility |
| --- | --- | --- |
| Process and configuration | `cmd/copilotd`, `internal/config` | CLI, flags/env/TOML, dependency wiring, startup and shutdown |
| Edge and Endpoint contracts | `internal/server`, `internal/endpoint` | Listener, routing, constant-time API-key validation, health/readiness, typed served contracts |
| Identity | `internal/identity` | Device flow, GitHub OAuth token file, single-flight startup/on-demand Copilot token minting; no scheduled token refresh ([ADR-0001](docs/adr/0001-on-demand-copilot-token-minting.md)) |
| Impersonation and cached values | `internal/impersonation`, `internal/cache` | Runtime version discovery and memory-only cached values with embedded fallbacks |
| Upstream call | `internal/upstream` | Authenticated upstream request construction, headers, correlation, bounded reads, failure classification |
| Forwarding and streaming | `internal/forward`, `internal/sse`, `internal/wsforward` | Raw HTTP/WebSocket forwarding, SSE framing and terminal handling, OpenAI SSE keepalives, cancellation |
| Inference shims | `internal/shim` | Ordered hook contract for opt-in parity transforms and read-only observers, including the Responses item-id stabilizer, buffered Usage meter observers for both inference Surfaces, and OpenAI SSE/WebSocket completion observation |
| Usage persistence | `internal/usage`, `internal/usage/sqlitestore` | Standard-library usage contract plus private local SQLite writer, migrations, bounded loss reporting, and finalization |
| Catalogs | `internal/catalog` | Provider-shaped and Codex model catalogs |
| Observability | `internal/logging`, `internal/requestsummary`, component-owned counters | Structured logs, request correlation, terminal summaries, metric scaffolding |
| Build and distribution | `flake.nix`, `.github/workflows/` | Reproducible builds, verification, release archives and checksums |

Hook contracts and buffering/state trade-offs are detailed in the
[shim design](docs/design/2026-07-16-phase-3-middleware-framework-design.md), with
the current post-commit contract in
[ADR-0014](docs/adr/0014-infallible-post-commit-shim-hooks.md).

## Platforms and distribution

Release packaging targets four native binaries:

- `x86_64-linux` (`linux/amd64`)
- `x86_64-windows` (`windows/amd64`)
- `arm64-windows` (`windows/arm64`)
- `aarch64-darwin` (`darwin/arm64`)

The [release workflow](.github/workflows/release.yml) cross-compiles archives and
publishes checksums. Nix provides development/build environments for Linux
x86-64 and macOS arm64. Builds disable cgo; Linux is fully static, while Darwin
still links the system `libSystem` library. No companion daemon is required; the
local usage database is opt-in. SQLite runtime, locking, and permission evidence
is native on Linux; Windows and Darwin have cgo-free build evidence only, and
Windows ACL behavior remains best effort rather than certified. Optional
OS-service installation is not implemented
([#191](https://github.com/ningw42/copilotd/issues/191)).

## Limitations and risks

- **Unsupported upstream integration.** copilotd impersonates the VS Code
  Copilot client. This is not an officially supported integration; GitHub's terms
  and abuse controls apply, and automated or bulk use may trigger restrictions.
- **Upstream drift.** Copilot can change endpoints, headers, model names, or
  streaming behavior at any time. Raw passthrough limits exposure, but identity,
  shims, and provider/client-shaped catalogs remain sensitive to changes.
- **Streaming transforms.** Stateful transforms and buffering can compromise
  latency or stream validity. The shim contract makes those costs explicit;
  there is no blanket parity guarantee with either provider's direct API.

## Documentation and development

- [Configuration](CONFIGURATION.md): operator settings.
- [Domain glossary](CONTEXT.md): canonical project terminology.
- [Architecture decisions](docs/adr/): accepted decisions and their trade-offs.
- [Design documents](docs/design/): feature designs, including historical phase
  plans and proposals; these are not a current completion checklist.
- [GitHub Issues](https://github.com/ningw42/copilotd/issues): proposed and planned
  work. Unfinished ideas from the initial phased plan are triage candidates, not
  automatically approved commitments.

The Go toolchain comes from the Nix development shell:

```sh
nix develop -c go test ./...
nix develop -c go test -race ./... -count=1
nix fmt
nix flake check
```
