# Convert copilotd's log records to ADR-0015

**Status:** proposed
**Date:** 2026-08-30

## Summary

[ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md) states
the durable rule for writing a copilotd log record: ordinary `log/slog` through
an explicit `*slog.Logger` receiver, under four structural obligations —
Component ownership with required positional injection, immutable context scope,
consequence-based level, and one plain key registry — plus access as the sole
terminal request summary. The tree does not comply. This design converts it.

The conversion touches **33 production emissions across 10 files**, **22 of them
currently non-context**, and every logger-bearing boundary in the binary. It

- adds the 47-constant key registry to `internal/logging`, with three renames
  (`route` → `inbound`, cached-value `version` → `cached_value_version`,
  cache-status `source` → `cached_value_source`) and seven new names;
- deletes every optional logger injection — four exported `Logger` struct
  fields, two `WithLogger` options, five `slog.Default()` fallback expressions,
  and `internal/catalog`'s `rendering.Logger != nil` guard — so omission stops
  compiling;
- splits the logger into one component-free base and eight
  `logging.ForComponent` children, decorated only at the composition root;
- derives request scope (`inbound`, `surface`, `ws`) once, in the typed
  registration wrapper, and hands it back to the after-handler access record
  through a narrow server-owned holder; adds an upstream-owned correlation
  holder for a late `upstream_request_id`; adds a `wsforward` result holder for
  the six WebSocket terminal facts;
- removes the `websocket session` record, slims `websocket established` to
  `status=101` plus `handshake_duration`, and deletes both `"unmatched"`
  sentinels and the hardcoded WebSocket pattern comparison;
- reassigns level by operational consequence everywhere, including
  `failure_class` across `identity.logMint`'s sites and Debug for **both** probe
  access samples;
- lands the standard-library AST gate and the focused behavior tests in the same
  sweep, with no baseline allow-list and no temporary exemption.

Thirteen observable changes follow; they are enumerated once, in
[Behaviour changes](#behaviour-changes).

Four things the closed tickets did not anticipate turned up in the live tree
and are settled here rather than left to the implementer: net/http's `ErrorLog`
bridge would silently acquire a false Component
([The one component-free consumer](#the-one-component-free-consumer)); today's
access record labels a `ServeMux` redirect it never served
([Absence is the unmatched state](#absence-is-the-unmatched-state)); two
production records can already emit duplicate top-level keys
([Two live key collisions](#two-live-key-collisions)); and making the cache use
the central registry closes an existing import chain into a cycle
([The registry import cycle](#the-registry-import-cycle)).

## Motivation

### The starting state, re-verified

Every count below was re-checked against the working tree at `bd55c14`.

| Fact | Value |
|---|---|
| Production emissions | 33 across 10 files |
| Non-context emissions | 22 (`cmd/copilotd` 8, `identity/deviceflow.go` 5, `identity/manager.go` 8, `server/server.go` 1) |
| Records carrying `component` | 0 |
| Exported optional `Logger` fields | 4 |
| `WithLogger` options | 2 |
| `slog.Default()` fallback expressions | 5 |
| Nil-guarded emissions | 1 |
| Distinct top-level attribute key literals | 42 |
| Structs or config values holding a `*slog.Logger` | 11 |

The two raw matches that look like emissions and are not remain
`err.Error()` at `cmd/copilotd/main.go:81` and
`errResponseBodyTooLarge.Error()` at `internal/upstream/read_bounded.go:37`;
excluding them, `read_bounded.go` has no emission and both
`internal/wsforward` `LogAttrs` sites are included.

### Ownership is unrepresented, and injection is optional

No record names its emitting package. Worse, a logger is optional at six
boundaries and defaulted at five expressions:

| Optional boundary | Site |
|---|---|
| `identity.DeviceFlowConfig.Logger` | `internal/identity/deviceflow.go:67` |
| `identity.ManagerConfig.Logger` | `internal/identity/manager.go:131` |
| `sse.Policy.Logger` | `internal/sse/pump.go:58` |
| `catalog.Rendering.Logger` | `internal/catalog/handler.go:20` |
| `forward.WithLogger` | `internal/forward/forward.go:54` |
| `cache.WithLogger` | `internal/cache/value.go:74` |

| `slog.Default()` fallback | Site | Record owner |
|---|---|---|
| cached-value refresh | `internal/cache/value.go:132` | `internal/cache` |
| forwarder-held pump logger | `internal/forward/forward.go:73` | **`internal/sse`** |
| device flow | `internal/identity/deviceflow.go:167` | `internal/identity` |
| Manager | `internal/identity/manager.go:207` | `internal/identity` |
| SSE pump | `internal/sse/pump.go:81` | `internal/sse` |

The forwarder's fallback is the one that makes the ownership rule concrete:
`internal/forward` emits **no records of its own**. It holds a logger solely to
assign `policy.Logger = f.logger` at `internal/forward/forward.go:285`, and the
only record that logger writes is the SSE pump's suppressed-shim-error warning.
Its owner is `internal/sse`, and after this conversion the string
`internal/forward` appears in no record.

The nil guard is `internal/catalog/handler.go:85`:

```go
if err == nil && rendering.Logger != nil {
    for _, skipped := range outcome.SkippedReviewers { ... }
}
```

and it is not hypothetical. `internal/server/handler.go:59` constructs a
`catalog.Rendering` literal that **omits `Logger` entirely** and compiles; only
`internal/server/handler.go:62` sets it. The omission is harmless today by
accident — `servesCodexShape` requires `ep.Surface() == endpoint.OpenAI`, so the
Anthropic catalog can never reach the guarded block — not by design. Required
positional injection exists to kill exactly this shape: a forgotten logger that
silently suppresses a copilotd-owned warning instead of failing to compile.

### Scope is reconstructed from strings at the boundary

`internal/server/middleware.go:48-63` rebuilds every scoping fact after the
handler returns:

```go
route := requestWithHolder.Pattern
if route == "" {
    route = "unmatched"
}
level := slog.LevelInfo
if r.URL.Path == healthPath {
    level = slog.LevelDebug
}
...
if route == "GET /openai/v1/responses" {
    attrs = append(attrs, slog.Bool("ws", true))
}
```

Three defects in nine lines: a client-visible `"unmatched"` sentinel (repeated
verbatim at `internal/wsforward/proxy.go:206`); WebSocket classification by
string comparison against a path that `internal/endpoint` already owns as a
typed contract; and a probe demotion keyed on `healthPath` alone, so `/readyz` —
polled on exactly the same cadence, and answering `503` whenever the process is
not locally ready — logs at Info.

`surface` is not emitted anywhere in the tree. `request_id` reaches only the
seven emissions that pass a request-derived context; four more use a
context-aware method whose context carries no request scope,
`internal/wsforward/proxy.go:258` emits from `context.Background()` and
re-attaches `request_id` by hand, and the remaining 22 cannot pick it up at all.

### One milestone, four layers

A successful WebSocket request can emit four Info records today — upstream
correlation, `websocket established`, `websocket session`, and `access` — plus a
fifth if it triggered an on-demand mint. `internal/upstream/caller.go:111` logs
`upstream response correlation` at Info on every correlated request, and then
drops the fact: nothing downstream carries `upstream_request_id`, so the access
record for a failed request cannot be joined to Copilot's own id without a
second query.

`identity.logMint` spends four call sites on one event
(`internal/identity/manager.go:408,413,423,426`), carrying the distinction in
message text — `"(transient)"`, `"(permanent)"`, `": not transient — check the
Copilot subscription"` — rather than in an attribute. The
`exchangeError.authClass` field is documented at `manager.go:74` as
"used only to shape the log message."

## Goals

- Every copilotd-owned record carries its owner's `component`; no record carries
  a false one.
- A forgotten logger fails to compile.
- `request_id`, `surface`, `inbound`, `ws`, and a differing
  `upstream_request_id` reach every participating record — including the
  after-handler access summary — through immutable derived contexts.
- One terminal summary per returning request, with the WebSocket terminal facts
  on it.
- Level follows operational consequence at every one of the 33 sites.
- Every top-level key resolves to one exported constant.
- The structural half is gated by a standard-library test on the existing
  `go test` step, and the gate lands with the conversion.

## Non-goals

- **No logging API of copilotd's own.** No logger type, no logging method, no
  custom handler semantics beyond the existing context injection, no runtime
  validation, no attribute bounding. Governed by
  [#143](https://github.com/ningw42/copilotd/issues/143).
- **No message-text rule.** ADR-0015 says message text stays ungoverned; the
  message rewrites below are conversion consequences of moving a distinction
  into an attribute, not a naming policy.
- **No start record, watchdog, or timeout.** Access is emitted after the handler
  returns, so a wedged request still produces no access record. That signal is
  [#138](https://github.com/ningw42/copilotd/issues/138)'s.
- **No configuration change.** `text` stays the unconditional default for both
  commands, `json` stays explicit, and no TTY, journald, or sink detection is
  added. `defaultLogFormat = "text"`, the shared serve/login descriptor, the
  text-handler fallback, and both `CONFIGURATION.md` default rows (lines 24 and
  44) are already correct and are not touched
  ([#150](https://github.com/ningw42/copilotd/issues/150)). The governed
  vocabulary applies equally to both encodings.
- **No per-component filtering, metrics, tracing, or `pprof`.**
- **No second redaction mechanism.** ADR-0012's `LogValue` already keeps config
  secrets out by construction.
- **No adaptation of third-party records.** They may omit Component; see
  [The one component-free consumer](#the-one-component-free-consumer) for the
  single boundary where copilotd routes them.
- **No change to `copilotd login`'s stdout progress output.** It stays as it is;
  whether it should stop being log records is out of scope on the map.

## The key registry

`internal/logging` gains one file of exported string constants. Identifiers
follow slog's own style; emitted names stay `snake_case`. The registry is a
declaration, not an allow-list: no descriptor, no owner metadata, no typed
constructor, no validation.

### The full inventory

Forty-seven constants. "Where" names the record-owning package(s); "Source" is
the decision that fixed the name.

| Constant | Key | Where | Source |
|---|---|---|---|
| `ServiceKey` | `service` | base | #145 |
| `VersionKey` | `version` | base (build version) | #145 |
| `ComponentKey` | `component` | every logger child | #141, #142 |
| `RequestIDKey` | `request_id` | request scope | #145 |
| `SurfaceKey` | `surface` | request scope | #145 (**new in tree**) |
| `InboundKey` | `inbound` | request scope | #145 (**renamed** from `route`) |
| `WSKey` | `ws` | request scope | #146 |
| `UpstreamRequestIDKey` | `upstream_request_id` | request scope (late) | #144 |
| `MethodKey` | `method` | `internal/server` | #146 |
| `StatusKey` | `status` | `internal/server`, `internal/identity`, `internal/wsforward` | existing |
| `BytesKey` | `bytes` | `internal/server` | #146 |
| `DurationKey` | `duration` | `internal/server` | #146 |
| `HandshakeDurationKey` | `handshake_duration` | `internal/wsforward` | #146 (**new**) |
| `OutcomeKey` | `outcome` | `internal/server` | #146 |
| `FramesKey` | `frames` | `internal/server` | #146 |
| `FallbacksKey` | `fallbacks` | `internal/server` | #146 |
| `CatalogShapeKey` | `catalog_shape` | `internal/server` | #146 |
| `TerminalReasonKey` | `terminal_reason` | `internal/server` | #146 (**moved**) |
| `CloseCodeKey` | `close_code` | `internal/server` | #146 (**moved**) |
| `MsgsC2UKey` | `msgs_c2u` | `internal/server` | #146 (**moved**) |
| `MsgsU2CKey` | `msgs_u2c` | `internal/server` | #146 (**moved**) |
| `BytesC2UKey` | `bytes_c2u` | `internal/server` | #146 (**moved**) |
| `BytesU2CKey` | `bytes_u2c` | `internal/server` | #146 (**moved**) |
| `PanicKey` | `panic` | `internal/server` | existing |
| `TimeoutKey` | `timeout` | `internal/server` | existing |
| `AddrKey` | `addr` | `internal/server`, `cmd/copilotd` | existing |
| `ErrorKey` | `error` | `internal/cache`, `internal/upstream`, `cmd/copilotd` | existing |
| `StageKey` | `stage` | `internal/sse` | existing |
| `CachedValueKey` | `cached_value` | `internal/cache`, `cmd/copilotd` | existing |
| `CachedValueVersionKey` | `cached_value_version` | `internal/cache`, `cmd/copilotd` | #145 (**renamed** from `version`) |
| `CachedValueSourceKey` | `cached_value_source` | `cmd/copilotd` | #145 (**renamed** from `source`) |
| `TriggerKey` | `trigger` | `internal/identity` | existing |
| `FailureClassKey` | `failure_class` | `internal/identity` | #144 (**new**) |
| `ExpiresAtKey` | `expires_at` | `internal/identity` | existing |
| `RefreshInKey` | `refresh_in` | `internal/identity` | existing |
| `AttemptKey` | `attempt` | `internal/identity` | existing |
| `AttemptsKey` | `attempts` | `internal/identity` | existing |
| `IntervalKey` | `interval` | `internal/identity` | existing |
| `VerificationURIKey` | `verification_uri` | `internal/identity` | existing |
| `ExpiresInKey` | `expires_in` | `internal/identity` | existing |
| `LoginKey` | `login` | `internal/identity` | existing |
| `PathKey` | `path` | `internal/identity` | existing |
| `ModelKey` | `model` | `internal/catalog` | existing |
| `ReviewerKey` | `reviewer` | `internal/catalog`, `cmd/copilotd` | existing |
| `BuildKey` | `build` | `cmd/copilotd` | existing |
| `ConfigKey` | `config` | `cmd/copilotd` | existing |
| `EnabledShimsKey` | `enabled_shims` | `cmd/copilotd` | existing |

Arithmetic: 42 distinct literals today, minus `route` and `source` (renamed
away), plus `component`, `surface`, `inbound`, `cached_value_version`,
`cached_value_source`, `failure_class`, and `handshake_duration` — 47.

### Two live key collisions

The two renames are not cosmetic. Both fix a duplicate top-level key that native
slog emits today, exactly as ADR-0015 says it will:

- **`version`.** The base logger attaches `version=<build version>`;
  `internal/cache/value.go:260` and `cmd/copilotd/main.go:392` add a
  record-local `version=<cached value version>`. Every successful cached-value
  refresh already renders `version` twice with different values.
- **`source`.** `NewWithWriter` sets `AddSource: level == slog.LevelDebug`, so at
  `--log-level=debug` slog emits its own `source=file:line`;
  `cmd/copilotd/main.go:391` adds `source=fallback|fetched` on the same handler.

After the renames no production record can produce a duplicate top-level key.
That is a property of the call sites, not of the handler: logging still does
nothing special about collisions, and a future one renders under native slog
semantics.

### The registry import cycle

**Not anticipated by any ticket.** Registry provenance makes `internal/cache`
import `internal/logging`, while `internal/logging.New` keeps its required
`config.ServeConfig` signature. The starting tree's configuration validator
imported `internal/impersonation` solely to reuse its bare-version predicate,
and impersonation imports cache. Leaving that edge in place would therefore
close `cache -> logging -> config -> impersonation -> cache`, which Go rejects.

The bare semantic-version predicate moves unchanged into the dependency-leaf
`internal/bareversion` package, and both configuration validation and
impersonation discovery call it there. This removes the cycle without changing
the accepted version grammar, configuration surface, defaults, or runtime
behaviour; focused tests at the leaf plus the existing config and impersonation
suites keep both consumers on the one predicate.

### `failure_class` values are not registry entries

The registry governs key **names**. `failure_class`'s four values —
`transient`, `auth`, `non_transient`, `unclassified` — are a bounded vocabulary
owned by the package that classifies, exactly like `sse.Outcome`,
`catalog.CatalogShape`, and `wsforward.SessionTerminal`. `internal/identity`
gains an unexported `failureClass` string type with those four constants. No
value vocabulary enters `internal/logging`.

## Logger construction

### The base and its children

`logging.New` / `logging.NewWithWriter` keep their signatures and return the
**component-free base**: the shared handler, `service`, and build `version`.
`cmd/copilotd` installs it with `slog.SetDefault` at `main.go:286` and
`main.go:529`, unchanged, so a record emitted inside an unadapted dependency
keeps no Component rather than a false one.

One new function:

```go
// ForComponent returns a child of base whose records name the copilotd package
// that owns their emission, as a repository-relative import path.
func ForComponent(base *slog.Logger, component string) *slog.Logger {
	return base.With(slog.String(ComponentKey, component))
}
```

**Decoration happens only in `cmd/copilotd`.** Every other package receives an
already-decorated logger as a required positional argument and passes it on
verbatim; no package may re-decorate a logger it was handed, because ordinary
slog's append semantics offer no safe relabel. That rule is what lets the
inventory below sit in one place and be checked syntactically, within the limit
stated under [the AST test](#the-ast-test).

### The closed inventory

The base originates at the two `logging.New` calls — `main.go:279` in `runServe`
and `main.go:520` in `runLogin` — and must reach three more `cmd/copilotd`
functions, because that is where the sinks are constructed. Each of those three
already carries one `logger *slog.Logger`; the parameter is renamed `base` and
changes meaning, not arity. Their thirteen existing test call sites — three for
`runBoundServe`, eight for `buildServeProvider`, two for
`configuredCodexModels` — already pass an undecorated `logging.NewWithWriter`
logger, so they keep compiling unchanged.

**Base-carrying functions.** A closed list; the base may be passed to nothing
else.

| Function | Parameter | Receives from |
|---|---|---|
| `runBoundServe` | `base` | `runServe` (`main.go:327`) |
| `buildServeProvider` | `base` | `runServe` (`main.go:302`) |
| `configuredCodexModels` | `base` | `runServe` (`main.go:309`) |

Decoration then happens inline, at each sink. Twelve sites, eight components:

| # | Enclosing function | Consumer | Component |
|---|---|---|---|
| 1 | `runServe` | local, for this package's own records | `cmd/copilotd` |
| 2 | `runBoundServe` | `runServeStartup` | `cmd/copilotd` |
| 3 | `runBoundServe` | `upstream.New` | `internal/upstream` |
| 4 | `runBoundServe` | `forward.New` (pump logger) | **`internal/sse`** |
| 5 | `runBoundServe` | `wsforward.New` | `internal/wsforward` |
| 6 | `runBoundServe` | `server.New` (server logger) | `internal/server` |
| 7 | `runBoundServe` | `server.New` (catalog logger) | `internal/catalog` |
| 8 | `buildServeProvider` | `identity.NewManager` | `internal/identity` |
| 9 | `buildServeProvider` | `impersonation.New` (cache logger) | **`internal/cache`** |
| 10 | `configuredCodexModels` | `catalog.NewModelsCache` (cache logger) | **`internal/cache`** |
| 11 | `runLogin` | local, for this package's own record | `cmd/copilotd` |
| 12 | `runLogin` | `identity.Login` | `internal/identity` |

Row 2 is why `runBoundServe` needs the base and not a child: the startup
cached-value summary at `main.go:389` is emitted by
`logCachedValueStartupOutcomes`, reached only through `runServeStartup`
(`main.go:340` → `:382` → `:387`), so `cmd/copilotd`'s own child is derived a
second time on that path. `runServe`'s child (row 1) reaches
`logCodexCatalogStaging` (`main.go:292`) and `logShimChain` (`main.go:294`) as
an ordinary argument: **children propagate freely; only the base is
restricted.**

Rows 4, 9, and 10 are the ownership rule doing work: the parameter is named for
the owner it carries (`sseLogger`, `cacheLogger`), not for the package
receiving it. `internal/catalog` receives two differently decorated loggers at
two boundaries — its own for the skipped-reviewer warning (row 7, forwarded by
`internal/server`), and a cache-decorated one for the Codex models cached value
(row 10). `internal/forward` and `internal/impersonation` own no record and are
never decorated.

### Required positional injection

Eleven construction boundaries take a `*slog.Logger`. Six change shape:

| Boundary | Today | After |
|---|---|---|
| `cache.New` | `New(src, options...)` + `WithLogger` | `New(logger, src, options...)`; `WithLogger` deleted |
| `sse.Pump` | `Policy.Logger`, nil ⇒ `slog.Default()` | `Pump(ctx, cancel, body, dst, logger, policy, transformer)`; `Policy.Logger` deleted |
| `forward.New` | `WithLogger` option | positional `sseLogger` before `options...`; `WithLogger` deleted |
| `identity.NewManager` | `ManagerConfig.Logger` | `NewManager(logger, cfg)`; field deleted |
| `identity.Login` | `DeviceFlowConfig.Logger` | `Login(ctx, logger, cfg)`; field deleted |
| `catalog.Handler` | `Rendering.Logger`, nil-guarded | `Handler(logger, ep, rendering, source)`; field and guard deleted |

Five already take a positional logger and change only their argument at the
composition root: `upstream.New`, `wsforward.New`, `server.New`,
`impersonation.New`, `catalog.NewModelsCache`.

`server.New` additionally gains the `internal/catalog` logger it forwards to
`newHandler` → `registerCatalog` → `catalog.Handler`, and the dependency error
log below. Its parameter list grows; that is the price of making the catalog's
owner explicit rather than optional. A `Loggers` struct was considered and
rejected in [Alternatives considered](#alternatives-considered): it re-creates
the omission this conversion exists to delete.

Go cannot forbid an explicitly passed typed nil. It now forbids omission, nil is
not a supported contract, and tests supply an explicit logger like everyone
else.

### The one component-free consumer

**Not anticipated by any ticket.** `internal/server/server.go:72` bridges
`net/http`'s own error stream into slog:

```go
ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
```

Once `logger` is `ForComponent(base, "internal/server")`, that handler carries
`component=internal/server`, and every record net/http writes there —
`http: TLS handshake error`, `http: superfluous response.WriteHeader call`, and
the rest — acquires a copilotd Component it did not earn. That is precisely
the false attribution ADR-0015's component-free base exists to prevent: these
records are *routed* by copilotd, not *emitted* by it.

The bridge therefore rides the base. `internal/logging` gains:

```go
// DependencyErrorLog adapts an external dependency's *log.Logger sink onto the
// component-free base, so its unadapted records keep no Component.
func DependencyErrorLog(base *slog.Logger, level slog.Level) *log.Logger {
	return slog.NewLogLogger(base.Handler(), level)
}
```

`cmd/copilotd` constructs it and passes it to `server.New` as a `*log.Logger`.
`internal/server` then holds no undecorated `*slog.Logger` at all, and the base
never leaves the composition root except through `slog.SetDefault` and this one
adapter. Level stays Warn, as today.

Two details are deliberate. The `level` parameter stays at the **call site**
rather than being hardcoded inside the helper: ADR-0015 puts level with the site
that knows the operational consequence and keeps level policy out of
`internal/logging`, and it is the composition root — not the logging package —
that judges net/http's error stream to be a contained abnormality. And the
helper exists at all, rather than an inline
`slog.NewLogLogger(base.Handler(), …)`, because it gives the base a *nameable*
permitted use alongside `slog.SetDefault` and `logging.ForComponent`: those
three and passing the base to a declared base-carrying parameter are the only
four rule 2 admits, so a bare `base.Handler()` at the root would otherwise have
to be exempted by shape or allowed generally.

## Request scope

### `logging.With`

```go
// With returns a derived context carrying attrs in addition to any already in
// scope. The parent is unchanged; nested calls append in scope order; there is
// no removal, override, or deduplication.
func With(ctx context.Context, attrs ...slog.Attr) context.Context
```

Copy-on-write over a `[]slog.Attr` under a private key: the parent's slice is
never appended to in place, so sibling contexts cannot observe one another.
`contextHandler.Handle` adds the accumulated attributes to the record after the
existing `request_id` injection. Ordering, duplicate keys, `Logger.With`, and
`WithGroup` remain native slog's business.

`request_id` keeps its own mechanism. `logging.WithRequestID` stores a
*retrievable* value — `upstream.Correlate` compares it, the outbound header
policy sends it, `wsforward` reads it — while `logging.With` carries
display-only scope. A caller must not put `request_id` in a `With` scope; that
would render the key twice.

The [#143](https://github.com/ningw42/copilotd/issues/143) group hazard is
inherited knowingly: attributes a handler injects in `Handle` land inside
whatever group is open, so a caller doing `logger.WithGroup("g")` would render
`g.request_id`. Nothing in production calls `WithGroup`; the existing
`TestContextHandlerPreservesAttrsAndGroups` keeps covering it. Fixing it means
taking over `groupOrAttrs` bookkeeping, which is the custom-handler machinery
#143 rejected.

### The registration wrapper and the server-owned handoff

`internal/server` gains one scope wrapper and one holder. The wrapper is applied
per registered pattern, outside the auth and readiness guards, so a `401` or
`503` inherits the same scope a success would:

```go
func scoped(attrs []slog.Attr, probe bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.With(r.Context(), attrs...)
		publishMatchedScope(r.Context(), matchedScope{ctx: ctx, probe: probe})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

`attrs` is computed once, at registration, from the typed contract — never from
the inbound request:

| Registration | Attributes | `probe` |
|---|---|---|
| `mux.Handle(pattern, ...)` for a probe | `inbound` | `true` |
| `registerForward`, `registerPassthrough`, `registerCatalog` | `inbound`, `surface` | `false` |
| `registerWS` (parameter type `endpoint.WSForward`) | `inbound`, `surface`, `ws=true` | `false` |

`inbound` is the exact pattern string the loop registered, so it is a compile-
time-known value that is by construction equal to `r.Pattern` for any request
that reaches the wrapper — including the `GET` pattern an implicit `HEAD`
matches. No client-controlled value enters a record, and `internal/endpoint`
gains no logging concern and no kind enum: `ws=true` exists in exactly one place
in the repository, inside the function whose parameter is
`endpoint.WSForward`.

`matchedScope` carries an immutable context pointer and one boolean classifying
the *binding*, not the request. It is not an attribute bag: nothing writes an
attribute into it, and `probe` never becomes a key.

`recoverMW` resolves the same handoff, so `panic recovered` inherits the scope
the wrapper published before the panic unwound.

### Absence is the unmatched state

**Not anticipated by any ticket.** Publishing scope from the wrapper is not
equivalent to reading `r.Pattern` after the fact, and the difference is a live
defect. `net/http`'s `ServeMux.findHandler` (`server.go:2689-2697`) sets
`r.Pattern` from the node that matched the *cleaned* path before returning a
`RedirectHandler`. A request to `//openai/v1/models` is therefore answered
`307` by the mux itself — every redirect in that dispatch path uses
`StatusTemporaryRedirect` (`server.go:2671`, `:2687`, `:2696`) — our handler
never runs, and today's access record still reports
`route="GET /openai/v1/models"`, labelling an endpoint that served nothing.

Under the wrapper, nothing is published and `inbound` is absent. The same holds
for `404` (`patStr = ""`) and for `405`, where a path matches but the method
does not and the mux answers from its own handler (`server.go:2699-2711`).
Absence means "no registered handler ran", which is both the honest fact and
what [#146](https://github.com/ningw42/copilotd/issues/146) asked for: no
sentinel, no raw path.

### The upstream-owned correlation holder

`internal/upstream` owns the correlation seam and one narrow holder:

```go
// Correlate adds upstream_request_id to a derived logging context when Copilot
// returned an id that differs from copilotd's own, publishes that context for
// after-handler access logging, and returns it. Otherwise it returns ctx.
func (c *Caller) Correlate(ctx context.Context, header http.Header) context.Context
```

The standalone retrieval event demotes to Debug and stops being the fact's only
home. Two entry points change shape so the derived context reaches the transport
explicitly rather than through the holder alone:

| Today | After |
|---|---|
| `Do(ctx, call) (*http.Response, *Failure)` | `Do(ctx, call) (*http.Response, context.Context, *Failure)` |
| `Buffered(ctx, call) (int, []byte, *Failure)` | `Buffered(ctx, call) (int, []byte, context.Context, *Failure)` |

`catalog.Source`'s one-method interface follows `Buffered`. Call sites:

- `forward.forward` (`forward.go:232`) passes the returned context to
  `sse.Pump`, so a suppressed-shim-error warning carries the upstream id. The
  returned context is a child of the cancellable context the pump already uses,
  so cancellation semantics are unchanged.
- `forward.PassthroughHandler` (`forward.go:149`) discards it: the passthrough
  tail emits nothing. Access still receives it through the holder — which is why
  the holder exists in addition to the return.
- `catalog.Handler` (`handler.go:51`) uses it for the skipped-reviewer warning.
- `wsforward.Proxy.Handler` (`proxy.go:189`) uses it for
  `websocket established`.

Access prefers the correlated context, then the matched-scope context, then the
request context. Preferring the correlated one loses nothing: it is derived from
a descendant of the scoped request context, so it already carries `inbound`,
`surface`, and `ws`.

One consequence worth stating because it looks alarming and is not: the
correlated context is usually **cancelled** by the time access reads it, because
the transports cancel their upstream context on return. `slog` uses a context
only to reach handler state; our `contextHandler` reads values and never
consults `ctx.Err()`. A cancelled context logs exactly like a live one.

### The `wsforward` result holder

`internal/wsforward` gains the store-once/read-once holder the other two
result-carrying packages already have — an unexported holder behind a private
context key, with a `With…Holder` / `Store…` / `…FromContext` trio
(`forward/stream_result.go:31,49,37`; `catalog/shape_result.go:26,43,31`). Only
the carried value differs: `forward.StreamResult` is a struct,
`catalog.CatalogShape` a bare string type, and `wsforward`'s is a struct.

```go
type SessionResult struct {
	Terminal  SessionTerminal
	CloseCode int
	MsgsC2U, MsgsU2C, BytesC2U, BytesU2C int64
}
func WithSessionResultHolder(ctx context.Context) context.Context
func StoreSessionResult(ctx context.Context, result SessionResult)
func SessionResultFromContext(ctx context.Context) (SessionResult, bool)
```

`accessLog` installs it beside the two existing holders; the proxy stores the
result where it calls `p.logSession` today, before the handler returns. No
terminal record is emitted from `context.Background()` ever again.

### What the holders are not

Five handoffs now cross the `http.Handler` return boundary: the server-owned
matched scope, the upstream-owned correlated context, and the three
package-owned result values (`forward`, `catalog`, `wsforward`). Each carries an
immutable value written once, before the handler returns, and read once, after.
None mutates, replaces, deduplicates, or bounds a logging attribute; none is a
shared attribute bag; and no package reads another's. All five are installed in
one place, `accessLog`, so the read side has exactly one context to consult.

The per-request cost is seven context allocations for a matched request, up from
three: the request id, five holders, and one `logging.With` scope. Each is a
small, request-scoped value, and the cost is paid on probes too.

## The access record

### Final field list

One record per request whose handler returns, `component=internal/server` even
for facts produced elsewhere.

| Source | Attribute | Condition |
|---|---|---|
| logger | `service`, `version`, `component` | always |
| context | `request_id` | always |
| context | `inbound` | a registered binding's scope wrapper ran |
| context | `surface` | matched an Endpoint (absent for probes and unmatched) |
| context | `ws=true` | matched the `endpoint.WSForward` registration |
| context | `upstream_request_id` | Copilot returned a differing id |
| record | `method` | always — the actual request method, which an implicit `HEAD` makes differ from `inbound`'s |
| record | `status` | always — `101` for an established WebSocket, which `websocket.Accept` writes through `statusWriter` before hijacking |
| record | `bytes` | always — downstream body bytes; `0` for `HEAD` (`suppressBodyBytes`) and `0` for an upgraded WebSocket |
| record | `duration` | always — arrival to handler return, covering handshake and session |
| record | `outcome`, `frames`, `fallbacks` | an SSE pump ran |
| record | `catalog_shape` | an OpenAI catalog rendered |
| record | `terminal_reason`, `close_code`, `msgs_c2u`, `msgs_u2c`, `bytes_c2u`, `bytes_u2c` | a WebSocket session was established |

There is no `unmatched` sentinel, no raw request path, and no second duration.

### Level selection

Evaluated in this order; the probe test is first so a `503` from `/readyz` stays
Debug, as [#144](https://github.com/ningw42/copilotd/issues/144) requires:

1. `matchedScope.probe` → **Debug**. Both probes, every sample, including
   not-ready.
2. `outcome ∈ {synthesized, stall, upstream_error, shim_error}` → **Warn**.
3. `terminal_reason == error` → **Warn**.
4. `status >= 500` → **Warn**.
5. otherwise → **Info**. Success, redirect, expected `4xx`, client cancellation,
   and normal WebSocket closure.

Never Error. A recovered panic still produces an Error at `recoverMW` and a Warn
access summary for the resulting `500`.

### WebSocket: two milestones, one terminal summary

`websocket established` stays a distinct Info milestone — a usable long-lived
session now exists — and slims to its own facts:

| Today (`proxy.go:208-215`) | After |
|---|---|
| `method`, `route`, `status`, `bytes=0`, `ws`, `duration` | `status=101`, `handshake_duration` |

`route`, `ws`, `surface`, and `request_id` arrive through scope;
`method` and `bytes` belong to access; the ambiguous `duration` becomes
`handshake_duration`, measured from `handshakeStart` exactly as today.

`websocket session` (`proxy.go:258`) is deleted, with `p.logSession` and its
manual `request_id` re-attachment. Its six bounded facts move onto access
through the result holder; its `duration` is dropped in favour of access's one
whole-request duration.

## Per-call-site conversion

All 33, in file order. "Ctx" marks an emission that uses a context-aware method
after conversion.

### `cmd/copilotd/main.go` — 8 sites, component `cmd/copilotd`

| Line | Level | Message | Change |
|---|---|---|---|
| 288 | Info | starting copilotd | keys `build`, `config` from registry |
| 306 | Error | cannot start: resolving the GitHub OAuth token failed | key only |
| 314 | Error | bind failed | keys `addr`, `error` |
| 328 | Error | server error | key only |
| 389 | Info | startup cached value refresh outcome | `source` → `cached_value_source`; `version` → `cached_value_version` |
| 400 | Info | Codex reviewer is staged while the Codex catalog is disabled | key only |
| 424 | Info | configured shim chain | key only |
| 531 | Info | starting device-flow login | keys `build`, `config` |

All eight stay non-context: `runServe`'s and `runLogin`'s contexts never carry
logging scope, and ADR-0015 makes participation a call-site decision rather than
an obligation of having a context in hand. Levels are unchanged — startup and
shutdown milestones are Info, and a failure that stops the process is Error.

Line 389 stays Info even when a value is serving its fallback: the abnormality
is reported once, by `internal/cache`'s Warn at the failed attempt, and this
record is the resulting-state summary rather than a second report of the same
event.

### `internal/cache/value.go` — 2 sites, component `internal/cache`

| Line | Level | Message | Change |
|---|---|---|---|
| 259 | Debug (Ctx) | cached value refresh succeeded | `version` → `cached_value_version` |
| 267 | Warn (Ctx) | cached value refresh failed | keys only |

Levels are already right: a refresh outcome on a TTL cadence is a poll sample
(Debug), a failed one is contained (Warn). The contexts come from `Prime` and
`Run` and carry no request scope, which is correct — a refresh is not request
work.

### `internal/catalog/handler.go` — 1 site, component `internal/catalog`

| Line | Level | Message | Change |
|---|---|---|---|
| 87 | Warn (Ctx) | Codex catalog reviewer was skipped | logger is now positional; `rendering.Logger != nil` guard deleted; emits under the correlated context |

Warn is right: a configured reviewer could not be injected, which is contained
but points at a configuration the operator chose.

### `internal/identity/deviceflow.go` — 5 sites, component `internal/identity`

| Line | Level | Message | Change |
|---|---|---|---|
| 115 | Info | device code requested | keys only |
| 131 | Info | device flow authorized | key only |
| 140 | Info | wrote github oauth token | key only |
| 227 | Debug | authorization pending | key only |
| 235 | Debug | slow down; backing off | key only |

Three Info records for one `copilotd login` are three distinct milestones, which
the semantic budget allows. The poll and backoff samples are already Debug. All
five stay non-context: login has no request scope.

### `internal/identity/manager.go` — 8 sites → 5, component `internal/identity`

| Line | Today | After |
|---|---|---|
| 254 | Warn — startup mint short-circuited on a permanent failure | unchanged; contained startup outcome beside `logMint`'s originating Error |
| 257 | Debug — startup mint attempt failed (transient), will retry | unchanged; retry-lifecycle sample owning `attempt`/`attempts` |
| 260 | Warn — startup mint exhausted its retries | unchanged; retry exhaustion is Warn |
| 400 | Info — minted copilot token | **Ctx**; keys `trigger`, `expires_at`, `refresh_in` |
| 408, 413, 423, 426 | four messages, four levels | **one** `LogAttrs` site: one message, `failure_class`, computed level |

`logMint` gains a `context.Context` parameter supplied by `mint`, so the
on-demand path propagates request scope while the startup path passes a context
that carries none. The exchange still runs on a background-scoped context, and
the singleflight still collapses concurrent callers onto one exchange: the
resulting record carries the winning caller's `request_id` and its `trigger`,
which is the honest attribution of one exchange to the request that occasioned
it.

### `failure_class` and the level mapping

`logMint`'s failure branch becomes one record whose level is a pure function of
classification and trigger:

| Condition in the tree | `failure_class` | Level |
|---|---|---|
| `!errors.As(err, &ee)` | `unclassified` | **Error** |
| `ee.authClass` (401/403/404, `manager.go:341`) | `auth` | **Error** |
| `ee.transient`, `trigger == "startup"` (`:313`, `:323`, `:326`, `:330`, `:343`) | `transient` | **Debug** |
| `ee.transient`, `trigger == "on-demand"` | `transient` | **Warn** |
| otherwise (`:301`, `:346`) | `non_transient` | **Error** |

The two transient rows are #144's rule without new plumbing: a startup attempt
is a retry sample, and the *terminal* startup outcome — exhaustion — is already
reported at Warn by `manager.go:260`, while an on-demand mint is single-attempt,
so its transient failure terminates the operation.

`unclassified` is unreachable today by construction: every error path in
`Manager.exchange` returns an `*exchangeError`. It earns Error rather than Warn
because reaching it means the classification boundary was violated — an
invariant, not an outcome.

`status` is emitted only when non-zero. Today a network-level failure logs
`status=0`, which is not an HTTP status.

`exchangeError.authClass`'s comment — "used only to shape the log message" —
becomes false here and is rewritten: it shapes `failure_class` and level.

### `internal/server/middleware.go` — 2 sites, component `internal/server`

| Line | Today | After |
|---|---|---|
| 81 | access, level from `healthPath` and stream outcome | the record and level rule in [The access record](#the-access-record); sentinel and hardcoded `ws` comparison deleted |
| 92 | Error — panic recovered | unchanged level; resolves the matched-scope handoff so it inherits `inbound`, `surface`, `ws` |

### `internal/server/server.go` — 2 sites, component `internal/server`

| Line | Level | Message | Change |
|---|---|---|---|
| 88 | Info (Ctx) | listening | key `addr` |
| 104 | Info | shutting down | key `timeout` |

### `internal/sse/pump.go` — 1 site, component `internal/sse`

| Line | Level | Message | Change |
|---|---|---|---|
| 164 | Warn (Ctx) | suppressed post-terminal shim error | logger now positional to `Pump`; key `stage`; inherits the correlated context |

Warn is right: a shim panic hidden from the wire by no-double-up is contained,
and repetition warrants investigation.

### `internal/upstream/caller.go` — 2 sites, component `internal/upstream`

| Line | Today | After |
|---|---|---|
| 111 | **Info** — upstream response correlation | **Debug**, and `Correlate` derives, publishes, and returns the context carrying `upstream_request_id` |
| 116 | Warn — upstream call failed | unchanged |

### `internal/wsforward/proxy.go` — 2 sites → 1, component `internal/wsforward`

| Line | Today | After |
|---|---|---|
| 208 | Info — websocket established, 6 attributes | Info — `status=101`, `handshake_duration`; emitted under the correlated context |
| 258 | Info/Warn — websocket session | **deleted**; six facts to access via the result holder |

### Counts after conversion

29 emissions across the same 10 files: 33 − 1 (`websocket session`) − 3
(`logMint`'s four failure sites collapsing to one). Of today's 22 non-context
sites, `logMint`'s five become two context-aware ones; the other 17 stay
non-context because their contexts — startup, shutdown, login — never carry
logging scope. Twelve emissions are context-aware afterwards, and seven of those
carry request scope.

## Comments that stop being true

| Site | Claim | Correction |
|---|---|---|
| `internal/sse/pump.go:56-57` | "Nil uses slog.Default." | logger is a required argument |
| `internal/forward/forward.go:52-53` | "A nil logger keeps the process default captured by New." | option deleted; the field is the `internal/sse`-owned pump logger |
| `internal/cache/value.go:73` | "WithLogger replaces the logger used for refresh outcomes." | option deleted |
| `internal/identity/manager.go:129-130` | "(default slog.Default())" | field deleted; positional argument |
| `internal/identity/deviceflow.go:66` | "Logger records each step (default slog.Default())" | field deleted; positional argument |
| `internal/identity/manager.go:74` | `authClass` "used only to shape the log message" | shapes `failure_class` and level |
| `internal/identity/manager.go:165` | "applying defaults for any zero field" | the logger is no longer a field with a default |
| `internal/identity/deviceflow.go:101` | "Zero DeviceFlowConfig fields take documented defaults." | true of every remaining field, not of the logger |
| `internal/server/middleware.go:29-34` | three at once: the `"unmatched"` fallback; labelling "by route template"; "the quiet health route" in the singular | `inbound` is absent when nothing matched; the label is the matched binding published by the registration wrapper; **both** probes log at Debug |
| `internal/server/handler.go:20-30` | the middleware order | the registration scope wrapper sits between the mux and the auth guard |
| `internal/logging/logging.go:103-106` | a re-wrapping handler "would silently drop attributes and groups" | directionally right, wrong victim: it drops the attributes the *wrapper injects*, not the caller's ([#143](https://github.com/ningw42/copilotd/issues/143)) |
| `internal/logging/logging.go:1-4` | "builds copilotd's structured logger and owns request-id correlation" | also owns the key registry, `ForComponent`, and the `With` scope seam |

## Enforcement

### The AST test

One test file, standard library only (`go/parser`, `go/ast`, `go/token`,
`path/filepath`), in `internal/logging` beside the existing tests. The package
already walks its own non-test source this way —
`TestLoggingHasNoNetHTTPDependency` at `internal/logging/logging_test.go:199`
parses the package with `go/parser` and asserts an import invariant — so the
gate is a wider application of an idiom the repository already runs, not new
machinery. It walks the repository root, resolved as `../..` from the package
directory, and runs on the existing `go test -race ./... -count=1` step. No lint
binary, no `go vet` step, no pre-commit hook, no installed tool. It parses only
non-`_test.go` files under `cmd/` and `internal/`, and uses no type information:
every rule below is syntactic.

**1. No production default logger.** Any `slog.Default()` call expression fails.
`slog.SetDefault` is a different selector and stays: it installs the
component-free base for unadapted dependencies.

**2. Closed construction inventory.** Every `logging.ForComponent` call in
production must live in `cmd/copilotd` and must match, exactly, the twelve-row
table in [The closed inventory](#the-closed-inventory) — enclosing function,
consumer, and component literal. Set equality is asserted in both directions, so
a missing sink fails as loudly as an unexpected one, and a changed owner is an
explicit test edit.

The base is then restricted, which is what stops a new sink from being fed an
undecorated logger. A **base-carrying identifier** is either a variable assigned
from `logging.New` / `logging.NewWithWriter`, or the declared parameter of a
function in the three-row base-carrying table. Inside those five functions,
every use of a base-carrying identifier must be one of:

- `slog.SetDefault(base)`;
- `logging.ForComponent(base, "<literal>")`;
- `logging.DependencyErrorLog(base, <level>)`;
- an argument to a base-carrying function, at that function's declared base
  parameter.

Any other use fails, and the base-carrying table is itself closed — passing the
base to an unlisted function is not a permitted use — so the propagation cannot
grow silently either. Nothing is traced through a call: the table *declares* the
three destinations, and each declared parameter is then checked by name inside
its own function body. Five function bodies in one package, no types, no
inter-procedural analysis.

**The residual, stated plainly.** A new sink fed by a *child*-carrying parameter
— a constructor added inside `runServeStartup`, say, which holds a
`cmd/copilotd` child — mentions neither the base nor `ForComponent`, and no
syntactic rule sees it. ADR-0015 already accepts that limit: "The Component
clause is not self-enforcing, so that closed inventory is its review gate, in
ADR-0012's spirit: a new sink or changed owner is an explicit test edit."

**3. No package-level slog emission.** `slog.Info`, `slog.WarnContext`,
`slog.Log`, and the rest of the package-level emission functions fail.

**4. Registry provenance for keys.** At every `slog` attribute constructor call
(`slog.String`, `slog.Int`, `slog.Any`, `slog.Group`, …) the key argument must
be a `logging.<Ident>Key` selector, or a bare registry identifier inside
`internal/logging` itself. The same requirement covers the two other legal
spellings of a top-level key: a string literal in a key position of an emission
call's variadic `...any` arguments, and an `slog.Attr` composite literal, whose
`Key` field is checked exactly like a constructor's first argument. String
literals, dynamic expressions, and package-local aliases fail. Two documented
exclusions: `internal/logging`'s own registry declaration, and `internal/config`,
whose nested setting names are produced by ADR-0012's descriptor table inside
`LogValue` and are governed there — and which the closed inventory and rule 3
independently prove emits nothing.

**5. No hardcoded WebSocket classification.** `logging.WSKey` may appear in
production only inside the registration function whose parameter type is
`endpoint.WSForward` (and in the registry declaration). Deriving `ws` by
comparing a matched pattern with a string fails.

**Anti-silence.** The test asserts it parsed a non-zero number of production
files and that every expected inventory row was found, so a walk that matches
nothing fails instead of passing.

### Focused behavior tests

Source analysis cannot see semantics, so ordinary tests pin what
[#147](https://github.com/ningw42/copilotd/issues/147) enumerated:

- **`logging`** — `With` derives immutable, append-only contexts; the parent and
  a sibling are unchanged after a child adds attributes; scope order is
  preserved; a duplicate key still renders twice under native slog; the existing
  attrs/groups test survives unchanged; `ForComponent` attaches `component` and
  the base carries none.
- **`server`** — a matched Endpoint yields exact `inbound` and canonical
  `surface`; a `404` and a `405` yield neither; a probe yields `inbound` and no
  `surface`, at Debug, including the not-ready `503`; `ws=true` appears only via
  the typed registration, including on a pre-upgrade rejection; exactly one
  access record per request; `5xx` is Warn and an abnormal SSE outcome is Warn;
  `panic recovered` carries the matched scope.
- **`upstream`** — a differing id derives, publishes, and returns a context; an
  equal or absent id returns the input unchanged; a later response-path record
  and the after-handler access record both carry `upstream_request_id` alongside
  `request_id`.
- **`wsforward`** — `websocket established` carries `status=101` and
  `handshake_duration` and nothing else record-local; the six terminal facts
  appear on access; no `websocket session` record exists; `terminal_reason=error`
  makes access Warn.
- **`identity`** — the four `failure_class` values and their levels; an
  on-demand mint record carries the caller's `request_id`; a startup mint record
  carries none.
- **`catalog`** — the skipped-reviewer warning fires with no nil guard.
- **`cache`** — the renamed keys.

### What the gate deliberately does not do

It does not infer that an in-scope `context.Context` requires a context-aware
method; does not infer level from an error value, a status, or syntax; does not
count Info records or judge milestone overlap; does not perform data-flow
analysis; does not validate, bound, or deduplicate attributes at runtime; does
not introduce a logger type or change duplicate-key behaviour. The
consequence-based level rule and the semantic Info budget stay review
obligations, supported by the scenario tests above.

## Behaviour changes

Thirteen, all deliberate.

**1. Every copilotd record gains `component`.** Records emitted inside
dependencies — including net/http's, through the `ErrorLog` bridge — keep none.

**2. `/readyz` access samples drop from Info to Debug.** Both probes are now
Debug, including a not-ready `503`. `/healthz` is unchanged.

**3. Access rises from Info to Warn on `5xx`.** Abnormal SSE outcomes were
already Warn; `terminal_reason=error` is newly Warn through access.

**4. `upstream response correlation` drops from Info to Debug,** and
`upstream_request_id` now rides on every later response-path record, including
access, where it was previously unavailable.

**5. `websocket session` disappears.** Its `terminal_reason`, `close_code`,
`msgs_c2u`, `msgs_u2c`, `bytes_c2u`, and `bytes_u2c` appear on access; its
`duration` is dropped in favour of access's whole-request `duration`.

**6. `websocket established` loses `method`, `route`, `bytes`, `ws`, and
`duration`,** keeps `status=101`, and gains `handshake_duration`. `route`,
`ws`, `surface`, and `request_id` arrive through scope.

**7. `route` becomes `inbound`, and the `unmatched` sentinel disappears** from
both `internal/server` and `internal/wsforward`. Absence is the unmatched state.

**8. A mux-answered redirect no longer reports a binding.** A cleaned-path `307`
that our handler never served currently logs the pattern it would have matched;
it now omits `inbound` and `surface`.

**9. Access gains `surface`,** which no record emits today.

**10. Cached-value records rename `version` to `cached_value_version` and the
startup summary renames `source` to `cached_value_source`,** removing two live
duplicate-key renderings.

**11. The four mint-failure messages become one message plus `failure_class`.**
The unclassified branch rises from Warn to Error; the transient branch splits
Debug (startup attempt) / Warn (on-demand). `status` is omitted when zero.

**12. Mint records join request scope.** An on-demand mint record now carries
the `request_id` of the request that triggered the exchange.

**13. `panic recovered` gains `inbound`, `surface`, and `ws`.**

No wire behaviour changes: no status, body, or header is affected, and the
divergence ledger is untouched. Log **destination**, **format default**, and the
`--log-level`/`--log-format`/`--log-file` surface are unchanged.

## Testing

Beyond the focused tests above, the sweep updates existing suites rather than
inventing a parallel harness:

- **Constructor call sites.** Required positional injection reaches roughly a
  hundred test call sites: 35 `Pump(`, 22 `NewManager(`, 21 `cache.New(`, 10
  `forward.New(`, 6 `Rendering{`, plus the `server`, `wsforward`, `upstream`,
  `impersonation`, and `NewModelsCache` constructors. Most are one-argument
  edits; the 28 `Logger:` struct-field initialisations — 21 on
  `identity.ManagerConfig`, 6 on `identity.DeviceFlowConfig`, 1 on `sse.Policy`
  — move to argument position. All six test `catalog.Rendering{...}` literals
  omit the logger today, relying on the nil guard; each must now supply one.
- **Assertion updates.** The suites that assert emitted output —
  `cmd/copilotd/upstream_request_id_logging_test.go`,
  `internal/server/server_test.go`,
  `internal/server/websocket_telemetry_integration_test.go`,
  `internal/wsforward/telemetry_test.go`, `internal/identity/manager_test.go`,
  `internal/cache/value_test.go`, `internal/logging/logging_test.go` — track the
  renames, the removed record, and the level changes.
- **Compile-time coverage.** The omitted `catalog.Rendering.Logger` at
  `internal/server/handler.go:59` is not a test case: after the conversion, the
  code that omits it does not build. That is the point.

## Migration order

Each step compiles and passes the full suite before the next begins.

1. **`internal/logging` foundation.** The 47-constant registry, `ForComponent`,
   `With`, the handler's scope rendering, `DependencyErrorLog`, the package doc
   and the `contextHandler` comment correction, and the composition tests. No
   call site converts. Nothing else can start without it.
2. **The composition root split.** `logging.New`'s result becomes the base and
   `slog.SetDefault` keeps it; `runServe` and `runLogin` derive a
   `cmd/copilotd` child for their own records (with registry keys and the two
   cache renames); `runBoundServe`, `buildServeProvider`, and
   `configuredCodexModels` rename their `logger` parameter to `base` — same
   arity, new meaning — and decorate inline at every sink they construct;
   `runBoundServe` derives a second `cmd/copilotd` child for `runServeStartup`;
   `upstream.New`, `wsforward.New`, and `server.New` — already positional — take
   decorated children; the `ErrorLog` bridge moves onto `DependencyErrorLog`.
   The thirteen test call sites of the three renamed functions already pass an
   undecorated logger and need no edit.
3. **Required injection: `cache`, `sse`, `forward`, `catalog`.** Two
   `WithLogger` options, `sse.Policy.Logger`, `catalog.Rendering.Logger` and its
   nil guard, three `slog.Default()` fallbacks; `server.New` gains the catalog
   logger it forwards; the cache key rename; the three stale comments in these
   packages.
4. **Required injection and levels: `identity`.** Both `Logger` fields, both
   `slog.Default()` fallbacks, `logMint`'s context parameter and its collapse
   onto `failure_class`, the level mapping, the five stale comments.
5. **Matched scope and the access record.** `logging.With` scope from the typed
   registration wrappers, the server-owned handoff, the probe classification,
   both sentinels' removal in `internal/server`, the hardcoded `ws` comparison's
   removal, the access field list and level rule, `recoverMW`'s scope, the
   `middleware.go` and `handler.go` comment rewrites.
6. **The upstream correlation handoff.** `Correlate` returns and publishes;
   `Do` and `Buffered` return the response-path context; `catalog.Source`
   follows; the four transports adopt it; access prefers it; the record demotes
   to Debug.
7. **WebSocket terminal facts onto access.** The `wsforward` result holder,
   `websocket session`'s deletion, `websocket established`'s slimming, access's
   six conditional fields and the `terminal_reason=error` Warn, the
   `wsforward` sentinel's removal.
8. **The gate.** The AST test with its closed inventory, plus the focused
   behavior tests that source analysis cannot replace.

After step 2, steps 3, 4, and 5 are independent of one another. Step 6 needs 3
(it changes `catalog.Handler`'s upstream call) and 5 (access's context
preference). Step 7 needs 5 and 6. Step 8 needs everything.

### Decomposition and atomicity

[#147](https://github.com/ningw42/copilotd/issues/147) ruled that the conversion
and its gate land **atomically in one sweep**, with no baseline allow-list and no
temporary exemption, and permitted decomposition only if no merged production
state claims the rule while violating the gate.

The eight steps above are decomposed into eight `ready-for-agent` issues, wired
with native blocker dependencies in that order. **The decomposition splits the
work, not the merge.** Every issue lands on one shared sweep branch; each is
required to leave that branch building and green; and the branch reaches
`master` as a single commit, the way the nine tickets of the upstream-call
concentration (#123–#131) landed as `aadbc75`. Step 8 is last, so the commit
that enables the gate is the same commit that finishes the conversion.

That satisfies the constraint literally and not merely in spirit: `master` has
exactly one state in which ADR-0015's gate exists, and in that state the tree
already complies. Intermediate states are unmerged branch states, which claim
nothing. No step is allowed to add an exemption, an allow-list, or a skipped
assertion to buy itself an earlier landing; a step that cannot keep the branch
green is a signal to re-cut the step, not to weaken step 8.

## CONTEXT.md changes

None. **Component** was added by commit `93d0fa2` with the owner / caller /
fact-origin distinction and the external-dependency boundary already stated, and
[#149](https://github.com/ningw42/copilotd/issues/149) explicitly warns against a
second glossary entry. Nothing else this design introduces is project domain
language: `inbound`, `surface`, `ws`, and `failure_class` are logging vocabulary,
declared once in the registry and explained in ADR-0015. The glossary's existing
reservation of **Route** for an exact upstream path is what forces `inbound` to
exist at all, and that entry needs no change.

## Alternatives considered

**A `Loggers` struct passed to each constructor,** instead of growing parameter
lists. Rejected: a struct field can be omitted, which is the exact failure —
`internal/server/handler.go:59` — that required positional injection exists to
turn into a compile error.

**Letting `internal/server` decorate the catalog logger** with its own
`ForComponent` call, rather than forwarding one from the root. Rejected: it
would put the component-free base inside `internal/server`, and "decoration
happens only at the composition root" is what makes the closed inventory
syntactically checkable and the ownership rule reviewable in one place.

**Keeping `component=internal/server` on net/http's `ErrorLog` records,**
arguing that copilotd owns the bridge. Rejected: the records are unadapted —
copilotd routes them, it does not write them — and ADR-0015 chose a
component-free base precisely so a dependency's record keeps no Component rather
than a false one.

**Reading `r.Pattern` in the access middleware** instead of publishing scope
from the registration wrapper. Rejected on three counts: it cannot carry
`surface` or `ws` without re-deriving them from strings; it labels mux-answered
redirects the handler never served; and it leaves the value's provenance
implicit at the site that must prove no client-controlled value enters a record.

**A single server-owned per-request record builder** that every package writes
its terminal facts into, replacing the five handoffs. Rejected: that is the
mutable attribute bag #146 forbids, and it would make one package the owner of
every other's vocabulary.

**A shared generic holder** — one `Holder[T]` the five packages instantiate,
instead of five copies of the store-once/read-once trio. This is a different
proposal from the record builder above: it keeps package ownership and
immutability, and is rejected for narrower reasons. First, the values are not
logging's to own: `forward.StreamResult` carries `Surface` and `Outcome` into
`ObserveStreamOutcome` at `middleware.go:66-67`, a **metric** observer, and
`wsforward`'s terminal is observed at `proxy.go:229`, so `internal/logging` —
whose job is record structure — is the wrong home, and no neutral one exists
yet. Second, two of the five are working code ADR-0015 does not touch
(`forward/stream_result.go`, `catalog/shape_result.go`); refactoring them
belongs outside a sweep #147 forces to land as one commit. The duplication is
real, but each copy is mechanical and already differs where its owner's
vocabulary does — `StoreCatalogShape` rejects a value outside its two-constant
enum (`shape_result.go:44-46`), `StoreStreamResult` validates nothing — which a
shared mechanism would have to accommodate anyway. If a sixth appears,
concentrating them in a neutral package is a clean follow-up.

**Threading a `retryFollows` flag into the singleflight closure** so `logMint`
could distinguish "transient, will retry" from "transient, exhausted" directly.
Rejected as unnecessary: `trigger` already carries the distinction the level
rule needs, and exhaustion is reported by the record that owns it,
`manager.go:260`.

**Merging `manager.go:257` into `logMint`'s transient record,** moving
`attempt`/`attempts` into the classified outcome. Rejected as scope creep: the
two records own different facts — one the classified exchange outcome, the other
the retry lifecycle — both are Debug, and #144's overlap prohibition binds
overlapping *Info* records.

**A `go/types`-based gate,** so the inventory could recognise a logger argument
by type rather than by callee and position. Rejected: `go/types` with a source
importer is slow and brittle inside `go test`, and the syntactic rules plus the
base-identifier restriction close the same gap. #147 ruled out growing the test
into semantic analysis.

**Enabling the gate first with a baseline allow-list,** converting behind it.
Rejected by #147 outright, and it would put a merged state on `master` that
claims the rule while exempting most of the tree from it.

## Related decisions

- [ADR-0015](../adr/0015-govern-log-record-structure-with-ordinary-slog.md) —
  the rule this design converts the tree to. Authoritative on every point;
  nothing here reopens it.
- [ADR-0012](../adr/0012-declare-config-settings-once-via-typed-descriptor-table.md)
  — the precedent for a closed in-repository review gate, and the owner of the
  nested config keys rule 4 excludes.
- [ADR-0013](../adr/0013-govern-authenticated-upstream-calls-in-internal-upstream.md)
  — `internal/upstream` is where the correlation seam already lives, so the
  derived response-path context is added there rather than in a transport.
- [ADR-0007](../adr/0007-served-endpoints-as-typed-contracts.md) — the typed
  contract that makes `endpoint.WSForward` the sole source of `ws`.
- [Govern copilotd's logging](https://github.com/ningw42/copilotd/issues/139) —
  the map, and its nine closed decisions:
  [#140](https://github.com/ningw42/copilotd/issues/140),
  [#141](https://github.com/ningw42/copilotd/issues/141),
  [#142](https://github.com/ningw42/copilotd/issues/142),
  [#143](https://github.com/ningw42/copilotd/issues/143),
  [#144](https://github.com/ningw42/copilotd/issues/144),
  [#145](https://github.com/ningw42/copilotd/issues/145),
  [#146](https://github.com/ningw42/copilotd/issues/146),
  [#147](https://github.com/ningw42/copilotd/issues/147),
  [#150](https://github.com/ningw42/copilotd/issues/150).
- [Make shim hook execution observable](https://github.com/ningw42/copilotd/issues/138)
  — owns the wedged-request signal this design deliberately does not add.
