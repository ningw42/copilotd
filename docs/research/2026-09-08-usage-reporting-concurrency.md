# Usage reporting: native concurrency and lifecycle evidence (#212)

## Scope and environment

This is executable integration evidence for #212, built on the verified #207–#211
baseline `a05018531cd66ee05cba3a04987dd6eceb9b2c89`. It does not defer or introduce
baseline safety: **the new behavioral characterizations passed without production
corrections**. No reader/writer schema, index, admission, deadline, authentication,
or lifecycle policy changed. The task's signed commit contains these tests and
this evidence; #213 owns the complete release/platform gate.

Observed locally through `nix develop`:

- Linux amd64; Go `go1.27.1`.
- Root-selected `modernc.org/sqlite v1.58.0`, embedded SQLite `3.53.4`.
- Root-selected `modernc.org/libc v1.75.7` (the historical ADR-0017 feasibility
  pin says `v1.75.6`; these measurements identify the actual selected dependency).
- Actual local SQLite files and TCP connections, synthetic upstream completions
  and credentials. No live Copilot compatibility or billing completeness claim.

## Production-listener evidence

All tests below use the existing `startUsageMeterServeHarness`, real `runBoundServe`,
configured Usage meter and writer, same TCP listener and middleware, and controlled
HTTP/SSE or WebSocket upstreams. They do not replace `Reporter.Query` with a stub.

| Test in `cmd/copilotd/` | What it establishes |
| --- | --- |
| `usage_report_concurrency_e2e_test.go`: `TestUsageReportsOverlapNativeInferenceAndAnotherCommittedWriter` | 64 Anthropic SSE and 64 OpenAI SSE completions, then an OpenAI WebSocket Turn. Concurrent reports overlap both inference completions and an independent SQLite writer's atomic two-table commits. Only literal committed marker pairs `(12,8012)` or `(24,16024)` are legal. Native rows/model/section totals remain coherent; normal asynchronous persistence eventually exposes 65 Anthropic and 66 OpenAI Turns, with literal input totals 780 and 528792. No writer flush is requested. The independent writer retains WAL, `busy_timeout=1000`, `synchronous=NORMAL`, and `query_only=0`. |
| `usage_report_slow_e2e_test.go`: `TestUsageSlowTCPReportsHoldSlotsReleaseSQLiteAndDoNotDeadlineSSE` | Two valid, materialized reports of over 6 MiB encounter real TCP backpressure with 64 KiB receive buffers. A third request immediately receives `429 report_busy` and `Retry-After: 1`. While both outputs remain blocked, another connection commits and completes `wal_checkpoint(TRUNCATE)` with `0/0/0`: neither report retains its read transaction. Both outputs expire under the route-local five-second write deadline; an already-open SSE stream then completes successfully beyond that deadline. |
| Same slow-TCP test | Saturated duplicate/malformed/empty/unknown parameters get 400; invalid dates, model UTF-8, zones and periods get 429 while full and 400 after release. POST remains 405. Headers and correlated, non-probe access records retain the fixed inbound registration and no Surface, raw query, model, usage payload, path, SQL, row ID or credential. |
| `usage_report_failures_e2e_test.go`: `TestUsageStorageFailuresDoNotChangeInferenceReadinessOrWriterAdmission` | Missing native table, UTF-16 storage, negative counts and invalid model encoding produce the exact generic `503 usage_unavailable` envelope on repeated requests. Fixtures exist before daemon startup; no live database replacement or schema change is implied. Missing/wrong inference API keys still get 401, a valid key still forwards, `/readyz` stays 200, and the real writer admits and finalizes the successful inference Turn. |
| Same file: `TestUsageEncodedLimitRejectsWholeRealReportAndReleasesSlots` | A legal 1 MiB model expands beyond 8 MiB when JSON-escaped in both native row and model total. Repeated GET and HEAD requests get 422 rather than partial 200, release slots, and leave readiness/admission intact. |
| `usage_report_e2e_test.go`: `TestDisabledReportDoesNotOpenHistoryOrValidateTimezone` | Existing disabled-file checks now also exercise real-listener HEAD/POST precedence: malformed HEAD remains disabled 503; malformed POST remains 405. No usage directory is created. |

### Shutdown

`usage_report_slow_e2e_test.go` adds:

- `TestUsageGracefulDrainFinishesReportAndInferenceBeforeWriterCutoff`: disconnect
  one blocked report, observe access completion and slot reuse, then begin drain
  with another blocked report and SSE in flight. Observe the listener close,
  receive the complete report, then release the SSE completion. Graceful shutdown
  and fresh finalization succeed, and the completion observed during drain is
  persisted. It is not misclassified as late after cutoff.
- `TestUsageForcedDrainCancelsRealSQLiteReadAndInference`: a 250,000-row selection
  holds an actual read snapshot, established by an independent committed write
  whose WAL pages cannot yet be checkpointed. With SSE also in flight, forced
  shutdown returns boundedly, cancels the report and inference connections, and
  allows `TRUNCATE` to return `0/0/0` after the report access record. This is not
  merely entry/cancellation of a controlled Query function.

The retained `usage_meter_e2e_test.go` tests
`TestRunBoundServeForcedWebSocketDrainAndFreshUsageFinalizationAreBounded` and
`TestRunBoundServeStopsUsageAdmissionBeforeReportingForcedDrainError` now also
carry real blocked report responses. They still assert the independent 75 ms
finalization budget, exact late/final-flush loss accounting, and cutoff **before**
synchronous server-error logging. They also verify that force-close truncates
report responses without waiting for the five-second report write budget.

No report worker, read-pool finalizer, or additional composition-root wait was
added. Existing meter loss-publication and shutdown regressions remain intact.

## SQLite/HTTP characterization and measurements

Concrete test paths under `internal/usage/report/`:

- `http_failures_test.go`,
  `TestRealSQLiteFailuresUseGenericHTTPResponsesAndReleaseAdmission`: real reads
  of missing/schema-incompatible storage, native read contention, and an explicit
  private cleanup-result fault all produce the exact generic 503 envelope, four
  times per handler without exhausting admission. Contention uses a closed-writer
  rollback-journal fixture; it **does not** switch a live daemon out of WAL.
  Cleanup fault injection performs the real read and `DB.Close`, then injects an
  error result. It is not evidence of a naturally occurring OS cleanup failure.
- Same file,
  `TestInterruptedRealSQLiteScanUsesHTTPDeadlinePrecedenceAndReleasesAdmission`:
  a 100,000-row selection with a private, shorter 10 ms parent HTTP deadline
  receives `504 report_timeout`; the adapter checks deadline precedence after
  actual `Reporter.Query`. Repeated failures and subsequent normal-budget
  empty-range success prove reuse. This is not a measurement claiming that every
  request consumes the production five-second work budget. Existing
  `reporthttp/handler_test.go` retains that fixed-budget contract.
- `driver_characterization_test.go`,
  `TestPinnedDriverFirstReadOnlyWALConnections`: two newly opened `mode=ro`
  connections read committed live WAL history, report UTF-8, `query_only=1` and
  native `busy_timeout=100`, refuse writes, close cleanly and permit truncation.
  The retained `TestPinnedDriverReadOnlyGuardAndInterruptCleanup` demonstrates
  the 1,048,577-byte model returning only byte length and NULL before full identity
  transfer, guarded exact Unicode filtering, actual native interruption, and
  reuse/close after interruption.
- Retained `report_test.go`:
  `TestQueryCapsNativeLockWaitingByRemainingBudget`, and `snapshot_test.go`:
  `TestQueryBothNativeSectionsShareOneCommittedSnapshot`. The latter continues
  to assert literal impossible-pair rejection while true atomic commits overlap
  the read. Whole-request million-row/group/model-byte limits also remain tested.
- `streaming_evidence_test.go`, `TestQueryIndexedStreamingNativeEvidence`:
  unchanged timestamp-indexed reporting over 100,000 committed Turns, two native
  groups, with literal row/model/section totals. No materialized summaries,
  additional indexes, or background cached values were introduced.

Literal local observations (not platform-independent performance promises):

| Operation | Observed result |
| --- | --- |
| 100,000-Turn combined `Reporter.Query`, non-race | 190.521072 ms; Anthropic input 600000, OpenAI input 400600000, each output 450000 |
| Native contention, 15 ms context remainder | 14.468595 ms, `usage_unavailable` because native wait returned before the deadline; not mislabeled a timeout |
| Four contended HTTP reads, 100 ms native cap each | 403.631248 ms non-race; 410.937297–411.552853 ms across three race runs |
| Four shorter-parent-deadline reads and normal empty-range reuse | 43.087246 ms non-race; 51.762539–52.507070 ms in race runs |
| Two backpressured report writes | 5.038344658 s non-race; 5.346624822–5.356910901 s in race runs; SSE completed at 5.357574963–5.368095921 s |
| Forced close with pinned real SQLite snapshot | 21.061295 ms non-race; 20.978850–21.242349 ms in race runs; two pinned WAL pages followed by `0/0/0` truncation |
| Concurrent other-writer commits | 496 non-race; 263–266 in repeated race runs; no mixed snapshots |

The read measurement is separate from fixture preparation. Under the race
detector, preparing the 250,000-row forced-read fixture takes most of its roughly
38-second test duration; the observed forced shutdown itself is roughly 21 ms.

## Verification and limitations

Commands executed through the Nix development shell:

```sh
nix develop -c go test ./cmd/copilotd ./internal/usage/... ./internal/server ./internal/config ./internal/logging -run '^$'
nix develop -c go vet ./internal/config ./internal/usage/... ./internal/server ./internal/logging ./cmd/copilotd
nix develop -c go test ./internal/config ./internal/usage/... ./internal/server ./internal/logging ./cmd/copilotd -count=1
nix develop -c go test -race ./cmd/copilotd ./internal/usage/report -run 'TestUsageReportsOverlap|TestUsageSlowTCP|TestUsageGracefulDrain|TestUsageForcedDrain|TestUsageStorageFailures|TestRunBoundServeForcedWebSocketDrainAndFreshUsageFinalizationAreBounded|TestRunBoundServeStopsUsageAdmissionBeforeReportingForcedDrainError|TestRealSQLiteFailures|TestInterruptedRealSQLite|TestPinnedDriver|TestQueryBothNativeSectionsShareOneCommittedSnapshot' -count=3 -v
nix develop -c go test -race ./cmd/copilotd -run 'TestDailyOpenAIUsageCommandThroughProductionListener|TestUsageEncodedLimitRejectsWholeRealReportAndReleasesSlots|TestDisabledReportDoesNotOpenHistoryOrValidateTimezone' -count=5
nix develop -c go test -race ./internal/config ./internal/usage/... ./internal/server ./internal/logging ./cmd/copilotd -skip '^TestQueryEnforcesWholeReportResourceLimits$' -count=1
nix fmt
git diff --check
```

All passed. The affected normal suite was rerun after the last test-fixture
changes (report 14.511 s, store 35.416 s, command 24.439 s) and includes the heavy,
real million-row limit fixture. The affected race suite also passed (report
43.219 s, command 72.167 s), skipping only that explicitly named heavy resource
fixture; the smaller integrated scan/driver/snapshot/lifecycle cases were not
skipped. The prior one-off #209 graceful-stop failure did not reproduce in 20
normal repetitions of `TestDailyOpenAIUsageCommandThroughProductionListener` or
five additional race repetitions. No cause is claimed and no lifecycle timeout
was inflated or error suppressed.

During test development, a 1 KiB receive-buffer fixture made draining already
queued TCP bytes too slow for the close assertion. The retained 64 KiB fixture
both demonstrates genuine backpressure (429 while outputs block) and permits
prompt observation of force-close. A quoted log-message expectation and the
placement of the test's shorter parent deadline were also corrected. These were
test-fixture corrections, not reproduced production defects or fake red/green
product cycles.

These are local Linux observations, not native macOS/Windows certification. Tests
are retained for #213's native runners; no runner labels, cross-builds or fixtures
alone certify runtime behavior. Progress through arbitrary stuck filesystem I/O
or synchronous log output still cannot be guaranteed by Go contexts. Shared
listener/disk access is not inference resource isolation or flood protection.
No freshness, queued-Turn visibility, all-consumption, or billing guarantee follows
from a successful committed snapshot. The complete repository/race/build/flake
release gate remains owned by #213; epic #206 remains open.
