# Serve daemon-owned Usage reports without authentication on the existing listener

**Status:** maintainer-approved direction on 2026-09-07; native Anthropic, OpenAI,
and combined reports with all calendar periods, explicit named zones, and month
range defaults implemented in #207–#209; #210 adds conservative Unix terminal-local
timezone discovery and the native Windows explicit-only policy. #211 adds exact
Reported-model filters, detailed native tables, and validated original-byte JSON.
#212 retains integrated concurrency/lifecycle evidence; #213 adds complete
executable acceptance and the native pipeline. The estimated-cost follow-up adds
daemon-owned valuation in #236 and its atomic additive version-1 HTTP contract in
#237; #238 adds CLI cost presentation and deterministic executable acceptance.
Actual native release certification is recorded per revision on #213, not implied
by implementation status. Protocol
details and verification gates live in the
[Usage reporting design](../design/2026-09-07-usage-reporting-design.md).

copilotd exposes calendar aggregates of its configured usage database through
an unauthenticated local HTTP handler on the same listener as inference.
`copilotd usage` is an HTTP client, using a configurable base URL and
rendering terminal tables or JSON rather than opening SQLite itself. The
maintainer chose this over a direct-file CLI and explicitly accepted that report
exposure follows `serve --addr`, including publicly reachable bindings.

This deliberately extends [ADR-0017](0017-persist-usage-in-local-sqlite.md)'s
external-query-only scope. It does not change the opt-in writer, private local
storage, best-effort collection, or [ADR-0018](0018-store-per-surface-native-usage.md)'s
native count semantics. The existing Usage meter toggle also gates reporting:
disabled means an explicit unavailable-capability response without opening a
historical database; enabled means unauthenticated aggregate disclosure. There
is no additional authentication or report-enabled setting in this design.

## Why this shape

A daemon-owned reporting module concentrates calendar arithmetic, native count
interpretation, missing-value coverage, and read limits behind one report
interface. A terminal client and a future browser presentation need not know
SQLite paths, schema, or writer lifecycle. Direct-file CLI access would work
without a daemon, but that benefit was deliberately traded for one daemon-owned
read interface. Arbitrary SQL and raw-Turn export are not part of that interface.

A separate loopback listener or authenticated report was considered and not
chosen. Sharing the listener keeps address/lifecycle configuration small; no
API key or browser login is needed to inspect usage. This is a disclosure policy,
not an inference-authentication exemption accidentally inherited from probes.

## Consequences

- Anyone who can reach an enabled report route can read aggregate history,
  including model identities and activity patterns. An inference API key does
  not restrict reports. Default loopback binding, missing CORS headers, and
  private database permissions are not substitutes for network access policy.
- Reports cover all persisted Turns in the configured database, including
  other writers and earlier process runs. They establish neither per-process
  attribution nor complete consumption or Copilot charges.
- Reads use separate read-only SQLite access and never migrate, force a flush,
  or borrow the writer's dedicated connection. Report failure does not change
  inference readiness or writer admission. Work/admission limits reduce normal
  interference but are not resource isolation from untrusted traffic.
- The CLI defaults to the terminal host's named timezone and sends it explicitly.
  Ambiguous local detection requires `--timezone`; neither a current UTC offset
  nor the daemon's local clock silently replaces that choice. Filesystem
  discovery initially evaluates every recognized root for ambiguity, forbidden
  provenance, and unreadable evidence. After one name is selected, its final
  consistency check is deliberately limited to the selected filename path and
  the root alias that established that name. Selected directory identity/type,
  symlink, and TZif changes still fail; unrelated directory size/mtime activity
  and later changes to unused roots do not.
- The CLI requires a running reachable daemon; offline history and reporting
  while the Usage meter is disabled remain outside this design.
- The HTTP handler has no upstream dependency and is not a project Endpoint,
  Route, or Surface. It retains local errors instead of borrowing an inference
  error dialect, and introduces no change to forwarded Copilot responses.

The implementation serves Anthropic, OpenAI, or both (the default) with daily
groups by default, all four calendar periods, explicit named zones, independent
current-month date defaults, and period-grouped native terminal sections. Both
selected histories share one snapshot and request-wide limits. Omitted report
timezones now use supported configuration visible to the CLI process on Linux/macOS;
SSH/container/WSL execution does not discover a physical workstation outside it.
Native Windows and unsupported/ambiguous configurations require an explicit name,
not an offset, registry mapping, copied-file guess, or silent UTC fallback.
Native evidence and its platform limits are recorded in the
[verification guide](../verification/usage-reporting.md) and revision-specific
#213 run results; failed runs retain diagnostic artifacts, while successful runs
do not. Cross-compilation alone is not certification. Exact UTF-8 model filters,
detailed native period/range tables, and original-byte JSON output now share the
same report and client validator. Terminal tables derive a presentation-only
`Total` for each period from its validated Reported-model rows; this does not
change or replace the server's per-model and whole-range aggregates or the
original-byte JSON. The #237 extension preserves the route, schema version, and
native fields while adding identified pricing provenance, exact USD cost/coverage
at every aggregate level, and row/model pricing matches. #238 places `Est. USD`
after `Model(s)` in each primary Surface table, adds exact-before-rounding period
cost totals, coverage notes, provenance/caveat text, and resolution details without
repeating money in secondary tables or introducing a cross-Surface total. A new
client represents a wholly absent extension as an older daemon and rejects
recognized partial shapes; unknown additive fields and validated original bytes
remain intact. Terminal-only native and monetary coverage counters use checked
int64 arithmetic; monetary amounts use the exact pricing representation. Overflow
in independently valid but inconsistent rows fails text rendering before stdout;
JSON still emits its validated original bytes. No Catalog normalization,
Requested-model substitution, HTTP-side/CLI-side pricing, or report-time network
fetch is introduced. Because the report path remains unauthenticated, model
identities, activity patterns, current valuation, and pricing coverage share the
same deliberate network disclosure.
External SQLite inspection remains supported alongside the new
bounded HTTP path. [#212 contention/lifecycle evidence](../research/2026-09-08-usage-reporting-concurrency.md)
and #213's native release gate remain distinct from feature implementation.
