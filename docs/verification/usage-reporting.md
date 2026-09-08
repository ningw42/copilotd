# Usage reporting release verification

## Implementation versus certification

The feature contract in [ADR-0019](../adr/0019-serve-unauthenticated-usage-reports.md)
and the [design](../design/2026-09-07-usage-reporting-design.md) is implemented by
#207–#211. [#212 evidence](../research/2026-09-08-usage-reporting-concurrency.md)
retains real inference/report concurrency, native SQLite interruption/cleanup,
TCP backpressure, and graceful/forced drain tests. #213 adds real-executable
acceptance, the native pipeline, and final local verification.

**A pipeline definition, cross-build, or local Linux pass is not four-platform
certification.** The authoritative release result is the implementation SHA,
per-target run/attempt/artifact links, failures or blockers, and archived evidence
recorded on [#213](https://github.com/ningw42/copilotd/issues/213). The first native
Actions run, [34186159779](https://github.com/ningw42/copilotd/actions/runs/34186159779),
executed all four targets at `7e3c195`: macOS arm64 passed; Linux native,
both Windows native jobs, and Linux full race failed. Those results and subsequent
corrections are retained below. A pass belongs to its exact revision, not an
undated blanket platform claim.
Unavailable runners, installation failures, missing mandatory passes, or failed
checks leave the release gate incomplete. #213 does not close or edit epic #206.

## Executable acceptance boundaries

[`cmd/copilotd/usage_executable_test.go`](../../cmd/copilotd/usage_executable_test.go)
runs a freshly built `copilotd`, or the exact prebuilt `COPILOTD_TEST_BINARY` supplied
by the verifier. It asserts stdout, stderr, and exit status, not shell snapshots
or a private renderer's output. Ordinary tests build the binary with Go from their
existing Nix development shell (native CI uses setup-go).

- Public `sqlitestore.Open/Record/Close` supplies **synthetic prior history**.
  Actual `copilotd serve` and `copilotd usage` processes then use the real listener,
  schema-v2 database, writer startup, read-only reporter and HTTP client. Synthetic
  prerequisite API key/GitHub OAuth token values are never live credentials.
  Discovery/refresh is disabled; the initial HTTPS Exchange CONNECT is observed
  and refused by a loopback proxy that never opens a tunnel. Reports still work.
- `observed_inference_to_executable` separately sends qualifying synthetic
  Anthropic/OpenAI completions through the **in-process production daemon**,
  meter and normal asynchronous writer, then invokes the actual CLI executable.
  This closes the observation-to-output chain without claiming that the prior
  history setup itself was live inference. Existing buffered/SSE/WebSocket and
  concurrency fixtures remain retained; no public upstream compatibility claim
  follows from synthetic responses.
- Acceptance includes both Surfaces/all, four periods, independent/default
  month bounds, explicit flag/env/TOML and process-discovered zones, exact case/
  whitespace/Unicode filters, compact/details/JSON, stored-Turn coverage and
  caveats, scope/help/root/version/operands, prefix-preserving proxying, empty/
  disabled/unreachable/older-daemon/protocol errors, and real output-write errors.
  A frozen owned HTTP boundary verifies original JSON bytes and additive fields.
  No OS can carry embedded NUL in argv; its identity contract remains tested at
  Go command and HTTP/report seams, not falsely claimed as a native argv case.
- Windows process cleanup uses `Kill`, not an unsupported `os.Interrupt`; this
  is **not** executable graceful-signal evidence. The cross-platform real-listener
  in-process graceful/forced tests still exercise the production lifecycle.
  The #212 client-ownership correction closes test-owned idle connections before
  drain; it did not change production timeouts or hide cleanup failures.

## Native pipeline and evidence

[`.github/workflows/test.yml`](../../.github/workflows/test.yml) adds four standard
native jobs while retaining the existing Linux full race suite and reusable
`workflow_call`. The release archive workflow is unchanged. Windows sets
`core.autocrlf=false`
**before** checkout on the disposable runner. The verifier records the effective
setting and `checkout-bytes.json`: actual byte counts, LF/CRLF counts, SHA-256,
and no-filter Git blob identity for the pinned catalog and both exercised SSE
fixtures. A clean Git status alone can conceal newline conversion. It fails if
these working-tree bytes differ from their committed blobs; it never normalizes
runtime payloads or changes pinned hashes.

| Runtime target | Hosted runner | setup-go architecture |
| --- | --- | --- |
| Linux amd64 | `ubuntu-24.04` | `x64` |
| macOS arm64 | `macos-15` | `arm64` |
| Windows amd64 | `windows-2025` | `x64` |
| Windows arm64 | `windows-11-arm` | `arm64` |

[`scripts/verify-usage/main.go`](../../scripts/verify-usage/main.go) is a small
verification-only Go program, not a shipped dependency or framework. CI runs:

```sh
go run ./scripts/verify-usage -target OS/ARCH -runner LABEL -native-ci
```

It does not set `GOOS`/`GOARCH` to manufacture a match. It checks runner identity,
native OS architecture, Go host/target, verifier/test-process runtime, and actual
executable build metadata; records the selected Go path/version; builds with
`CGO_ENABLED=0`; and executes uncached `go test -json ./... -count=1`, including
heavy resource fixtures and actual executable acceptance. Windows native OS
architecture comes from modern PowerShell/.NET, not an emulated process's name.
macOS also checks Apple Silicon hardware; the Go host/test/binary must be arm64.
No WSL, Rosetta, x64-on-Arm binary, or `go test -c` substitutes for native execution.

Root-selected dependencies are retained with `go list -m all`. The introduction
revision uses Go 1.27.1, `modernc.org/sqlite v1.58.0`, SQLite 3.53.4, and
`modernc.org/libc v1.75.7`. ADR-0017's original feasibility run used libc v1.75.6;
that historical result is not silently relabeled as the current selection.
The actual SQLite version and concrete timings are logged by the driver and
concurrency tests. Future runs must identify their actual toolchain/modules.

### Fail-closed test accounting

The verifier's `mandatoryTests` list is the executable inventory. It requires
explicit pass events for first-open/read-only WAL/new-connection pragmas,
native lock caps, size-guarded model transfer/exact filtering, shared snapshots,
scan interruption, cleanup, whole-report limits, concurrent inference/writers,
blocked TCP/released SQLite, graceful/forced drain and writer finalization, the
client teardown regression, and real CLI/native runtime tests. It also requires
explicit passes for withheld Content-Length/chunked request bodies; final flush
and recovery across success, HEAD and early errors; same-connection delayed
inference/SSE after reports; actual closed stdout pipes in compact/details/JSON;
recovered writer-log severity; and the real WebSocket slow-reader timeout.
Unix SIGPIPE and INT/TERM executable checks are mandatory on Linux/macOS and
explicit `not_applicable` entries on Windows, where their build tags exclude them.
All tests execute; this inventory does not replace or narrow the suite.

`tests.log-accounting.json` retains required names, every final test status,
all skips, and failures. Any unexpected Usage-scope skip fails verification,
including skipped symlink fixtures. `TestUsageNativeRuntime` preflights real
`os.Symlink` for files **and directories** before certification. Windows's
explicit Unix-only mode tests and three Unix process-`TZ` tests are recorded as
N/A; the store subprocess helper is intentionally skipped in the parent test
process and exercised by its owning multi-process test. Non-Usage opt-in Codex
binary audit skips remain visible, not claimed as performed.

Linux retains the full `CGO_ENABLED=1` race suite. Windows arm64 does not support
Go's race detector; no impossible or fake-success race lane is introduced. Native
CGO-disabled SQLite/integration tests remain mandatory on both Windows targets.

### Timezone evidence is platform-specific

Every native acceptance run invokes the actual CLI with `TZ`, `TZDIR`, and
`ZONEINFO` absent and captures `/etc/localtime`, recognized root aliases and
resolved paths on Unix. A supported image records its accepted name; an
unsupported untouched configuration must retain the correct override guidance.
Then Linux/macOS CI separately installs a known `Europe/Berlin` system symlink
on the **disposable hosted VM**, tests the real system-link path with no explicit
zone, and restores the original link/file. Commands, results and controlled-vs-
untouched labels are retained. `-native-ci` refuses to mutate a local machine.
This does not relax discovery policy or mislabel a named `TZ` as system discovery.

- Linux also requires the two static-executable `unshare -Ur chroot` tests. The
  no-host-data jail contains only the executable; the discovery jail separately
  supplies a named TZif/Nix-style root fixture. `COPILOTD_TEST_STATIC_BINARY` is
  set, so these tests cannot silently skip. Namespace preflight is mandatory.
  Ubuntu's unprivileged-userns AppArmor setting is recorded, temporarily enabled
  only on the disposable VM where needed, and restored; any remaining isolation
  failure is a blocker. Isolation fixtures resolve absolute host `unshare` and
  `chroot` paths before sanitizing the child environment; `PATH=/absent` proves
  the jailed CLI needs neither developer search paths nor companion tools.
- Native Windows requires explicit named zones in flag/env/TOML, including
  `Europe/Berlin` and `UTC`, and intentional no-auto guidance even with OS `TZ`.
  Both daemon and CLI run with absent runtime `ZONEINFO`/`GOROOT` sources after
  build. Windows has no Unix platform zoneinfo fallback, so these are actual
  embedded-loading observations on each native architecture.
- Normal macOS named loading/system discovery is **not** no-host-data isolation.
  Setting only `ZONEINFO`/`GOROOT` absent does not remove its platform zone files.
  No macOS no-host-data sandbox claim is made; Darwin still requires libSystem.
  Keep native macOS discovery evidence distinct from Linux/Windows fallback proof.

### Retention and interpretation

Native artifacts `usage-native-OS-ARCH-ATTEMPT` are uploaded even on failure for
90 days. They contain source/workflow SHA/ref, run URL/attempt/job, runner/image/
OS architecture, Go environment and root modules, executable SHA-256/build
metadata and executable, exact commands/exit codes/timings, uncached Go JSON logs,
required-test/skip accounting, CLI argv/stdout/stderr/JSON/caveats, system timezone
observations, and the final result. Only synthetic fixtures and selected public
metadata are recorded; no full environment or real usage/credential dump.
Linux race logs have a separate `usage-linux-full-race-ATTEMPT` artifact.

The job summary links artifact URL/digest. The coordinator must download/archive
artifacts before expiration and summarize exact final-SHA results, run/attempt,
artifact identity/digest and limitations on #213. Verify **the same final source
revision** on all four targets after any fixes; do not combine passes from
incompatible revisions. A setup failure without an artifact remains a visible
blocker; inspect the job setup logs and image version as well.

A hosted VM pass covers its actual image/ISA/local filesystem and fixture
conditions, not all desktops, Windows inherited/sidecar ACLs, antivirus policies,
reparse points or OS releases. Read-only WAL can coordinate through sidecars;
it is not zero filesystem activity. Cleanup error injection is labeled as such,
not a naturally reproduced OS close failure. Context/native busy caps cannot
preempt arbitrary stuck filesystem or synchronous log I/O. Shared disk/listener
limits are not inference isolation or flood protection. Best-effort stored-Turn
coverage and captured `generated_at` never establish all consumption, commit-time
freshness, billing, or complete durability.

## Local gate (Nix)

Run focused acceptance/typechecking while developing, then all final gates:

```sh
nix develop -c go test ./cmd/copilotd -run 'TestUsageExecutableAcceptance|TestUsageNativeRuntime' -count=1
nix develop -c go vet ./...
nix develop -c go test -race ./... -count=1
nix fmt
nix flake check
# Native local Linux CGO0 full suite, build metadata, CLI evidence and isolation;
# no local system setting changes (do not pass -native-ci):
nix develop -c go run ./scripts/verify-usage -target linux/amd64 -runner local-nix
```

Also build `./cmd/copilotd` with `CGO_ENABLED=0` for all four release target pairs
through `nix develop`, placing outputs in ignored scratch. Those are cross-build
results only. Full local checks supplement, not replace, the native release gate.
Do not skip the million-row/group/model-budget fixtures in the final race run.

**Initial local checkpoint observation (`7e98593`):** the final full race invocation passed,
including the heavy report fixtures (423.455 s for the report package), as did
vet, formatting, flake checks, the four cross-builds and the Linux CGO0 verifier
(28 required entries, including both static-isolation tests). However, an earlier
full race run failed the existing unchanged SSE test
`TestPumpFrameArrivingDuringSlowKeepaliveBeatsStall` with `stall` instead of
`clean`; an isolated 2,000-repeat run reproduced it. That checkpoint did not
change the inference engine or fix that failure. Its later green run was not a
correction. The initial failure remains part of the release history.

### SSE blocker correction

The user approved a separate, narrow correction at the public `sse.Pump` seam.
The reader captured a timely frame's timestamp before registering its channel
handoff. A scheduling pause in that interval let stall arbitration see no pending
read and synthesize a terminal despite observed upstream progress. The clock
notification was not stale: a deterministic Clock/ResponseWriter fixture captures
the frame ten seconds before stall, pauses the clock return, then uses
`testing/synctest` quiescence to let the pump run. The original implementation
failed 20/20 cases; changing only the channel buffer failed 3/3 as well.

The correction publishes a completed read before sampling its timestamp, then
acknowledges consumption through an unbuffered timestamp handoff. Timer arbitration
can wait for that completed read's clock sample, never an unfinished upstream
read. This closes the observation/publication gap without extending timers,
adding a grace period, or expanding the one-pending-frame read-ahead bound.
Cancellation remains authoritative; every exit still cancels, closes and joins.

`internal/sse/pump_progress_test.go` retains the deterministic regression and
controls for inclusive deadline arrival, genuinely late arrival, cancellation
while a timestamp is pending, bounded read-ahead, byte-verbatim output and
joined/fallback-count-safe teardown. Mutating the late-frame comparison or
buffering the acknowledgment makes the corresponding control fail. The original
2,000-repeat race command also passed three consecutive runs after correction
(300,000 inner scenarios), alongside the affected SSE/forward/server/shim suites.
The corrective full local race run passed, including the heavy report fixtures
(418.937 s), as did vet, formatting, all local flake checks, the Linux CGO0
verifier (28 required entries), and all four executable/SSE-test cross-builds.
Exact RED/GREEN commands and results are retained in the separate corrective
commit and coordinator evidence. These local results do not replace #213's
still-required final-revision native matrix or final review.

### Final Spec-review network/output corrections

The final Spec review identified two gaps not covered by the original slow-reader
and failing-Go-writer tests. Both were reproduced before their corrections:

- **Unread HTTP request bodies:** two keep-alive GETs with withheld body bytes
  could block net/http's implicit request-body drain during response flushing,
  retain both report slots, and leave later reports returning 429 beyond the
  five-second write budget. The report handler now applies the same route-local
  deadline to reads and writes, including initial generic-recovery coverage and
  net/http's post-handler final flush/body cleanup. It does not reject GET bodies,
  enable a global server timeout, or change inference deadlines.
- **Actual Unix stdout pipes:** the built usage executable inherited a pipe with
  no readers as stdout fd 1 and terminated with SIGPIPE instead of returning the
  required exit 1 and CLI error. A scoped SIGPIPE notification now makes EPIPE
  reach the existing error translator. Its lifetime includes the stderr attempt;
  it is removed afterward, without changing help/serve/login signal policy or
  the usage command's INT/TERM cancellation. Windows retains ordinary pipe errors.

Retained regressions:

- [`internal/server/usage_report_body_test.go`](../../internal/server/usage_report_body_test.go):
  `TestUsageIncompleteRequestBodiesReleaseReportSlotsWithinWriteBudget` uses real
  SQLite, the production listener and complete Query results to distinguish work
  from blocked network output. Content-Length and chunked requests occupy both
  slots, then return at about five seconds without a client close; later reports
  succeed. Its status probe waits until both queries return so it cannot steal
  admission during fixture setup. `TestReportIncompleteBodiesBoundFinalFlushAndRecovery`
  covers small successes, HEAD, method/disabled/syntax/semantic errors and generic
  panic recovery, including post-handler flushing. Removing just the read-deadline
  call makes both slot cases and all seven cleanup cases fail at the test's 7.5s
  safety bound; restoring it passes three repetitions of every case.
- [`cmd/copilotd/usage_report_body_e2e_test.go`](../../cmd/copilotd/usage_report_body_e2e_test.go):
  `TestUsageReportDeadlinesDoNotLeakIntoReusedInferenceConnections` uses the same
  raw HTTP/1 connections first for complete GET reports, then authenticated
  inference. An upload and an SSE response still complete past the preceding
  report's five-second deadline, with no client reconnect/retry to hide leakage.
  Existing blocked-TCP/WAL checkpoint/SSE/shutdown regressions remain unchanged.
- `TestUsageExecutableAcceptance/closed_stdout_pipe` in the executable harness
  checks compact/details/JSON exit 1 and the stderr error using a real inherited
  pipe, separately from the retained read-only-file and partial/short/newline
  writer tests. With both stdout and stderr closed it still requires exit 1,
  without claiming stderr delivery. Unix-only actual-executable tests in
  [`usage_signals_unix_test.go`](../../cmd/copilotd/usage_signals_unix_test.go)
  retain informational commands' SIGPIPE behavior and usage INT/TERM cancellation.

The corrective local full normal suite (including heavy report fixtures), focused
race suites for reporthttp/reportcli/cmd/server/config/logging/forward/SSE, vet,
formatting and diff checks passed. All four CGO-disabled executable and affected
command/listener test binaries cross-built successfully. This is local Linux and
cross-build evidence, not a native Windows/macOS pass. The separate native-CI
fixture failures and final full-race/flake/native release gates remain coordinator
work; this correction does not certify #213 or close epic #206.

### First native-run fixture corrections

Run `34186159779`, attempt 1, at `7e3c195` is retained unchanged by the
coordinator, including all five artifacts and their IDs/digests. Its failures
are not superseded by a green rerun or by these setup changes:

- **Linux launch:** the child environment omitted PATH; Ubuntu's `chroot` was
  outside the default executable search path. Both isolation tests failed with
  `unshare: failed to execute chroot: No such file or directory`. Making PATH
  deliberately unusable reproduces the same failure locally. Absolute host-tool
  resolution passes the unchanged jailed-executable assertions with that PATH.
- **Windows source fidelity:** the pinned catalog grew from 515145 to 516510
  bytes, exactly its 1365 LF newlines converted to CRLF. SSE separators and
  verbatim comparisons failed too. Pre-checkout conversion control preserves
  authored bytes, including any intentionally CRLF fixtures; the new byte
  evidence independently verifies this rather than trusting Git status.
- **Simulated Unix filesystems on Windows:** Go converts `os.Symlink` targets
  to native separators. The private filesystem adapter now returns those real
  `os.Readlink` observations as Unix slash names. Each created fixture checks
  that the observed target round-trips to its authored name, without stripping
  volume prefixes or replacing real symlink/Lstat/Open operations. Production
  timezone discovery and Windows explicit-only policy are unchanged.
- **Backpressure:** Windows buffered complete legal ~6.29 MB (>6 MiB) report bodies,
  so the original fixture had not blocked an application write. Only the slow
  scenarios now use real accepted TCP sockets with a requested 16 KiB send
  buffer before HTTP writes. The >6 MiB, <=8 MiB body, two occupied slots/third
  429, five-second write limit, released SQLite transaction, forced truncation,
  graceful completion and SSE survival checks remain. The existing 16 MiB
  WebSocket slow-reader fixture gets the same accepted-socket control while
  keeping its 25ms production write timeout, 1011 close and session-join checks.
  This WebSocket diagnosis remains a native-Windows hypothesis until rerun.
  Failure diagnostics bound unexpected body prefixes instead of flooding logs;
  the first run's complete original evidence is preserved.
- **Recovered writer-log severity:** Linux race reported one queue drop, 128
  intentional runtime losses, 128 final-flush losses and unconfirmed cleanup.
  The test closed with a one-second context after a pressure log, which is not
  an acknowledgment of queue drain. A temporary external-lock timing probe
  found only 256/257 of 1280 admitted valid Turns committed at that log (3/3).
  The correction waits for real externally observed committed history derived
  from the synthetic valid Turn count minus the public logged queue-drop count,
  then checks final severity with the **same** one-second close context. The
  probe passed 3/3 after correction; the temporary timing change was removed.
  No writer code, counters, runtime budget or short-budget finalization tests
  changed. Repeated local race checks additionally cover the retained fixture.

Local correction verification passed: the original writer-severity scenario
30 times under race, the four slow-report and WebSocket scenarios 10 times under
race, the full normal suite, vet, formatting, and all local flake checks. The
full race suite includes the heavy report fixtures (418.446 s for that package).
Fresh Linux CGO0 verification passed all 45 explicit required entries, including
both isolated executable tests and new network/pipe regressions. All four CGO0
executables and affected command/CLI/store/WebSocket test binaries also built;
those cross-builds are not native execution. The commit body and coordinator's
retained logs identify the exact commands and revision.

These corrections preserve the separate Spec-review handler/signal fixes and
SSE correction. Local checks and cross-builds are evidence only for what they
actually execute. Windows checkout, symlink and TCP behavior still require the
next native Actions run, and **all four native targets plus Linux full race must
pass at the same final revision**. #213 remains incomplete until that evidence
and the final independent review clear; epic #206 remains open.
