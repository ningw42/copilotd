# Guarantee a stream terminal; mark copilotd's own signals off-band

**Status:** accepted

Once a Route whose contract includes SSE semantics commits a streamed `200
text/event-stream` response, its HTTP status is locked, so a mid-stream upstream
failure can only be signalled in-band. copilotd synthesizes a native-shaped
terminal `error` event whenever that upstream stream dies without one
(truncation, stall, or read error), so a client's SSE parser never hangs waiting
for `message_stop` / `response.completed`. Every such copilotd-originated signal
keeps the provider's native wire shape and is identified **off-band** — a
`copilotd:` message prefix, copilotd's resolved `X-Request-Id` response header,
and structured logs/metrics — never by a nonstandard field on the wire. An
upstream `X-Request-Id` is suppressed on the downstream response so it cannot
compete with that authoritative correlation ID.

The Route contract selects this guarantee; a `Content-Type` value alone does
not. A raw passthrough Route such as `(GitHubCopilot, /models)` treats response
content as opaque and never enters SSE processing, even if Copilot reports
`text/event-stream`. A post-commit failure on that Route therefore terminates
the raw body without synthesizing a terminal. This preserves the support
Route's no-interpretation contract rather than making transport metadata choose
application semantics.

## Considered options

- **Nonstandard wire marker** (e.g. `"copilotd_origin": true` on the error object):
  rejected — it risks a strict-parse client rejecting the response and breaks the
  "looks first-party" promise.
- **Off-band marking** (chosen): native shape on the wire; origin is authoritative
  via the request-id and logs.

## Consequences

- Clients see only native-shaped errors; copilotd's resolved request-id is the
  authoritative origin channel for operators and the sole downstream
  `X-Request-Id` value.
- Only Routes whose contracts include SSE semantics synthesize terminals; raw
  passthrough Routes terminate post-commit failures without interpreting the
  body.
- The set of copilotd-originated signals is enumerated exhaustively (the
  "divergence ledger") and centralized in one package (`internal/apierror`), so the
  proxy's only divergence from a first-party endpoint stays auditable in one place.
- The policy binds future shims too: no parity feature may add a wire marker to
  distinguish copilotd's output from Copilot's.

See `docs/design/2026-07-15-phase-2-sse-streaming-engine-design.md` §7.
