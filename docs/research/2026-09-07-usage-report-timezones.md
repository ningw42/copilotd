# Usage report: terminal-local named timezone feasibility

**Observed:** 2026-09-07. **Scope:** design only; no production implementation.
Targets: linux/amd64, windows/amd64, windows/arm64, darwin/arm64; CGO disabled.

**Design disposition:** the [reporting design](../design/2026-09-07-usage-reporting-design.md)
selects the conservative platform approach below, but not the optional generated
name allowlist. It also recognizes an explicitly empty Unix OS `TZ` as the
configured UTC choice, rather than treating that case as failed discovery.
Recommendations below remain research alternatives; the design owns policy.

## Executive finding

A single pure-Go binary can load named calendar zones without installed tzdata.
Discovering the terminal's **name** is separate: Unix usually supplies usable
configuration; native Windows supplies Windows identifiers, not an unambiguous
IANA identifier. Recommend conservative Unix discovery and an explicit
`--timezone` requirement on native Windows in v1, rather than a mapping dependency.
Never substitute the daemon's timezone, a current offset, or Go's UTC fallback.

## Verified primary-source facts

- The repository's Nix shell reports **go1.27.1 linux/amd64**. Source inspected at
  `$GOROOT/src/time`, with GOROOT
  `/nix/store/s365s84b3hysnazdw7h0lcyk5hfizrny-go-1.27.1/share/go`.
  Nix patches Unix zone search to prepend its tzdata store directory; this path
  is not portable to deployed machines. Upstream references are pinned below.
- `Location.String()` returns its descriptive name, including caller-chosen
  `FixedZone` names. Unix loading `/etc/localtime` sets that name to `Local`;
  Windows does likewise. `Time.Zone()` returns an abbreviation and offset **at
  that instant**. Neither is an IANA-name discovery API. [G1][G2][G3][G4]
- Go Unix initialization checks `TZ`: unset reads `/etc/localtime`; empty means
  UTC; one leading colon is removed; absolute paths load files; other values
  are looked up as zone files. Failure silently sets `Local` to UTC. The CLI
  must inspect configuration itself, not trust that fallback. [G2]
- GNU libc supports geographical/file and POSIX/proleptic rule forms of `TZ`.
  Apple also documents file and rule forms. Go's Unix initializer does **not**
  parse arbitrary POSIX rules from `TZ`; its rule parser serves TZif extension
  rules. Thus a terminal's `TZ=EST5EDT,M3.2.0,M11.1.0` can work in libc but fail
  in Go. Some rule-looking strings, e.g. `EST5EDT`, also name TZDB entries;
  loading one does not prove which interpretation the terminal intended. [G1][G2][U1][A2]
- systemd identifies the system zone from `/etc/localtime`, an absolute or
  relative symlink to a zoneinfo file. A copied TZif file supplies rules but
  does not preserve that configuration name. Missing localtime defaults to UTC
  in systemd/Go, but is not positive evidence of a named selection. [U2][G2]
- `/etc/timezone` is not a portable authority. Debian says systemd and some
  desktops change only `/etc/localtime`; since tzdata 2024b-5 it no longer
  automatically creates `/etc/timezone`. An existing file can therefore be
  stale; absence is normal. [U3][U4]
- Apple's source defines macOS localtime as `/etc/localtime`, and modern TZDIR
  as `/var/db/timezone/zoneinfo` (older configurations use `/usr/share/zoneinfo`).
  It expressly does **not** guarantee TZDIR prefixes the localtime symlink,
  but says the prefix's final component is `zoneinfo`. [A1]
- Windows `GetDynamicTimeZoneInformation` exposes `TimeZoneKeyName`, a registry
  key name, plus dynamic-DST settings. `StandardName`/`DaylightName` are
  descriptions, not IANA IDs. Go instead calls `GetTimeZoneInformation`, names
  the location `Local`, and documents historical/future DST limitations. [W1][W2][G3]
- CLDR's Windows mapping is territory-dependent and sometimes one-to-many
  **within** a territory. `Central Standard Time` maps to several US zones;
  `Pacific Standard Time` maps to Los Angeles for US and Vancouver for CA.
  Territory `001` is a default, not discovery of the user's actual locality.
  Custom/disabled dynamic-DST settings add further fidelity problems. [W1][W3][W4]

## Embedded data: sufficient for loading, not strict identity proof

Importing `_ "time/tzdata"` (or building with `-tags timetzdata`) embeds the
TZDB, documented as about 450 KB. It uses Go code and needs no CGO. [G5]
`LoadLocation` can consequently resolve known names on hosts with no data.
However, reject `""` and `"Local"` explicitly: both successfully load special
locations. `UTC` is a legitimate explicit named choice. [G1]

Embedding is a **fallback**, not an embedded-only validator. `ZONEINFO` and
platform files take precedence and can contain arbitrary names or modified
rules. The inspected implementation tries embedded data before GOROOT (the
public comment lists those last two in the reverse order). The package exports
no API to enumerate names or force only its embedded database. [G1][G5][G6]
For strict portable-name validation, recommend a generated, embedded name set
from the pinned Go `lib/time/zoneinfo.zip`, plus `UTC`, followed by
`LoadLocation`. This is not a Windows mapping dependency. Refresh the set with
the toolchain. It proves name membership, **not identical rules across hosts**;
forcing one rules snapshot would require owning embedded TZif data and using
`LoadLocationFromTZData`, rather than relying solely on `time/tzdata`. [G6]

## Recommended conservative v1 algorithm

1. Define “terminal-local” as configuration visible to the **CLI process**,
   not the physical workstation behind SSH, a container, WSL, or a remote daemon.
2. If `--timezone` is present, validate it and send that exact accepted name;
   invalid/empty values error, without trying detection. Accept `UTC` and
   slash-containing names/aliases in the pinned name set (e.g. `Europe/Paris`,
   `US/Eastern`, `Etc/UTC`). Reject `Local`, bare abbreviations/legacy short
   names, paths, `.`/`..` components, backslashes, offsets and POSIX rules.
   Requiring slash-style names except UTC is deliberately stricter than TZDB;
   e.g. use `Asia/Tokyo`, not `Japan`. Do not infer an `Etc/GMT*` from an offset.
3. On Linux/macOS, inspect `TZ` with presence distinguished from emptiness.
   If present, remove at most one leading colon; accept a validated name, or
   resolve an absolute zone-file path using step 4. Empty, invalid, unsupported
   rule/custom-file values error immediately—do not fall through to OS default.
   Empty `TZ` means UTC to Go/libc, but this policy requires `--timezone UTC`
   because it contains no named selection. Treat `TZDIR`/`ZONEINFO` values whose
   cleaned paths equal a platform root path in step 4 as system data (including
   NixOS's system-exported `TZDIR=/etc/zoneinfo`); custom values remain
   unsupported for automatic discovery and require `--timezone`.
4. With `TZ` absent, inspect `/etc/localtime`. Traverse relative/absolute
   symlinks with a bound, rejecting loops, missing/unreadable targets and
   inconsistent results. On Linux recognize installed zoneinfo roots
   `/usr/share/zoneinfo`, `/usr/share/lib/zoneinfo`, `/usr/lib/locale/TZ`,
   `/etc/zoneinfo`, including their resolved roots (important on NixOS).
   On macOS use Apple's documented terminal `zoneinfo/` path component,
   including `/var/db/timezone/zoneinfo` and resolved/versioned prefixes.
   Preserve the suffix while resolving root aliases; do not merely split an
   arbitrary Linux pathname containing `zoneinfo`. Require one distinct
   validated suffix and a readable TZif target. Reject `right/` and `posix/`
   trees rather than stripping them, custom files, and ordinary copied files.
   For a `TZ` absolute path directly under a recognized root, the path itself
   can supply the suffix; `/etc/localtime` still needs a name-bearing symlink.
5. Do **not** use `/etc/timezone` alone or reverse-match copied TZif bytes.
   Ignore deprecated `/etc/timezone` when an authoritative localtime link is
   usable; otherwise error. This deliberately excludes older copied-file
   installations and some containers rather than trusting stale metadata.
6. On native Windows, absent `--timezone`, error explaining that Windows zone
   IDs cannot be conservatively auto-mapped in v1. Do not use registry display
   names, offset matching, or CLDR's `001` default. Do not interpret Windows
   `TZ` as Unix terminal-local discovery: Go's Windows local initialization
   ignores it. WSL follows the Linux rules, not native Windows rules. [G3]
7. On success send the name to the daemon for calendar calculations. On any
   unsupported/ambiguous discovery: `cannot determine a named local timezone
   (<reason>); pass --timezone Area/City (or --timezone UTC)`; make no request.
   Explicit override bypasses discovery, not validation. No silent UTC path.

## Experiments and native-runtime gaps

Temporary probe only, under `/tmp/copilotd-tz-research`; no production code.
All Go commands ran through `nix develop /home/ning/github/copilotd -c ...`.

- `CGO_ENABLED=0` probe with `time/tzdata` cross-built successfully for all four
  requested targets. The pinned `go test time -run '^TestEmbeddedTZData$'
  -count=1` also passed with CGO disabled.
- **Native linux/amd64:** ran the static probe inside `unshare -Ur chroot`
  with no system/Nix/GOROOT zone database and `ZONEINFO=/absent`,
  `GOROOT=/absent`. New York loaded winter/summer offsets -18000/-14400;
  Tokyo, `US/Eastern`, and UTC loaded. Invalid names/rules errored.
- In that isolation, bad/rule `TZ` silently made Go Local UTC; copied
  `/etc/localtime` yielded `Local`; custom `TZ=/custom` yielded `/custom`.
  A fabricated `Fictional/Invented` TZif under `ZONEINFO` loaded successfully,
  confirming why plain `LoadLocation` is not an IANA-membership check.
- No native Windows or macOS execution was available. Cross-compilation proves
  build feasibility, not detection correctness. Before implementation ships,
  native macOS must verify current symlink/versioned-root layouts, relative
  links and failures; native Windows on both architectures must verify explicit
  zones without installed tzdata and the intentional no-auto error.
  Linux symlink detection itself remains a proposed algorithm, not tested code.
- Guarantee is a portable selected **identifier**, not recovery of arbitrary
  custom OS rules or equal client/daemon tzdata revisions. Newer-than-bundled
  identifiers may require a newer release; do not mislabel that as UTC.

## Primary sources

[G1]: https://github.com/golang/go/blob/go1.27.1/src/time/zoneinfo.go
[G2]: https://github.com/golang/go/blob/go1.27.1/src/time/zoneinfo_unix.go
[G3]: https://github.com/golang/go/blob/go1.27.1/src/time/zoneinfo_windows.go
[G4]: https://github.com/golang/go/blob/go1.27.1/src/time/time.go
[G5]: https://github.com/golang/go/blob/go1.27.1/src/time/tzdata/tzdata.go
[G6]: https://github.com/golang/go/blob/go1.27.1/src/time/zoneinfo_read.go
[U1]: https://sourceware.org/glibc/manual/latest/html_node/TZ-Variable.html
[U2]: https://www.freedesktop.org/software/systemd/man/254/localtime.html
[U3]: https://salsa.debian.org/glibc-team/tzdata/-/blob/debian/2025b-1/debian/NEWS
[U4]: https://salsa.debian.org/glibc-team/tzdata/-/blob/debian/2025b-1/debian/README.Debian
[A1]: https://github.com/apple-oss-distributions/Libc/blob/71bbe350ab79eef58113991d817ccc6165061a64/stdtime/FreeBSD/tzfile.h#L39-L57
[A2]: https://github.com/apple-oss-distributions/Libc/blob/71bbe350ab79eef58113991d817ccc6165061a64/gen/tzset.3
[W1]: https://learn.microsoft.com/en-us/windows/win32/api/timezoneapi/ns-timezoneapi-dynamic_time_zone_information
[W2]: https://learn.microsoft.com/en-us/windows/win32/api/timezoneapi/nf-timezoneapi-getdynamictimezoneinformation
[W3]: https://unicode.org/reports/tr35/tr35-dates.html#Windows_Zones
[W4]: https://github.com/unicode-org/cldr/blob/release-48/common/supplemental/windowsZones.xml
