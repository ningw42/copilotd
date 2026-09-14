# models.dev pricing floor provenance

## Current vendored artifact identity

[`identity.json`](identity.json) is the single checked-in authority for the
current artifact and source-audit identities. Its `artifact` record identifies
the byte-for-byte [`api.json`](api.json) response by URL, UTC observation time,
byte size, and SHA-256. The pricing package checks its actual embedded bytes
against the recorded size and digest; the pinned cache test uses that same
record for the expected content version.

The live artifact URL is mutable and the response does not contain a source
commit. Exact response bytes, including whether a terminal newline is present,
are preserved. The repository's EOF fixer excludes exactly this artifact path.

## Separate source and license audit

The manifest's independent `source_audit` record identifies the source
repository, audit time, immutable commit, schema and license paths, and the
vendored [`LICENSE`](LICENSE) SHA-256. Tests check manifest completeness and
identity formats, and hash the embedded license against that record.

The audited commit is evidence for the source schema and applicable MIT license.
It does not identify the separately fetched live `api.json` bytes. The
[2026-09-12 service-tier design](../../../../docs/design/2026-09-12-openai-service-tier-pricing-design.md)
preserves dated schema and pricing observations; those historical identities
remain tied to their original observations when the current floor advances.

## Selected-projection contract

The selected projection follows the observed schema names: root provider keys,
provider `id` and `models`, model `id` and optional `cost`, recognized `input`,
`output`, `reasoning`, `cache_read`, `cache_write`, `input_audio`, and
`output_audio` rates, structured `tiers[].tier` context `size`, and the generated
deprecated `context_over_200k` row. The projection retains the base vector and
every valid structured context tier, ordered by exact `tier.size`; structured
tiers are authoritative for every provider. The generated legacy row is retained
only as a strict 200,000-token compatibility tier when structured tiers are
absent, because its name can discard a higher exact threshold. Each chargeable
rate keeps its source presence; an absent selected-row rate is not filled from
another row.

For OpenAI models, the projection also validates optional `experimental.modes`
and retains at most one Fast declaration identified by an ASCII-case `fast` or
`priority` mode name or `provider.body.service_tier` mapping. Every recognized
mode cost field is validated, including modes that are not selected. The Fast
base vector is retained exactly as authored. For each retained normal context
tier, the projection derives each available Fast category exactly as `Fast base
× normal context / normal base`, with no intermediate rounding or borrowed
factor. Missing/zero-denominator/non-terminating/out-of-bounds categories remain
absent. Explicit Fast `tiers` or `context_over_200k` shapes are rejected rather
than silently ignored because supporting them would require a new selection
policy. Other modes remain outside the retained pricing projection. The accepted
artifact bytes, content hash, source audit identity, and provider/model identity
accounting are unchanged by these derived values.

## Bumping the floor

1. Fetch the manifest's `artifact.url` deliberately into a temporary file. Do
   not fetch during tests or builds. Record the UTC completion time, exact byte
   size, and full SHA-256 before changing the tree.
2. Independently resolve the current commit of the recorded source repository
   and audit its schema and license at the recorded paths at that immutable
   commit. Do not label the live artifact with this commit.
3. Replace `api.json` without reformatting or re-encoding it. Replace `LICENSE`
   only with the exact audited license bytes.
4. Update `identity.json` with the fetched URL/time/size/SHA and, separately,
   the source repository/audit time/commit/schema/license identity.
5. Review schema changes against the selected-projection accept contract. A
   routine data bump needs no edits to runtime/cache code, tests, configuration,
   or this guide. Update them only for a real schema, policy, source-location,
   or legal change; add a focused public-seam test before changing accepted
   fields or selection rules. Keep synthetic fixtures and dated audit evidence
   fixed rather than retargeting them to new prices or model counts.
6. Run the focused checks and the complete local verification through Nix:

   ```sh
   nix fmt
   nix develop -c go test ./internal/usage/pricing/... -count=1
   nix flake check
   ```

The pricing package's identity test hashes its private production embed of
`modelsdevdata/api.json`; the modelsdevdata tests verify the manifest and license
separately. A manifest-only update is not evidence of a successful bump.
