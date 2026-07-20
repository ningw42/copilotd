# Discover impersonation versions at runtime with an embedded fallback

**Status:** proposed

copilotd impersonates the VS Code Copilot client through a fixed header set. Two of
those headers carry version numbers that rot — `Editor-Version` (the VS Code
version) and `Editor-Plugin-Version` / `User-Agent` (both the GitHub Copilot Chat
extension version). copilotd **discovers** those two facts at runtime — once at
startup and every 24h — from their public release endpoints
(`update.code.visualstudio.com` for VS Code, the Visual Studio Marketplace
`extensionquery` for `GitHub.copilot-chat`), holds them in memory, and derives the
headers from them. Discovery is **authoritative**: there is no operator override. A
value the operator configures becomes an **embedded fallback**, served only when
discovery has not succeeded (cold start offline, air-gapped, or an upstream
hiccup), so behavior is never worse than the previous static defaults. Setting
`--impersonation-refresh-interval=0` disables discovery entirely, pinning to the
fallback for air-gapped or locked-egress deployments. The two non-version headers
(`Copilot-Integration-Id`, `X-GitHub-Api-Version`) remain static. Nothing is
written to disk; the cache lives in memory beside the Copilot token. This reverses
the prior stance — recorded in `config.go` §6.7 — that these were operator-set
"knobs because they are version-sensitive."

## Considered options

- **Static operator-set knobs** (status quo): rejected — the pinned defaults
  (`vscode/1.104.1`, `copilot-chat/0.26.7`) drift silently behind live releases,
  rotating them is a manual chore, and the values are opaque enough that no
  operator should have to supply them.
- **Live overrides that win over discovery**: rejected — an override that beats a
  successful discovery reintroduces the same manual, rot-prone knob this decision
  removes. Configured values are demoted to the fallback; discovery always wins when
  it succeeds. The only supported hand-set path is disabling discovery outright
  (`--impersonation-refresh-interval=0`), which is explicit rather than a silent
  override.
- **Proactive rediscovery inside each token mint**: rejected — versions change
  ~monthly while the mint runs every ~25 min, so it would re-discover ~50× too
  often, couple two unrelated cadences, and still not refresh the forward path
  between mints. A separate 24h loop is decoupled and far cheaper.
- **Discovery gates `/readyz`**: rejected — coupling uptime to a cosmetic version
  string when the fallback already works would make copilotd refuse traffic it could
  serve. Readiness stays "last mint outcome"; discovery is best-effort and only
  *observed* on `/readyz`.
- **Persist the cache to a file**: rejected — durable state at rest violates
  ROADMAP §2 and the Copilot token's in-memory model. The cache is memory-only.
- **Runtime discovery with an embedded fallback, memory-only** (chosen): the two
  facts self-update with zero operator action, the fallback guarantees a never-worse
  baseline, and the refresh-with-fallback mechanism is a concrete `versionFact`
  helper inside `internal/impersonation` — not a pre-extracted generic primitive.

## Consequences

- The two rotating headers stay current automatically; the embedded fallback keeps a
  never-worse-than-today baseline when discovery is unavailable. After a first
  success, a later failure holds the last-good value rather than reverting to the
  fallback.
- No operator can force a version over a successful discovery — a deliberate
  simplification. A wrong discovered value is a bug fixed in discovery, not routed
  around by a knob. The one supported hand-set path is disabling discovery with
  `--impersonation-refresh-interval=0`.
- One new outbound dependency is introduced: two public, unauthenticated Microsoft
  endpoints, hit at startup and every 24h. They are not the Copilot exchange or
  inference endpoints and carry no credentials, so they add none of the
  idle-exchange abuse signal ADR-0001 avoided. `--impersonation-refresh-interval=0`
  opts out of the dependency entirely.
- Startup discovery **precedes the first mint** on a bounded best-effort wait
  (≤5s) so the first exchange already presents current versions — but it is a wait,
  not a gate: a slow or failed discovery leaves the fact on its fallback and the
  mint proceeds, so discovery outcome never gates readiness. The listener is bound
  first, so `/healthz` and a degraded `/readyz` serve throughout.
- `/readyz` gains a non-secret `impersonation` block (effective headers + per-fact
  source and last-success), present in both ready and degraded responses; its
  `status` field is unchanged.
- The flag surface changes: `--editor-version`, `--editor-plugin-version`, and
  `--copilot-user-agent` are removed in favor of the bare-version fallbacks
  `--vscode-version` and `--plugin-version`; `--impersonation-refresh-interval`
  (default 24h, `0` disables discovery) is added. Pre-1.0, no aliases are kept.
- The refresh-with-fallback helper stays a concrete `versionFact` inside
  `internal/impersonation`. Issue #53 (the vendored Codex `models.json` snapshot)
  is the same shape but not a present consumer; if it adopts this mechanism,
  extracting and generalizing it is that issue's responsibility.

See `docs/design/2026-07-20-impersonation-version-discovery-design.md`.
