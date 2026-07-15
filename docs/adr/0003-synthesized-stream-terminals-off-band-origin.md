# Guarantee a stream terminal; mark copilotd's own signals off-band

**Status:** accepted

Once a streamed `200 text/event-stream` response is committed, its HTTP status is
locked, so a mid-stream upstream failure can only be signalled in-band. copilotd
synthesizes a native-shaped terminal `error` event whenever an upstream stream
dies without one (truncation, stall, or read error), so a client's SSE parser
never hangs waiting for `message_stop` / `response.completed`. Every such
copilotd-originated signal keeps the provider's native wire shape and is
identified **off-band** — a `copilotd:` message prefix, the `X-Request-Id`
response header, and structured logs/metrics — never by a nonstandard field on the
wire.

## Considered options

- **Nonstandard wire marker** (e.g. `"copilotd_origin": true` on the error object):
  rejected — it risks a strict-parse client rejecting the response and breaks the
  "looks first-party" promise.
- **Off-band marking** (chosen): native shape on the wire; origin is authoritative
  via the request-id and logs.

## Consequences

- Clients see only native-shaped errors; the request-id is the authoritative origin
  channel for operators.
- The set of copilotd-originated signals is enumerated exhaustively (the
  "divergence ledger") and centralized in one package (`internal/apierror`), so the
  proxy's only divergence from a first-party endpoint stays auditable in one place.
- The policy binds future shims too: no parity feature may add a wire marker to
  distinguish copilotd's output from Copilot's.

See `docs/design/2026-07-15-phase-2-sse-streaming-engine-design.md` §7.
