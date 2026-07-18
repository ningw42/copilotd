# Phase 0 — Foundations / walking skeleton — Design

Status: approved design (refined via grilling session), pending implementation plan
Date: 2026-07-13
Roadmap reference: `ROADMAP.md` §7 "Phase 0 — Foundations / walking skeleton"

## 1. Goal & outcome

Stand up the walking skeleton for `copilotd`: the binary compiles, runs natively
on the host, serves a health endpoint, emits structured logs with a per-request
request-id, and shuts down gracefully. No upstream calls, no auth, no provider
routes yet.

**Outcome:** the binary runs and is observable (via structured logging).

This phase establishes the *seams* that later phases hang off — the middleware
chain, the config package, the observability entry points — without building any
of their machinery early.

## 2. Scope

**In scope (Phase 0):**

- Go module layout and package boundaries.
- Configuration loading (flags + env + optional TOML file) via `peterbourgon/ff/v4`.
- Structured logging via `log/slog`, with a per-request request-id propagated
  through `context`.
- HTTP server + stdlib router with a single route: `GET /healthz`.
- Request-id, access-log, and panic-recovery middleware.
- Graceful shutdown on `SIGINT`/`SIGTERM`, with a second-signal hard-kill.
- Build version metadata injected at link time.
- Nix `packages.default` (build) + `apps.default` (`nix run`), the package wired
  into `checks`.
- TDD unit + smoke tests across all units.

**Out of scope (deferred — see §10 for phase mapping):** metrics/Prometheus/
OpenTelemetry/traces, provider routes (`/anthropic/*`, `/openai/*`), `/models`,
inbound auth, GitHub↔Copilot identity, the raw forwarder, the SSE streaming
engine, the middleware/onion framework, per-request body bounding, cross-
compilation, service install, and CI.

### 2.1 Conscious deviation from the roadmap

The roadmap places **metrics scaffolding** in Phase 0 ("observability from day
one"). This design **defers metrics** (Prometheus/OTel/traces) to a later phase
and ships **structured logging only**. This is a deliberate decision to keep the
skeleton minimal, not an oversight.

The forward-compatible hook is preserved: the access-log middleware labels each
request by **route template** (the matched `http.ServeMux` pattern), which is the
same low-cardinality key metrics will reuse when they land.

## 3. Guiding decisions & rationale

| Decision | Choice | Rationale |
| --- | --- | --- |
| Dependency stance | Stdlib-first, pragmatic | Stdlib is the default; adopt a third-party library only when it clearly wins on ecosystem, usability, or API design (not merely performance). |
| Router | stdlib `net/http` `ServeMux` | Go 1.22+ method+pattern routing is sufficient for the route set; no third-party router earns its weight for a health-only skeleton. |
| Logging | `log/slog`, text handler, set as global default | Zero-dep, context-aware, the ecosystem lingua franca. For a network-bound proxy, log-encoding speed is dominated by upstream latency, so zap's perf edge does not clearly win. Escape hatch: `go.uber.org/zap/exp/zapslog` can back a `slog.Handler` later with **zero call-site changes**. |
| Config | `peterbourgon/ff/v4` + TOML file | "stdlib `flag`, plus env, plus optional file" with clean precedence. TOML gives human-friendly, sectioned config that grows without a rewrite. Config is read in one place, so a later migration to `koanf` (if it outgrows ff) is localized. `viper` rejected: heavy dep tree, global singleton, precedence surprises. |
| Request-id | UUIDv4 via `github.com/google/uuid` | Standard, recognizable, tooling-friendly; hyphens fall inside the honored-inbound charset, so inbound UUIDs validate cleanly. |
| Metrics | Deferred | Chose slog-only for Phase 0 (see §2.1). |
| Build | Nix `packages.default` + `apps.default` (`buildGoModule`) | Reproducible binary matching the existing flake-centric setup; `nix run` ergonomics; package wired into `checks` for local verification. Cross-compilation and CI deferred. |

## 4. Module layout & package boundaries

Module path: `github.com/ningw42/copilotd`. Go **1.26**, pinned explicitly (see
§8 and §11) — not floated against whatever nixpkgs-unstable happens to provide.

```
copilotd/
├── go.mod                      # module github.com/ningw42/copilotd
├── go.sum
├── cmd/copilotd/main.go        # thin entrypoint — wiring only
└── internal/
    ├── config/                 # Config struct + Load() via ff/v4 (+ LogValue)
    ├── logging/                # slog construction + request-id + ctx helpers
    ├── server/                 # router, health, http middleware, lifecycle
    └── build/                  # version metadata (ldflags-injected)
```

Each unit — *what it does · how it is used · what it depends on*:

- **`cmd/copilotd`** — the composition root. Parses config, builds the logger and
  sets it as the `slog` default, logs effective config, **binds the listener from
  `cfg.Addr`**, constructs the server, runs it under a signal-aware context, and
  handles `--version`. No business logic. Depends on all four internal packages.

- **`internal/config`** — `type Config` + `Load(args []string, lookupEnv func(string) (string, bool)) (Config, error)`.
  Loads via `ff/v4` (flags > env > TOML file > default), then validates. Args and
  env are **injected**, so `Load` is pure and table-testable. `Config` implements
  `slog.LogValuer`, emitting only non-secret fields (redaction by construction).
  Depends on `ff/v4` + a TOML parser only.

- **`internal/logging`** — `New(cfg) (*slog.Logger, io.Closer, error)`; context
  helpers `WithRequestID(ctx, id)` / `RequestIDFrom(ctx)`; the `contextHandler`
  that injects `request_id`; inbound request-id validation. **Deliberately imports
  no `net/http`.** Depends on `log/slog` + `github.com/google/uuid`.

- **`internal/server`** — builds the `http.ServeMux`, the `/healthz` handler, the
  middleware chain (`func(http.Handler) http.Handler` wrappers), and the
  `http.Server` with timeouts and `Run(ctx, ln net.Listener)` graceful shutdown.
  Takes an **injected listener** (bound by `main`) so tests can drive a `:0`
  listener. Depends on `net/http`, `logging`, `config`, `build`.

- **`internal/build`** — `var Version, Commit, Date string` (set via
  `-ldflags -X`) + `String()`. No deps.

**Key boundary:** `logging` knows nothing about HTTP; `server` knows nothing
about how logs are formatted. The middleware in `server` uses `logging`'s context
helpers as its only coupling point.

## 5. Configuration

`ff/v4`-backed. Env prefix `COPILOTD_`. Precedence: **flags > env > TOML file >
default**. The optional config file is TOML, parsed via `ff/v4`'s config-file
mechanism (first-party parser if available, otherwise a small
`ConfigFileParser` adapter over `pelletier/go-toml/v2` — an implementation
detail to confirm, not a design fork).

| Flag | Env | TOML key | Default | Purpose |
| --- | --- | --- | --- | --- |
| `--addr` | `COPILOTD_ADDR` | `addr` | `127.0.0.1:8080` | Bind address |
| `--log-level` | `COPILOTD_LOG_LEVEL` | `log-level` | `info` | `debug`\|`info`\|`warn`\|`error` |
| `--log-format` | `COPILOTD_LOG_FORMAT` | `log-format` | `text` | `text`\|`json` |
| `--log-file` | `COPILOTD_LOG_FILE` | `log-file` | *(empty)* | Path; empty = stderr |
| `--shutdown-timeout` | `COPILOTD_SHUTDOWN_TIMEOUT` | `shutdown-timeout` | `10s` | Graceful-shutdown grace period |
| `--config` | `COPILOTD_CONFIG` | — | *(empty)* | Optional TOML config file |
| `--version` | — | — | — | Print build info, exit 0 |

Deliberate choices:

- **Default bind `127.0.0.1`, not `0.0.0.0`.** This is a credential-handling
  proxy; a loopback default keeps it off the network until the operator opts in.
- **`--log-format` retained despite defaulting to text.** The `json` toggle is
  near-free now and useful for aggregators later, so the seam is included rather
  than retrofitted.
- **`Config` implements `slog.LogValuer`.** The startup config line (§6) and any
  accidental `Config` log go through it, emitting only non-secret fields. When
  Phase 1 adds token fields they are redacted by construction, not by discipline.

`Load()` validates: `addr` is a valid `host:port`; level/format are in-set;
shutdown-timeout > 0. Invalid config produces a clear error and a non-zero exit
**before** binding the listener. `--version` short-circuits in `main` before
`Load()`.

## 6. Logging & request-id

**Logger construction.** `logging.New(cfg) (*slog.Logger, io.Closer, error)`
builds one shared `*slog.Logger`: level mapped from `--log-level`; `TextHandler`
or `JSONHandler` per `--log-format`; `AddSource` enabled **only when the level is
`debug`** (source locations are debugging signal, noise/overhead at `info`);
writing to stderr or the opened `--log-file` (the returned `io.Closer` lets
`main` close the file on shutdown). Base attributes: `service` + version. `main`
calls `slog.SetDefault(logger)` so stray global-`slog` calls, dependencies, and
the `http.Server.ErrorLog` bridge share one handler/format/destination.

**Request-id via context, not threaded loggers.** A `contextHandler` wraps the
chosen text/json handler; in `Handle` it pulls `request_id` from the record's
context and adds it as an attribute. Any code that calls
`logger.InfoContext(ctx, …)` emits `request_id` with no plumbing. Context helpers
(`WithRequestID`, `RequestIDFrom`) live in `logging`, keeping it free of
`net/http`.

> **Implementation constraint (known `slog` footgun).** `contextHandler` must
> implement all four `slog.Handler` methods and **re-wrap** on `WithAttrs`/
> `WithGroup` (returning a `contextHandler` around the inner handler's result),
> and delegate `Enabled`/`Handle`. A naive wrapper that returns the inner handler
> or an unwrapped `self` silently drops attributes and groups.

**Startup config log.** After building the logger, `main` logs the effective
(resolved) configuration once at `info` via the `Config` `slog.LogValuer`, so the
resolved precedence is visible for ops.

**Middleware** (in `server`, composed request-id → access-log → recover; see §7
for why this order):

- **RequestID** — honor an inbound `X-Request-Id` **only if safe** (≤ 128 chars,
  charset `[A-Za-z0-9._-]`); otherwise generate a **UUIDv4** via
  `github.com/google/uuid`. A malformed/oversized inbound value is **ignored and
  regenerated** (never a `400`; optionally logged at debug) — a bad correlation
  header must not fail the request, and a rejection path is a needless DoS lever.
  Store the final id in context; echo it in the response `X-Request-Id` header.
- **AccessLog** — one structured line per request: method, **route template**,
  status, bytes, duration, request_id. Route template comes from `r.Pattern`
  (Go 1.23+) with an `"unmatched"` fallback on 404. **Quiet routes** (`/healthz`)
  are logged at **debug** so constant health polling does not flood `info`;
  consequently `info`-level access logs are empty in Phase 0 (only `/healthz`
  exists) — expected, not a bug. Needs a status-capturing `ResponseWriter`
  wrapper.
- **Recover** — a panic becomes a generic `500` (`internal server error`, no
  stack, no JSON envelope) plus an error log with request_id. Correlation to the
  logged panic is via the `X-Request-Id` response header (set by the outermost
  middleware), so the body carries no structure. Provider-shaped error bodies are
  a Phase-1 concern and intentionally absent here.

**Secret-safety principle (established now).** No full header/body dumps in logs;
config logging goes through the `LogValuer`. There are no secrets in Phase 0, but
the redaction discipline is set here so the Phase 1 auth/identity work inherits it
rather than retrofits it.

## 7. HTTP server, router, health & lifecycle

**Router.** stdlib `http.ServeMux`. Phase 0 registers one route: `GET /healthz`
(which also answers `HEAD`). The middleware chain wraps the mux:
`RequestID(AccessLog(Recover(mux)))`. The order is intentional: **RequestID is
outermost** so its context mutation is visible to the inner two (both the
access-log line and a recovered panic carry the request_id); **Recover is
innermost** so it catches panics from the route handler and the resulting `500`
is what AccessLog records.

**Health handler.** Liveness only. `200` with body `{"status":"ok"}`,
`Content-Type: application/json`. The build version is **not** exposed on this
unauthenticated endpoint (needless fingerprinting surface on a credential-handling
daemon); version stays available operator-side via `--version` and the startup
log. Readiness can split out in Phase 1 when there are upstream dependencies to
check.

**`http.Server` timeouts.** These are **inbound** (client ↔ copilotd) timeouts,
distinct from the outbound (copilotd ↔ Copilot) client timeouts configured in
Phase 1.

| Setting | Value | Reason |
| --- | --- | --- |
| `ReadHeaderTimeout` | `5s` | Slowloris guard; headers are tiny and inference-independent. |
| `IdleTimeout` | `60s` | Reap idle keep-alive connections; never applies mid-request. |
| `ReadTimeout` | `0` (unset) | A blunt global cap on request *upload* time fights large LLM payloads (long histories, base64 images) on slow links. Real bounding — `http.MaxBytesReader` (size) + per-request context deadline — is introduced in Phase 1 when there are bodies. Phase 0 (`/healthz`, no body) needs none. |
| `WriteTimeout` | `0` (unset) | Response duration is where LLM slowness lives (minutes-long SSE streams). A global write deadline would silently kill them; Phase 2 bounds streams via context + client-disconnect propagation. |

Non-zero values are hardcoded named constants (YAGNI — promoting a constant to a
flag later is localized). `Server.ErrorLog` is bridged into slog (server-internal
errors become structured `warn` lines).

**Lifecycle — `server.Run(ctx, ln net.Listener)`.**

1. `main` binds `ln` from `cfg.Addr` (bind failure → logged, exit 1, distinct
   from a serve error) and builds the context via
   `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`.
2. `Serve(ln)` runs in a goroutine; startup logs `listening on <ln.Addr()>`.
3. On `ctx.Done()` (first signal): call the `NotifyContext` stop function to
   **restore default signal handling** (so a *second* signal hard-kills via the
   OS default), then `srv.Shutdown(shutdownCtx)` bounded by `--shutdown-timeout`;
   if it overruns, fall back to `srv.Close()`.
4. `http.ErrServerClosed` is treated as clean; any other error propagates.

**`main` flow & exit codes.** `--version` prints and exits 0 → `config.Load`
(error → stderr + exit 1) → build logger + `slog.SetDefault` → log effective
config → bind listener (error → exit 1) → `server.Run` → close log file → exit 0
on clean shutdown, 1 on server error.

## 8. Build & version

**`internal/build`** holds `Version`, `Commit`, `Date` string vars (defaulting to
`"dev"`/`"unknown"` for a bare `go build`/`go run`) plus `String()`. Set at link
time via `-ldflags -X github.com/ningw42/copilotd/internal/build.<Var>=…`.
Surfaced in the startup log and `--version` (not `/healthz`).

**Nix outputs** — added to `flake.nix` alongside the existing `checks`/`devShells`:

- **`packages.default`** via `buildGoModule`:
  - `CGO_ENABLED = 0`; `ldflags = "-s -w"` to strip.
  - **Static-binary caveat:** truly static only on **Linux**. On **Darwin**, Go
    always dynamically links `libSystem` (Apple supports no fully-static
    binaries), so the aarch64-darwin artifact is self-contained *except* for
    `libSystem`. The "single static binary" claim is Linux-precise.
  - Version metadata sourced **deterministically from the flake**: `Commit` from
    `self.shortRev or "dirty"`, `Date` from `self.lastModifiedDate` — no impure
    `date`/VCS calls, so the build stays reproducible.
  - **Go version pinned explicitly to 1.26** (project practice — pin a specific
    Go version, don't float). `go.mod` carries `go 1.26`; the Nix toolchain is
    `pkgs.go_1_26` in *both* the devShell `packages` and the package build
    (`buildGoModule.override { go = pkgs.go_1_26; }`), so dev and build share one
    compiler and `nix flake update` cannot silently bump the Go minor. Bump the
    version deliberately, once per Go release. Caveat: nixpkgs retires old
    versioned Go attributes over time, so a far-future nixpkgs bump may require
    updating the attribute name.
  - **Dependencies:** non-vendored. `go.mod`/`go.sum` are the source of truth (Go
    verifies each module against `go.sum`); `buildGoModule` uses a single
    `vendorHash` over the whole fetched dependency set. The first build prints the
    correct `vendorHash` to fill in; it changes only when deps change.
  - `doCheck` left at its default (**true**), so `buildGoModule` runs
    `go test ./...` in its `checkPhase` during the package build.
- **`apps.default`** wrapping the package, so `nix run` runs the binary.
- **`checks`** gains the package, so `nix flake check` compiles it *and* runs the
  test suite (via the package's `checkPhase`) alongside the existing formatting +
  pre-commit checks. This is local verification, distinct from the deferred CI.

The existing `treefmt` (gofmt) and git-hooks already format Go on commit — no
tooling change needed.

## 9. Testing strategy

TDD throughout (red → green → refactor). Every unit is testable because
dependencies are injected. Stdlib `testing` + `net/http/httptest` only (reach for
`testify` only if assertion noise becomes a real problem). Run with `-race`.

- **`config`** — table-driven `Load` tests: defaults; env override;
  flag-over-env precedence; TOML-file-under-env-under-flag; each validation error
  (bad addr, unknown level/format, non-positive timeout). Args and `lookupEnv`
  injected — no global/OS state. Plus a `LogValue()` test asserting only the
  expected (non-secret) fields are emitted.
- **`logging`** — level mapping; text vs json handler selection; `AddSource`
  on/off by level; the `contextHandler` injects `request_id` from context
  (asserted against a `bytes.Buffer` sink) **and** correctly preserves attrs/
  groups through `WithAttrs`/`WithGroup`; request-id generation is a valid UUIDv4;
  inbound `X-Request-Id` validation (honored when ≤128 & charset-clean, else
  regenerated).
- **`server`** — health handler returns 200 + `{"status":"ok"}` (no version);
  middleware chain sets and echoes `X-Request-Id`; access-log emits one line with
  the route template and logs `/healthz` at debug (`httptest`); Recover turns a
  panicking handler into a generic 500 without leaking the stack.
- **Lifecycle smoke test** — construct a `net.Listener` on `127.0.0.1:0`, pass it
  to `Run`, hit `/healthz` at `ln.Addr()`, cancel the context, assert `Run`
  returns cleanly within the grace period. This is the "the binary runs and is
  observable" outcome as an automated test.

## 10. Deferrals mapped to phases

| Deferred item | Lands in |
| --- | --- |
| Metrics / Prometheus / OTel / traces | Later phase (conscious deviation, §2.1) |
| Provider routes (`/anthropic/*`, `/openai/*`) | Phase 1 |
| `/models` (GitHub Copilot-native, then provider/client-shaped) | Phase 4 / Phase 6 |
| Inbound auth (managed token) | Phase 1 |
| GitHub↔Copilot identity, device flow, token exchange | Phase 1 |
| Raw forwarder | Phase 1 |
| Per-request body bounding (`MaxBytesReader` + context deadline), `ReadTimeout` policy | Phase 1 |
| Provider-shaped error bodies | Phase 1 |
| SSE streaming engine (reason `WriteTimeout` is 0) | Phase 2 |
| Middleware / onion framework (request/stream transform) | Phase 3 |
| Cross-compilation to the four targets, service install | Phase 6 |
| CI | Not scheduled (Nix package output + local `nix flake check` only) |
| Readiness split, nested config / koanf, testify | If/when a real need appears |

## 11. Notes & open items

- **Dependencies (Phase 0):** `github.com/peterbourgon/ff/v4` (+ a TOML parser —
  first-party if available, else a small adapter over
  `github.com/pelletier/go-toml/v2`), `github.com/google/uuid`. Everything else is
  stdlib (`log/slog`, `net/http`, `crypto/rand`).
- **Escape hatches** recorded above (zap via `zapslog`, koanf for config, `json`
  log format) exist so later needs are localized changes, not rewrites.
- **Go version:** pinned to **1.26** (§8) as a standing project practice (pin a
  specific version, don't float). This comfortably satisfies `r.Pattern`
  (≥ 1.23), so no route-template fallback is needed. At implementation, confirm
  1.26 is current and `pkgs.go_1_26` exists in the locked nixpkgs (facts); bump
  the pin deliberately on future Go releases.
- **To confirm at implementation (facts, not design forks):** the exact current
  `ff/v4` patch version and whether it ships a first-party TOML `ConfigFileParser`
  (if not, the ~15-line adapter over `pelletier/go-toml/v2`).
