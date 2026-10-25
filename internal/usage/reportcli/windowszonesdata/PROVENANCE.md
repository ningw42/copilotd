# CLDR Windows-zone mapping provenance

## Pinned source

The representative native-Windows report-timezone policy uses the ordered
`mapZone` candidates from Unicode CLDR **release 49**. The checked-in source is
an unmodified copy of:

- Repository: `https://github.com/unicode-org/cldr`
- Tag: `release-49`
- Tag target / immutable commit:
  `96e54b273d2da4ec6fbad350ba19ff23fa24a588`
- Commit tree: `bd84336f381c121e90534054a441c37fe0d9d500`
- Source path: `common/supplemental/windowsZones.xml`
- Git blob: `2dc8c386cb3aed1a8c21b4ba7bb7b8d4f3535507`
- Size: 49,892 bytes
- SHA-256:
  `5f902f94106a396cb7774f0b0e1ca4dfecb686494045350c0b4935d378e3913b`

GitHub's tag-ref API reported that `release-49` directly targets the recorded
commit. The commit API reported the recorded tree. GitHub's contents API
reported the source blob and size; the SHA-256 was independently computed over
the immutable raw-commit response. [`identity.json`](identity.json) is the
machine-readable authority for these facts.

The XML itself reports Windows mapping version `7e11800` and TZDB type version
`2021a`. Those are upstream mapping metadata, not substitutes for the CLDR
release, commit, and content hash.

## License

[`LICENSE`](LICENSE) is copied unmodified from the same CLDR commit:

- Git blob: `b4132054a5d2b95b6f1a4c8b511a211086023e55`
- Size: 3,193 bytes
- SHA-256:
  `65eb33419e22a297550a811b5b535f5e532f9a7e0114b516c0ea18afd53c3804`
- SPDX identifier: `Unicode-3.0`

Release 49 adds a separate notice restricting the LDML specification itself;
all other repository content, including this mapping data, remains under the
Unicode License V3. The complete upstream notice is retained.

The generated Go table is a mechanical representation of that licensed data.
Candidate order inside each `mapZone type="..."` attribute is preserved because
the selection policy deliberately chooses the first candidate.

## Offline deterministic regeneration

The generator reads only the vendored XML and identity manifest. It performs no
network or tag resolution, verifies the source size/hash, validates the mapping
shape and required `001` defaults, sorts map keys for stable Go output, and
preserves candidate order:

```sh
nix develop -c go generate ./internal/usage/reportcli/windowszonesdata
# Or directly:
nix develop -c go run ./scripts/generate-windows-zones
```

Check that generated output is current without modifying the tree:

```sh
nix develop -c go run ./scripts/generate-windows-zones -check
```

The package tests run the check with `GOPROXY=off`, pin the independent source
and license identities, exercise representative exact/default/ordered/absent
mappings, and validate every emitted name through the shared report timezone
loader.

## Release 49 mapping audit

The 2026-10-25 audit compared exact release-48 and release-49 XML bytes. The
new source adds British Columbia, Alberta, and Manitoba Windows keys and moves
the corresponding Canadian candidates out of the Pacific, Mountain, and
Central mappings. It retains candidate order and the existing `001` defaults
for those older keys. The inventory increases from 139 to 142 Windows keys
and retains the same 445 distinct IANA names. Focused tests cover the new keys
and changed or removed Canadian mappings.

## Upgrade policy

A CLDR upgrade is a separately reviewed data change. Resolve and record the new
release tag's immutable commit and tree; fetch the XML and Unicode license by
that commit; independently verify Git blobs, byte sizes, and SHA-256 values;
update `identity.json`, this provenance record, and the vendored bytes; regenerate
the table; then review mapping changes and rerun the complete Usage verification
gate. Builds and tests must never fetch CLDR dynamically.
