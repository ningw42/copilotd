# Serve daemon-owned Usage reports without authentication on the existing listener

**Status:** accepted (maintainer-approved direction on 2026-09-07) and
implemented. Protocol details, the per-issue rollout, and verification gates
live in the [Usage reporting design](../design/2026-09-07-usage-reporting-design.md),
the [estimated-cost design](../design/2026-09-11-usage-cost-reporting-design.md),
and the [verification guide](../verification/usage-reporting.md). Native release
certification is recorded per revision on #213, not implied by this status.

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
  including model identities, activity patterns, current valuation, and pricing
  coverage. An inference API key does not restrict reports. Default loopback
  binding, missing CORS headers, and private database permissions are not
  substitutes for network access policy.
- Reports cover all persisted Turns in the configured database, including
  other writers and earlier process runs. They establish neither per-process
  attribution nor complete consumption or Copilot charges.
- Reads use separate read-only SQLite access and never migrate, force a flush,
  or borrow the writer's dedicated connection. Report failure does not change
  inference readiness or writer admission. Work/admission limits reduce normal
  interference but are not resource isolation from untrusted traffic. External
  SQLite inspection remains supported alongside the bounded HTTP path.
- The daemon owns calendar arithmetic; only an accepted IANA-style timezone name
  crosses HTTP. The CLI defaults to the terminal host's named timezone and sends
  it explicitly. Ambiguous or unsupported local detection requires `--timezone`
  before any HTTP request; neither a current UTC offset, the daemon's local
  clock, nor UTC silently replaces that choice. Explicit flag, environment, and
  TOML values keep precedence. Unix discovery is conservative and fails on
  ambiguous or unreadable evidence. Native Windows uses a representative mapping
  from direct Win32 evidence through pinned CLDR data — a mapping policy, not
  discovery of a unique city. The exact discovery rules are in the design's
  timezone section and [CONFIGURATION.md](../../CONFIGURATION.md).
- Estimated cost is daemon-owned valuation at report time. There is no
  HTTP-side or CLI-side pricing, no report-time network fetch, and no Catalog
  normalization or Requested-model substitution. The extension is additive on
  the same route and schema version: a client treats a wholly absent extension
  as an older daemon and rejects recognized partial shapes.
- JSON output is the validated original response bytes. Terminal `Total` rows
  are presentation only, derived from validated rows with checked arithmetic,
  and never replace the server's aggregates. There is no cross-Surface total.
- The CLI requires a running reachable daemon; offline history and reporting
  while the Usage meter is disabled remain outside this design.
- The HTTP handler has no upstream dependency and is not a project Endpoint,
  Route, or Surface. It retains local errors instead of borrowing an inference
  error dialect, and introduces no change to forwarded Copilot responses.
