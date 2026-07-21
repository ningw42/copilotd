# Discover impersonation versions at runtime with an embedded fallback

**Status:** proposed
**Date:** 2026-07-20

## Summary

copilotd impersonates the VS Code Copilot client through a fixed header set
(`Editor-Version`, `Editor-Plugin-Version`, `User-Agent`, `Copilot-Integration-Id`,
`X-GitHub-Api-Version`). Two of those headers carry version numbers that go stale:
the pinned defaults ship as `vscode/1.104.1` and `copilot-chat/0.26.7`, while the
live releases are already many versions ahead. Today keeping them current is a
manual rotation chore, and an operator who wants freshness must hand-set
nonsense version strings.

This design makes the two rotating version facts **self-updating**. On startup —
and every 24h thereafter — copilotd discovers the current VS Code version and the
current GitHub Copilot Chat extension version from their public release endpoints,
holds them in memory, and derives the version-bearing headers from them.
Discovery is authoritative: there is no operator override. The former version
flags are demoted to an **embedded fallback** used only when discovery has not
succeeded (offline, air-gapped, upstream hiccup), so behavior is never worse than
today's static defaults. Nothing is written to disk; the cache lives in memory
beside the Copilot token. An operator who genuinely wants no outbound discovery —
air-gapped, or a locked-down egress allowlist — sets
`--impersonation-refresh-interval=0`, which disables discovery and pins to the
fallback.

The refresh-with-fallback mechanism is a small concrete helper inside
`internal/impersonation` (`versionFact`), deliberately **not** extracted into a
generic primitive: it has one present shape (a bare version string) and the only
mooted future consumer, issue #53 (the vendored Codex `models.json` snapshot), has
an unsettled runtime-vs-build-time question. If #53 adopts this mechanism,
generalizing it is that issue's responsibility.

## Motivation

### Only two facts actually rotate

The impersonation set has five headers, but they are not equally volatile:

| Header | What it is | Rotates? | Discoverable source |
| --- | --- | --- | --- |
| `Editor-Version` = `vscode/1.104.1` | VS Code app version | **Yes** | VS Code stable releases API |
| `Editor-Plugin-Version` = `copilot-chat/0.26.7` | Copilot Chat ext version | **Yes** | Marketplace `extensionquery` |
| `User-Agent` = `GitHubCopilotChat/0.26.7` | *same* Copilot Chat version | **Yes** (derived) | (same as above) |
| `Copilot-Integration-Id` = `vscode-chat` | Fixed integration id | No | — |
| `X-GitHub-Api-Version` = `2025-04-01` | GitHub REST API date pin | Rarely | No clean endpoint |

There are really only **two underlying facts** that rot — the VS Code version and
the Copilot Chat extension version — and the extension version feeds *two* headers
(`Editor-Plugin-Version` and `User-Agent`). The other two headers are stable
identifiers, not versions, and stay static. (`github-client-id`, the device-flow
OAuth app id, is used only by `copilotd login`, never by `serve`, and is out of
scope.)

### The pinned defaults rot silently

The exchange and every inference request carry these headers so upstream
client/user-agent allowlist checks pass. When the pinned versions drift far enough
from what a real VS Code + Copilot Chat install would send, the impersonation
weakens. Keeping the two defaults current is manual, and the values are opaque
enough that no operator should have to supply them.

### Prior art

`copilot-proxy` (upstream `Jer-y/copilot-proxy`) already discovers the **VS Code
version** at startup from `update.code.visualstudio.com/api/releases/stable`,
caching it in memory with a hardcoded `FALLBACK`; it leaves the Copilot Chat
version, integration id, api-version, and client id static. This design follows
the same best-effort/fallback shape, extends it to the Copilot Chat extension
version, adds a periodic refresh, and surfaces the result on `/readyz`.

## Design

### `versionFact`: the self-refreshing version helper

A small concrete helper inside `internal/impersonation` (standard library only)
holding one bare version string that refreshes itself in the background, with an
embedded fallback. It is deliberately **not** generic and **not** its own package:
both consumers are `string`, and the only mooted future consumer (#53) has an
unsettled shape, so generalizing now would be a guess.

```go
type source string

const (
	sourceDiscovered source = "discovered" // last refresh succeeded (or a prior one did)
	sourceFallback   source = "fallback"   // never discovered; serving the embedded fallback
)

type snapshot struct {
	source      source
	lastSuccess time.Time // zero until the first successful discovery
	lastAttempt time.Time
	lastErr     string    // "" when the last attempt succeeded
}

type versionFact struct { /* fallback, discover func, clock, logger, RWMutex-guarded state */ }

func newVersionFact(fallback string, discover func(context.Context) (string, error), opts ...option) *versionFact

func (f *versionFact) current() (string, snapshot)       // atomic read; string == fallback while source == sourceFallback
func (f *versionFact) refresh(ctx context.Context) error  // one attempt; updates state
func (f *versionFact) run(ctx context.Context, interval time.Duration) // ticker loop of refresh
```

Semantics:

- **Cold.** Until the first success, `current()` returns the fallback with
  `source == sourceFallback`.
- **Success.** `refresh` swaps the value in, sets `source == sourceDiscovered`,
  stamps `lastSuccess`, clears `lastErr`.
- **Warm failure.** After a prior success, a failed `refresh` keeps the
  **last-good** value (`source` stays `sourceDiscovered`); it only records
  `lastAttempt`/`lastErr` and lets `lastSuccess` age. A transient upstream blip
  never downgrades a known-good version — the same "survive a blip" logic the
  Copilot token uses across idle expiry.
- **run** does not refresh immediately; the caller primes explicitly (see
  lifecycle) so startup ordering is under the caller's control.

An `option` covers an injectable clock (`func() time.Time`) and a logger, so the
loop and the aging logic are deterministically testable.

### `internal/impersonation`: facts, fallbacks, and the header set

This package owns the two version facts, the discovery edge, and the assembler that
turns facts into headers.

```go
package impersonation

type Set struct {
	vscode *versionFact // bare "1.104.1" fallback, discovers "1.129.1"
	plugin *versionFact // bare "0.26.7"  fallback, discovers "0.48.1"
	integrationID string       // static
	apiVersion    string       // static
}

func New(cfg Config, edge Edge, logger *slog.Logger) *Set

func (s *Set) Header() http.Header       // the live impersonation set
func (s *Set) Observe() Observed         // non-secret snapshot for /readyz
func (s *Set) Prime(ctx context.Context) // bounded, blocking first discovery
func (s *Set) Run(ctx context.Context, interval time.Duration) // periodic refresh
```

**One derivation path for fallback and discovery alike.** The `versionFact` holds
the *bare* version (`"1.104.1"`), exactly the shape discovery returns, and a single
set of pure functions prefixes it into headers:

- `Editor-Version` = `"vscode/" + v`
- `Editor-Plugin-Version` = `"copilot-chat/" + c`
- `User-Agent` = `"GitHubCopilotChat/" + c`

Because the fallback is a bare version and the same functions derive both cases,
`derive(fallback)` reproduces today's exact default strings — there is no separate
"full-string fallback" to keep in sync. `Header()` reads `vscode.current()` and
`plugin.current()`, derives the three version headers, and sets the two static
headers:

```go
func (s *Set) Header() http.Header {
	v, _ := s.vscode.current()
	c, _ := s.plugin.current()
	h := http.Header{}
	h.Set("Copilot-Integration-Id", s.integrationID)
	h.Set("Editor-Version", "vscode/"+v)
	h.Set("Editor-Plugin-Version", "copilot-chat/"+c)
	h.Set("User-Agent", "GitHubCopilotChat/"+c)
	h.Set("X-GitHub-Api-Version", s.apiVersion)
	return h
}
```

`Header()` builds a fresh map on every call, so a mid-flight refresh is picked up
by the next exchange and the next forwarded request with no shared mutable state.

**The discovery edge** is injected so it is stubbable in tests, mirroring how the
GitHub exchange base URL is already injected:

```go
type Edge struct {
	VSCodeBaseURL      string       // prod: https://update.code.visualstudio.com
	MarketplaceBaseURL string       // prod: https://marketplace.visualstudio.com
	Client             *http.Client // plain client; no Copilot credentials or impersonation headers
}
```

Two discovery functions, each bound into a version fact as its `discover`:

- **VS Code** — `GET {VSCodeBaseURL}/api/releases/stable` returns a JSON array of
  version strings newest-first; take element `[0]`.
- **Copilot Chat** — `POST {MarketplaceBaseURL}/_apis/public/gallery/extensionquery`
  with header `Accept: application/json;api-version=7.2-preview.1` and a body
  filtering `filterType 7` = `GitHub.copilot-chat`, with `flags = 0x11`
  (`IncludeVersions | IncludeVersionProperties` — the full version list *with*
  properties, deliberately **not** `IncludeLatestVersionOnly`, which would return
  only the newest version and could hand back a pre-release). Walk
  `results[0].extensions[0].versions`, skip any version whose `properties` contain
  `Microsoft.VisualStudio.Code.PreRelease == "true"`, and take the first (newest)
  remaining **stable** version.

Both validate the result against `^\d+\.\d+\.\d+` before accepting it; a malformed
or implausible response is treated as a failure so garbage can never overwrite a
good value. Discovery sends no Copilot credentials or impersonation headers — the
targets are public update/marketplace endpoints.

### The identity seam

`Manager` stops holding a static `http.Header` and depends on a one-method
interface it defines:

```go
// internal/identity
type Impersonation interface { Header() http.Header }
```

`ManagerConfig.Impersonation` becomes this interface. The two existing read sites
change from ranging over `m.impersonation` to ranging over
`m.impersonation.Header()`:

- `exchange` applies the current headers to the token-exchange request.
- `credentialFrom` sets `Headers: m.impersonation.Header()` on the `Credential`,
  which the forward and WebSocket-forward paths already consume.

For tests and any static caller, the package provides a trivial adapter:

```go
func StaticImpersonation(h http.Header) Impersonation
```

`*impersonation.Set` satisfies `Impersonation` directly. No import cycle: `identity`
defines the interface; `impersonation` does not import `identity`; `main`
wires the concrete `Set` into the `Manager`.

### Startup and refresh lifecycle

Discovery **precedes the first mint** on a bounded best-effort wait so the very
first exchange already looks current — but it is a *wait, not a gate*: a slow or
failed `Prime` leaves the fact on its fallback and the mint proceeds regardless.
Discovery *outcome* never gates readiness; `Prime` only spends up to 5s of boot
time trying. The HTTP listener is bound before this runs, so `/healthz` and a
degraded `/readyz` serve throughout.

```go
go func() {
	imp.Prime(serveCtx)                              // both discoveries, concurrent, ≤5s overall
	go imp.Run(serveCtx, cfg.ImpersonationRefreshInterval) // 24h ticker loop
	mgr.StartupMint(serveCtx)                        // first mint carries fresh headers
}()
```

`Prime` wraps `serveCtx` in a 5s timeout, runs both facts' `refresh` concurrently,
and returns when both settle or the deadline fires — whichever comes first. A
discovery that misses the deadline leaves that fact on its fallback; the mint
proceeds regardless. `Run` starts the periodic loop **after** priming, so there is
no redundant immediate refresh. Both facts share the one configurable interval. On
`serveCtx` cancellation at shutdown, `Prime` returns early and `Run` exits cleanly.
A cold `Prime` miss is not retried before the next 24h tick — the fallback (today's
working value) covers the gap, so the single fixed cadence needs no cold-start
special case.

**Disabling discovery.** When `ImpersonationRefreshInterval == 0`, the discovery
orchestration is skipped — no `Prime`, no `Run`, and no outbound **discovery** calls to
the Microsoft endpoints — and both facts stay on their fallback for the process
lifetime. `StartupMint` still runs (carrying fallback headers) and its GitHub
**exchange** call is unaffected, so readiness behaves exactly as today. This is the
supported air-gapped /
locked-egress mode; combined with the two fallback flags it is also the one honest
way to pin a version by hand (discovery is off, so there is nothing to override).

Readiness is unchanged: `/readyz` reports the **last mint outcome** only. Discovery
never gates it.

### Configuration (fallbacks 5 → 4, no overrides)

The version-override flags are removed and replaced by two **bare-version fallback**
flags; the two static-identifier flags are unchanged; one cadence flag is added.

Removed: `--editor-version`, `--editor-plugin-version`, `--copilot-user-agent`
(clean rename, no aliases — copilotd is pre-1.0).

| Flag / env | Default | Role |
| --- | --- | --- |
| `--vscode-version` / `COPILOTD_VSCODE_VERSION` | `1.104.1` | Fallback for the VS Code fact; derives `Editor-Version`. |
| `--plugin-version` / `COPILOTD_PLUGIN_VERSION` | `0.26.7` | Fallback for the Copilot Chat fact; derives `Editor-Plugin-Version` and `User-Agent`. |
| `--copilot-integration-id` / `COPILOTD_COPILOT_INTEGRATION_ID` | `vscode-chat` | Static `Copilot-Integration-Id`. |
| `--github-api-version` / `COPILOTD_GITHUB_API_VERSION` | `2025-04-01` | Static `X-GitHub-Api-Version`. |
| `--impersonation-refresh-interval` / `COPILOTD_IMPERSONATION_REFRESH_INTERVAL` | `24h` | Re-discovery cadence; must be `>= 0`. `0` disables discovery (pins to fallback). |

Precedence collapses to two tiers, with **no override layer**:

```
discovered  >  fallback (configured value, else embedded default)
```

A set fallback flag never wins over a successful discovery; it only supplies the
value served until (or unless) discovery succeeds. There is deliberately no way to
force a version *over* a live discovery — if discovery returns something wrong, the
fix is in discovery, not an operator knob. The one supported way to serve a
hand-set version is to turn discovery off (`--impersonation-refresh-interval=0`),
which is explicit rather than a silent override. The two fallback flags validate as
non-empty bare versions. `ServeConfig` gains `VSCodeVersionFallback`,
`PluginVersionFallback`, and `ImpersonationRefreshInterval`; `CopilotIntegrationID`
and `GithubAPIVersion` remain. The startup config log lists the two version
fallbacks, the two static ids, and the interval.

### `/readyz` observability

`/readyz` stays unauthenticated and keeps its coarse `status` bit, so existing
consumers are unaffected. It gains an `impersonation` block reporting the effective
headers and per-fact freshness. The block is present in both the ready (`200`) and
degraded (`503`) responses — impersonation freshness is independent of mint
readiness — and `HEAD` still writes no body.

```json
{
  "status": "ready",
  "impersonation": {
    "effective_headers": {
      "Editor-Version": "vscode/1.129.1",
      "Editor-Plugin-Version": "copilot-chat/0.48.1",
      "User-Agent": "GitHubCopilotChat/0.48.1",
      "Copilot-Integration-Id": "vscode-chat",
      "X-GitHub-Api-Version": "2025-04-01"
    },
    "discovery": {
      "vscode":       { "source": "discovered", "last_success": "2026-07-20T12:00:00Z" },
      "copilot_chat": { "source": "fallback",   "last_success": null }
    }
  }
}
```

Only **non-secret** state appears: the effective version strings (already logged
normally as non-secret) plus each fact's `source` and `last_success`. No token, no
mint detail, and no raw discovery-error text — a failed discovery is conveyed by
`source: "fallback"` (never succeeded) or an aging `last_success` (succeeded
before), never by an error string that could leak an upstream URL or internal to an
unauthenticated caller. `Set.Observe()` returns the `Observed` DTO carrying this
shape. The readyz handler consumes it through a one-method interface **defined in
`server`** (mirroring how `identity` defines its `Impersonation` interface):
`server` imports `impersonation` only for the `Observed` type and owns the JSON
rendering, keeping handlers and rendering out of `impersonation`. When discovery is
disabled (`interval == 0`), the block still renders, with both facts at
`source: "fallback"` and null `last_success`.

### Error handling and resilience

- Each discovery call is bounded (5s), and `Prime` caps the combined startup wait
  at 5s.
- Failures never touch readiness and never overwrite a good value — cold failures
  hold the fallback, warm failures hold the last-good.
- Malformed / implausible responses (shape-check miss) are failures, not
  poison-writes.
- The refresh loop stops on context cancellation at shutdown.
- Logging: startup discovery outcome at info; each refresh success at debug;
  refresh failure at warn (naturally rate-limited by the 24h cadence).

## Testing

Test-first, matching the package layout:

- **`versionFact`** — injected `discover` and a fake clock: cold serves fallback;
  success swaps to discovered; warm failure holds last-good and ages
  `lastSuccess`; `run` refreshes on each tick; `current()` is race-clean under
  `-race`.
- **Discovery functions** — `httptest` servers returning a canned VS Code array
  and a Marketplace payload that *includes* a pre-release version, proving it is
  filtered out; malformed bodies and timeouts return errors.
- **`impersonation.Set`** — assembler correctness across all four states (both
  discovered, VS-Code-only, Copilot-only, neither → exact fallback strings);
  `Observe()` exposes only the non-secret shape.
- **lifecycle** — `interval == 0` skips `Prime`/`Run` and makes no outbound discovery call
  (wired against a discovery edge that fails the test if hit); a cold `Prime` miss
  leaves the fact on its fallback and does not retry before the next tick.
- **`identity.Manager`** — interface-based impersonation; `exchange` and
  `credentialFrom` read the *current* header; a swap between calls is reflected on
  the next call. Existing tests wrap their static header via `StaticImpersonation`.
- **`server` `/readyz`** — ready and degraded bodies both carry the block; `HEAD`
  writes nothing; a fallback fact renders `source: "fallback"` with null
  `last_success` and no error text.
- **e2e `serve`** — inject stub discovery base URLs (same pattern as the injected
  GitHub exchange base URL) and assert the exchange/forward requests carry the
  discovered versions and that `/readyz` reports them.

## Considered alternatives

- **Mutable header + setter on the `Manager`** (a refresh loop calls
  `mgr.SetImpersonation`): rejected — it bolts version-refresh state and API onto
  the identity `Manager`, blurring its single responsibility (minting), and leaves
  #53 nothing to reuse.
- **Rediscover inside each mint** (piggyback on the ~25-min token re-mint):
  rejected — it re-discovers ~50× more often than versions change, couples two
  unrelated cadences, and does nothing for the forward path between mints.
- **Keep the version flags as live overrides that win over discovery**: rejected
  at the maintainer's direction — an override that beats discovery reintroduces
  exactly the manual, rot-prone knob this design removes. Configured values become
  the fallback instead; the only supported hand-set path is turning discovery off
  entirely (`--impersonation-refresh-interval=0`).
- **Discovery gates readiness** (`/readyz` = mint AND discovery): rejected — it
  would couple uptime to a cosmetic version string when the fallback already works.
- **Persist the cache to a file**: rejected — it would add durable state at rest,
  against ROADMAP §2 and the Copilot token's in-memory model. This is the exact
  point where #53's original file-cache framing diverges from this project's
  principles.

## Consequences

- The two rotating headers stay current with zero operator action; the fallback
  guarantees behavior is never worse than today's static defaults.
- There is no operator override for a successfully discovered version — a
  deliberate simplification recorded in ADR-0008. The one supported way to serve a
  hand-set version is to disable discovery with
  `--impersonation-refresh-interval=0`.
- One **new outbound dependency** is introduced: two public, unauthenticated
  Microsoft endpoints (VS Code update + Marketplace), hit at startup and every
  24h. Because they are *not* the Copilot exchange or inference endpoints and
  carry no credentials, they add none of the idle-exchange abuse signal that
  ADR-0001 avoided; a daily update-check is far below normal editor traffic to
  those hosts. `--impersonation-refresh-interval=0` opts out of the dependency
  entirely, for air-gapped or locked-egress deployments.
- `/readyz` reveals slightly more than a coarse bit — but only non-secret version
  strings and freshness — and stays backward-compatible on its `status` field.
- The flag surface changes: `--editor-version`, `--editor-plugin-version`, and
  `--copilot-user-agent` are removed in favor of `--vscode-version` and
  `--plugin-version`; `--impersonation-refresh-interval` (`0` disables discovery)
  is added. Pre-1.0, no aliases are kept.
- The refresh-with-fallback helper (`versionFact`) stays **concrete inside
  `internal/impersonation`**. Issue #53 is the same *shape* but not a present
  consumer; if it adopts this mechanism, extracting and generalizing it is #53's
  responsibility.

## Relationship to issue #53

#53 (commit-based caching of the vendored Codex `models.json` snapshot) is the
same *shape* — a vendored value that rots, refreshed from upstream with an embedded
fallback. It stays a **separate** effort: it carries a fidelity/trust surface
(pulled prompts become a model's live values) and an unresolved runtime-vs-build-time
question this work does not. This design keeps its refresh helper **concrete inside
`internal/impersonation`** rather than pre-extracting a shared primitive: with only
one present consumer and #53's shape unsettled, generalizing now would be a guess.
If #53 adopts this mechanism, extracting and generalizing it is that issue's
responsibility.

## ADR

This design is recorded as **ADR-0008** (`docs/adr/0008-discover-impersonation-versions-at-runtime.md`).
