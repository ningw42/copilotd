# Govern log record structure with ordinary `log/slog`

**Status:** accepted

copilotd writes every log record — whole binary, startup and `copilotd login`
included — with ordinary `log/slog` through an explicit `*slog.Logger` receiver:
no copilotd logger type, no logging method of its own, no handler-side level or
vocabulary policy, no runtime validation, no per-Component filtering. Four
structural obligations govern a record; message text is not one of them.

- **Component.** `internal/logging` builds one Component-free base carrying the
  shared handler, `service`, and build `version`, installed as `slog.Default()`
  so an unadapted dependency record keeps no Component rather than a false one.
  Every copilotd record goes through a `logging.ForComponent(base, "<owner>")`
  child: `component` is the repository-relative path of the package owning the
  emission — never its caller, beneficiary, or a fact's origin (`CONTEXT.md`).
  Shared infrastructure keeps its own identity, and access is `internal/server`
  even when it carries others' facts. Every logger-bearing boundary takes its
  `*slog.Logger` as a **required positional argument** — no config field,
  option, nil-tolerant default, or nil guard — so omission fails to compile.
- **Scope.** A fact true for a stretch of work is added with
  `logging.With(ctx, attrs...)`: an immutable, append-only derived context with
  no removal, override, or deduplication. A fact learned later earns a later
  child context — a differing `upstream_request_id` joins one later
  response-path records use, alongside and never replacing `request_id`.
  Request scope carries `request_id`, `surface`, `inbound`, and `ws`. Logging
  validates and bounds nothing; the adding site owns selection, provenance, and
  size. Participation is a call-site decision: an in-scope `context.Context`
  obliges nothing.
- **Level** follows operational consequence, not an error value or status class:
  Debug for detail and poll or retry samples, Info for a distinct normal
  operator milestone, Warn for a contained abnormality, Error when the command
  or process stopped, an invariant broke, or an operator must act. A failed
  request is not automatically Error. The Info budget is semantic, not numeric:
  distinct milestones may each be Info, but two layers may not both report one.
- **Keys.** Every application-supplied top-level key is an exported constant in
  one plain registry in `internal/logging`, referenced at logger-attached,
  record-local, and context-carried sites; emitted names stay `snake_case`. The
  registry is a declaration — no allow-list, descriptor table, ownership
  metadata, typed constructors, or validation — so collisions render under
  native slog semantics. `inbound` is the matched `net/http.ServeMux` pattern;
  `route` stays reserved for an exact upstream Route. A distinction that matters
  is an attribute (`failure_class`, `trigger`), never a message spelling.

Access is the sole terminal request summary: one record per returning handler,
absorbing SSE, catalog, and WebSocket terminal facts, `websocket established`
staying distinct. `text` stays the unconditional default format for both
commands, `json` only on explicit configuration, never inferred from the
environment.

**Message text stays ungoverned.** Naming the event in a short lower-case phrase
and leaving the particulars to attributes is a suggestion, not a rule.

The structural half rides a standard-library Go AST test on the existing
`go test` CI step — no lint binary, `go vet` step, pre-commit hook, or added
tool. It bans production `slog.Default()` and package-level emission, pairs
every logger sink with its required `logging.ForComponent` child in a closed
inventory, requires every top-level key to resolve to a registry constant, and
lets only typed `endpoint.WSForward` registration set `ws`. Focused behavior
tests pin what source analysis cannot: immutable composition, scope, late
correlation, ownership, and the terminal summary. Nothing semantic is inferred:
not level, not Info counts, not method choice from an in-scope context.

## Why

Structure is what an operator queries and what drifts silently; message text is
what a human reads and improves one line at a time. Today it is folklore: 33
production emissions across 10 files invent their own keys, none names its
package, five default-logger fallbacks hide an absent logger, and level is
habit. Each obligation therefore sits where its fact already lives: ownership at
construction, scope at the boundary that learns it, level at the site that knows
the consequence.

## Considered options

- **Convention and review only:** rejected — the current tree is what convention
  produced, and an omission is invisible in a diff.
- **A copilotd logger type with its own methods:** rejected — it moves each
  decision away from the site that owns it, and a plain constant registry left
  it nothing to enforce.
- **Derive `component` from the record's caller,** in the handler or at package
  init: rejected on simplicity — three lines against forty-odd, and neither
  variant can say `cmd/copilotd`: the linker writes `main` for any
  `package main`.
- **A typed descriptor table for attributes,** as
  [ADR-0012](0012-declare-config-settings-once-via-typed-descriptor-table.md)
  built for config: rejected — a config row earns its keep by deriving default,
  parsing, precedence, redaction, and validation at once; logging has no such
  engine.
- **Ordinary slog under four obligations, with a source-level gate** (chosen):
  the compiler holds logger injection, the AST test the rest of the structure,
  and everything semantic stays a review obligation backed by tests.

## Consequences

- Every optional injection disappears: four exported `Logger` fields, two
  `WithLogger` options, five `slog.Default()` fallbacks, and
  `internal/catalog`'s `rendering.Logger != nil` guard. Go cannot forbid an
  explicitly passed nil; it now forbids omission.
- The Component clause is not self-enforcing, so that closed inventory is its
  review gate, in ADR-0012's spirit: a new sink or a changed owner is an
  explicit test edit.
- Access is emitted after its handler returns, so a wedged request still
  produces no access record; this rule adds no start line, watchdog, or timeout.
  That signal remains issue #138's.
- Still out of scope: metrics, tracing, and `pprof`; per-Component level
  filtering; a second redaction mechanism, since ADR-0012's `LogValue` already
  keeps config secrets out; and adapting third-party records to synthesize a
  Component.
- The tree does not comply today; the conversion is designed separately and
  lands atomically with its gate, so no merged state claims a rule it violates.

The decisions sit on issue #139 and its closed children; the conversion of
existing call sites is designed under #149.
