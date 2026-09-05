# Concentrate SSE data-payload interpretation and replacement

Status: selected for implementation.

Implements Candidate 2 from the 2026-09-04 architecture review, narrowed to a
small on-demand interface in `internal/sse`. The integration and review baseline
is `dafb19b86a634d76568de2c0f922b8a64ffdab68` (after the Catalog shape concentration
and upstream failure-path correlation fixes). The work uses the single branch
`refactor/sse-data-payload`.

## Problem

The SSE reader's `data.type` fallback and the Responses item-id stabilizer each
interpret `data:` fields independently. The stabilizer additionally owns field
positions and reconstruction of the surrounding SSE bytes. This splits knowledge
of SSE wire grammar between `internal/sse` and `internal/shim`.

The reader and stabilizer are already two consumers of this interpretation; a
second shipped shim is not required to justify concentrating it. This is a
behavior-preserving ownership change, not a newly demonstrated production bug.

## Interface

Add two stateless operations on `sse.Frame`:

```go
func (f Frame) Data() (payload []byte, present bool)
func (f Frame) WithData(payload []byte) (Frame, bool)
```

`Data` extracts the joined data payload from the current `Raw`. The presence flag
distinguishes an absent data field from a present empty field. Returned payload
bytes are independent of `Raw`, so modifying them cannot alter the input frame.

`WithData` returns a frame with replaced data-field values, preserving `Type` and
all non-data bytes. Its boolean reports whether replacement was possible. No data
fields, or an unterminated final data field that would need expansion, produce the
original frame and `false`. A byte-identical payload produces the original frame
and `true`, retaining the original `Raw` allocation. Neither operation mutates the
input frame or the supplied payload.

Both operations use one private parser. The reader's minimal `data.type` fallback
uses that same parser. Field positions and reconstruction stay private to `sse`;
there is no exported field-layout type, cached payload, or new `Frame` field.

Stateless operations deliberately favor a small interface over a parsed-view
lifetime contract. A changed stabilizer frame is parsed again by `WithData`;
unchanged frames need only `Data`. This avoids stale derived state when an earlier
shim replaces `Raw`, including frames constructed directly by a shim rather than
by `Reader`.

## Wire contract

Preserve the existing interpretation and replacement behavior:

- Recognize the existing `data:` spelling, remove at most one ASCII space after
  its colon, and join repeated field values with `\n`.
- Preserve LF and CRLF line endings. Do not add support for bare-CR framing or
  colonless fields in this refactor.
- Keep comments, event names, `id:`, `retry:`, unknown fields, data-field prefixes,
  and the terminating blank line byte-verbatim.
- Assign replacement logical lines to the existing data fields in order. Keep
  surplus original fields as empty fields rather than deleting them. Consequently
  the re-extracted payload can contain trailing joining newlines beyond those in
  the supplied payload; this is the existing framing-preserving behavior, not a
  promise of arbitrary payload byte equality.
- When more replacement lines are needed, expand the last data field using its
  own prefix and line ending. If that field has no line ending, decline the
  replacement without modifying the frame.
- Preserve `Frame.Type` unchanged. This is not a new event-classification step.

The Responses item-id stabilizer retains all JSON interpretation, genuine-id
pinning, per-turn reset, and uncertainty policy. It extracts data, runs its existing
rewrite, and uses `WithData` only when the rewrite changed bytes. A failed
replacement declines by passthrough. Its WebSocket Message adapter is unchanged.

`Frame.Raw` remains authoritative. The nil-transformer/event-line fast path gains
no eager payload extraction or JSON decoding. Event-line precedence, minimal
`data.type` fallback, fallback counting, terminal detection, and stream lifecycle
are unchanged.

## Agreed test seams

The user confirmed tests at the public SSE payload interface and retention of the
existing reader, shim, and HTTP/WebSocket integration regressions.

Use red-green slices at the new interface to cover:

- Data extraction with no, empty, repeated, LF, and CRLF fields, including exact
  optional-space handling and independent returned bytes.
- Replacement with literal expected wire bytes: unchanged payload, fewer and more
  logical lines, differing prefixes/endings, and unchanged non-data metadata.
- Declined replacement on data-less or unexpandable input.
- Chained replacements reading the latest `Raw`, without mutating previous frames.

Keep the reader's multiline fallback and pump corpus tests, the stabilizer's
framing-preservation and decline-by-passthrough tests, and transport-level gate,
scope, and multi-turn regressions. Adapt the one shim test that uses its private
payload parser to the public SSE interface; do not delete integration assertions
merely because framing now has one owner. Supplement literal examples with
bounded fuzz checks for no-op preservation and input immutability if useful.

Verification uses the Nix development shell: focused tests and Go vet during
implementation, the race-enabled full suite at completion, `nix fmt`, and
`nix flake check`. Both review rounds compare against the integration commit.

## Scope and architectural decisions

No configuration, dependencies, endpoint changes, new divergence, JSON-policy
changes, or `FrameTransformer` signature changes. No attempt to consolidate SSE
frames with WebSocket Messages. No unrelated architecture-review candidates.

This preserves [ADR-0002](../adr/0002-frame-aware-payload-opaque-sse-engine.md),
[ADR-0011](../adr/0011-responses-item-id-stabilization-policy.md), and
[ADR-0014](../adr/0014-infallible-post-commit-shim-hooks.md). SSE owns wire mechanics;
the Shim owns its Alteration policy. No new architectural decision is required.
