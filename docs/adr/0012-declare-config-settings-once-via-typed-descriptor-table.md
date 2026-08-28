# Declare each config setting once via a typed descriptor table

**Status:** accepted

copilotd's configuration module declares each `serve`/`login` setting **exactly
once**, as a typed descriptor — a generic `field[C, T]` behind a type-erased
`spec[C]` interface — and a single generic engine derives every per-setting
aspect from that one row: default value, `ff` flag registration, env/TOML overlay
and parse, precedence, `LogValue` redaction, and validation. Precedence
(`flags > env > file > default`) and the resolve order
(`default → file → env → flag → validate`) are written once in the engine, not
re-encoded per field. The mechanism uses **compiler-checked generics
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

- Adding or changing a setting is a one-row production edit, and the compiler
  enforces that all of its aspects agree. The exact emitted-log-key test remains
  an intentional safety review gate: a newly logged non-secret key must also be
  approved in that test's closed expected set.
- `--config` stays out of the generic value path as a small, named carve-out: it
  is bootstrap-only, so a registration-only spec holds its help position while
  selecting the TOML file before resolution. `codex-auto-review-model-overrides`
  is an ordinary map-valued descriptor: every supplied TOML, environment, and
  flag value is parsed when its layer is applied, and each valid higher layer
  replaces the complete map.
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
- Apart from that structural change, the original declare-once migration was
  runtime behavior-preserving. Follow-up issue #132 deliberately changed
  malformed reviewer-override handling: every supplied layer is parsed eagerly,
  lower-precedence errors are fatal, and TOML/environment errors identify their
  source. Valid-value precedence and resolved maps, help output, logging and
  redaction, reviewer selection, and Codex catalog rendering remain unchanged;
  focused config tests pin both the changed failure behavior and these preserved
  invariants.

See `docs/design/2026-07-24-config-declare-once-design.md` for the historical
rollout design. Its two-phase reviewer-override carve-out and finalization phase
were superseded by issues #132 and #133; this ADR states the live engine
contract.
