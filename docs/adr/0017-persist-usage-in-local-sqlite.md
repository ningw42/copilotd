# Persist opt-in token usage in a private local SQLite database

**Status:** accepted

The Usage meter is the sole usage-specific exception to copilotd's no-database
state-at-rest policy. When explicitly enabled, it will persist best-effort Turn
observations in a private local SQLite database using the cgo-free
`modernc.org/sqlite v1.58.0` driver, which embeds SQLite 3.53.4 and requires the
matching `modernc.org/libc v1.75.6`. The meter remains off by default, creates no
usage files while disabled, requires no companion service, and does not change
the in-memory-only treatment of Copilot tokens or cached values.

## Why SQLite

The database is an external-query boundary: operators can inspect native
per-Surface rows using ordinary SQLite tooling without copilotd growing a query
API or aggregation service. SQLite provides transactional batches, a
forward-migrated typed schema, concurrent readers under WAL, and explicit `NULL`
semantics that JSONL would leave to every reader.

Considered alternatives:

- **In-memory only:** rejected because observations would disappear at every
  normal restart and could not provide the requested local consumption history.
- **JSONL:** rejected because crash-truncated records, concurrent writers, schema
  evolution, nullable typing, and external query indexes would all need bespoke
  policy or repair logic.
- **A required external database or companion service:** rejected because it
  would break the single-binary, single-user operating model.
- **SQLite with a cgo driver:** rejected because release builds use
  `CGO_ENABLED=0` on all four targets.
- **Private local SQLite with the validated pure-Go driver** (chosen): it
  satisfies the query and schema needs while preserving an opt-in single-process
  component and cgo-free release builds.

## Driver and platform evidence

The retained [driver feasibility evidence](../research/sqlite-feasibility/FINDINGS.md)
demonstrates a reachable driver open/use path building with `CGO_ENABLED=0` for
`linux/amd64`, `windows/amd64`, `windows/arm64`, and `darwin/arm64`. Only Linux
received native runtime, WAL, locking, migration, filesystem, and race tests. The
Windows and Darwin results are compile/link evidence, not runtime certification;
Darwin retains its normal system-library links. Windows ACL inheritance, sidecar
ACLs, reparse-point behavior, locking, and cleanup remain explicitly accepted
limitations, not certified behavior.

## Files, permissions, and durability

An enabled meter may create the main database plus `-wal` and `-shm` sidecars.
They must remain together on a local filesystem; network, roaming, and
synchronized live locations are unsupported. On Unix, startup creates or
validates a private `0700` parent, pre-creates a missing regular main file with
exclusive `0600` permissions, never truncates an existing file, and refuses
unsafe existing parents, modes, symlinks, and non-regular destinations. The
private parent is the sidecar protection boundary. On Windows, exclusive
creation and regular-file checks are best effort; Go mode bits are not an ACL
guarantee.

The writer uses WAL with `synchronous=NORMAL`, one admitted dedicated connection,
bounded in-memory admission, and bounded transaction batches. This is
best-effort durability, not billing-grade or exactly-once storage: queue
pressure, runtime write failure, forced shutdown, process kill, OS crash, power
loss, or stuck filesystem I/O can lose observations. A one-second flush target
is not a one-second loss bound. Failed or ambiguously committed batches are never
replayed automatically.

## Bounded finalization

Normal shutdown uses one fresh `ShutdownTimeout` extension **after** the existing
HTTP/WebSocket drain or force-close sequence. Admission stays open during that
first drain. After `Server.Run` returns, the composition root atomically cuts off
admission; racing or later producers return promptly, increment a late-loss
count, and can never send to a closed channel. With the current ten-second
default, the coordinator therefore waits about twenty seconds in total: up to
ten seconds for server drain and then up to ten seconds for meter finalization.

The single writer owns final queue draining, writes, and native cleanup. Queue
and write stages receive a fresh bounded context. Before every potentially
blocking SQL stage, including lock acquisition, write, and commit, the writer
recomputes the remaining monotonic budget and caps native `busy_timeout`;
`ExecContext` alone does not preempt this driver's busy wait. Deadline, storage
failure, and ambiguous completion conservatively count every not-confirmed row
as lost, with no replay.

While the logger is still alive, finalization publishes one aggregate covering
queue-full drops, runtime write losses, late-after-cutoff drops, final-flush
losses, and whether native cleanup completed. The snapshot covers losses observed
through publication only; a post-snapshot producer cannot be promised inclusion.
The coordinator abandons its wait at the bound, reports cleanup as unconfirmed
when necessary, and may leave native cleanup finishing in the writer worker. No
Go deadline can guarantee return from arbitrary stuck filesystem I/O or guarantee
OS process exit. The same bounded finalizer applies after bind or serve failure
once the store has opened.

This decision accepts the trade-offs documented in the
[Usage meter design](../design/2026-07-26-token-usage-meter-design.md);
implementation remains staged after this architecture checkpoint.
