# SQLite driver and lifecycle feasibility

**Issue:** #195
**Evidence date:** 2026-09-05
**Repository baseline:** `91e635c965dd846e73e9c78ad68d0defaacb8280`
**Feature starting tip:** `09a9bb7af98634be1896964b4ae25973d53db199`

## Verdict

`modernc.org/sqlite v1.58.0` is a feasible candidate for the current cgo-free
release matrix. A command that opens and uses the real driver cross-compiles with
`CGO_ENABLED=0` for all four required targets:

- `linux/amd64`
- `windows/amd64`
- `windows/arm64`
- `darwin/arm64`

The Linux artifact also passes the real-driver filesystem, WAL, contention,
migration, reader/writer, cancellation, and race probes described below. No
build or Linux-runtime design blocker was found.

These probes are feasibility evidence and did not themselves grant production
persistence approval. That later explicit approval is recorded in
[ADR-0017](../../adr/0017-persist-usage-in-local-sqlite.md), which retains the
evidence limits: Windows and Darwin have compile/link evidence, not runtime
certification, and Windows ACL behavior is unverified. Nothing in this nested
module is imported by the root module or a production package.

## Disposable evidence boundary

All code is isolated in the nested module
`docs/research/sqlite-feasibility`. Its `go.mod` pins the candidate without
changing the repository root's `go.mod`, `go.sum`, or `flake.nix` vendor hash.
The retained programs are probes, not a usage writer:

- `cmd/driverprobe` opens, migrates, writes, and reads a database under a newly
  created temporary directory, then removes the directory.
- `cmd/artifactcheck` uses standard-library executable readers to report object
  format, architecture, and imported libraries.
- Tests use `t.TempDir` and paths named `usage.db` only inside those disposable
  directories. The subprocess test passes only such a path to its helpers.

The probes have no option for an operator database. No database, sidecar, build
binary, or raw command log is retained in the repository.

## Candidate and primary sources

Chosen versions:

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
  requires Go 1.25 and pins `modernc.org/libc v1.75.6`; its warning says a
  downstream must use the exact matching libc version. The nested module's
  resolved graph does so.
- [`CHANGELOG.md`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/CHANGELOG.md)
  identifies v1.58.0 as the SQLite 3.53.4 update. It also records that
  `darwin/arm64`, `windows/amd64`, and `windows/arm64` are supported and that
  the driver has been fully cgo-free since v1.5.0.
- [`builder.json`](https://gitlab.com/cznic/sqlite/-/blob/v1.58.0/builder.json)
  includes the four target pairs in the upstream test matrix.

`nix develop -c go mod download -json modernc.org/sqlite@v1.58.0` produced the
source identity and checksums above. The selected release is not among the
retracted versions in the driver's `go.mod`.

A v1.58.0-specific Linux caveat is relevant to future production review: the
release adds opt-in Open File Description locking but leaves it off by default.
The changelog explains the ordinary POSIX-lock hazard when unrelated file
descriptors for the same database inode are closed in the same process. This
probe used the cross-platform default and does not recommend enabling the
Linux-only mode without a separate policy decision. Production code should at
minimum avoid opening and closing unrelated descriptors for a live database.
The pre-creation probe closes its descriptor before SQLite opens or locks the
file.

## Release-target build and artifact evidence

The retained `driverprobe` does more than blank-import the package: its main path
calls `PreparePrivateDatabase`, obtains the dedicated `*sql.Conn` from `Admit`,
executes a migration and insert, and queries the inserted value. Therefore each
cross-built command contains a reachable real-driver open/use path.

Build command, run from the authoritative feature worktree:

```sh
rm -rf /tmp/copilotd-sqlite-feasibility-builds
mkdir -p /tmp/copilotd-sqlite-feasibility-builds
nix develop -c sh -c '
  set -eu
  cd docs/research/sqlite-feasibility
  for target in linux/amd64 windows/amd64 windows/arm64 darwin/arm64; do
    goos=${target%/*}
    goarch=${target#*/}
    suffix=
    [ "$goos" = windows ] && suffix=.exe
    output=/tmp/copilotd-sqlite-feasibility-builds/driverprobe-${goos}-${goarch}${suffix}
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
      go build -trimpath -o "$output" ./cmd/driverprobe
    file "$output"
  done
'
```

All four commands exited zero. Artifact results:

| Target | `file` / object-reader result | Certification level |
| --- | --- | --- |
| `linux/amd64` | ELF x86-64, **statically linked**; imported-library list empty | Build, link inspection, and runtime probe on the development host |
| `windows/amd64` | PE32+ x86-64; machine `0x8664` | Build/link only; not run on Windows |
| `windows/arm64` | PE32+ ARM64; machine `0xaa64` | Build/link only; not run on Windows ARM64 |
| `darwin/arm64` | Mach-O arm64 with `DYLDLINK`; imports `/usr/lib/libSystem.B.dylib` and `/usr/lib/libresolv.9.dylib` | Build/link only; not run on Darwin |

Linux is genuinely static. Darwin is not and must not be described as static:
it has the expected libSystem dependency; this particular probe also imports
libresolv. The existing non-SQLite copilotd Darwin artifact already imports
libSystem, libresolv, CoreFoundation, and Security, so compile success does not
remove the ordinary Apple system-library dependency.

The following retained/reproducible checks were also run:

```sh
# Report executable imports with debug/{elf,macho,pe}.
nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  go run ./cmd/artifactcheck /tmp/copilotd-sqlite-feasibility-builds/driverprobe-*
'

# Report embedded modules and build settings for every artifact.
nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  for artifact in /tmp/copilotd-sqlite-feasibility-builds/driverprobe-*; do
    go version -m "$artifact"
  done
'

# Inspect linked driver symbols.
nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  for artifact in /tmp/copilotd-sqlite-feasibility-builds/driverprobe-*; do
    go tool nm "$artifact" 2>/dev/null | grep -m 3 modernc.org/sqlite
  done
'
```

For every target, `go version -m` reported Go 1.27.0,
`modernc.org/sqlite v1.58.0`, `modernc.org/libc v1.75.6`, the requested GOOS and
GOARCH, and `CGO_ENABLED=0`. `go tool nm` found linked symbols including
`modernc.org/sqlite.(*Driver).Open` in every artifact.

## Real-driver lifecycle evidence

Linux runtime environment: Linux 6.18.44 x86-64; `stat -f -c %T /tmp` reported
`ext2/ext3` for the local temporary filesystem. These tests do not certify a
network, roaming, or synchronized filesystem; those remain unsupported by the
design.

### Dedicated connection and configuration

`Admit` limits its `*sql.DB` to one connection and obtains one dedicated
`*sql.Conn`. Every connection-scoped PRAGMA, `BEGIN IMMEDIATE`, schema query,
migration, and later probe write uses that same pinned native connection.

The green probe establishes and reads back:

- `PRAGMA journal_mode=WAL` returns `wal`; the returned value is checked rather
  than assuming the requested mode took effect.
- `PRAGMA synchronous=NORMAL`; `PRAGMA synchronous` reads back `1`.
- `PRAGMA user_version` is read only after `BEGIN IMMEDIATE` succeeds.
- Pending migration SQL and the `user_version` bump commit in that transaction.
- After admission, `PRAGMA busy_timeout` reads back the full runtime value
  `5000` ms rather than the startup budget's last reduced value.

Two simultaneously started goroutine openers passed repeatedly. More
importantly, two separately executed copies of the Go test binary simultaneously
pre-created and admitted the same fresh file; exactly one `O_EXCL` creation won,
both driver opens completed, and a later connection observed `user_version=1`.
The migration's `CREATE TABLE` would have exposed a stale version check outside
the serialized transaction.

A database at `user_version=2` was refused by a probe supporting one migration.
Its trace orders `BEGIN IMMEDIATE` before the `user_version` read, and the error
names both versions.

### Immediate WAL busy and one monotonic budget

With one real connection holding `BEGIN IMMEDIATE` on a fresh rollback-journal
database, a second dedicated connection set and read back a native
`busy_timeout=2000`, then attempted `PRAGMA journal_mode=WAL`. The operation
returned SQLite code 5 (`SQLITE_BUSY`) in 92.44 microseconds on the recorded run,
not after the two-second timeout. This confirms that native timeout alone does
not cover WAL activation.

`Admit` therefore retries only pre-acquisition `SQLITE_BUSY`, closes the failed
attempt, and starts setup on a fresh native connection. One monotonic deadline
is created before the first attempt. The remaining time is recomputed and the
native busy timeout is reduced before connection open, WAL activation,
`synchronous=NORMAL`, and `BEGIN IMMEDIATE`.

A real sequential-contention test held WAL activation first, then acquired a
second real write transaction immediately before the probe's transaction stage.
With the exact default five-second budget, the recorded run showed:

- first WAL native timeout: `4.999s`;
- later `BEGIN IMMEDIATE` native timeout: `4.839s`;
- total elapsed: `393.769277ms`.

Thus the later stage receives a smaller remainder, not a reset five seconds.
The test checks that WAL produced native `SQLITE_BUSY` and that the later
transaction also had to wait for a separately held real lock.

With a 150 ms test budget and a continuously held lock, the probe exhausted one
budget in `150.556994ms`; 15 connection-open trace events completed before the
16th attempt observed deadline exhaustion. The returned error explicitly says
`startup contention budget exhausted` and retains
`context.DeadlineExceeded`.

An existing private regular file containing non-database sentinel bytes produced
a non-contention driver error after one attempt. Its bytes were unchanged.
Non-contention errors are not retried.

### Migration errors are outside retry

A two-step migration where step one creates a table and step two contains invalid
SQL acquired `BEGIN IMMEDIATE` once, failed at migration 2, and was not retried.
A fresh admission observed `user_version=0` and no table from step one, proving
the migration and version bump rolled back together.

This behavior is deliberately different from pre-acquisition lock handling: a
post-acquisition migration error is a startup failure even if its SQLite code
could otherwise look transient.

### Context cancellation under write contention

A real contention experiment disproved the initial hypothesis that
`ExecContext` cancellation promptly preempts the driver's busy wait. A second
connection used `busy_timeout=500ms`, attempted `BEGIN IMMEDIATE` behind a held
writer, and received a 50 ms context deadline. It returned
`context deadline exceeded` only after `501.02113ms`, near the native timeout,
then remained usable for `SELECT 1`.

The first exploratory form used a five-second native timeout and failed its
prompt-cancellation assertion after `5.004477578s`. The retained test encodes the
observed behavior instead of claiming the desired one.

Consequence: a bounded startup, runtime batch, or final flush must cap the native
busy timeout to the operation's current remaining budget **before every
potentially blocking lock operation**. `ExecContext` is still required for
already-canceled contexts and other interruptible work, but context cancellation
alone is not a contention bound for this candidate.

### External reader and WAL writer

An admitted writer inserted one row. A fresh external driver connection enabled
`query_only`, began a read transaction, and observed that row. While that read
snapshot remained active, the admitted connection acquired `BEGIN IMMEDIATE`,
inserted a second row, and committed successfully. The reader retained its
one-row snapshot and observed two rows after ending the transaction. This is the
required external-reader coexistence evidence on the tested Linux filesystem.

## Filesystem and permission evidence

Unix probes set process umask to `000` around creation and then verify:

1. a missing parent is created as `0700`;
2. the main file is pre-created with
   `O_CREATE|O_EXCL|O_RDWR` and mode `0600`, then closed before SQLite opens it;
3. an existing file follows validation and is never opened with truncation;
4. an existing parent at `0777` is refused and remains `0777`—the probe never
   chmods an operator directory;
5. a symlink destination and other non-regular destination are refused;
6. an existing main file at `0644` is refused;
7. 16 concurrent in-process creators produce exactly one creation winner; and
8. two concurrent processes also safely race fresh-file creation.

With a live WAL connection and a committed write under umask `000`, all three
regular files existed. The recorded Linux modes were `0600` for `usage.db`,
`usage.db-wal`, and `usage.db-shm`. The security contract must nevertheless rely
on the enclosing `0700` directory rather than assuming a stable SQLite sidecar
mode.

On Windows, the retained build uses best-effort exclusive creation and
regular-file validation only. Go's Unix-like `FileMode` values neither set nor
prove a Windows ACL. Neither Windows target was executed, so ACL inheritance,
sidecar ACLs, concurrent creation, WAL locking, and final-path reparse-point
handling remain unresolved runtime evidence. ADR-0017 accepts this as an explicit
best-effort limitation rather than certification; inherited ACLs and sidecar
protection remain unverified, and a
Unix mode assertion cannot establish Windows behavior.

## Historical bounded final-flush proposal, later accepted

This section preserves the proposal reviewed for #196. Its policy was later
explicitly accepted in
[ADR-0017](../../adr/0017-persist-usage-in-local-sqlite.md) and reconciled into
the authoritative [Usage meter design](../../design/2026-07-26-token-usage-meter-design.md).

### Current coordinator behavior

`internal/server.Server.Run` enters shutdown when its context is canceled.
`shutdown` creates one
`context.WithTimeout(context.Background(), cfg.ShutdownTimeout)`, calls
`ws.StartDrain`, then `http.Shutdown`, then `ws.Shutdown` with that same context.
If either drain reports an error, it force-closes HTTP. The WebSocket proxy
force-cancels and force-closes surviving sessions when the shared deadline wins.
The default `ShutdownTimeout` is 10 seconds.

`cmd/copilotd.runServe` keeps the base logger open with a deferred logger close
until `runBoundServe` has returned and its server error has been logged. That
composition-root interval is the place where store finalization can report its
last aggregate without moving storage into `internal/server`.

### Sharing the existing deadline

Reusing the original deadline for a final flush after HTTP/WebSocket drain is
not recommended. A legitimate drain can consume all of it, leaving no chance to
persist rows admitted by the drained requests. Starting the flush concurrently
would instead require an early admission cutoff and could lose rows from handlers
that shutdown is specifically allowing to finish. A deferred context-free
`store.Close()` would avoid both choices only by becoming unbounded.

### Accepted policy: one explicit bounded extension

The later architecture decision accepts this policy:

1. Let `Server.Run` complete its existing drain or force-close sequence under
   the first `ShutdownTimeout` deadline. Admission remains open during this
   phase, so completing HTTP and WebSocket hooks can still submit rows.
2. Immediately after `Server.Run` returns, atomically cut off store admission.
   `Record` racing with or following the cutoff must return promptly, increment
   a `late_after_cutoff` loss counter, and never send to a closed channel.
3. Create a **fresh**
   `context.WithTimeout(context.Background(), cfg.ShutdownTimeout)` for final
   flush. This is an explicit extension after drain, so the configured graceful
   maximum is at most `2 * ShutdownTimeout` (20 seconds with the current
   default), apart from the filesystem limitation below. It adds no second
   setting or hidden timeout constant.
4. The single writer owns finalization. It drains the already accepted bounded
   queue and attempts bounded batches. Before each possibly blocking
   `BEGIN IMMEDIATE`/write/commit stage, recompute the extension's remaining
   monotonic time and cap native `busy_timeout` to that remainder as well as
   passing the context. The cancellation experiment above is why both controls
   are mandatory.
5. On deadline, failed storage, or an ambiguously completed batch, do not replay.
   Conservatively count every queued or not-confirmed row as lost and return to
   the coordinator. A disk-full, read-only, I/O, corruption, or other
   non-contention failure is not a retry loop. A batch already reported committed
   is not loss; one whose result is unknown is unconfirmed loss.
6. Before the logger closes, emit one final aggregate from Component
   `internal/usage/sqlitestore` using ADR-0015 keys. It should include at least
   queue-full drops, runtime write losses, late-after-cutoff drops, final-flush
   losses, and whether driver cleanup completed. The snapshot covers losses
   observed through publication. A permanently stuck producer that calls
   `Record` after that final snapshot is inherently unreportable during process
   exit; forced shutdown cannot promise otherwise.
7. Apply the same bounded finalizer on bind/serve failure after a store has been
   opened. Usually its queue is empty, but cleanup must not hide in an unbounded
   defer.

### What this can and cannot bound

The policy bounds how long the coordinator **waits** for queue draining,
contention, final writes, and driver cleanup. It does not make arbitrary
filesystem I/O preemptible.

Go's `database/sql.Conn.Close` documentation says it blocks until concurrent
operations finish; `DB.Close` waits for started queries. Neither takes a context.
The candidate's native `conn.Close` takes its connection mutex and calls
`sqlite3_close_v2`, also without a context. Therefore the writer should own the
connection and cleanup in its worker goroutine; the coordinator waits only until
the fresh deadline. If the worker has not returned, publish
`driver_cleanup_completed=false` and let serve process exit abandon that
background cleanup rather than blocking forever.

Normally the recomputed native timeout causes a contended SQLite call to return
before cleanup starts, making close prompt. No Go deadline can guarantee that a
kernel or remote/broken filesystem call returns, however, and even abandoning a
Go goroutine cannot promise an operating-system process-exit bound under
arbitrary storage failure. The second signal's existing hard-kill behavior
remains the ultimate operator escape. These limitations are reasons to require a
local filesystem, not reasons to claim an unbounded close is safe.

## TDD and verification record

Tests exercise the user-approved disposable driver-probe and filesystem/database
system boundaries. They use the real candidate driver and real temporary files,
locks, connections, processes, and contexts; there is no Python SQLite substitute
and no mock writer queue or goroutine.

Representative red-to-green observations:

| Slice | Red observation | Green observation |
| --- | --- | --- |
| Private pre-creation | focused test failed to compile: `undefined: PreparePrivateDatabase` | parent `0700` and file `0600` under umask `000` |
| Dedicated WAL admission | focused test failed to compile: `undefined: Admit` and `AdmissionOptions` | SQLite 3.53.4, WAL, NORMAL, migration v1 on one `*sql.Conn` |
| Budget exhaustion contract | test returned bare `context deadline exceeded` | error now identifies the exhausted shared budget and wraps the deadline |
| Runtime timeout restoration | test observed `busy_timeout=4992`, not `5000` | admitted connection now reads back the full 5000 ms runtime policy |
| Busy cancellation hypothesis | prompt-cancel hypothesis failed after `5.004477578s` with a 5 s native timeout | retained test honestly demonstrates ~500 ms native wait despite a 50 ms context |

Every cycle used the same Nix-wrapped focused form. The commands that produced
the representative red observations were:

```sh
nix develop -c sh -c 'cd docs/research/sqlite-feasibility && go test -run TestPreparePrivateDatabaseCreatesPrivateParentAndFileUnderPermissiveUmask -count=1'
nix develop -c sh -c 'cd docs/research/sqlite-feasibility && go test -run TestAdmitConfiguresDedicatedWALConnectionAndMigratesAtomically -count=1 -v'
nix develop -c sh -c 'cd docs/research/sqlite-feasibility && go test -run TestAdmitExhaustsOneContentionBudgetAcrossFreshAttempts -count=1 -v'
nix develop -c sh -c 'cd docs/research/sqlite-feasibility && go test -run TestContendedExecContextHonorsCancellation -count=1 -v'
```

The dedicated-admission command was intentionally rerun for two different red
slices: first the undefined public probe, later the `4992` ms runtime-timeout
observation. The cancellation test was renamed only after its expected prompt
return was disproved; the replacement names and asserts the observed native
busy-timeout behavior.

Successful focused commands:

```sh
nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  CGO_ENABLED=0 go test ./... -count=1
  CGO_ENABLED=0 go run ./cmd/driverprobe
'

nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  CGO_ENABLED=0 go test \
    -run "Test(WALActivation|ContendedExecContext|AdmitShares|AdmitExhausts|ConcurrentFreshAdmissions|ConcurrentProcesses|ExternalReader)" \
    -count=5
'

nix develop -c sh -c '
  cd docs/research/sqlite-feasibility
  go test -race ./... -count=1
'
```

The full nested-module cgo-free test and command run exited zero and printed:

```text
driver=v1.58.0 sqlite=3.53.4 journal_mode=wal synchronous=1 value=linked
```

The race run exited zero. The repeated contention/process subset and every
four-target cross-build exited zero. The root repository's full `go test ./...`,
`nix flake check`, and production release build were intentionally not run for
this evidence slice; the coordination contract reserves the complete suite for
final integration verification.

## Remaining evidence and accepted limitations

- Run the retained real-driver tests natively on Windows amd64, Windows arm64,
  and Darwin arm64 before calling those runtime combinations certified.
- Resolve and test the Windows ACL policy, including WAL and SHM sidecars and
  reparse points. Build success and Go mode bits are not ACL evidence.
- Keep live databases on a local filesystem. Network, roaming, and synchronized
  filesystems were neither tested nor approved.
- ADR-0017 accepts the fresh `ShutdownTimeout` extension and background-cleanup
  escape; this evidence task did not self-approve them.
- If Linux OFD locking is considered, evaluate its process-wide and Linux-only
  consequences separately; v1.58.0 leaves it off by default and this probe did
  too.
- WAL with `synchronous=NORMAL` remains best-effort durability. These probes test
  configuration, transactions, and contention, not power-loss survival.
