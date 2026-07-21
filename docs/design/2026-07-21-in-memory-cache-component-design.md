# Unify in-memory cached values into one component (and Codex `models.json` freshness)

**Status:** proposed
**Date:** 2026-07-21

## Summary

copilotd holds several "small bits in memory" that are refreshed over the process
lifetime. Two of them share one shape — a value served from an **embedded
fallback**, refreshed best-effort from upstream, held **in memory only**:

- the two **impersonation version facts** (`internal/impersonation.versionFact`),
  discovered from public Microsoft endpoints on a 24h cadence; and
- the vendored **Codex `models.json`** snapshot (`internal/catalog`), today a
  static `go:embed` that goes stale relative to upstream `openai/codex`.

ADR-0008 deliberately left the refresh-with-fallback helper **concrete** inside
`internal/impersonation`, and named issue #53 (freshening the Codex snapshot) as
"the same shape but not a present consumer," making the extraction #53's
responsibility. This design does that extraction. It introduces a small generic
component — **`internal/cache`** — that owns freshness, concurrency, fallback,
observability, and readiness for one cached value, ports impersonation onto it
with no behavior change, and adds the Codex `models.json` freshness (#53) as a
second consumer.

The component is memory-only: nothing is written to disk, so the ROADMAP §2 "no
state at rest" principle is preserved exactly as it is for the impersonation
cache and the Copilot token. The Codex consumer tracks `openai/codex`'s **latest
release tag**, refreshes on a static TTL, and keeps the embedded snapshot as the
guaranteed-parseable floor. A `--codex-catalog-refresh-interval=0` disables the
outbound refresh for air-gapped or locked-egress deployments, mirroring
`--impersonation-refresh-interval=0`.

The Copilot token cache stays **out of scope** — it is a materially different
animal (see §Scope).

## Motivation

### Three in-memory caches, two of one shape

| Cache | Where | Shape |
| --- | --- | --- |
| Copilot token | `identity/manager.go` | TTL from the exchange payload (`expires_at` + safety margin), `singleflight`-collapsed, minted **on demand in the request path** plus startup, **no fallback** (hard secret), tracks the mint outcome as readiness |
| Impersonation version facts (×2) | `impersonation/version_fact.go` | embedded **fallback** + fetched value, startup `Prime` (≤5s) + periodic `Run` (24h), **holds last-good** on failure, best-effort, non-secret observability snapshot |
| Codex `models.json` | `catalog/codex_snapshot.go` | today a static `go:embed` decoded once at init — #53's target |

The impersonation fact and the Codex snapshot are the same shape: *a vendored
value that rots, refreshed from upstream with an embedded fallback, held in
memory*. The Copilot token is not (see §Scope). This design unifies the two that
match and leaves the token alone.

### The memory win the Codex consumer gets for free

`models.json` is ~291 KB but only **8 models** — the bulk is each model's large
`base_instructions` prompt. The `catalog` package holds it **twice** today: the
`go:embed` bytes (in the binary's read-only data segment — demand-paged, shared,
effectively not heap) *and* the parsed `map[slug]map[field]RawMessage`, ~300 KB on
the heap held for the **entire process lifetime**, because the `RawMessage` values
copy the field bytes.

The freshened design does **not** retain a parsed form. The cache holds a cheap
version identity plus the raw bytes, and only when those bytes actually differ
from the embedded floor; the read path parses on demand. Steady-state heap for the
Codex catalog drops from ~300 KB held-forever to a few tens of bytes (the version
label), rising to ~291 KB only while a fetched release is genuinely ahead of the
floor. See §"The Codex `models.json` consumer".

### ADR-0008 anticipated this

ADR-0008 recorded the refresh-with-fallback helper as a concrete `versionFact`,
explicitly declining to pre-extract a generic primitive because there was one
present consumer and #53's runtime-vs-build-time question was unsettled. That
question is now settled — runtime, memory-only, latest-release-tag — so the
extraction is warranted and is exactly what #53 asked to own.

## Design

### `internal/cache`: one engine, N consumers

A new standard-library-only package. A consumer declares a **`Source[V]`** — the
static recipe for one cached value — and receives a **`Value[V]`**, the live,
concurrency-safe object that runs it. A process-wide **`Registry`** aggregates the
operations that are genuinely collective (startup fan-out, observation,
readiness); everything type-specific stays on the per-entity `Value`.

#### `Source[V]` — the recipe

```go
package cache

// Source is the static, declarative recipe for one cached value. It does nothing
// on its own; a Value runs it. V is the served type — string for a version, []byte
// for a snapshot.
type Source[V any] struct {
	// Fallback is the embedded floor, served until (and unless) a fetch succeeds.
	Fallback V
	// FallbackVersion identifies the floor, e.g. "rust-v0.144.5".
	FallbackVersion string
	// TTL is the refresh cadence. Static. TTL <= 0 disables refresh (air-gapped).
	TTL time.Duration

	// Version cheaply fetches the latest version identity WITHOUT the full content,
	// so an unchanged version short-circuits the download. Optional: nil means
	// "no cheap peek — always Fetch and compare by Hash".
	Version func(context.Context) (string, error)
	// Fetch retrieves the latest content and the version it corresponds to.
	Fetch func(context.Context) (value V, version string, err error)
	// Hash derives a content identity for a value, so a version bump whose content
	// is unchanged skips the validate/swap. Optional: nil means "compare by value".
	Hash func(V) string
	// Validate rejects a fetched value that does not meet the consumer's contract
	// (the required-field-drift gate). A rejected value never enters the cache.
	// Optional: nil means "accept any successful fetch".
	Validate func(V) error

	// RequiredForReadiness gates /readyz: when true, the Registry reports not-ready
	// until this value's first successful fetch. Both current consumers set false.
	RequiredForReadiness bool

	// Name is the stable key this value reports under in /readyz.
	Name string
}
```

#### `Value[V]` — the per-entity engine

`Value[V]` is the direct generalization of today's `versionFact`. It holds the
current value, its version, its hash, the freshness timestamps, and the mutexes,
and it runs the **refresh ladder**. The type-specific read, `Current`, lives here
because only the typed `Value` knows its `V`.

```go
func New[V any](src Source[V], opts ...Option) *Value[V]

func (v *Value[V]) Current() (V, Status) // effective value (fetched, else fallback) + snapshot
func (v *Value[V]) Run(ctx context.Context) // its own refresh loop on its OWN TTL
```

`Value[V]` also satisfies an unexported `entry` interface — `prime`, `run`,
`observe`, `satisfied` — through which the `Registry` drives it without knowing
`V`. Consumers keep the typed `*Value[V]` handle for `Current`; the `Registry`
holds the erased `entry`.

**The refresh ladder** (one `attempt`, run by `Prime` and by each `Run` tick) is
the two-level short-circuit that the version/hash split buys:

```
attempt(ctx):
  v := Version(ctx)                    // (1) cheap peek — identity only, no content
  if v == current.version:  touch(); return          // ← unchanged ref: no download at all

  value, v := Fetch(ctx)               // (3) only now the full read
  if Hash(value) == current.hash:                    // (5) content identical across a version bump…
      setVersion(v); touch(); return                 //     …re-key the label, skip validate/swap
  if err := Validate(value); err != nil:             //     accept-gate: never poison the cache
      holdLastGood(); recordErr(err); return
  swap(value, v, Hash(value))          // new good value in
```

Semantics carried over verbatim from `versionFact` (ADR-0008):

- **Cold.** Until the first success, `Current()` returns the fallback with
  `source == fallback`.
- **Warm failure.** After a prior success, a failed attempt keeps the
  **last-good** value; it records `lastAttempt`/`lastErr` and lets `lastSuccess`
  age. A transient blip never downgrades a known-good value.
- **`Run` does not fire at t=0.** The startup fetch is `Prime`'s job (below), so
  the ticker waits one TTL before its first tick — no double-fetch at boot.

`Run` owns its own `time.Ticker(src.TTL)`; there is **no shared ticker**. An
injected clock and an injectable ticker seam (as `versionFact` has today) keep the
loop and the aging logic deterministically testable.

```go
func (v *Value[V]) Run(ctx context.Context) {
	if v.src.TTL <= 0 { return }          // disabled (air-gapped)
	ticker := v.newTicker(v.src.TTL)      // its OWN TTL
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():  return
		case <-ticker.C():  v.attempt(ctx)
		}
	}
}
```

#### `Registry` — the aggregate

The `Registry` holds every registered `entry` and owns exactly the three
operations that are collective, plus a launcher for the per-entity loops:

```go
type Registry struct { /* []entry, guarded */ }

func (r *Registry) Register(e entry)          // impersonation and catalog register their Values

func (r *Registry) Prime(ctx context.Context) // fan-out the bounded startup fetch across all entries
func (r *Registry) Start(ctx context.Context) // launch each entry's own Run(ctx) goroutine (per-TTL)
func (r *Registry) Observe() []Status         // collect the snapshot of every entry that publishes one
func (r *Registry) Ready() bool               // AND over every entry's Satisfied()
```

- `Prime` runs every entry's startup attempt **concurrently**, bounded by one
  overall deadline (5s), and returns when all settle or the deadline fires — the
  generalization of `Set.Prime`. A miss leaves that entry on its fallback.
- `Start` is a **thin launcher**, not a shared loop: it ranges the entries and
  starts each one's `Run(ctx)`. TTLs stay per-entity. It exists only so `main`
  has one call site instead of N.
- `Observe` collects each entry's `Status`; an entry may decline to publish (the
  bool below), so a value with nothing useful to show adds no `/readyz` noise.
- `Ready` folds each entry's `Satisfied()` — `true` when the entry is not
  required, or is required and has succeeded at least once.

`Status` is the type-erased, **non-secret** freshness record:

```go
type Status struct {
	Name        string     // "vscode", "copilot_chat", "codex_models"
	Source      string     // "fetched" (a fetch succeeded, or a prior one did) | "fallback"
	Version     string     // effective version label, e.g. "rust-v0.145.0" or "1.129.1"
	LastSuccess *time.Time // nil while the fallback is in use
}

// entry is the unexported, type-erased view the Registry drives.
type entry interface {
	prime(context.Context) error
	run(context.Context)
	observe() (Status, bool) // bool == "publish me in /readyz"
	satisfied() bool
}
```

This is precisely the split settled during design: `Current`/`Run` are inherently
per-entity (one is typed, one is a per-TTL loop); `Prime`/`Observe`/`Ready` are
aggregates the `Registry` folds over the per-entity `prime`/`observe`/`satisfied`.

### Consumer: `internal/impersonation` (behavior-preserving port)

`versionFact` is deleted. `Set` keeps its two facts as `*cache.Value[string]` and
its domain composition (`Header()` assembling two versions into five headers) —
that stays in `impersonation`, since it is not generic. `Set.Prime`/`Set.Run`/
`Set.Observe` are removed; the `Registry` drives lifecycle and observation.

```go
func New(cfg Config, edge Edge, reg *cache.Registry, logger *slog.Logger) *Set {
	s := &Set{
		vscode: cache.New(cache.Source[string]{
			Name: "vscode", FallbackVersion: cfg.VSCodeVersionFallback,
			Fallback: cfg.VSCodeVersionFallback, TTL: cfg.RefreshInterval,
			Fetch: func(ctx context.Context) (string, string, error) {
				v, err := edge.discoverVSCode(ctx); return v, v, err
			},
			Validate: validateBareVersion, // required-field-drift analogue for a version
			// Version, Hash nil: the fetch is tiny, so peek==fetch and value==identity.
			// RequiredForReadiness false: discovery never gates readiness (ADR-0008).
		}, cache.WithLogger(logger)),
		plugin: /* … copilot_chat, same shape … */,
		integrationID: cfg.CopilotIntegrationID, apiVersion: cfg.GithubAPIVersion,
	}
	reg.Register(s.vscode); reg.Register(s.plugin)
	return s
}

func (s *Set) Header() http.Header { // unchanged: reads Current() of each fact, derives 5 headers
	v, _ := s.vscode.Current(); c, _ := s.plugin.Current()
	return s.header(v, c)
}
```

For impersonation, `Version` and `Hash` are `nil` (the fetch is a tiny payload, so
there is no cheap peek to add and the value *is* its own identity), and
`RequiredForReadiness` is `false`. The degenerate cells fall out of the same
engine at no cost — the codex-specific peek does not burden impersonation. Behavior
is identical to ADR-0008; `source` renders as `"fetched"` rather than
`"discovered"` (see §Observability).

### The Codex `models.json` consumer (#53)

A single `*cache.Value[[]byte]`, wired in `internal/catalog`:

| `Source[[]byte]` field | Codex value |
| --- | --- |
| `Fallback` | the `go:embed` snapshot bytes (the guaranteed-parseable floor) |
| `FallbackVersion` | the vendored tag, `rust-v0.144.5` |
| `TTL` | `--codex-catalog-refresh-interval` (default 24h; `0` disables) |
| `Version` | `GET /repos/openai/codex/releases/latest` → the release **tag** (cheap; no blob) |
| `Fetch` | GET `models.json` at that tag → `([]byte, tag, err)` |
| `Hash` | content hash of the bytes |
| `Validate` | `decodeCodexModels` parses cleanly — **the required-field-drift gate** |
| `RequiredForReadiness` | `false` (the floor always serves) |
| `Name` | `codex_models` |

**Latest release tag as the version identity.** The tracked ref is
`openai/codex`'s newest release tag (the `rust-vX.Y.Z` lineage the vendored
snapshot is already pinned to). A tag is immutable and pins a commit, which
satisfies #53's "commit-based" intent while giving the peek a cheap, human-readable
compare. The ladder for Codex:

1. **Peek** `releases/latest` → tag. Tag unchanged → touch, done (no 291 KB
   download). Most ticks stop here.
2. **Fetch** `models.json` at the tag. Compute its content hash.
3. **Hash unchanged** (a release that didn't touch `models.json`) → re-key the
   version label to the new tag, skip validate/swap.
4. **Validate** with `decodeCodexModels`; a blob that does not parse into the
   expected `ModelInfo` shape is **rejected** — we hold last-good (or the floor)
   and log. This is the drift protection #53 calls out: a newer `models.json` that
   our renderer can't honor never displaces a good value.
5. **Swap** the bytes + tag + hash in.

**Parse on the read path; validate on accept.** The package-level
`var codexModels = mustDecodeCodexModels(...)` is removed. The catalog render path
calls `decodeCodexModels(value)` on `Current()` at request time. The Codex catalog
`/models` path is cold — clients fetch the list rarely, and Codex caches it 300s
client-side — so re-parsing 8 models per read is negligible. The **only** retained
copy is the raw bytes, and only while a fetched release is ahead of the floor; when
`Current()` returns the embed (the common case), no extra bytes are held. Parsing
still happens once at **accept** time (step 4) to validate, but that parse is
discarded, not retained.

The embedded snapshot, its `LICENSE`/`NOTICE`/`PROVENANCE`, and the `decodeCodexModels`
validator are unchanged; the snapshot's role shifts from "the served value" to
"the floor and the accept-time contract."

### Startup and refresh lifecycle

`main` builds one `Registry`, wires both consumers into it, and drives it around
the existing startup mint — the generalization of today's impersonation block:

```go
reg := cache.NewRegistry()
imp := impersonation.New(impCfg, edge, reg, logger) // registers its 2 facts
catalog.NewModelsCache(codexCfg, reg, logger)       // registers codex_models

go func() {
	reg.Prime(serveCtx)        // all entries' startup fetch, concurrent, ≤5s overall
	reg.Start(serveCtx)        // launch each entry's own per-TTL Run loop
	mgr.StartupMint(serveCtx)  // first mint carries fresh impersonation headers
}()
```

`Prime` is a **wait, not a gate**: a slow or failed startup fetch leaves that entry
on its fallback and startup proceeds. Neither a fetch outcome nor the mint gates
`/readyz` for the current consumers (both `RequiredForReadiness == false`). The
listener is bound before this runs, so `/healthz` and a locally-ready `/readyz`
serve throughout. On `serveCtx` cancellation at shutdown, `Prime` returns early and
every `Run` exits cleanly.

**Disabling refresh.** `TTL <= 0` makes `Value.Run` a no-op and `Prime` skip that
entry's outbound fetch, pinning it to the fallback for the process lifetime — the
air-gapped / locked-egress mode, per consumer. `--impersonation-refresh-interval=0`
and `--codex-catalog-refresh-interval=0` are independent.

### Configuration

One new flag, following the `--codex-catalog-*` family and the
`--impersonation-refresh-interval` precedent:

| Flag / env | Default | Role |
| --- | --- | --- |
| `--codex-catalog-refresh-interval` / `COPILOTD_CODEX_CATALOG_REFRESH_INTERVAL` | `24h` | Codex `models.json` refresh cadence; must be `>= 0`. `0` disables the outbound refresh (pins to the embedded snapshot). |

`ServeConfig` gains `CodexCatalogRefreshInterval`; the startup config log lists it
beside the existing `codex-catalog-*` fields. No change to
`--codex-catalog-enabled`, `--codex-auto-review-model`, or
`--codex-catalog-override-limits`; the freshness cache sits underneath them and is
inert whenever the Codex catalog is not served.

### Observability (`/readyz`)

`/readyz` stays unauthenticated and keeps its coarse `status` bit. The per-fact
freshness that ADR-0008 nested under `impersonation.discovery` moves into a uniform
`caches` view fed by `Registry.Observe()`, so every cached value — impersonation
and Codex alike — reports the same non-secret shape. Impersonation's
domain-specific `effective_headers` composition stays under an `impersonation`
block, rendered by `server` from `Set.Header()` (it is not a generic freshness
fact, so it does not belong in the `caches` view).

```json
{
  "status": "ready",
  "caches": {
    "vscode":       { "source": "fetched",  "version": "1.129.1",        "last_success": "2026-07-21T12:00:00Z" },
    "copilot_chat": { "source": "fallback", "version": "0.26.7",         "last_success": null },
    "codex_models": { "source": "fetched",  "version": "rust-v0.145.0",  "last_success": "2026-07-21T11:00:00Z" }
  },
  "impersonation": {
    "effective_headers": {
      "Editor-Version": "vscode/1.129.1",
      "Editor-Plugin-Version": "copilot-chat/0.26.7",
      "User-Agent": "GitHubCopilotChat/0.26.7",
      "Copilot-Integration-Id": "vscode-chat",
      "X-GitHub-Api-Version": "2025-04-01"
    }
  }
}
```

Only non-secret state appears: each cache's `source`, effective `version`, and
`last_success`, plus the already-non-secret effective headers. No token, no raw
fetch-error text — a failure is conveyed by `source: "fallback"` (never fetched) or
an aging `last_success` (fetched before), never by an error string that could leak
an upstream URL to an unauthenticated caller. `HEAD` still writes no body. When a
consumer's refresh is disabled (`TTL == 0`), its entry still renders, at
`source: "fallback"` with null `last_success`.

This is a deliberate, backward-compatible-on-`status` evolution of ADR-0008's
`/readyz` shape (the per-fact freshness relocates; `effective_headers` is retained;
`source` reads `"fetched"`, not `"discovered"`). It is a task in the extraction
sub-issue, not a silent side effect.

### Error handling and resilience

- Each fetch is individually bounded; `Prime` caps the combined startup wait at 5s.
- Failures never touch readiness (for the current `false` consumers) and never
  overwrite a good value — cold failures hold the fallback, warm failures hold the
  last-good.
- A malformed / unparseable fetch (impersonation shape-check, Codex
  `decodeCodexModels`) is a failure, not a poison-write — the accept-gate rejects
  it before the swap.
- Every `Run` stops on context cancellation at shutdown.
- Logging: startup outcome at info; each refresh success at debug; refresh failure
  at warn (naturally rate-limited by the TTL cadence).

## Scope

**The Copilot token Manager stays a separate component.** The `Source[V]` lens
proves the mismatch: the token has **no static TTL** (its lifetime comes from the
exchange payload's `expires_at`), **no fallback** (it is a hard secret — an expired
token cannot be "held last-good"), and **no version/hash**. Four of the six
properties do not apply. It also mints **on demand in the request hot path** under
`singleflight`, and its success *is* the readiness signal rather than a
`RequiredForReadiness` flag. Folding it in would either bloat the generic engine or
strip the token's carefully tuned behavior. It remains in `internal/identity`,
unchanged.

**The provider-shaped Catalog is not a cache** and is untouched: it fetches
Copilot's `/models` fresh on every request (`catalog/handler.go`), holding nothing.

## Testing

Test-first, matching the package layout:

- **`cache.Value`** — injected `Fetch`/`Version`/`Hash`/`Validate` and a fake
  clock and ticker: cold serves fallback; success swaps; warm failure holds
  last-good and ages `lastSuccess`; the ladder skips the download when `Version`
  is unchanged, skips validate/swap when `Hash` is unchanged, and rejects (holds
  last-good) when `Validate` fails; `Current()` is race-clean under `-race`;
  `TTL <= 0` makes `Run` a no-op.
- **`cache.Registry`** — `Prime` fans out concurrently and honors the 5s bound;
  `Start` launches one loop per entry on its own TTL; `Observe` collects only
  publishing entries; `Ready` is the AND of `Satisfied()`, exercising a
  `RequiredForReadiness=true` entry that is unsatisfied until its first success
  (dead in production, covered here so the gating path is real).
- **impersonation port** — assembler correctness across all four fallback/fetched
  states → exact fallback strings; the two facts register and drive through the
  `Registry`; `Header()` reflects a mid-flight swap on the next call; existing
  discovery-function and `httptest` tests are unchanged.
- **Codex `models.json` consumer** — `httptest` GitHub edge: peek returns an
  unchanged tag → no blob fetched; a new tag with identical content → re-key, no
  re-validate; a new tag with a good blob → swap; a new tag with a blob that fails
  `decodeCodexModels` → rejected, floor/last-good retained; the render path parses
  `Current()` and never a package-level global; disabled TTL never calls the edge.
- **`identity.Manager`** — unchanged; still reads impersonation through the
  `Impersonation` interface.
- **`server` `/readyz`** — the `caches` block carries every registered entry; a
  fallback entry renders `source: "fallback"` / null `last_success`; the
  `impersonation.effective_headers` block is retained; `HEAD` writes nothing.
- **e2e `serve`** — inject stub GitHub / Microsoft base URLs (the existing
  injected-base-URL pattern) and assert the Codex catalog serves fetched entries
  when ahead of the floor and that `/readyz` reports every cache.

## Considered alternatives

- **Build-time CI re-vendoring instead of a runtime cache**: rejected for this
  epic — it keeps a static embed with zero runtime dependency, but then there is
  no in-memory cache to unify and #53 becomes a CI-automation issue. The chosen
  runtime/memory-only path both freshens Codex *and* is the thing worth unifying.
  (CI bumping the embedded **floor** periodically remains compatible and orthogonal.)
- **Track the default branch head (or arbitrary latest commit)**: rejected — it
  pulls unreleased, in-progress prompt edits the moment they land, on a path where
  our fetched entries become a model's live values (no field-level merge upstream).
  The latest **release tag** advances only at reviewed release boundaries.
- **Retain the parsed `map` in the cache** (parse-on-accept, keep the result):
  rejected — it reinstates the ~300 KB held-forever the redesign removes, for a
  cold read path that re-parses 8 models trivially. The cache holds bytes +
  provenance; the read path owns parsing.
- **Fold the Copilot token into the component**: rejected — see §Scope; four of
  the six `Source` properties do not apply, and unifying would degrade the token's
  hot-path minting.
- **Persist the cache to a file**: rejected — durable state at rest violates
  ROADMAP §2 and the token/impersonation in-memory model. The component is
  memory-only.
- **`Run` on the `Registry` as a single shared loop**: rejected — TTLs are
  per-entity, so the loop is per-entity. The `Registry` only offers `Start`, a
  launcher that fans out to each entry's own ticker.

## Consequences

- One tested freshness/concurrency/fallback/observability/readiness engine backs
  two (soon: any number of) cached values. `versionFact` is deleted; impersonation
  behavior is unchanged.
- The Codex catalog stops going stale silently: it tracks `openai/codex`'s latest
  release tag and refreshes on a TTL, with the embedded snapshot as a
  never-worse-than-today floor and drift-safe accept-gating.
- Steady-state heap for the Codex catalog drops from ~300 KB held-forever to a few
  tens of bytes (or ~291 KB only while ahead of the floor); parsing moves to the
  cold read path.
- **One new outbound dependency** on a Copilot-only-otherwise path: unauthenticated
  `api.github.com` release/`raw` reads for `openai/codex`, hit at startup and every
  24h. It carries no credentials and is not the Copilot exchange or inference
  endpoint, so it adds none of the idle-exchange abuse signal ADR-0001 avoided.
  `--codex-catalog-refresh-interval=0` opts out entirely.
- This introduces a **runtime cache of external content**, which is a considered
  exception to ROADMAP §2's "no runtime cache" phrasing — reconciled by the cache
  being **memory-only** (nothing at rest) with an embedded floor, exactly as the
  impersonation cache already is. Recorded as **ADR-0009** and noted in the ROADMAP.
- `/readyz` gains a uniform `caches` block and relocates impersonation's per-fact
  freshness into it; `status` is unchanged and backward-compatible.
- The readiness-gating path exists (`RequiredForReadiness`) though no current
  consumer uses it — built now per the maintainer's direction, covered by tests.

## Epic decomposition

This design anchors an `epic`. It is delivered as two sub-issues; the extraction
lands first as the correct, behavior-preserving baseline, and the Codex freshness
builds on it.

1. **Extract `internal/cache`; port impersonation.** Introduce `Source[V]`,
   `Value[V]`, `Registry`, `Status`, the refresh ladder, and the readiness gating;
   delete `versionFact`; port the two impersonation facts with no behavior change;
   relocate `/readyz` freshness into the `caches` block; add the `CONTEXT.md`
   glossary terms. *Blocks (2). Can start immediately.*
2. **Codex `models.json` freshness as a `cache` consumer.** Add the
   `*cache.Value[[]byte]`, latest-release-tag tracking, version/hash ladder,
   `decodeCodexModels` accept-gate, parse-on-read (remove the package-level parsed
   global), `--codex-catalog-refresh-interval`, and the `codex_models` `/readyz`
   entry; author **ADR-0009** and update `CONFIGURATION.md` and the ROADMAP.
   *Blocked by (1).*

## Glossary additions (`CONTEXT.md`)

New canonical terms, added in sub-issue (1):

- **Cached value** — an in-memory value served from an embedded **fallback** and
  refreshed best-effort from upstream on a static TTL, holding last-good on
  failure; never persisted. The impersonation version facts and the Codex
  `models.json` snapshot are cached values. *Avoid*: cache (unqualified), which is
  also used loosely for the Copilot token.
- **Refresh ladder** — the version → hash → validate short-circuit a cached value
  runs on each attempt: an unchanged **version** skips the download; an unchanged
  content **hash** skips validate/swap; a failed **validate** (the accept-gate)
  rejects the fetch and holds last-good.
- **Cache registry** — the process-wide aggregate that primes, launches, observes,
  and reports readiness across all cached values.

The existing reserved terms are unaffected: **refresh** stays reserved for the
token-mint sense — the component deliberately avoids the word in its API, which
*attempts* and *fetches* — and **discovery** stays impersonation's edge-specific
term for its Microsoft-endpoint fetch.

## ADR

The runtime-cache decision (memory-only, embedded floor, latest-release-tag,
outbound `openai/codex` dependency, disable-able) is recorded as **ADR-0009**,
authored in sub-issue (2). ADR-0008 remains the record for the impersonation
discovery decision; this design fulfils its deferred extraction.
