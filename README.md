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

## Usage reports

**Enabling `serve --shim-usage-meter-enabled` also exposes aggregate history
without authentication on the existing `--addr` listener.** Anyone who can reach
`GET /usage/v1/report` can read model identities, activity patterns, estimated
current valuation, and pricing coverage. Inference
API keys, missing CORS headers, and private database permissions do not protect
this HTTP path; use bind/firewall/reverse-proxy policy. HTTP is plain TCP unless
an operator supplies a TLS reverse proxy or tunnel.

An enabled Usage meter also registers a memory-only `models.dev/api.json`
pricing cached value. Its identified vendored floor is immediately available;
best-effort refresh uses a credential-free, redirect-refusing public request and
holds last-good on failure. Original-provider rates are limited to OpenAI,
Anthropic, Google, and xAI. For each priceable Turn, the resolved Pricing
model's base rate vector applies unless its complete native input strictly
exceeds a structured context threshold; the greatest matching threshold then
selects that tier without filling omitted rates from another row. Structured
`tiers[].tier.size` is authoritative for every provider. The deprecated
`context_over_200k` row is a strict 200,000-token compatibility fallback only
when structured tiers are absent. OpenAI context is complete `input_tokens`;
Anthropic context is the checked sum of uncached input, cache creation, and
cache read. Prices are neither persisted nor a Copilot bill, and source failure
does not gate readiness or native reports. Set
`--usage-pricing-refresh-interval=0` to pin the embedded floor. Each report uses
one captured local snapshot; report requests perform no pricing network work.
The version-1 HTTP response identifies that snapshot under `pricing`, adds exact
USD `cost` and priced/unpriced Turn coverage to every row/model/section total,
and adds `pricing_match` to rows and range-model entries. Unpriceable Turns remain
successful native report data.

Reports support Anthropic and OpenAI, separately or together (the default), with
daily, Monday-weekly, monthly, or yearly groups in a named timezone:

```sh
copilotd usage # Current month, daily groups, supported Unix terminal-local zone
copilotd usage --timezone Europe/Berlin # Explicit override on every platform
copilotd usage --period week --timezone US/Eastern --since 2026-09-01 --until 2026-10-01
copilotd usage --timezone UTC --surface openai --model gpt-example --details
copilotd usage --timezone UTC --period month --since 2026-01-01 --json
# Optional: --surface anthropic or --surface openai (default: all)
# Optional: --endpoint https://example.test/copilotd
```

`--endpoint` defaults to `http://127.0.0.1:8080`; a path prefix is preserved when
appending `/usage/v1/report`. The command is an HTTP client, never an offline
SQLite reader. It shows exact native counts and stored-Turn coverage in
Reported-model rows grouped by period, with a `Day`, `Week`, `Month`, or `Year`
first-column heading. Each primary Surface table places **Est. USD** immediately
after `Model(s)` and before `Turns`; secondary native tables do not repeat money.
Amounts use exact daemon-supplied USD values rounded half up to three fractional
digits. Each period starts with a terminal-only `Total` derived from its validated
model rows and separated from the model breakdown. Text sums exact amounts before
rounding, uses checked int64 coverage/native subtotal arithmetic, and fails before
stdout if inconsistent rows or monetary addition would overflow; `--json` still
emits the independently validated original bytes.
Whole-range per-model and section totals remain in JSON but are not repeated in
the text tables. Anthropic appears first with **Uncached input**,
Output, Cache create, and Cache read; OpenAI retains complete Input, Output, Cache write,
and Cache read. TTL/thinking/reasoning subsets are never stacked onto their
parent counts, and there is no normalized input or cross-Surface token grand
total. Both sections share one read snapshot and request-wide limits.
Reports cover committed observations in the configured database,
including other writers and previous daemon runs; they neither flush queued
Turns nor guarantee freshness, completeness, consumption, or charges.
`generated_at` is the captured query clock, not a database commit timestamp or
freshness/completeness watermark. Metric coverage describes only stored Turns,
not all consumption.

Disabled metering returns an explicit error, not empty history. An enabled
empty selection prints `No stored Turns in the selected range.` Read failures,
unreachable daemons, invalid queries, and protocol errors remain failures.
Unselected native sections are omitted; selected empty sections stay visible.
Each omitted date bound independently uses the requested zone's current-month
start or next-month start from one daemon clock capture; period changes grouping
only. Date edges use actual timezone transitions, not fixed 24-hour durations.
The server supplies the effective range and retains bucket clipping/progress
metadata in JSON; static table period labels show only the bucket start date.
Named zones use embedded fallback data without a companion timezone asset;
operator/platform data can take precedence, and daemon rules are authoritative.
When `--timezone` is omitted, Linux/macOS use the CLI process's named `TZ` or a
verified system timezone symlink. Exactly empty OS `TZ` means configured UTC;
empty report-timezone overrides are errors. SSH, containers, and WSL use their
own process-visible configuration, not a physical workstation or remote daemon.
Copied/custom files, ambiguous names, unsupported rules, and `TZDIR` or
`ZONEINFO` values outside recognized system zoneinfo root paths cannot be
discovered: pass `--timezone Area/City` (or `--timezone UTC`). NixOS's
system-exported `TZDIR=/etc/zoneinfo` is recognized.
Native Windows always requires an explicit timezone, also settable through
`COPILOTD_TIMEZONE` or selected TOML. Explicit choices bypass discovery, not name
validation. Native runtime claims are revision-specific: see the
[verification guide](docs/verification/usage-reporting.md) and the retained
[#213 results](https://github.com/ningw42/copilotd/issues/213), not runner labels or
cross-builds alone.

`--model` selects an exact non-empty valid UTF-8 **Reported model**, preserving
case, whitespace, and Unicode without Catalog alias expansion or Requested-model
substitution. Unknown identities succeed empty. `--details` adds native reasoning
and reported-total counts for OpenAI, thinking/cache-TTL counts for Anthropic,
and a distinct Reported-model → Pricing-model resolution list with method or
explicit unknown/ambiguous status. A match does not by itself establish that a
Turn is priceable. Text safely ASCII-escapes
model identities without surrounding quotes, including boundary spaces. `—` means unreported, `0` means reported
zero, and `*` shows partial stored-Turn coverage. Static tables use rounded Lip Gloss borders without color
or interactive terminal control; `range_partial` and `in_progress` remain
available in JSON.
`--json` emits the complete validated original response plus a newline, preserving
exact decimal count and amount strings, Unicode, pricing fields, and unknown
additive fields. Pricing provenance and every nested cost/match object are an
atomic additive extension: the client accepts a wholly absent extension as an
older daemon, but rejects partial or dangling recognized fields. The current
`pricing` object omits the former presentation-only `context_policy`; clients
accept that legacy member as unknown additive data. Cost amounts are
exact nonnegative decimal strings; `null` means a nonempty aggregate has no
priceable Turns, while empty and priceable-free aggregates carry `"0"`. Coverage
partitions stored Turns into priced Turns and five explicit exclusion reasons.
In text, a partial priced subtotal has `*`, a wholly unpriced nonempty group has
`—`, and compact period/model notes list priced/total stored Turns plus every
nonzero exclusion reason. The header identifies models.dev standard/context
rates, fetched/fallback provenance, and successful fetch time when present,
followed by the current-rate and non-billing caveat. This source-level wording
also remains accurate for a legacy daemon whose additive `context_policy`
described highest-tier selection. A new CLI talking to an older daemon keeps native counts
usable and says `Estimated cost unavailable (daemon does not provide prices)`;
it never prices locally. `--details` does not change JSON or make another request.
There is no raw-Turn export, HTML, chart output, or cross-Surface monetary/token
grand total.
[Contention and lifecycle integration evidence](docs/research/2026-09-08-usage-reporting-concurrency.md)
includes real blocked TCP output and native SQLite cleanup; the
[release verification guide](docs/verification/usage-reporting.md) separates
implemented behavior from the required native release gate.
See [usage configuration](CONFIGURATION.md#usage) for limits and protocol details.

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
([ADR-0017](docs/adr/0017-persist-usage-in-local-sqlite.md)). The implementation
records qualifying **buffered and SSE Anthropic Messages plus buffered, SSE, and
WebSocket OpenAI Responses** completions: all five supported Surface/transport
paths. HTTP buffered/SSE rows also record the explicit upstream-bound
**Requested model** separately from the unchanged upstream **Reported model**;
WebSocket requested-model attribution remains absent. With the flag off,
`serve` creates no usage files or writer and installs no metering hook. See the
[complete meter configuration and operating contract](CONFIGURATION.md#--shim-usage-meter-enabled).

The owner-only GitHub OAuth token file remains the only other persisted
application state; an injected GitHub OAuth token needs no file. Copilot tokens
and best-effort cached values, including the models.dev pricing snapshot, stay
in memory with embedded fallbacks and no disk persistence
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
| Inference shims | `internal/shim` | Ordered hook contract for opt-in parity transforms and read-only observers, including the Responses item-id stabilizer and Usage meter completion observation on all five supported Surface/transport paths |
| Usage persistence | `internal/usage`, `internal/usage/sqlitestore` | Standard-library usage contract plus private local SQLite writer, migrations, bounded loss reporting, and finalization |
| Usage reporting | `internal/usage/report`, `internal/usage/reporthttp`, `internal/usage/reportcli` | Bounded snapshot aggregation, local HTTP contract and strict client validation, safe terminal presentation |
| Usage pricing | `internal/usage/pricing` | Validated original-provider pricing snapshots, exact rate/amount arithmetic, and the memory-only models.dev source |
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
local usage database is opt-in. The additive [native test matrix](.github/workflows/test.yml)
executes CGO-disabled acceptance on all four targets and records architecture,
SQLite, timezone, command, and skip evidence; Linux also runs the full race
suite. Only failed jobs upload evidence artifacts. A workflow definition is not
certification: consult the
[revision-specific verification record](docs/verification/usage-reporting.md).
Windows ACL behavior remains best effort, not a general desktop/ACL guarantee.
Optional OS-service installation is not implemented
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
