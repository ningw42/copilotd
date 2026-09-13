# CLDR Windows-zone mapping provenance

## Pinned source

The representative native-Windows report-timezone policy uses the ordered
`mapZone` candidates from Unicode CLDR **release 48**. The checked-in source is
an unmodified copy of:

- Repository: `https://github.com/unicode-org/cldr`
- Tag: `release-48`
- Tag target / immutable commit:
  `acd6d88ae493633240e19a87a721076a8a75c310`
- Commit tree: `bafae8fc919257506cb84327781ce4912b9b0c0b`
- Source path: `common/supplemental/windowsZones.xml`
- Git blob: `26a62c3f0645851fa13ebddbce9b6f20528d5777`
- Size: 49,378 bytes
- SHA-256:
  `9cf3db6a31fb382fee21b70be6feba1e82766b0fcd06e6261fb7936a73e537ff`

GitHub's tag-ref API reported that `release-48` directly targets the recorded
commit. The commit API reported the recorded tree. GitHub's contents API
reported the source blob and size; the SHA-256 was independently computed over
the immutable raw-commit response. [`identity.json`](identity.json) is the
machine-readable authority for these facts.

The XML itself reports Windows mapping version `7e11800` and TZDB type version
`2021a`. Those are upstream mapping metadata, not substitutes for the CLDR
release, commit, and content hash.

## License

[`LICENSE`](LICENSE) is copied unmodified from the same CLDR commit:

- Git blob: `861b74f3c812088755fd185d964e82a244503bbd`
- Size: 2,033 bytes
- SHA-256:
  `b4c0ae8ef04f7059f96ce5bbe0467f9fe6f6d81bbe13517701dfeb961fb4d0b6`
- SPDX identifier: `Unicode-3.0`

The generated Go table is a mechanical representation of that licensed data.
Candidate order inside each `mapZone type="..."` attribute is preserved because
the selection policy deliberately chooses the first candidate.

## Offline deterministic regeneration

The generator reads only the vendored XML and identity manifest. It performs no
network or tag resolution, verifies the source size/hash, validates the mapping
shape and required `001` defaults, sorts map keys for stable Go output, and
preserves candidate order:

```sh
go generate ./internal/usage/reportcli/windowszonesdata
# Or directly:
go run ./scripts/generate-windows-zones
```

Check that generated output is current without modifying the tree:

```sh
go run ./scripts/generate-windows-zones -check
```

The package tests run the check with `GOPROXY=off`, pin the independent source
and license identities, exercise representative exact/default/ordered/absent
mappings, and validate every emitted name through the shared report timezone
loader.

## Upgrade policy

A CLDR upgrade is a separately reviewed data change. Resolve and record the new
release tag's immutable commit and tree; fetch the XML and Unicode license by
that commit; independently verify Git blobs, byte sizes, and SHA-256 values;
update `identity.json`, this provenance record, and the vendored bytes; regenerate
the table; then review mapping changes and rerun the complete Usage verification
gate. Builds and tests must never fetch CLDR dynamically.
