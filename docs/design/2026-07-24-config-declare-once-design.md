# Config: declare each setting once — Design

Status: approved design (via brainstorming + grilling), pending implementation
plan; mechanism recorded in ADR-0012
Date: 2026-07-24
Review reference: architecture review candidate 02, "Declare each config setting once"
Affected: `internal/config/config.go`, `internal/config/config_test.go`,
`cmd/copilotd/main.go`; plus `internal/server/{server,handler}.go`,
`internal/catalog`, and the `server`/`catalog` tests for the Codex reshape (§7.1)

## 1. Goal & outcome

Collapse the per-setting duplication in `internal/config/config.go` so that each
configuration setting is **declared in one place**. A single typed descriptor per
setting drives all of: default value, flag registration, env/TOML overlay and
parse, precedence, `LogValue` redaction, and validation. Adding or changing a
setting becomes a one-row edit instead of an eight-site edit kept in agreement
only by discipline.

The refactor is **runtime behavior-preserving**. Precedence, resolved values,
error message content, help output, `LogValue` redaction, and the rendered Codex
catalog all stay observably the same. `config_test.go` (1569 lines) is the safety
net and stays green — unchanged — throughout the table-engine work (steps 2-5).
The one deliberate *structural* change is the Codex flatten (§7.1, step 1): the
`codex-*` settings flatten onto `ServeConfig` and the renderer receives its own
projected contract. That slice edits `config_test.go` in exactly one place — the
nested `Codex: CodexConfig{…}` literal at `config_test.go:1085-1093` flattens to
top-level fields, while every assertion string stays identical (the log keys are
unchanged) — and migrates the Codex test literals across three surfaces (§7.1,
§10). No resolved value or rendered byte changes.

## 2. Problem

Today each `serve` setting is declared across roughly eight parallel places:

1. a `defaultXxx` const,
2. the `ServeConfig` struct field,
3. the `RegisterServe` flag registration (name, default, usage),
4. the `Resolve` default-init struct literal,
5. the `Resolve` flag layer (`if set["…"] { cfg.X = *f.x }`),
6. the `overlay()` env/file parse+convert ladder,
7. the `validate()` check,
8. the `LogValue()` enumeration.

There are ~30 settings, so these are six-to-eight differently-ordered copies of
one field list. Nothing — no compiler check, no test — forces them to agree, so
they drift silently: a setting settable by flag but not env, a value dropped from
the startup log, a secret forgotten in the redaction list. The interface a
maintainer must edit is as large as the module's behavior — the file is shallow.

`login` carries a smaller copy of the same shape (`commonFlags`,
`applyFlagValues`, `overlayLogin`, `applyEnvLogin`, its own `validate`), so the
common operational settings are declared twice again across the serve/login fork.

## 3. Decisions & non-goals

Three decisions, settled during brainstorming, frame the design:

- **Ambition: full declare-once engine.** One per-field descriptor drives all
  eight aspects, including retiring the serve/login fork. (Not a partial
  consolidation, not a mechanical LogValue-only dedup.)
- **Mechanism: typed descriptor table.** A generic `field[C, T]` behind a
  type-erased `spec[C]` interface — compiler-checked, no reflection. This matches
  the package's existing explicit/pure style (it already injects `lookupEnv`
  rather than use `ff`'s native env support, to keep `Resolve` pure and
  table-testable). Reflection/struct-tags and code generation were considered and
  rejected as out of character. (Recorded as
  [ADR-0012](../adr/0012-declare-config-settings-once-via-typed-descriptor-table.md).)
- **Unknown-key errors: deferred.** This pass does not add rejection of typo'd
  TOML/env keys. A typo'd key remains a silent no-op, exactly as today. (Adding
  it later is a clean follow-up: the descriptor table already yields the
  known-key set; the TOML layer can diff against it without touching the injected
  env seam.)

Non-goals (explicit):

- **Logging/common/timeout fields keep their flat layout.** `ServeConfig` and
  `LoginConfig` keep their current flat fields for everything except Codex.
  `config.ServeConfig{LogLevel: "info", LogFormat: "text"}` is a flat literal in
  ~15 test files across `server`, `logging`, `forward`, `wsforward`, and `cmd`,
  and `logging.New` reads `cfg.LogLevel/LogFormat/LogFile` directly. Embedding a
  shared `CommonConfig` sub-struct would ripple into all of those, so the
  serve/login fork is unified at the engine level (§6), not by reshaping the
  structs.
- **Codex settings do reshape (deliberately).** `config.CodexConfig` is a
  redundant pass-through that `newHandler` already re-projects into
  `catalog.CodexDescriptor`. The five `codex-*` settings flatten onto
  `ServeConfig`, and the renderer receives its own projected contract (§7.1). This
  is the one intentional structural change.
- **No runtime behavior change.** Same precedence, resolved values, error text,
  help, redaction, and rendered Codex catalog. Internal types and ~15 Codex test
  literals change; no observable output does.
- **No new dependency.** Continue using `ff/v4` for flag registration.

## 4. Core types — the one declaration

One generic implementation, `field[C, T]`, behind a type-erased `spec[C]`
interface, so heterogeneous settings live in a single `[]spec[C]`. `C` is the
config struct being resolved (`ServeConfig` or `LoginConfig`); `T` is the
setting's value type.

```go
// spec is the type-erased face of one setting. The engine only ever sees []spec[C].
type spec[C any] interface {
    key() string                                  // canonical key, e.g. "outbound-timeout"
    register(fs *ff.FlagSet)                       // ff typed flag; captures storage + default
    applyDefault(*C)                               // write the declared default
    applyOverlay(*C, raw, source string) error     // file/env: parse + store; source names the layer
    applyFlag(*C, set map[string]bool)             // copy ff-parsed value iff --key was set
    logAttr(*C) (slog.Attr, bool)                  // (attr, include?) — secret ⇒ include=false
    validate(*C) error                             // run the field's check
}

// field is the single generic implementation, one instance per setting.
type field[C, T any] struct {
    name, usage string
    def         T
    get         func(*C) *T             // accessor into the resolved config value
    parse       func(string) (T, error) // string → T for the file/env layers
    reg         func(*ff.FlagSet, string, T, string) *T // the right ff typed registrar
    logf        func(string, T) slog.Attr             // slog.Duration/String/Int64/Int/Bool by key
    check       func(key string, v T) error // nil ⇒ no validation; key passed so the message matches today's
    secret      bool                    // true ⇒ omitted from LogValue (redaction by construction)
    stored      *T                      // ff storage pointer, captured at register()
}
```

The constructors return a `*field[C, T]` (satisfying `spec[C]` with pointer
receivers) so `register()` can record `stored` for the flag layer. `check` takes
the `key` because the current validation messages embed it (and, for `required`,
the derived env-var name), so validators reconstruct today's text exactly.

Rows never spell all that out. A small set of **typed constructors** wire
`parse`/`reg`/`logf`/`def` per Go type, so each setting is one line:

```go
func durationField[C any](name string, def time.Duration, get func(*C)*time.Duration, check func(time.Duration) error, usage string) spec[C]
func stringField[C any](name, def string, get func(*C)*string, check func(string) error, usage string) spec[C]        // parse=identity, logf=slog.String
func int64Field[C any](name string, def int64, get func(*C)*int64, check func(int64) error, usage string) spec[C]       // parse=ParseInt, logf=slog.Int64
func intField[C any](name string, def int, get func(*C)*int, check func(int) error, usage string) spec[C]               // parse=Atoi,     logf=slog.Int
func boolField[C any](name string, def bool, get func(*C)*bool, usage string) spec[C]                                   // reg=BoolLongDefault, logf=slog.Bool, no check
func secretStringField[C any](name string, get func(*C)*string, check func(string) error, usage string) spec[C]         // stringField with secret:true, def ""
```

Shared validators are named once. Each takes the setting's `key` so it can
reproduce today's exact message text:

```go
func positive[T ~int64 | ~int](key string, v T) error   { /* "invalid <key> <v>: must be positive" */ }
func nonNegative[T ~int64 | ~int](key string, v T) error { /* "invalid <key> <v>: must be >= 0" */ }
func oneOf(allowed []string) func(key, v string) error   { /* log-level, log-format */ }
func required(key, v string) error                        { /* "<key> is required: set --<key>, <ENV>, or <key> in the config file" */ }
func validAddr(key, v string) error                       { /* host:port + port range */ }
func bareVersion(key, v string) error                     { /* impersonation.IsBareVersion */ }
```

`time.Duration` satisfies `~int64`, so `positive`/`nonNegative` cover duration,
`int64`, and `int` settings with one definition each. The engine calls
`check(f.name, *f.get(c))` during the validate pass.

The serve table is then the single source of truth. Illustrative rows (the
timeout group):

```go
func serveSpecific() []spec[ServeConfig] {
    return []spec[ServeConfig]{
        stringField("addr", defaultAddr, func(c *ServeConfig) *string { return &c.Addr }, validAddr, "bind address (host:port)"),
        durationField("shutdown-timeout", defaultShutdownTimeout, func(c *ServeConfig) *time.Duration { return &c.ShutdownTimeout }, positive, "graceful shutdown grace period"),
        secretStringField("apikey", func(c *ServeConfig) *string { return &c.APIKey }, required, "required inbound API key clients must present (secret)"),
        durationField("outbound-timeout", defaultOutboundTimeout, func(c *ServeConfig) *time.Duration { return &c.OutboundTimeout }, positive, "buffered upstream response timeout"),
        // ...one row per remaining serve setting, in registration order
    }
}
```

## 5. How each aspect derives from a row

| Aspect (today's parallel list) | Derived from the row |
|---|---|
| `defaultXxx` const | `def` (consts may remain as named values the row references) |
| `ServeConfig` struct field | **stays** — it is the output; `get` points at it |
| `RegisterServe` flag registration | `register()` calls `reg(fs, name, def, usage)`, saves `stored` |
| `Resolve` default-init literal | engine calls `applyDefault` across the table |
| `overlay()` env/file parse ladder | `applyOverlay` = `parse(raw)` then write via `get`; engine loops |
| `Resolve` flag layer `if set[…]` | `applyFlag` copies `*stored → *get(c)` when the flag was set |
| `LogValue()` enumeration | engine loops table, emits `logAttr`; `secret` rows omitted |
| `validate()` check | engine loops `validate`; a nil `check` is skipped |

The engine is one small generic function. Precedence (flags > env > file >
default) is written **once**, here, not re-encoded per field:

```go
func resolve[C any](specs []spec[C], fs *ff.FlagSet, target *C, path string, env func(string) (string, bool), finalize func(*C) error) error {
    for _, s := range specs {
        s.applyDefault(target)
    }
    if path != "" {
        vals, err := parseTOMLKeys(path) // map[string]string, existing fftoml.Parse
        if err != nil {
            return err
        }
        for _, s := range specs {
            if raw, ok := vals[s.key()]; ok {
                if err := s.applyOverlay(target, raw, "config file"); err != nil {
                    return err
                }
            }
        }
    }
    for _, s := range specs {
        if raw, ok := env(envVarName(s.key())); ok {
            if err := s.applyOverlay(target, raw, "env"); err != nil {
                return err
            }
        }
    }
    set := setFlags(fs)
    for _, s := range specs {
        s.applyFlag(target, set)
    }
    if finalize != nil { // §7.2.2: the one two-phase field parses+validates here
        if err := finalize(target); err != nil {
            return err
        }
    }
    for _, s := range specs {
        if err := s.validate(target); err != nil {
            return err
        }
    }
    return nil
}
```

The full order — `default → file → env → flag → finalize → validate` — is owned
here, in one place, not re-encoded per caller; `finalize` is `nil` for every
setting except `codex-auto-review-model-overrides` (§7.2.2), and login passes
`nil`.

The `[]spec[C]` table is built and `register()`'d **once**, in `RegisterServe`/
`RegisterLogin` — where each `register(fs)` both declares the ff flag and records
`stored` — and is held on the `*ServeFlags`/`*LoginFlags` handle. `ff.Parse` (in
`main`, before `Resolve`) fills those `stored` pointers. `ServeFlags.Resolve` and
`LoginFlags.Resolve` are then thin wrappers that **reuse** the already-built
table: they resolve the config path, call `resolve` over the held `[]spec[C]`
(passing the serve overrides `finalize`, or `nil` for login), and return the
typed config. Building the table inside `Resolve` instead would manufacture fresh
specs whose `stored` was never bound by `register()`/`ff.Parse`, silently dropping
every flag override. `LogValue` becomes a loop over the same table into
`slog.GroupValue`.

## 6. serve/login unification

The fork retires at the **engine** level, without reshaping the shared fields
(contrast the deliberate Codex flattening in §7.1). The five shared operational
settings are declared once as metadata; each command supplies one-line accessors
into its own flat struct:

```go
type commonTargets[C any] struct {
    logLevel, logFormat, logFile, oauthTokenFile func(*C) *string
}

// commonFields returns the five shared rows in the help-mandated order:
// log-level, log-format, log-file, config, github-oauth-token-file.
func commonFields[C any](t commonTargets[C]) []spec[C] { /* ... */ }

func serveSpecs() []spec[ServeConfig] { return append(commonFields(serveTargets), serveSpecific()...) }
func loginSpecs() []spec[LoginConfig] { return append(commonFields(loginTargets), loginSpecific()...) }
```

This deletes `commonFlags`, `applyFlagValues`, `overlayLogin`, and
`applyEnvLogin`; the fork becomes two `append`s over shared metadata plus
per-command accessors.

## 7. The Codex reshape and remaining carve-outs

### 7.1 Codex: symmetric config + scoped renderer contract

Today `config.CodexConfig` does double duty: it groups the `codex-*` settings on
the config side **and** is threaded to the catalog handler — where `newHandler`
(`handler.go:53-61`) immediately re-projects it into `catalog.CodexDescriptor`
(gate + `Models` + `CodexRenderConfig`). It is a redundant carrier, and it forces
an asymmetry: `codex-catalog-refresh-interval` is a flat `ServeConfig` field while
its four siblings nest under `Codex`.

Resolution — decouple the two concerns:

- **Config side flattens.** Delete `config.CodexConfig`; the five `codex-*`
  settings become top-level `ServeConfig` siblings, matching the already-flat
  refresh interval and the existing flat `shim-*` settings:

  ```go
  CodexCatalogEnabled           bool
  CodexAutoReviewModel          string
  CodexAutoReviewModelOverrides map[string]string
  CodexOverrideLimits           bool
  CodexCatalogRefreshInterval   time.Duration   // already flat today
  ```

  Every `codex-*` key now maps 1:1 to a flat field — uniform declare-once rows,
  no nested accessors.

- **The renderer gets a projected contract.** The composition root builds
  `catalog.CodexDescriptor` from the resolved config, selecting only what the
  renderer needs; the refresh interval branches to the cache wiring as today:

  ```go
  codexDesc := catalog.CodexDescriptor{
      Enabled: cfg.CodexCatalogEnabled,
      Models:  codexModels,                        // runtime cache value
      RenderConfig: catalog.CodexRenderConfig{
          AutoReviewModel:          cfg.CodexAutoReviewModel,
          AutoReviewModelOverrides: cfg.CodexAutoReviewModelOverrides,
          OverrideLimits:           cfg.CodexOverrideLimits,
      },
  }
  // codexModels is produced by configuredCodexModels(cfg, ...), which reads
  // cfg.CodexCatalogEnabled (gate) + cfg.CodexCatalogRefreshInterval (cadence);
  // the refresh interval feeds the cache only, never the renderer.
  ```

  `server.New` and `newHandler` take this `catalog.CodexDescriptor`
  **positionally**, replacing the current `config.CodexConfig` parameter *and* the
  functional-option variadic:

  ```go
  func New(cfg config.ServeConfig, logger *slog.Logger, provider identity.Provider,
           observers ReadyObservers, fwd *forward.Forwarder, wsProxy *wsforward.Proxy,
           streamOutcomes StreamOutcomeObserver, codex catalog.CodexDescriptor) *Server

  func newHandler(apikey string, provider identity.Provider, observers ReadyObservers,
                  fwd *forward.Forwarder, logger *slog.Logger, streamOutcomes StreamOutcomeObserver,
                  codex catalog.CodexDescriptor, wsProxy *wsforward.Proxy) http.Handler
  ```

  This **deletes the entire functional-option apparatus at both layers** — the
  server-level `serverOptions`/`Option`/`WithCodexModels` and the handler-level
  `handlerOptions`/`handlerOption`/`withCodexModels`, plus `newHandler`'s in-body
  descriptor construction — since each existed only to thread the `codexModels`
  cache value, which now rides in `descriptor.Models`. The now-unused imports drop
  with them: `internal/cache` from `server.go` and `handler.go`, and
  `internal/config` from `handler.go`. `server.New` keeps taking the whole `cfg`
  (for `APIKey` and `ShutdownTimeout`) but reads **no** `codex-*` field from it —
  the descriptor is the sole codex carrier across the render seam. A one-line
  invariant comment on `server.New`/`newHandler` records this (codex-* is read only
  via the descriptor, never from `cfg`), so a later edit can't silently reintroduce
  a second source. No `config.*` type crosses the render seam, and the refresh
  interval is never threaded into the renderer. This unifies the two Codex-wiring
  paths (gate/render config from config, `Models` from an option) into one
  descriptor built in one place.

Cost: the `server.New`/`newHandler` signature change ripples to the test sites
that build the old Codex value, across **three** surfaces (not a single rename):
(1) descriptor literals at the render seam (`server`/`catalog` tests build
`catalog.CodexDescriptor{...}` — arguably a fix, since server tests should build a
catalog type, not reach into `config`); (2) flattened `cfg` field-sets
(`cmd/copilotd/main_test.go`, `serve_e2e_test.go`, and
`catalog_openai_integration_test.go`, including its
`newStack(codex config.CodexConfig)` helper parameter); and (3) the `main.go` call
site (`server.WithCodexModels(...)` → the positional descriptor). `config_test.go`
changes in exactly **one** place — the nested `Codex: CodexConfig{…}` literal at
`:1085-1093` flattens to top-level fields — with every assertion string unchanged;
it is otherwise untouched by this slice.

### 7.2 Remaining carve-outs

Two settings still do not fit the plain `field[C, T]` shape and stay small, named
exceptions rather than being forced into the table:

1. **`--config` (bootstrap-only).** The config-file path selects which file to
   load; it is not a `ServeConfig`/`LoginConfig` field. Model it as a
   registration-only spec that appears in-position for help ordering and exposes
   its flag pointer to the existing `resolveConfigPath` (flag > env). It no-ops
   `applyDefault`/`applyOverlay`/`logAttr`/`validate`.
2. **`codex-auto-review-model-overrides` (two-phase parse).** The winning scalar
   is captured into a raw scratch value during the overlay/flag layers, then
   parsed into `CodexAutoReviewModelOverrides` by the `finalize(cfg)` hook (§5)
   that runs after the flag layer and before validation — exactly the current
   ordering. Because the `[]spec[C]` table is built once and reused (§5), the
   scratch must be an **unexported `ServeConfig` field**
   (`autoReviewModelOverridesRaw`, as today) — not a `Resolve`-local, which a
   once-built spec would capture as one shared variable across every `Resolve`
   call. For this field parse *is* validation (a malformed pair never yields a
   map), so its per-row `check` is `nil`, the `finalize` parse is its sole
   validation point, and `validate()` never inspects the map. `LogValue` still
   renders the map via the existing `formatAutoReviewModelOverrides`. (Whether this
   hook can later be removed entirely is tracked as issue #110.)

Note — two things that look special but need no special path: the
`github-oauth-token-file` **dynamic default** is just a `def` value the row passes
(`defaultOAuthTokenFile()`), and the flattened `codex-*` fields are now ordinary
top-level rows.

## 8. Fidelity constraints (what the tests pin)

These are preserved by construction and verified by the existing suite:

- **Help order.** `TestRunGeneratedHelpMatchesCommandTree` requires the five
  shared flags to render in the order `--log-level, --log-format, --log-file,
  --config, --github-oauth-token-file`, and all shared flags to precede
  command-specific ones. Table order = registration order = `commonFields(...)`
  (fixed order) ++ command-specific, which satisfies this.
- **LogValue.** `TestConfigLogValueEmitsOnlyNonSecretFields` /
  `TestLoginConfigLogValueEmitsAllFields` assert field **presence** via
  `strings.Contains` and secret **absence** — they are order-independent. Emitting
  in table order is safe; `secret:true` rows (`apikey`, `github-oauth-token`) are
  omitted, and `--config`/`--github-oauth-token` remain unlogged.
- **Validation.** `TestLoadValidationErrors` triggers exactly one bad field per
  case and matches an error substring. Running `validate` in table order changes
  no single-field message; no test asserts multi-field validation precedence.
- **Error text.** `applyOverlay` reuses today's templates verbatim —
  `"invalid %s %q from %s: %w"` for parse failures (keyed by canonical name and
  layer) and `"invalid %s …: must be positive"` / `"… must be >= 0"` for checks —
  so `strings.Contains` assertions pass unchanged.
- **Flag parse errors** still surface at `ff` parse time (before `Resolve`),
  because `applyFlag` copies the already-parsed typed value rather than
  re-parsing a string. Behavior identical to today.

## 9. Rollout plan (incremental)

The refactor lands in slices; the full suite stays green after each step.

1. **Codex reshape (independent, land first).** Flatten `config.CodexConfig` into
   `ServeConfig`, change `server.New`/`newHandler` to take
   `catalog.CodexDescriptor` positionally, **delete the `WithCodexModels`/
   `withCodexModels` option machinery at both layers** and drop the now-unused
   imports (`internal/cache` from `server.go`/`handler.go`, `internal/config` from
   `handler.go`), and move the projection to the composition root (§7.1). Migrate
   the Codex test literals across the three surfaces (§7.1), including the single
   flattened `Codex: CodexConfig{…}` literal at `config_test.go:1085-1093`. The
   field rename also fans out inside `config.go` (defaults literal, `overlay`,
   `validate`, `LogValue`) and to `configuredCodexModels`/`logCodexCatalogStaging`
   in `main`. Self-contained at the render seam; unblocks uniform flat `codex-*`
   rows in the table.
2. **Scaffold.** Add `spec`/`field`, the typed constructors, shared validators,
   and the generic `resolve` engine alongside the existing code (initially
   unused). No behavior change.
3. **Migrate serve by group**, deleting the corresponding lines from
   `overlay`/`validate`/`LogValue`/`Resolve`/`RegisterServe` as each group moves:
   timeouts → byte caps → shim toggles → codex (incl. the overrides finalize
   hook) → impersonation → common. Full suite green after each group.
4. **Migrate login**; delete `commonFlags`, `applyFlagValues`, `overlayLogin`,
   `applyEnvLogin`.
5. **Delete the dead scaffolding**: the `ServeFlags`/`LoginFlags` pointer fields
   that are now redundant with `stored`, and the hand-written overlay/validate
   ladders.

## 10. Testing strategy

- The existing `config_test.go` is the primary safety net. Through the
  table-engine steps (2-5) it must pass **unchanged** — any diff there is a signal
  to stop and reconcile, not to edit the test. The one exception is the Codex
  flatten (step 1), which edits exactly one literal in it (the nested
  `Codex: CodexConfig{…}` at `:1085-1093` → top-level fields); every assertion
  string is unchanged.
- Add focused unit tests for the new engine primitives: a `field[C, T]` round
  trips default → file → env → flag precedence; the `finalize` hook runs after the
  flag layer and before `validate`; `secret` omits from `logAttr`; a nil `check`
  skips validation; `oneOf`/`positive`/`nonNegative`/`required` produce the
  expected messages.
- `cmd/copilotd/main_test.go` help-order and flag-presence tests must pass
  unchanged (they pin the ordering constraint in §8).
- The Codex reshape (§7.1) migrates the Codex test literals across **three**
  surfaces — render-seam descriptor literals (`server`/`catalog`), flattened `cfg`
  field-sets (`main_test.go`, `serve_e2e_test.go`,
  `catalog_openai_integration_test.go` incl. its `newStack` helper), and the
  `main.go` call site — plus the single `config_test.go` literal above. Their
  assertions (rendered bytes, gating) stay the same.

## 11. Risks & mitigations

- **Generic ergonomics.** `field[C, T]` behind `spec[C]` with per-type
  constructors is the boundary of what stays readable without reflection.
  Mitigation: keep the constructor set small (5–6) and the accessor closures
  one line each; if a type needs a bespoke parser it gets its own constructor.
- **Order coupling.** Three behaviors read table order (help, validation, log),
  but the tests pin far less than that implies: the help test fixes only the five
  common flags' internal order and "commons precede command-specific" — the order
  *among* serve-specific rows is not asserted — and the `LogValue`/validation
  tests are order-independent (§8). Mitigation: keeping registration order
  satisfies the help constraint by construction; a single ordered table stays the
  source of truth, with real headroom to reorder specifics if needed.
- **Special-case creep.** The two carve-outs in §7.2 are the known irregulars.
  Mitigation: keep them explicitly named and out of the generic path rather than
  widening `field` to absorb them.
- **Blast radius.** This is the widest-reaching candidate in the review, and the
  Codex reshape (§7.1) additionally touches `server`/`catalog`, `main`, and their
  tests — including one `config_test.go` literal (the sole change to that
  safety-net file, in step 1). Mitigation: land the Codex reshape as its own
  self-contained slice first (§9), then the group-by-group config rollout keeps
  every later commit green and small.
