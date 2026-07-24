# Declare each config setting once via a typed descriptor table

**Status:** accepted

copilotd's configuration module declares each `serve`/`login` setting **exactly
once**, as a typed descriptor — a generic `field[C, T]` behind a type-erased
`spec[C]` interface — and a single generic engine derives every per-setting
aspect from that one row: default value, `ff` flag registration, env/TOML overlay
and parse, precedence, `LogValue` redaction, and validation. Precedence
(`flags > env > file > default`) and the resolve order
(`default → file → env → flag → finalize → validate`) are written once in the
engine, not re-encoded per field. The mechanism uses **compiler-checked generics
— no reflection, no struct tags, no code generation**.

## Why

Today each setting is spread across roughly eight parallel sites — a `defaultXxx`
const, the struct field, flag registration, the default-init literal, the flag
layer, the env/file parse ladder, `validate`, and the `LogValue` enumeration —
kept in agreement only by discipline. Nothing forces them to match, so they drift
silently: a setting settable by flag but not env, a value dropped from the startup
log, a secret forgotten in the redaction list. The interface a maintainer must
edit is as large as the module's behavior. Collapsing the eight sites into one
typed row makes the compiler, not discipline, the thing that keeps them agreeing.

## Considered options

- **Reflection / struct-tags** — rejected. It is out of character for this
  package's explicit, pure style (it already injects `lookupEnv` rather than use
  `ff`'s native environment support, precisely to keep `Resolve` pure and
  table-testable), and it trades compile-time checking for run-time tag parsing.
- **Code generation** — rejected. A generator plus a generated artifact adds a
  build step and a second source of truth out of character with the hand-written,
  auditable module.
- **Partial consolidation (e.g. a `LogValue`-only dedup)** — rejected. It fixes
  one of the eight sites and leaves the other seven free to drift, so the
  drift-by-discipline problem remains.
- **Typed descriptor table** (chosen) — one generic `field[C, T]` behind
  `spec[C]`, with a small set (~6) of per-type constructors so each setting is one
  line. Compiler-checked, reflection-free, and in keeping with the package's
  existing explicit style; the engine owns precedence and ordering in one place.

## Consequences

- Adding or changing a setting is a one-row edit, and the compiler enforces that
  all of its aspects agree.
- Two settings stay out of the generic path as small, named carve-outs rather
  than widening `field`: `--config` (bootstrap-only; a registration-only spec that
  holds help ordering) and `codex-auto-review-model-overrides` (the sole
  two-phase field — a raw scalar staged across the layers and parsed once by the
  engine's `finalize` hook, before `validate`). Whether that hook can later be
  removed is tracked in issue #110.
- The engine introduces a small generic surface (`field[C, T]`, `spec[C]`, the
  per-type constructors, shared validators). That surface is the deliberate
  readability boundary chosen instead of reflection; if a setting needs a bespoke
  parser it gets its own constructor rather than a reflective escape hatch.
- The serve/login fork is retired at the engine level: the five shared operational
  settings are declared once as metadata, and each command supplies one-line
  accessors into its own flat struct.
- As the one structural change, the Codex settings flatten onto `ServeConfig` and
  the renderer receives its own projected `catalog.CodexDescriptor`, so no
  `config.*` type crosses the render seam.
- The change is runtime behavior-preserving: precedence, resolved values, error
  text, help output, `LogValue` redaction, and the rendered Codex catalog are all
  unchanged; `config_test.go` is the safety net (changed only by the single Codex
  literal in the flatten slice).

See `docs/design/2026-07-24-config-declare-once-design.md`.
