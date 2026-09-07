# SQLite driver and lifecycle feasibility

**Issue:** #195
**Historical evidence date:** 2026-09-05
**Historical repository baseline:** `91e635c965dd846e73e9c78ad68d0defaacb8280`
**Historical feature starting tip:** `09a9bb7af98634be1896964b4ae25973d53db199`
**Root-test migration:** PR #202 follow-up, 2026-09-06

## Verdict and evidence boundary

The 2026-09-05 experiments found `modernc.org/sqlite v1.58.0` feasible for the
cgo-free release matrix. A command with a reachable real-driver open, migration,
insert, and query path built with `CGO_ENABLED=0` for `linux/amd64`,
`windows/amd64`, `windows/arm64`, and `darwin/arm64`. Linux filesystem, WAL,
contention, migration, reader/writer, cancellation, and race probes passed. The
recorded timings and artifact results below are **historical probe results**, not
measurements of the current production binary.

These experiments did not themselves grant production persistence approval.
Maintainer Ning Wang's [2026-09-06 current approval](../../design/2026-07-26-token-usage-meter-design.md#maintainer-approval-2026-09-06)
accepts [ADR-0017](../../adr/0017-persist-usage-in-local-sqlite.md) and its evidence
limits; historical pre-implementation approval remains unverified. Windows and
Darwin have compile/link evidence, not runtime certification. Windows ACL
behavior remains unverified.

The disposable nested Go module has been removed. Its distinct checks now live
beside the real store in the **root module**, using the root-selected driver;
there is no second admission implementation, migration engine, driver pin, or
research executable to maintain. Root `go.mod`, `go.sum`, and the Nix vendor hash
remain production dependencies. This document stays at its original URL for the
ADR evidence link.

The original module was introduced in
[`3d52c6f44951f54df44d956a1f852da80e3f40ae`](https://github.com/ningw42/copilotd/tree/3d52c6f44951f54df44d956a1f852da80e3f40ae/docs/research/sqlite-feasibility)
and reviewed at `1719f5cb9f8631db4aea44ed37792b2d22dc7d8f`. Those snapshots are
supplementary historical source, **not the only executable reproduction**: use
the maintained root checks below. All runtime reproducers use temporary files;
no operator database, sidecar, binary, or raw command log is checked in.

## Candidate and primary sources

Exact candidate identity used in the historical experiments and selected in the
root dependency graph at this migration:

| Item | Version/evidence |
| --- | --- |
| Go toolchain | `go1.27.0 linux/amd64`, matching the root `go 1.27` and Nix pin |
| Driver | `modernc.org/sqlite v1.58.0` |
| Driver source identity | tag `v1.58.0`, Git commit `722282f38b49191a4e24569eeac960bc033bd8f0` |
| Module checksum | `h1:38u40/bwkfM7f0Myhosl+SEMltSDxnGdQf8o6Kjmys0=` |
| SQLite library reported at runtime | `3.53.4` |
| Required matching libc pin | `modernc.org/libc v1.75.6` |

Primary driver sources at the selected tag:

- [`doc.go`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/doc.go)
  describes the package as a cgo-free `database/sql` driver and lists all four
  copilotd targets as supported with SQLite 3.53.4.
- [`go.mod`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/go.mod)
  requires Go 1.25 and pins `modernc.org/libc v1.75.6`; its warning requires the
  exact matching libc version downstream. The historical probe and current root
  graph both use it.
- [`CHANGELOG.md`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/CHANGELOG.md)
  identifies v1.58.0 as the SQLite 3.53.4 update, records support for
  `darwin/arm64`, `windows/amd64`, and `windows/arm64`, and states that the driver
  has been fully cgo-free since v1.5.0.
- [`builder.json`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/builder.json)
  includes all four target pairs in the upstream test matrix.

`nix develop -c go mod download -json modernc.org/sqlite@v1.58.0` produced the
source identity and checksum above. The selected release is not among the
retracted versions in the driver's `go.mod`.

A v1.58.0-specific Linux caveat remains relevant: the release adds opt-in Open
File Description locking but leaves it off by default. The changelog explains
the ordinary POSIX-lock hazard when unrelated descriptors for the same database
inode are closed in one process. The probe used the cross-platform default;
Linux-only OFD locking requires a separate policy decision. Avoid unrelated
open/close operations on a live database inode. Main-file pre-creation closes
its descriptor before SQLite opens or locks the file.

## Current reproduction: root tests and feature binary

Run from the repository root, with Go supplied by `nix develop`. On 2026-09-06,
the transferred driver/admission/process subset below passed on Linux both
normally and under `-race`, three repetitions each. These are current root-test
results, separate from the 2026-09-05 timings and probe builds. They add no native
Windows/Darwin evidence or substitute for final four-target feature builds.

The compact checks preserve the unique experiments without duplicating the
production migration/filesystem/writer tests:

| Root test file | Distinct check |
| --- | --- |
| [`driver_characterization_test.go`](../../../internal/usage/sqlitestore/driver_characterization_test.go) | Native immediate WAL `SQLITE_BUSY` despite a 2000 ms timeout; 500 ms native wait versus a 50 ms context, followed by connection reuse. Upgrade-sensitive driver characterization, **not a requirement that future drivers remain slow to cancel**. |
| [`admission_test.go`](../../../internal/usage/sqlitestore/admission_test.go) | WAL, NORMAL, full runtime `busy_timeout=5000`, migrated current schema `user_version=2`, and SQLite 3.53.4 read from the actual physical connection returned by production admission, not an unrelated external connection. |
| [`process_test.go`](../../../internal/usage/sqlitestore/process_test.go) | Two OS subprocesses both ready before releasing fresh `Store.Open`; each records a Turn and performs bounded `Close`; shared schema and both rows verified afterward. Child cleanup is bounded. The barrier attempts concurrency, not proof of measured overlap inside SQLite. |
| [`startup_contention_test.go`](../../../internal/usage/sqlitestore/startup_contention_test.go) | Real production startup first contends at WAL, then at `BEGIN IMMEDIATE`; physical timeout readback proves the later stage receives a shrinking remainder of one five-second budget. |
| [`flush_test.go`](../../../internal/usage/sqlitestore/flush_test.go) | Real-store fill flush observed independently of the timer and shutdown. |

Existing [`store_test.go`](../../../internal/usage/sqlitestore/store_test.go),
platform/permission and reporting tests cover the real migration, reopening,
future-version refusal, non-contention failure, external reader, runtime
writer/loss, privacy, and shutdown behavior. Composition-root tests under
[`cmd/copilotd`](../../../cmd/copilotd/usage_meter_e2e_test.go) cover actual enabled
serve wiring and finalization; the old admission-only prototype had no usage
queue, writer, or bounded finalizer.

```sh
# Compact transferred checks, including subprocesses, repeated a few times.
nix develop -c go test ./internal/usage/sqlitestore \
  -run 'Test(Driver|AdmissionConfigures|StoreConcurrentProcesses)' -count=3 -v
nix develop -c go test -race ./internal/usage/sqlitestore \
  -run 'Test(Driver|AdmissionConfigures|StoreConcurrentProcesses)' -count=3 -v

# Production lifecycle and composition coverage, not a separate prototype.
nix develop -c go test ./internal/usage/sqlitestore ./cmd/copilotd -count=1
nix develop -c go test -race ./internal/usage/sqlitestore ./cmd/copilotd -count=1
nix flake check
```

Build the **actual feature-bearing binary**, not the deleted driver probe. Its
composition root reaches `sqlitestore.Open` when metering is enabled, so runtime
flags do not remove the linked driver. These are reproduction instructions,
not a claim that the historical four probe builds certify today's binary:

```sh
nix develop -c sh -c '
  set -eu
  output_dir=$(mktemp -d /tmp/copilotd-usage-builds.XXXXXX)
  printf "Artifacts: %s\n" "$output_dir"
  for target in linux/amd64 windows/amd64 windows/arm64 darwin/arm64; do
    goos=${target%/*}
    goarch=${target#*/}
    suffix=
    [ "$goos" = windows ] && suffix=.exe
    output="$output_dir/copilotd-${goos}-${goarch}${suffix}"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
      go build -trimpath -o "$output" ./cmd/copilotd
    go version -m "$output"
    go tool nm "$output" > "$output.nm"
    grep "modernc.org/sqlite.*Driver.*Open" "$output.nm"
  done
'
```

Check embedded driver/libc versions, GOOS/GOARCH, and `CGO_ENABLED=0` in each
artifact's metadata. For link inspection, use `file` on each binary; on Linux,
`readelf -l -d` identifies an ELF interpreter/dynamic dependencies. For Mach-O,
use `llvm-objdump --macho --dylibs-used` (or `otool -L` on Darwin); for PE use
`llvm-readobj --file-headers --coff-imports`. These object-inspection tools must
be available separately; there is no retained research inspector. Cross-building
and inspecting an executable do **not** execute it on Windows or Darwin.

## Historical release-target build and artifact evidence (2026-09-05)

The disposable `driverprobe` opened, migrated, inserted, and read via the real
candidate driver. All four cgo-free builds exited zero. Standard-library
`debug/elf`, `debug/macho`, and `debug/pe` inspection plus `file` reported:

| Target | Object / linkage result | Certification level |
| --- | --- | --- |
| `linux/amd64` | ELF x86-64, **statically linked**; imported-library list empty | Build, link inspection, and runtime probe on the development host |
| `windows/amd64` | PE32+ x86-64; machine `0x8664` | Build/link only; not run on Windows |
| `windows/arm64` | PE32+ ARM64; machine `0xaa64` | Build/link only; not run on Windows ARM64 |
| `darwin/arm64` | Mach-O arm64 with `DYLDLINK`; imports `/usr/lib/libSystem.B.dylib` and `/usr/lib/libresolv.9.dylib` | Build/link only; not run on Darwin |

Linux was genuinely static. Darwin was not: the probe linked libSystem and
libresolv. The then-existing non-SQLite copilotd Darwin artifact already imported
libSystem, libresolv, CoreFoundation, and Security. Cgo-free does not mean fully
static on Darwin or remove ordinary Apple system-library dependencies.

For each probe artifact, `go version -m` reported Go 1.27.0,
`modernc.org/sqlite v1.58.0`, `modernc.org/libc v1.75.6`, the requested GOOS and
GOARCH, and `CGO_ENABLED=0`. `go tool nm` found linked driver symbols including
`modernc.org/sqlite.(*Driver).Open` in every target. The nested cgo-free test
suite, five-repeat contention/process subset, race suite, and Linux probe run
all exited zero. The Linux command printed:

```text
driver=v1.58.0 sqlite=3.53.4 journal_mode=wal synchronous=1 value=linked
```

The root full suite, Nix checks, and production release builds were not run as
part of that historical feasibility slice. Current root reproduction above
replaces the obsolete nested-module commands; final integration verification
must still check the actual release artifacts.

## Historical real-driver lifecycle evidence (2026-09-05)

Linux runtime environment: Linux 6.18.44 x86-64; `stat -f -c %T /tmp` reported
`ext2/ext3` for the local temporary filesystem. No network, roaming, or
synchronized filesystem was certified.

### Dedicated connection, migration, and readers

The probe pinned one `*sql.Conn` for configuration, `BEGIN IMMEDIATE`, schema
reads, migration, and subsequent writes. Readbacks confirmed WAL, synchronous
`1` (NORMAL), migrated version `1`, and the restored full runtime timeout
`5000` ms. An earlier failed restoration check observed `4992` ms; this is why
reading the actual admitted connection matters.

The version read occurred after transaction acquisition; pending DDL and the
version bump committed together. A future `user_version=2` was refused by the
one-migration probe, naming both versions. A two-step migration with invalid SQL
in step two rolled back step one's table and the version bump, without retrying.
Non-database sentinel bytes produced a non-contention error after one attempt
and remained unchanged. Post-acquisition migration failure was outside the
pre-acquisition BUSY retry policy.

Two goroutine openers passed repeatedly. Two separately executed test processes
also completed fresh-file creation/admission and left `user_version=1`. That
process check did **not** measure overlap or count creation winners; exactly one
`O_EXCL` winner was asserted in the separate 16-creator in-process test. Current
root subprocess tests strengthen the start barrier and verify real usage rows.

A read-only external connection held a one-row WAL snapshot while the admitted
writer inserted and committed another row. The reader kept the one-row snapshot
until ending its transaction, then observed two rows. The real-store reader test
now checks this against the asynchronous production writer.

### Immediate WAL busy and one monotonic budget

With a real `BEGIN IMMEDIATE` held on a fresh rollback-journal database, a second
connection read back `busy_timeout=2000`, attempted `PRAGMA journal_mode=WAL`, and
received SQLite code 5 (`SQLITE_BUSY`) in **92.44 microseconds**, rather than two
seconds. Native timeout alone therefore did not cover WAL activation.

The probe retried only pre-acquisition BUSY, closing failed attempts and
restarting setup on fresh connections. One monotonic deadline covered connection
setup, WAL activation, synchronous configuration, and transaction acquisition;
each potentially blocking stage received a newly capped native timeout.

A real sequential-contention experiment held WAL activation first and then a
second lock at `BEGIN IMMEDIATE`. With the default five-second budget it recorded
initial WAL cap **4.999 s**, later BEGIN cap **4.839 s**, and total elapsed
**393.769277 ms**. A continuously held lock exhausted a separate 150 ms probe
budget in **150.556994 ms**: 15 open trace events completed before the 16th
attempt observed exhaustion. The error named `startup contention budget
exhausted` and retained `context.DeadlineExceeded`.

These timings describe the former probe. The maintained sequential regression
now runs the **production** admission path and reads actual native caps; the
prototype is no longer a substitute for that production gate.

### Cancellation counterexample

A real contention experiment disproved the hypothesis that `ExecContext`
cancellation promptly preempts the selected driver's native busy wait. A second
connection used **500 ms** native timeout and a **50 ms** context for contended
`BEGIN IMMEDIATE`; it returned `context deadline exceeded` only after
**501.02113 ms**, then remained usable for `SELECT 1`.

The initial exploratory prompt-cancellation assertion had failed after
**5.004477578 s** with a five-second native timeout. The maintained driver test
characterizes the observed 500/50 ms behavior and subsequent connection reuse;
a future driver that cancels promptly should trigger reevaluation, not be
rejected as violating a product requirement to stay slow.

Consequence for this candidate: cap native `busy_timeout` to the operation's
current remaining budget **before every potentially blocking lock operation**.
Contexts remain required for already-canceled contexts and other interruptible
work, but context cancellation alone is not its contention bound. A prompt
`Store.Close` coordinator return cannot alone prove native timeout recapping,
since the coordinator may stop waiting while the worker finishes.

## Filesystem and permission evidence

Historical Unix probes set umask to `000` and verified private `0700` parent and
exclusive `0600` main-file creation, closing the pre-creation handle before
SQLite opened it. Existing data was never truncated; unsafe `0777` parents were
refused without chmod, as were symlink/non-regular destinations and `0644` main
files. In-process and subprocess creation cases completed safely, within the
oracle limits above.

With a live WAL connection and committed write under umask `000`, Linux modes
were `0600` for `usage.db`, `usage.db-wal`, and `usage.db-shm`. The security
contract nevertheless relies on the enclosing `0700` directory, not stable
SQLite sidecar modes. Production permission and literal-path tests remain in
the root suite; copying the former probe's file-preparation engine would not
certify the production implementation.

Windows uses best-effort exclusive creation and regular-file validation. Go's
Unix-like `FileMode` values neither set nor prove a Windows ACL. Neither Windows
target was executed in the historical experiments, and this root-test migration
adds no native Windows or Darwin evidence. ACL inheritance, sidecar ACLs,
reparse points, concurrent creation, WAL locking, and cleanup remain unverified
there. The [current approval](../../design/2026-07-26-token-usage-meter-design.md#maintainer-approval-2026-09-06)
accepts the documented limitation, not runtime or ACL certification.

## Historical final-flush proposal and current acceptance

The admission-only probe had ordinary cleanup, **not** a queue, usage writer, or
bounded `Close(ctx)`. Its shutdown work was feasibility reasoning prepared for
#196. [ADR-0017](../../adr/0017-persist-usage-in-local-sqlite.md) and the
[2026-09-06 approval](../../design/2026-07-26-token-usage-meter-design.md#maintainer-approval-2026-09-06)
now cover the following policy, without establishing approval at the historical
#196 checkpoint. Production store and composition-root tests, not the deleted
probe, exercise its implementation.

### Accepted policy and loss scope

1. `Server.Run` completes HTTP/WebSocket drain or force-close under the existing
   shared `ShutdownTimeout`; store admission stays open so completing hooks may
   submit Turns. Reusing the spent drain deadline could leave no flush time;
   closing admission during drain would discard otherwise completing work.
2. After `Server.Run` returns, the composition root atomically cuts off admission
   and gives finalization a **fresh `ShutdownTimeout`**. Racing/late `Record`
   calls return promptly, count late loss, and never send to a closed channel.
3. The single writer drains the accepted bounded queue and attempts bounded
   batches, recomputing the remaining native timeout before each potentially
   blocking BEGIN/write/commit stage, in addition to passing the context.
4. Deadline, failed storage, or ambiguous completion is not a replay loop.
   Queued/not-confirmed observations are conservatively lost; confirmed commits
   are not. Queue-full drops, runtime write losses, late-after-cutoff drops, and
   final-flush losses are reported in a final aggregate, including whether
   native cleanup completed. A producer arriving after the published snapshot
   cannot be promised inclusion during process exit.
5. Keep the logger alive through final publication from Component
   `internal/usage/sqlitestore` using ADR-0015 keys. Apply the same bounded
   finalizer after bind/serve failure once a store has opened.

This permits a total **SQL/native-wait budget of `2 * ShutdownTimeout`** for
drain plus finalization (20 seconds at the ten-second default), not a blanket
wall-clock process-exit promise. Final synchronous logging, including waiting
for an earlier runtime log, is excluded and may extend return. No Go deadline
makes arbitrary filesystem or log I/O preemptible.

[`sql.Conn.Close`](https://pkg.go.dev/database/sql#Conn.Close) waits for concurrent
operations; [`sql.DB.Close`](https://pkg.go.dev/database/sql#DB.Close) waits for
started queries. Neither accepts a context. The candidate's native `conn.Close`
takes its mutex and calls `sqlite3_close_v2`, also without a context. The writer
therefore owns cleanup while the coordinator waits only to its deadline. If
unfinished, report `driver_cleanup_completed=false` and allow serve exit to
abandon background cleanup. Even abandoning a goroutine does not guarantee OS
process exit under arbitrary kernel/storage failure; the existing second-signal
hard-kill remains the operator escape.

## Remaining evidence and accepted limitations

- Run the **root** real-driver/store tests natively on Windows amd64, Windows
  arm64, and Darwin arm64 before calling those runtime combinations certified.
  Current Linux results and cross-builds do not supply that evidence.
- Resolve and test the Windows ACL policy, including WAL/SHM sidecars and reparse
  points. Go mode bits and successful builds are not ACL evidence.
- Keep live databases and sidecars together on a local filesystem. Network,
  roaming, and synchronized live filesystems are unsupported.
- The [current approval](../../design/2026-07-26-token-usage-meter-design.md#maintainer-approval-2026-09-06)
  accepts the fresh shutdown extension, background-cleanup escape, and exclusion
  of synchronous logging; it does not retroactively verify pre-implementation
  approval timing.
- Evaluate Linux-only OFD locking separately if considered; v1.58.0 and these
  checks use the default locking policy.
- WAL with `synchronous=NORMAL` is best-effort durability, not power-loss
  certification. Queue pressure, write failures, forced shutdown, hard process
  kill, OS crash, power loss, or stuck I/O may lose observations. The ~1 s flush
  target is not a one-second loss bound under backlog or failure.
