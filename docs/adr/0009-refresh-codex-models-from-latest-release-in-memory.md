# Refresh Codex models from the latest release in memory

**Status:** accepted

This decision amends ADR-0005 for freshness and version pinning:
`rust-v0.144.5` becomes the embedded floor rather than the sole served source.
ADR-0005's complete-entry fidelity and enumerated-mutation rules remain
unchanged; the accept-gate rejects any fetched entry that cannot satisfy that
required `ModelInfo` contract. It also amends ADR-0008 by generalizing
`versionFact` into the cache registry and moving per-fact freshness into the
uniform `/readyz` `caches` block while retaining effective impersonation headers.
The embedded floor may advance independently when a newer stable snapshot passes
that contract; it was advanced to `rust-v0.153.4` on 2026-09-05.

The Codex catalog serves `models.json` from a memory-only **cached value**. Its
embedded [vendored snapshot](../../internal/catalog/codexdata/models.json), with
current identity in [release.json](../../internal/catalog/codexdata/release.json)
and audit evidence in [PROVENANCE.md](../../internal/catalog/codexdata/PROVENANCE.md),
is the guaranteed-parseable **fallback**. When the Codex catalog is enabled,
copilotd checks
`openai/codex`'s latest GitHub release tag at startup and on a 24-hour-by-default
cadence, resolves that stable tag to its commit, then fetches
`codex-rs/models-manager/models.json` at the immutable commit SHA. The tag stays
the human-readable cached-value version; an unchanged tag holds the already
accepted commit's content. The **refresh ladder** rejects malformed content through
`validateCodexModels`, holds last-good on failure, and returns to the embedded
allocation when fetched bytes equal the floor. The read path decodes the
currently served bytes with `parseCodexModels` and threads that model map into
the Codex renderer without repeating accept-time contract validation; it does
not retain a second parsed copy.

This outbound dependency is public and credential-isolated. A dedicated plain
HTTP client talks to GitHub without the API key, GitHub OAuth token, Copilot
token, or impersonation headers, and each release peek and content read has its
own five-second context bound. `--codex-catalog-enabled=false` registers no
`codex_models` cached value and performs no request. A
`--codex-catalog-refresh-interval=0` value keeps the enabled catalog pinned to
the embedded fallback while retaining its non-secret `/readyz` observation.

## Considered options

- **Keep the vendored snapshot static** — rejected because Codex's internal
  `ModelInfo` evolves independently and the shipped catalog silently becomes
  stale until copilotd is rebuilt.
- **Track the default-branch head** — rejected because an unreleased prompt or
  schema edit could become live immediately. A stable release tag is reviewed
  and human-readable; resolving it before content retrieval gives the fetched
  artifact an immutable commit identity.
- **Persist fetched bytes** — rejected. The embedded fallback makes disk state
  unnecessary, and persistence would violate the ROADMAP's single-file
  state-at-rest boundary.
- **Reuse the GitHub OAuth client or token** — rejected. The public release and
  content reads need no identity; isolation prevents accidental credential
  disclosure and keeps this best-effort dependency outside the identity
  lifecycle.

## Consequences

The Codex catalog can advance between copilotd releases without becoming less
reliable than the vendored floor. A schema drift that
`validateCodexModels` cannot honor never replaces a good value. Startup and
periodic failures do not affect readiness. `/readyz` reports `codex_models`
through the cache registry only when the catalog is enabled.

The process now holds externally fetched content in memory. This is consistent
with ROADMAP principle 4: memory-only cached values are not state at rest;
nothing new is written beside the single owner-only GitHub OAuth token file.
