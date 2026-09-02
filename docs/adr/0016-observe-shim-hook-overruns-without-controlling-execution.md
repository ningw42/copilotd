# Observe shim hook overruns without controlling execution

**Status:** accepted

copilotd detects a **Hook overrun** when one individual post-commit **Shim
registration** invocation remains in flight beyond one global threshold. The
framework emits a proactive structured warning at threshold crossing and, if
the invocation later ends, one matching warning describing that end. Detection
never cancels, interrupts, bounds, or tears down the hook.

The global `shim-hook-overrun-threshold` setting defaults to `1s`. Zero disables
monitoring; negative values are invalid. The threshold is intentionally
conservative for hooks whose contract is prompt CPU-bound work: it avoids
turning transient scheduler pressure into routine warnings while still making a
wedge visible quickly. Per-role and per-registration overrides are not provided.

## Cross-transport record contract

Every threshold crossing produces one Warn record with:

- Component `internal/shim`;
- message `shim hook overrun`;
- `shim`, the stable registration name;
- `hook`, one of `event_transform`, `stream_finalize`,
  `client_message_transform`, or `server_message_transform`;
- `hook_state=in_flight`;
- `duration`, the actual monotonic elapsed time when the watchdog publishes the
  record, which may exceed the configured threshold because of scheduling; and
- `threshold`, the configured global duration.

A later normal return produces the same identifying and timing fields with
`hook_state=returned` and total elapsed duration. A later panic uses
`hook_state=panicked`, then re-panics the same value so the existing SSE or
WebSocket recovery boundary retains its established wire behavior and terminal
classification. For those two supported hook outcomes, a completion racing
threshold publication yields either no records or one crossing/ending pair. A
permanently stuck invocation deliberately has only its crossing record.

`runtime.Goexit` is neither a return nor a panic and terminates the caller's
goroutine after running defers. The monitor preserves that runtime behavior
instead of converting it to `panic(nil)`. Because the governed ending vocabulary
must not falsely label it `returned` or `panicked`, a Goexit after threshold
crossing retains only the crossing record; its caller cannot continue to a
transport terminal summary. This is a transparency edge case outside the shim
hook outcome contract, not a fourth `hook_state`.

The monitor receives a correlated logging context separately from the hook's
execution context. SSE supplies its correlated response context. WebSocket
supplies the correlated handshake response context, including the WebSocket
marker, while hooks continue to execute under the process-rooted session
cancellation context. Request cancellation and server drain therefore do not
disarm or retime a watchdog whose hook is still executing.

Records are metadata-only. They contain no request, frame, message, panic-value,
or stack payload. Warn filtering suppresses only output; it does not suppress
threshold detection or request-summary counting.

## Terminal request-summary boundary

Constructing a shim Chain is the sole applicability predicate for the terminal
`hook_overruns` field. Chain construction creates one thread-safe recorder
shared by every adapter for that request or session. Each winning threshold
publication increments it once. Both WebSocket directions can increment it
concurrently.

A returning request that constructed a Chain publishes the integer count,
including an explicit zero. A request that never constructed a Chain omits the
field as not applicable; examples include probes, unmatched requests, Catalogs,
raw passthrough forwarding, and WebSocket failures before Chain construction.
An increment racing or following terminal-summary `Finish` is ignored. A
positive count does not independently alter access-record level.

The recorder, not a hook execution context or WebSocket session-result field,
bridges the count to the terminal access record. A hook that never returns still
prevents its handler and terminal summary from returning; manufacturing an
access record for that stuck handler belongs outside this decision and remains
the boundary established by issue #146.

## Reusable-watchdog cost model

Each active SSE adapter and each active WebSocket direction owns one reusable
watchdog. The watchdog allocates one callback timer at adapter construction and
resets that timer for successive synchronous invocations. An ordinary monitored
invocation pays fixed synchronization and monotonic-clock costs but creates no
goroutine and allocates no timer. The threshold-zero path creates no watchdog or
timer, and a Chain with no participating post-commit hook retains its nil-adapter
fast path.

The timer callback performs only threshold publication and returns, including
for a permanently stuck hook; monitoring adds no second execution waiting on
the hook and emits no periodic reminders. Reset races reject a queued callback
from a previous invocation until the current invocation has actually reached
its own deadline.

Focused benchmarks compare the enabled and unmonitored paths without imposing a
machine-specific nanosecond ceiling. On the implementation host, enabled
monitoring added fixed synchronization while retaining the same allocation
counts: SSE remained at three existing frame-fold allocations per operation,
and both WebSocket directions remained at zero allocations per operation.

Post-commit calls are synchronous and single-flight per SSE adapter or WebSocket
direction. Consequently one direction can produce at most one crossing/ending
pair per threshold interval. Every overrun is nevertheless reported: a
sufficiently long stream may accumulate an unbounded number of pairs. That
record-volume trade-off is accepted so a finite earlier overrun can never
suppress a later permanent one.

## Considered alternatives

- **Post-return duration histogram:** rejected. It adds hot-path observation but
  cannot report the case that matters most—a hook that never returns—and this
  repository has no operator-facing metric exporter.
- **In-flight gauge or counter:** rejected. It does not proactively identify a
  threshold crossing or the responsible registration, and there is no exporter
  through which an operator could inspect it.
- **Opt-in `pprof`:** rejected. It is a broad pull-based diagnostic, not a
  proactive correlated signal, and would add a new operational surface for a
  narrowly identifiable framework event.
- **Shim-facing logger, metrics, or observer API:** rejected. Hook timing is a
  framework-owned concern; expanding the shim contract would make every
  implementation participate in its own policing.
- **Execution timeout or cancellation:** rejected for the same reasons as closed
  issue #31. Enforcement mechanisms change execution semantics and introduce
  teardown and leaked-goroutine failure modes. This decision detects only.
- **Proactive structured warnings with a reusable framework watchdog** (chosen):
  they use the existing operator-visible logging surface, can report a hook that
  never returns, preserve hook and transport behavior, and bound normal-path
  cost.

## Consequences

- All four post-commit roles are observable with one bounded vocabulary and one
  Component across SSE and WebSocket transports.
- Pre-commit request, Prelude, and buffered-body hooks remain unmonitored.
- Existing stream and WebSocket metric shapes, access-level policy, and wire
  behavior are unchanged.
- Long-lived streams may legitimately emit many warning pairs; operators can
  disable monitoring globally with a zero threshold when that trade-off is not
  acceptable.
- A permanently stuck hook produces its crossing warning but no ending warning,
  terminal access record, or terminal metric.

This decision extends
[ADR-0014](0014-infallible-post-commit-shim-hooks.md), which leaves post-commit
promptness review-enforced and execution unbounded, and follows
[ADR-0015](0015-govern-log-record-structure-with-ordinary-slog.md) for Component,
scope, level, and governed-key structure. It does not supersede either decision.
