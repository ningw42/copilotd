# Config: declare each setting once — Design

Status: approved design (via brainstorming), pending implementation plan
Date: 2026-07-24
Review reference: architecture review candidate 02, "Declare each config setting once"
Affected: `internal/config/config.go`, `internal/config/config_test.go`,
`cmd/copilotd/main.go` (only where it constructs flag sets)

## 1. Goal & outcome

Collapse the per-setting duplication in `internal/config/config.go` so that each
configuration setting is **declared in one place**. A single typed descriptor per
setting drives all of: default value, flag registration, env/TOML overlay and
parse, precedence, `LogValue` redaction, and validation. Adding or changing a
setting becomes a one-row edit instead of an eight-site edit kept in agreement
only by discipline.

The refactor is **behavior-preserving**. Precedence, resolved values, error
message content, help output, and `LogValue` redaction all stay observably the
same. The existing test suite (`config_test.go`, 1569 lines) is the safety net
and stays green — unchanged — at every step.

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
  rejected as out of character.
- **Unknown-key errors: deferred.** This pass does not add rejection of typo'd
  TOML/env keys. A typo'd key remains a silent no-op, exactly as today. (Adding
  it later is a clean follow-up: the descriptor table already yields the
  known-key set; the TOML layer can diff against it without touching the injected
  env seam.)

Non-goals (explicit):

- **No change to the public config structs.** `ServeConfig` and `LoginConfig`
  keep their exact current flat field layout. `config.ServeConfig{LogLevel:
  "info", LogFormat: "text"}` is constructed as a flat literal in ~15 test files
  across `server`, `logging`, `forward`, `wsforward`, and `cmd`, and
  `logging.New` reads `cfg.LogLevel/LogFormat/LogFile` directly. Embedding a
  shared `CommonConfig` sub-struct would ripple into all of those; we unify at
  the engine level instead.
- **No behavior change** beyond internal structure. Same precedence, same
  resolved values, same error text, same help, same redaction.
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
func resolve[C any](specs []spec[C], fs *ff.FlagSet, target *C, path string, env func(string) (string, bool)) error {
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
    for _, s := range specs {
        if err := s.validate(target); err != nil {
            return err
        }
    }
    return nil
}
```

`ServeFlags.Resolve` and `LoginFlags.Resolve` become thin wrappers that build the
right `[]spec[C]`, run `resolve`, run any finalize hook (§7), and return the typed
config. `LogValue` becomes a loop over the same table into `slog.GroupValue`.

## 6. serve/login unification

The fork retires at the **engine** level, with the public structs untouched. The
five shared operational settings are declared once as metadata; each command
supplies one-line accessors into its own flat struct:

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

## 7. Special cases (explicit carve-outs)

Four settings do not fit the plain `field[C, T]` shape. Each stays a small,
named exception rather than being forced into the table:

1. **`--config` (bootstrap-only).** The config-file path selects which file to
   load; it is not a `ServeConfig`/`LoginConfig` field. Model it as a
   registration-only spec that appears in-position for help ordering and exposes
   its flag pointer to the existing `resolveConfigPath` (flag > env). It no-ops
   `applyDefault`/`applyOverlay`/`logAttr`/`validate`.
2. **`codex-auto-review-model-overrides` (two-phase parse).** The winning scalar
   is captured into the existing `autoReviewModelOverridesRaw` scratch field
   during overlay/flag layers, then parsed into `AutoReviewModelOverrides` by a
   `finalize(cfg)` hook that runs after the flag layer and before validation —
   exactly the current ordering. `LogValue` renders it via the existing
   `formatAutoReviewModelOverrides`.
3. **`github-oauth-token-file` dynamic default.** Its default comes from
   `defaultOAuthTokenFile()` (depends on `os.UserConfigDir()`), not a const. The
   constructors take a `def` value, so the row simply passes
   `defaultOAuthTokenFile()`.
4. **Nested `Codex` fields.** The `get` accessor reaches into the sub-struct,
   e.g. `func(c *ServeConfig) *bool { return &c.Codex.Enabled }`. No struct change
   needed; the operator-facing keys stay flat.

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

1. **Scaffold.** Add `spec`/`field`, the typed constructors, shared validators,
   and the generic `resolve` engine alongside the existing code (initially
   unused). No behavior change.
2. **Migrate serve by group**, deleting the corresponding lines from
   `overlay`/`validate`/`LogValue`/`Resolve`/`RegisterServe` as each group moves:
   timeouts → byte caps → shim toggles → codex (incl. the overrides finalize
   hook) → impersonation → common. Full suite green after each group.
3. **Migrate login**; delete `commonFlags`, `applyFlagValues`, `overlayLogin`,
   `applyEnvLogin`.
4. **Delete the dead scaffolding**: the `ServeFlags`/`LoginFlags` pointer fields
   that are now redundant with `stored`, and the hand-written overlay/validate
   ladders.

## 10. Testing strategy

- The existing `config_test.go` is the primary safety net and must pass
  unchanged throughout (behavior preservation). Any diff to it during the
  refactor is a signal to stop and reconcile, not to edit the test.
- Add focused unit tests for the new engine primitives: a `field[C, T]` round
  trips default → file → env → flag precedence; `secret` omits from `logAttr`; a
  nil `check` skips validation; `oneOf`/`positive`/`nonNegative`/`required`
  produce the expected messages.
- `cmd/copilotd/main_test.go` help-order and flag-presence tests must pass
  unchanged (they pin the ordering constraint in §8).

## 11. Risks & mitigations

- **Generic ergonomics.** `field[C, T]` behind `spec[C]` with per-type
  constructors is the boundary of what stays readable without reflection.
  Mitigation: keep the constructor set small (5–6) and the accessor closures
  one line each; if a type needs a bespoke parser it gets its own constructor.
- **Order coupling.** Three behaviors key off table order (help, validation,
  log). Mitigation: §8 shows one order (registration order) satisfies all three;
  a single ordered table is the source of truth.
- **Special-case creep.** The four carve-outs in §7 are the known irregulars.
  Mitigation: keep them explicitly named and out of the generic path rather than
  widening `field` to absorb them.
- **Blast radius.** This is the widest-reaching candidate in the review.
  Mitigation: the group-by-group rollout in §9 keeps every intermediate commit
  green and small.
