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
recorded on [#213](https://github.com/ningw42/copilotd/issues/213). At introduction
of this guide, native Actions execution has not yet occurred. Later results belong
to that revision-specific record rather than an undated blanket platform claim.
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
`workflow_call`. The release archive workflow is unchanged.

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
client teardown regression, and real CLI/native runtime tests. All tests execute;
this inventory does not replace or narrow the suite.

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
  failure is a blocker.
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

**Initial local checkpoint observation:** the final full race invocation passed,
including the heavy report fixtures (423.455 s for the report package), as did
vet, formatting, flake checks, the four cross-builds and the Linux CGO0 verifier
(28 required entries, including both static-isolation tests). However, an earlier
full race run failed the existing unchanged SSE test
`TestPumpFrameArrivingDuringSlowKeepaliveBeatsStall` with `stall` instead of
`clean`; an isolated 2,000-repeat run reproduced it. This checkpoint does not
change the inference engine or claim to fix that failure. The later green run
is not a correction: the coordinator must resolve this recorded blocker before
clearing #213. Exact command/results are retained in the checkpoint commit and
coordinator evidence; subsequent resolution belongs to its own revision record.
