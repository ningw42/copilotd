# Discover impersonation versions at runtime with an embedded floor

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
flags are demoted to an **embedded floor** used only when discovery has not
succeeded (offline, air-gapped, upstream hiccup), so behavior is never worse than
today's static defaults. Nothing is written to disk; the cache lives in memory
beside the Copilot token.

The refresh-with-floor mechanism is factored into a small generic primitive,
`internal/refresh`, so that issue #53 (the vendored Codex `models.json` snapshot)
can adopt the same shape once its own runtime-vs-build-time question is settled.

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
the same best-effort/floor shape, extends it to the Copilot Chat extension
version, adds a periodic refresh, and surfaces the result on `/readyz`.

## Design

### `internal/refresh`: the shared primitive

A dependency-light generic package (standard library only) holding one value that
refreshes itself in the background, with an embedded floor.

```go
package refresh

type Source string

const (
	SourceDiscovered Source = "discovered" // last refresh succeeded (or a prior one did)
	SourceFloor      Source = "floor"      // never discovered; serving the embedded floor
)

type Snapshot struct {
	Source      Source
	LastSuccess time.Time // zero until the first successful discovery
	LastAttempt time.Time
	LastErr     string    // "" when the last attempt succeeded
}

type Value[T any] struct { /* floor, discover func, clock, logger, RWMutex-guarded state */ }

func New[T any](floor T, discover func(context.Context) (T, error), opts ...Option) *Value[T]

func (v *Value[T]) Current() (T, Snapshot)   // atomic read; T == floor while Source == SourceFloor
func (v *Value[T]) Refresh(ctx context.Context) error // one attempt; updates state
func (v *Value[T]) Run(ctx context.Context, interval time.Duration) // ticker loop of Refresh
```

Semantics:

- **Cold.** Until the first success, `Current()` returns the floor with
  `Source == SourceFloor`.
- **Success.** `Refresh` swaps the value in, sets `Source == SourceDiscovered`,
  stamps `LastSuccess`, clears `LastErr`.
- **Warm failure.** After a prior success, a failed `Refresh` keeps the
  **last-good** value (`Source` stays `SourceDiscovered`); it only records
  `LastAttempt`/`LastErr` and lets `LastSuccess` age. A transient upstream blip
  never downgrades a known-good version — the same "survive a blip" logic the
  Copilot token uses across idle expiry.
- **Run** does not refresh immediately; the caller primes explicitly (see
  lifecycle) so startup ordering is under the caller's control.

`Option` covers an injectable clock (`func() time.Time`) and a logger, so the loop
and the aging logic are deterministically testable. The package is a leaf; nothing
flows back into it.

### `internal/impersonation`: facts, floors, and the header set

This package owns the two refreshers, the discovery edge, and the assembler that
turns facts into headers.

```go
package impersonation

type Set struct {
	vscode *refresh.Value[string] // bare "1.104.1" floor, discovers "1.129.1"
	plugin *refresh.Value[string] // bare "0.26.7"  floor, discovers "0.48.1"
	integrationID string           // static
	apiVersion    string           // static
}

func New(cfg Config, edge Edge, logger *slog.Logger) *Set

func (s *Set) Header() http.Header       // the live impersonation set
func (s *Set) Observe() Observed         // non-secret snapshot for /readyz
func (s *Set) Prime(ctx context.Context) // bounded, blocking first discovery
func (s *Set) Run(ctx context.Context, interval time.Duration) // periodic refresh
```

**One derivation path for floor and discovery alike.** The refresher holds the
*bare* version (`"1.104.1"`), exactly the shape discovery returns, and a single
set of pure functions prefixes it into headers:

- `Editor-Version` = `"vscode/" + v`
- `Editor-Plugin-Version` = `"copilot-chat/" + c`
- `User-Agent` = `"GitHubCopilotChat/" + c`

Because the floor is a bare version and the same functions derive both cases,
`derive(floor)` reproduces today's exact default strings — there is no separate
"full-string floor" to keep in sync. `Header()` reads `vscode.Current()` and
`plugin.Current()`, derives the three version headers, and sets the two static
headers:

```go
func (s *Set) Header() http.Header {
	v, _ := s.vscode.Current()
	c, _ := s.plugin.Current()
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

Two discovery functions, each bound into a refresher as its `discover`:

- **VS Code** — `GET {VSCodeBaseURL}/api/releases/stable` returns a JSON array of
  version strings newest-first; take element `[0]`.
- **Copilot Chat** — `POST {MarketplaceBaseURL}/_apis/public/gallery/extensionquery`
  with header `Accept: application/json;api-version=7.2-preview.1` and a body
  filtering `filterType 7` = `GitHub.copilot-chat`, with `flags = 0x101`
  (`IncludeVersions | IncludeVersionProperties` — the full version list *with*
  properties, deliberately **not** `IncludeLatestVersionOnly`, which would return
  only the newest version and could hand back a pre-release). Walk
  `results[0].extensions[0].versions`, skip any version whose `properties` contain
  `Microsoft.VisualStudio.Code.PreRelease == "true"`, and take the first (newest)
  remaining **stable** version.

Both validate the result against `^\d+\.\d+\.\d+` before accepting it; a malformed
or implausible response is treated as a failure so garbage can never overwrite a
good floor. Discovery sends no Copilot credentials or impersonation headers — the
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
defines the interface; `impersonation` imports `refresh` but not `identity`; `main`
wires the concrete `Set` into the `Manager`.

### Startup and refresh lifecycle

Discovery **blocks the first mint** so the very first exchange already looks
current, but is bounded so a hung endpoint cannot stall boot. The HTTP listener is
bound before this runs, so `/healthz` and a degraded `/readyz` serve throughout.

```go
go func() {
	imp.Prime(serveCtx)                              // both discoveries, concurrent, ≤5s overall
	go imp.Run(serveCtx, cfg.ImpersonationRefreshInterval) // 24h ticker loop
	mgr.StartupMint(serveCtx)                        // first mint carries fresh headers
}()
```

`Prime` wraps `serveCtx` in a 5s timeout, runs both refreshers' `Refresh`
concurrently, and returns when both settle or the deadline fires — whichever comes
first. A discovery that misses the deadline leaves that fact on its floor; the mint
proceeds regardless. `Run` starts the periodic loop **after** priming, so there is
no redundant immediate refresh. Both refreshers share the one configurable
interval. On `serveCtx` cancellation at shutdown, `Prime` returns early and `Run`
exits cleanly.

Readiness is unchanged: `/readyz` reports the **last mint outcome** only. Discovery
never gates it.

### Configuration (floors 5 → 4, no overrides)

The version-override flags are removed and replaced by two **bare-version floor**
flags; the two static-identifier flags are unchanged; one cadence flag is added.

Removed: `--editor-version`, `--editor-plugin-version`, `--copilot-user-agent`
(clean rename, no aliases — copilotd is pre-1.0).

| Flag / env | Default | Role |
| --- | --- | --- |
| `--vscode-version` / `COPILOTD_VSCODE_VERSION` | `1.104.1` | Floor for the VS Code fact; derives `Editor-Version`. |
| `--plugin-version` / `COPILOTD_PLUGIN_VERSION` | `0.26.7` | Floor for the Copilot Chat fact; derives `Editor-Plugin-Version` and `User-Agent`. |
| `--copilot-integration-id` / `COPILOTD_COPILOT_INTEGRATION_ID` | `vscode-chat` | Static `Copilot-Integration-Id`. |
| `--github-api-version` / `COPILOTD_GITHUB_API_VERSION` | `2025-04-01` | Static `X-GitHub-Api-Version`. |
| `--impersonation-refresh-interval` / `COPILOTD_IMPERSONATION_REFRESH_INTERVAL` | `24h` | Re-discovery cadence; must be `> 0`. |

Precedence collapses to two tiers, with **no override layer**:

```
discovered  >  floor (configured value, else embedded default)
```

A set floor flag never wins over a successful discovery; it only supplies the
fallback. There is deliberately no way to force a version over discovery — if
discovery returns something wrong, the fix is in discovery, not an operator knob.
The two floor flags validate as non-empty bare versions. `ServeConfig` gains
`VSCodeVersionFloor`, `PluginVersionFloor`, and `ImpersonationRefreshInterval`;
`CopilotIntegrationID` and `GithubAPIVersion` remain. The startup config log lists
the four floors and the interval.

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
      "copilot_chat": { "source": "floor",      "last_success": null }
    }
  }
}
```

Only **non-secret** state appears: the effective version strings (already logged
normally as non-secret) plus each fact's `source` and `last_success`. No token, no
mint detail, and no raw discovery-error text — a failed discovery is conveyed by
`source: "floor"` (never succeeded) or an aging `last_success` (succeeded before),
never by an error string that could leak an upstream URL or internal to an
unauthenticated caller. `Set.Observe()` returns this shape; the readyz handler
takes an observer alongside the readiness `Provider`.

### Error handling and resilience

- Each discovery call is bounded (5s), and `Prime` caps the combined startup wait
  at 5s.
- Failures never touch readiness and never overwrite a good value — cold failures
  hold the floor, warm failures hold the last-good.
- Malformed / implausible responses (shape-check miss) are failures, not
  poison-writes.
- The refresh loop stops on context cancellation at shutdown.
- Logging: startup discovery outcome at info; each refresh success at debug;
  refresh failure at warn (naturally rate-limited by the 24h cadence).

## Testing

Test-first, matching the package layout:

- **`refresh.Value`** — injected `discover` and a fake clock: cold serves floor;
  success swaps to discovered; warm failure holds last-good and ages
  `LastSuccess`; `Run` refreshes on each tick; `Current()` is race-clean under
  `-race`.
- **Discovery functions** — `httptest` servers returning a canned VS Code array
  and a Marketplace payload that *includes* a pre-release version, proving it is
  filtered out; malformed bodies and timeouts return errors.
- **`impersonation.Set`** — assembler correctness across all four states (both
  discovered, VS-Code-only, Copilot-only, neither → exact floor strings);
  `Observe()` exposes only the non-secret shape.
- **`identity.Manager`** — interface-based impersonation; `exchange` and
  `credentialFrom` read the *current* header; a swap between calls is reflected on
  the next call. Existing tests wrap their static header via `StaticImpersonation`.
- **`server` `/readyz`** — ready and degraded bodies both carry the block; `HEAD`
  writes nothing; a floored fact renders `source: "floor"` with null
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
  the floor instead.
- **Discovery gates readiness** (`/readyz` = mint AND discovery): rejected — it
  would couple uptime to a cosmetic version string when the floor already works.
- **Persist the cache to a file**: rejected — it would add durable state at rest,
  against ROADMAP §2 and the Copilot token's in-memory model. This is the exact
  point where #53's original file-cache framing diverges from this project's
  principles.

## Consequences

- The two rotating headers stay current with zero operator action; the floor
  guarantees behavior is never worse than today's static defaults.
- There is no operator override for a successfully discovered version — a
  deliberate simplification recorded in ADR-0008.
- One **new outbound dependency** is introduced: two public, unauthenticated
  Microsoft endpoints (VS Code update + Marketplace), hit at startup and every
  24h. Because they are *not* the Copilot exchange or inference endpoints and
  carry no credentials, they add none of the idle-exchange abuse signal that
  ADR-0001 avoided; a daily update-check is far below normal editor traffic to
  those hosts.
- `/readyz` reveals slightly more than a coarse bit — but only non-secret version
  strings and freshness — and stays backward-compatible on its `status` field.
- The flag surface changes: `--editor-version`, `--editor-plugin-version`, and
  `--copilot-user-agent` are removed in favor of `--vscode-version` and
  `--plugin-version`. Pre-1.0, no aliases are kept.
- `internal/refresh` is a reusable primitive; issue #53 is its next candidate
  consumer once that issue settles its runtime-vs-build-time question.

## Relationship to issue #53

#53 (commit-based caching of the vendored Codex `models.json` snapshot) is the
same *shape* — a vendored value that rots, refreshed from upstream with an embedded
floor. It stays a **separate** effort: it carries a fidelity/trust surface (pulled
prompts become a model's live values) and an unresolved runtime-vs-build-time
question this work does not. This design deliberately extracts `internal/refresh`
so #53 can adopt it later without re-inventing the mechanism.

## ADR

This design is recorded as **ADR-0008** (`docs/adr/0008-discover-impersonation-versions-at-runtime.md`).
