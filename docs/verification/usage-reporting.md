# Usage reporting release verification

## Implementation versus certification

The native reporting contract in
[ADR-0019](../adr/0019-serve-unauthenticated-usage-reports.md) and the
[design](../design/2026-09-07-usage-reporting-design.md) is implemented by
#207–#211. [#212 evidence](../research/2026-09-08-usage-reporting-concurrency.md)
retains real inference/report concurrency, native SQLite interruption/cleanup,
TCP backpressure, and graceful/forced drain tests. #213 adds real-executable
acceptance, the native pipeline, and final local verification. The additive
[estimated-cost design](../design/2026-09-11-usage-cost-reporting-design.md) is
implemented through #232–#238, including CLI rendering and deterministic local
pricing-source executable acceptance. It inherits this same-revision native gate;
implementation or local Linux evidence alone is not renewed four-platform
certification.

**A pipeline definition, cross-build, or local Linux pass is not four-platform
certification.** The authoritative release result is the implementation SHA,
per-target run/attempt status and logs, failures or blockers, and any failed-run
evidence recorded on [#213](https://github.com/ningw42/copilotd/issues/213). The first native
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
  whitespace/Unicode filters, compact/details/JSON, native and pricing coverage,
  provenance/caveats, scope/help/root/version/operands, prefix-preserving proxying,
  empty/disabled/unreachable/older-daemon/protocol errors, and real output-write
  errors. A frozen owned HTTP boundary verifies original JSON bytes and additive
  fields.
- `TestUsageCostExecutableAcceptance` adds a credential-free local models.dev
  source through the existing production source seam, the shared cache registry,
  in-process production daemon/HTTP report path, and actual CLI executable. Its
  fixed OpenAI/Anthropic/combined history covers matched/unknown/ambiguous models,
  Surface-independent original-provider identity, per-Turn context-tier selection
  and one aggregate cache-write rate, a short OpenAI-native Turn valued at base
  rates, exact current-price and identity repricing without new database writes,
  report-time no-network behavior, omission of presentation-only
  `pricing.context_policy`, and a coherent old snapshot while replacement refresh
  is deliberately blocked. The ordinary
  actual-`serve` acceptance separately pins refresh to `0` and uses the embedded
  floor; neither path contacts models.dev.
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
and no-filter Git blob identity for the pinned Codex catalog, vendored models.dev
pricing artifact, and both exercised SSE fixtures. A clean Git status alone can
conceal newline conversion. It fails if
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

Root-selected dependencies are recorded with `go list -m all`. The introduction
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
recovered writer-log severity; and the real WebSocket slow-reader timeout. The
inventory also names cost-source floor/fetch/fallback isolation, one-snapshot
reporting, public CLI rounding/exact-total/coverage/compatibility behavior, and
the actual cost executable test so those capabilities cannot disappear behind a
package-level pass.
Bodyless work-deadline precedence is also required. Unix SIGPIPE (including
malformed flags and help-validation errors with closed stderr) and INT/TERM
executable checks are mandatory on Linux/macOS and explicit `not_applicable`
entries on Windows, where their build tags exclude them.
All tests execute; this inventory does not replace or narrow the suite.

`tests.log-accounting.json` records required names, every final test status,
all skips, and failures. Any unexpected Usage-scope skip fails verification,
including skipped symlink fixtures. `TestUsageNativeRuntime` preflights real
`os.Symlink` for files **and directories** before certification. Windows's
explicit Unix-only mode tests and three Unix process-`TZ` tests are recorded as
N/A; the store subprocess helper is intentionally skipped in the parent test
process and exercised by its owning multi-process test. Non-Usage opt-in Codex
binary audit skips remain visible, not claimed as performed.

Linux runs the full `CGO_ENABLED=1` race suite. Windows arm64 does not support
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
untouched labels are recorded. `-native-ci` refuses to mutate a local machine.
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

### Failure-artifact retention and interpretation

Only failed jobs upload evidence artifacts, retained for 90 days. Successful jobs
do not upload artifacts. Native failure artifacts
`usage-native-OS-ARCH-ATTEMPT` contain source/workflow SHA/ref, run
URL/attempt/job, runner/image/OS architecture, Go environment and root modules,
executable SHA-256/build metadata and executable, exact commands/exit
codes/timings, uncached Go JSON logs, required-test/skip accounting, CLI
argv/stdout/stderr/JSON/caveats, system timezone observations, and the final
result. Only synthetic fixtures and selected public metadata are recorded; no
full environment or real usage/credential dump. Failed Linux full-race jobs use
a separate `usage-linux-full-race-ATTEMPT` artifact.

A failed-job summary links the artifact URL/digest when upload succeeds. The
coordinator should download a needed failure artifact before expiration and
record its identity/digest on #213. Successful certification records the exact
final SHA, run/attempt, job results, logs, and limitations without a successful-run
artifact. Verify **the same final source revision** on all four targets after any
fixes; do not combine passes from incompatible revisions. A setup failure without
an artifact remains a visible blocker; inspect the job setup logs and image
version as well.

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
nix develop -c go test ./cmd/copilotd -run 'TestUsageExecutableAcceptance|TestUsageCostExecutableAcceptance|TestUsageNativeRuntime' -count=1
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
Do not skip the row/group/model-budget fixtures in the final race run. They
exercise the real SQLite and report paths with small injected limits; a separate
same-package assertion pins the published production limits without requiring
production-scale fixtures in every run.

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
  five-second write budget. At `9ad5282`, the report handler applied the same
  route-local deadline to reads and writes, including an initial recovery guard
  and net/http's post-handler final flush/body cleanup. The follow-up below
  corrects that initial guard's interaction with work deadlines. Neither change
  rejects GET bodies, enables a global server timeout, or changes inference deadlines.
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

### Follow-up Spec-review deadline and parse-error corrections

Re-review at `5e8e80f` confirmed the two original defects above were corrected,
then identified two edge cases. They were reproduced separately before code changes:

- **Bodyless work versus the initial read deadline:** an ordinary real-HTTP probe
  had returned 504 in all eleven attempts, so source ordering alone was not
  treated as a reproduced failure. The retained public HTTP/ResponseController
  regression in
  [`internal/usage/reporthttp/deadline_test.go`](../../internal/usage/reporthttp/deadline_test.go)
  widens the scheduling interval by 250 ms after the first native read deadline
  is installed. Actual net/http background reads remain active; the client sends
  no body or pipelined bytes and does not cancel/retry. Before correction, all
  three runs returned 503 `usage_unavailable`, with `context.Canceled` after
  about 4.75 seconds of query work. A test-only control omitting just that first
  read deadline returned 504 with `context.DeadlineExceeded` twice; the control
  was then removed. The handler now arms network deadlines when responding,
  not before work. Existing normal/early responses retain their five-second
  read/write budget, and deferred panic unwinding arms a fresh budget before
  handing the original panic to generic recovery. The work context and its
  cancellation/driver-error precedence remain unchanged; no extra timeout,
  grace period, body policy, or upstream wait is introduced.
- **Malformed usage with closed stderr:** actual `usage --unknown` and
  `usage --help --unknown` processes separately terminated with SIGPIPE (13),
  not exit 1. The scoped notification now includes parse errors. The pinned
  ff parser retains `GetSelected` on failure; syntax-only help validation also
  returns its selected command because `--help` can precede `usage`. Only a
  selected usage execution/error installs the existing Notify/Stop subscription;
  valid help retains its original signal policy. No first-argv guessing,
  Ignore/Reset, receiver goroutine, or broader command error policy is added.
  Actual-executable tests in `usage_signals_unix_test.go` cover unknown/missing/
  malformed flags, case-insensitive and `--` dispatch, operands, repeated help
  flags, and root help preceding malformed usage. Normal stderr controls still
  receive the CLI diagnostic with no stdout; closed stderr still exits 1 without
  claiming diagnostic delivery. Valid root/help/version/serve/login output and
  usage INT/TERM behavior remain covered.

Local verification passed the complete normal config/report/reporthttp/reportcli/
server/command suites (including heavy report fixtures), race-enabled affected
suites excluding the heavy report package, ten race repetitions of the bodyless
regression, and three race repetitions each of retained body-drain/recovery,
connection-reuse, stdout/both-pipe, malformed-stderr, informational-signal and
cancellation cases. Compilation, full vet, formatting and diff checks passed.
The executable and command/server/reporthttp test binaries cross-built with
CGO disabled for all four targets. The verifier inventory adds the new required
regressions, not skips or weaker assertions. Full-repository race/flake and native
reruns remain the next coordinator checkpoint after the separate socket-fixture
correction. This follow-up does not certify #213 or close epic #206.

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

### Second native-run socket fixture corrections

[Run `34191678530`](https://github.com/ningw42/copilotd/actions/runs/34191678530),
attempt 1, at `5e8e80f`, is retained alongside the first run. macOS arm64 passed
again. The checkout-byte, simulated Unix-path, Linux tool-lookup and recovered
writer-log checks passed. The remaining failures were socket scenarios:

- Both Windows targets still buffered complete 6293011/6293012-byte reports:
  third requests returned 200 instead of 429, and forced-close checks received
  complete bodies. The WebSocket slow-reader test still did not observe upstream
  1011 closure/session teardown within its unchanged two-second bound. Merely
  requesting a positive 16 KiB accepted-socket send buffer was insufficient.
- Linux native and full race truncated the graceful report at 4543845 and
  4609328 bytes respectively, with `unexpected EOF`. Its artificial small send
  buffer remained in effect after the reader resumed; these failures did not
  establish a production shutdown bug.

The next fixture experiment changes **only private real sockets**. Slow HTTP
clients set `SO_RCVBUF=65536` in `net.Dialer.Control` before connecting. The
motionless WebSocket downstream gets the same control through its private
`DialOptions.HTTPClient` transport; upstream and unrelated clients do not.
Accepted fixture sockets request `SO_SNDBUF=0` on Windows before their first
HTTP/WebSocket write, retaining 16 KiB on Unix. Set/get errors fail setup;
logs retain endpoint addresses, requested/read-back options and HTTP body
prefetch, not a claim that options alone prove pending writes.

This timing and zero-buffer choice follows Microsoft's
[receive-window negotiation guidance](https://learn.microsoft.com/en-us/windows/win32/winsock/sio-set-compatibility-mode),
[overlapped I/O buffering](https://learn.microsoft.com/en-us/windows/win32/winsock/overlapped-i-o-2),
and documented [zero send buffering](https://learn.microsoft.com/en-us/windows/win32/winsock/tcp-ip-specific-issues-2).
Go uses overlapped `WSASend` and already updates the accepted socket context;
positive buffer sizes are not hard per-write quotas. No registry/netsh setting,
loopback fast-path toggle, socket duplication or production socket policy is added.

The graceful fixture now retains real accepted TCP connections and matches both
endpoints to the particular slow client. **After observing the drain's listener
closure**, it requests 4 MiB for that server's send buffer and that client's
receive buffer, then reads the entire report. Forced-close cases never release
the server buffer. Deadline cases additionally consume the server-closed response
and require truncation without a client read timeout. Two slots/third 429,
independent commit and TRUNCATE while output waits, >6 MiB/<=8 MiB bodies,
SSE beyond the report deadline, graceful admitted inference and writer ordering
remain asserted. No 2s shutdown, 5s report or 25ms WebSocket budget is extended.

The unchanged local Linux baseline passed five race repetitions; that was not a
reproduction of the hosted failure. A temporary one-second paused-reader probe
inside the existing drain budget failed 3/3 with only the client buffer enlarged
(about 650ms transfer), then passed 10/10 with the matched server also enlarged
(63–70ms). A harsher 1.5s pause reproduced truncation 3/3; enlargement removed
truncation in all three comparisons but one still exhausted overall shutdown
polling time. That mixed result is retained, not reported as a pass. Both
artificial pauses were removed. The ordinary slow/graceful/forced HTTP, native
SQLite cancellation and WebSocket cases then passed ten local race repetitions.
These are fixture-sensitivity and Linux results, **not Windows certification**.

The socket correction's full local normal and race suites passed, including the
heavy report fixtures (417.434s under race), as did vet, formatting and four-target
CGO0 executable/affected-test builds. However, `nix flake check` failed the unchanged
`TestReportIncompleteBodiesBoundFinalFlushAndRecovery/syntax`: at 5.000647343s it
received clean EOF with the complete 400 response, rather than the expected zero
response bytes. The socket task changed neither that assertion nor the handler;
at that checkpoint, the retained flake failure required separate diagnosis and
remained a local release blocker. Full-suite passes did not supersede it. The
assertion correction below retains that RED and explains the mistaken expectation.

Both native Windows architectures must still confirm actual blocking, 1011 and
session teardown, deadline/force truncation, and graceful throughput. All four
native targets and Linux full race must pass on one common final revision,
including the separate deadline/parse-error corrections above. #213 and final
review remain incomplete pending that evidence; epic #206 remains open.

### Incomplete-body assertion correction

The socket checkpoint's flake RED at `048b62c` was an **overstrict assertion**, not
an overrun or an appended error: `syntax` received the complete original 400
`invalid_query` and clean EOF at 5.000647343s. Both incomplete-body tests had
incorrectly required zero wire bytes. The contract bounds network waiting and
forbids a replacement error after a failed write; it does not promise nondelivery.

In pinned Go 1.27.1, `net/http` drains the unread body before writing headers,
marks a failed early drain for connection closure, and can still flush the original
response. Read and write deadline notifications need not be observed atomically.
The final flush can therefore deliver nothing, an original prefix, or the complete
original response. A chunked prefix can include the entire JSON body but lack the
terminating chunk; raw TCP EOF is not a claim of successful HTTP body delivery.

The corrected production-listener tests retain real Content-Length/chunked inputs,
real SQLite queries/materialization, two occupied slots/third 429, later 200,
all seven success/HEAD/early-error/recovery cases, and the unchanged 4.5–7.5s
observation window around the five-second budget. Reads must finish at server EOF,
not a client timeout or forced client close. A separate completed TCP exchange is
checked for its literal status, required headers, error code/body or native report
values. Received bytes must be an **exact prefix of that original exchange**;
wrong status/headers/body, appended JSON and a second response cannot pass.
The fixtures pin HTTP Date, supply a known request ID, and pin only the returned
Query value's incidental `generated_at`, without replacing SQLite collection or
recalculating report values. Prefix checks also cover partial HTTP headers without
introducing a private partial-protocol parser.

Public ResponseController fixtures retain identical deadline values while making
notification ordering observable: read-first defers installation of the native
write deadline until after the real body-drain/flush; write-first waits for the
installed deadline before the first write. These are deliberately controlled
scheduling cases, **not claims about unmodified native timer scheduling**. They
require complete original 400 delivery, zero delivery, a chunked original prefix,
and original large-report bytes with slot release. The old zero-byte assertion
failed three read-first runs before correction. Temporary attack-only HTTP writer
mutations for 418, unrelated body bytes, appended replacement JSON, and appended
HTTP response text each failed the corrected prefix assertion; all mutations were
removed. An ordinary, unordered small-success case also delivered its complete
original 200 at the deadline during local verification.

These changes affect tests and verification evidence only. Production handler,
work/read/write deadlines, recovery, signal handling, inference/SSE, writer and
socket policy are unchanged. Final-revision native Windows/macOS/Linux and Linux
full-race CI evidence is still required; this assertion correction does not
certify #213 or close epic #206.
