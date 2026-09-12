# models.dev pricing floor provenance

## Fetched artifact identity

`api.json` is the byte-for-byte response fetched from:

- URL: `https://models.dev/api.json`
- Completed observation: `2026-09-11T11:57:10Z`
- Size: `4,584,629` bytes
- SHA-256: `db685655368231dce789b060e493a21899cb861c9930cc52521795776ab6e3b5`

These facts identify the vendored artifact. The live response does not contain a
source commit, and the service did not expose an immutable artifact URL during
this observation. The response has no terminal newline; the repository's EOF
fixer excludes exactly this path so normal hooks preserve the identified bytes.

## Separate source and license audit

The source schema and MIT license were separately audited at
`anomalyco/models.dev` commit
`b8244af8bd1ca8c0e60103f058854d5510a2534d`, observed at
`2026-09-11T11:57:03Z`:

- Repository: `https://github.com/anomalyco/models.dev`
- Schema: `packages/core/src/schema.ts`
- License: `LICENSE`
- Vendored license SHA-256:
  `dc2dc41c9fea2fd3c41c21c6f484844cab367800b88ff24781f980ad3a9d160a`

That commit is audit evidence for the source schema and applicable license. It
is deliberately **not** claimed as the source identity of the separately
fetched live `api.json` bytes.

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

1. Fetch `https://models.dev/api.json` deliberately into a temporary file. Do
   not fetch during tests or builds. Record the UTC completion time, exact byte
   size, and full SHA-256 before changing the tree.
2. Independently resolve the current `anomalyco/models.dev` source commit and
   audit `packages/core/src/schema.ts` plus `LICENSE` at that immutable commit.
   Do not label the live artifact with this commit.
3. Replace `api.json` without reformatting or re-encoding it. Replace `LICENSE`
   only with the exact audited license bytes.
4. Update `identity.json` with the fetched URL/time/size/SHA and, separately,
   the source repository/audit time/commit/schema/license identity.
5. Review schema changes against the selected-projection accept contract. Add a
   focused public-seam test before changing accepted fields or selection rules.
6. Run through Nix at least:

   ```sh
   nix develop -c go test ./internal/usage/pricing/modelsdevdata -count=1
   nix develop -c go test ./internal/usage/pricing -count=1
   ```

The pricing package's identity test hashes its private production embed of
`modelsdevdata/api.json`; the modelsdevdata tests verify the manifest and license
separately. A manifest-only update is not evidence of a successful bump.
