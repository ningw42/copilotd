# Serve daemon-owned Usage reports without authentication on the existing listener

**Status:** maintainer-approved direction on 2026-09-07; native Anthropic, OpenAI,
and combined reports with all calendar periods, explicit named zones, and month
range defaults implemented in #207–#209; #210 adds conservative Unix terminal-local
timezone discovery and the native Windows explicit-only policy. Remaining
capabilities and native release verification are pending. Protocol details
and verification gates live in the
[Usage reporting design](../design/2026-09-07-usage-reporting-design.md).

copilotd will expose calendar aggregates of its configured usage database through
an unauthenticated local HTTP handler on the same listener as inference.
`copilotd usage` will be an HTTP client, using a configurable base URL and
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
  nor the daemon's local clock silently replaces that choice.
- The CLI requires a running reachable daemon; offline history and reporting
  while the Usage meter is disabled remain outside this design.
- The HTTP handler has no upstream dependency and is not a project Endpoint,
  Route, or Surface. It retains local errors instead of borrowing an inference
  error dialect, and introduces no change to forwarded Copilot responses.

The implementation serves Anthropic, OpenAI, or both (the default) with daily
groups by default, all four calendar periods, explicit named zones, independent
current-month date defaults, and compact native terminal sections. Both selected
histories share one snapshot and request-wide limits. Omitted report timezones
now use supported configuration visible to the CLI process on Linux/macOS;
SSH/container/WSL execution does not discover a physical workstation outside it.
Native Windows and unsupported/ambiguous configurations require an explicit name,
not an offset, registry mapping, copied-file guess, or silent UTC fallback.
Native Linux executable evidence is retained; macOS/Windows native release gates
remain pending. Filters, details, and CLI JSON output remain planned;
their final defaults are not silently substituted. External SQLite inspection remains
supported alongside the new bounded HTTP path.
