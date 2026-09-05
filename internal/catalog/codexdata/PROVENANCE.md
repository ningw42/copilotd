# Provenance and upstream contract

## Current vendored snapshot identity

[`release.json`](release.json) is the single checked-in authority for the current
Codex release and artifact identities. It records the source repository, stable
tag and peeled commit; the `models.json` path, Git blob, SHA-256, byte size, and
audited bundled default; the release/tag/manifest audit identities; and the exact
executable-audit asset, archive digest, and executable digest.

`models.json` is vendored without modification from the source recorded there.
`LICENSE` and `NOTICE` are copied from the repository root at the same audited
commit.

The round-trip test computes SHA-256, byte size, and Git's
`blob <length>\0<content>` object identity over the actual embedded production
bytes and requires the independent checked-in values in `release.json`.

## Stable release audit

The audit metadata in `release.json` was collected from GitHub's first-party
`/repos/openai/codex/releases/latest` response and the annotated tag object. The
release was published as stable (`draft: false`, `prerelease: false`), and the
recorded tag object peels to the recorded commit; that commit, rather than the
release's `target_commitish: "main"`, is the source pin.

The runtime cached value keeps the stable tag as its cheap version label, then
resolves a changed tag through GitHub's commit endpoint and requests
`models.json` with `ref=<40-hex commit>`. This closes the tag-movement window
between resolution and content retrieval, and an unchanged tag cannot silently
replace last-good bytes. Commit resolution requests the 40-byte GitHub SHA media
type, and every response on this credential-free edge is capped at 8 MiB.

The release also publishes the manifest asset identified in `release.json`.
GitHub's generated commit tarball and commit ZIP have no API-provided digest, so
they are not used as the vendored snapshot identity.

## Historical contract-audit baseline

The deserialization, merge, selection, Guardian, and executable observations
below were audited against `rust-v0.153.4` on 2026-09-05. Their immutable source
citations intentionally remain pinned to that reviewed commit as historical
contract evidence; they are not a second current-release identity. The current
artifact identity remains `release.json`.

For that bump, `LICENSE` and `NOTICE` were byte-identical to their
`rust-v0.152.1` counterparts; `models.json` added the `gpt-6-astra` entry and
updated existing model metadata. GitHub reported the audited release as
`immutable: false`, and its tag and commit were unsigned. The commit and
individual Git blob IDs were therefore the durable identities; the release/tag
names established which stable release was current at that audit cutoff.

First-party evidence retained for that dated audit includes the GitHub
[release response][audit-release], [tag object][audit-tag-object],
[commit][audit-commit], [`models.json` blob][audit-catalog-object] and
[raw bytes][audit-catalog-raw], [manifest metadata][audit-checksums], generated
[source tarball][audit-source-tar] and [source ZIP][audit-source-zip], and the
[executable asset][audit-codex-binary].

## Deserialization contract and test grounding

`TestVendoredCodexCatalogRoundTripFidelity` proves that the exact vendored
snapshot passes validation, cache acceptance, handler rendering, reviewer
resolution, a derived limits overlay, and field-for-field preservation. Codex's
`ModelInfo` and `ModelMessages` schema was unchanged from `rust-v0.152.1`; the
audited snapshot positively exercises several optional paths that previously had
only synthetic witnesses. Synthetic contract tests retain coverage of absent,
nullable, defaulted, and malformed forms.

`ModelsResponse` requires a non-null `models` array. Each element is decoded
as `ModelInfo`; unknown object members are accepted because the type does not
deny unknown fields. The audited bundled file has eleven entries and contains
legacy/transport members not present in `ModelInfo` (including `available_in_plans`,
`minimal_client_version`, `prefer_websockets`, `requires_sandboxed_review`,
`supports_parallel_tool_calls`, and `supports_reasoning_summaries`). They are
ignored by Rust deserialization but remain upstream bytes that copilotd's raw
fidelity contract must preserve. [Types][model-types] [catalog][catalog]

The following non-optional `ModelInfo` members have no Serde default and are
therefore required, non-null, and correctly typed: `slug`, `display_name`,
`supported_reasoning_levels`, `shell_type`, `visibility`, `supported_in_api`,
`priority`, `support_verbosity`, `truncation_policy`, and
`experimental_supported_tools`. The Rust type itself permits empty strings,
empty vectors, and any signed `i64` truncation limit; enum parsing is the type
constraint. `shell_type` also accepts the legacy aliases `default`, `local`,
and `shell_command`. [Types][model-types] [enum tests][model-tests]

Missing values receive these defaults (explicit JSON `null` still fails for
the non-optional values): `additional_speed_tiers` and `service_tiers` become
empty arrays; the three instruction-inclusion flags become `false`, `false`,
and `true`; `supports_reasoning_summary_parameter` becomes `true`;
`default_reasoning_summary` becomes `auto`; `web_search_tool_type` becomes
`text`; `supports_image_detail_original`, `supports_search_tool`,
`use_responses_lite`, `node_repl_auto_review_required`, and
`node_repl_disabled` become `false`; `effective_context_window_percent`
becomes `95`; and `input_modalities` becomes `["text", "image"]`.
[Types][model-types] [default tests][model-tests]

The optional members accept both absence and JSON `null`: `description`,
`default_reasoning_level`, `default_service_tier`, `availability_nux`,
`upgrade`, `model_messages`, `default_verbosity`, `apply_patch_tool_type`,
`context_window`, `max_context_window`, `auto_compact_token_limit`, `comp_hash`,
`auto_review_model_override`, `model_specialty`, `tool_mode`,
`multi_agent_version`, and `multi_agent_reasoning_effort`. Unknown non-empty
reasoning-effort strings are retained as custom efforts, while an empty effort
is rejected. Unknown string values for `tool_mode` and `multi_agent_version`
are intentionally downgraded to `None`; non-string values still fail.
[Types][model-types] [reasoning tests][model-tests]

`ModelMessages` itself is optional. Its instruction template, instruction
variables, built-in tool messages,
approval/collaboration/auto-review/permission/multi-agent sections, persistent
instructions, token-budget settings, Guardian V2 settings, and confirmation
policies are all optional and accept missing or `null`. The optional
`tools.send_user_message_async.description`, `auto_review.node_repl_policy`,
and `multi_agent.mode.proactive` leaves likewise accept missing, `null`, or a
string. When one of the nested objects is present, its non-optional leaves
remain required. `ModelTokenBudgetConfig`'s five original leaves are required;
its new `enabled` and `use_history_notes_extension` booleans default to `false`
when absent but reject explicit `null` or a non-boolean. Other option-valued
leaves preserve the distinction between a missing/null value and an empty
string. Guardian V2 and its transcript contain only optional leaves.
[Messages][model-messages] [Guardian V2][guardian-v2]
[message tests][model-tests]

Instruction validity is a `ModelsResponse` semantic, not a requirement that
both representations be populated. A legacy `base_instructions` string is
promoted into `model_messages.instructions_template` only when the canonical
template is absent. The canonical template wins when both exist. A model is
rejected only when neither source yields `Some(template)`; an empty string is
still `Some` and is accepted. Serialization always re-adds the legacy
`base_instructions`, rendering the default personality when variables exist.
[Promotion code][instruction-promotion] [promotion tests][model-tests]

copilotd intentionally remains stricter than Serde for empty effective
instructions: although `Some("")` deserializes, Codex then sends empty model
instructions. The accept gate therefore permits either representation but
requires the representation Codex will select to be non-empty. Empty slugs,
empty display names, and duplicate slugs are likewise rejected as local
semantic-safety constraints rather than Rust presence constraints.

The ten legacy catalog entries supply both instruction forms. The new
`gpt-6-astra` entry has no `base_instructions` and uses only a non-empty
canonical `model_messages.instructions_template`, directly witnessing Codex's
promotion contract. Seven entries explicitly set `instructions_variables` to
`null`; the other four use an object. Sibling sections (`auto_review`,
`collaboration_modes`, `guardian_v2`, `multi_agent`, `permissions`, and
`token_budget`) are present as objects or explicit `null` and must survive
copilotd rendering unchanged unless an ADR-governed overlay names the field. [Catalog][catalog]

The vendored snapshot still does not positively exercise every mirrored
optional/default field: `effective_context_window_percent` is absent from all
eleven entries; `availability_nux` is null throughout; and `tools` is null on
`gpt-6-astra` and absent elsewhere, so no catalog entry exercises
`tools.send_user_message_async`. Astra now positively exercises
`multi_agent_reasoning_effort`, `persistent_instructions`,
`confirmation_policies`, `multi_agent.mode.proactive`, and both defaulted token
budget flags. Astra, Luna, and `codex-auto-review` also populate the audited
`auto_review` leaves, including `node_repl_policy`. Synthetic positive and
malformed-shape tests cover the remaining validator paths.

Reasoning levels are an array of `{effort, description}` objects: both members
are required, descriptions are strings, and effort is any non-empty string
(known values at the audited baseline include `none`, `minimal`, `low`, `medium`, `high`,
`xhigh`, `max`, `ultra`, and `persistent`). Truncation is a required
`{mode, limit}` object whose mode is `bytes` or `tokens`. Input modalities, tool
selectors, visibility, verbosity, service tiers, context/compaction values,
and the instruction/capability flags are consumed from `ModelInfo`, so raw
preservation is behaviorally significant even where a field has a default.
[Types][model-types] [runtime model use][model-runtime]

## Remote loading, merge, selection, and Guardian behavior

Codex fetches `GET <provider-base>/models?client_version=<major.minor.patch>`
with a five-second bound. Existing provider query parameters precede the
`client_version` parameter. The request carries provider-configured headers,
the default `originator` and `User-Agent`, and provider auth; command auth trims
the command's stdout and applies it as `Authorization: Bearer <token>`. The
response body is the `{"models":[...]}` `ModelsResponse` envelope; an `ETag`
response header is cached. Prerelease suffixes are stripped from the package
version used for both the query and cache compatibility. [Models client][models-client]
[endpoint][models-endpoint] [default headers][default-headers]
[command auth][command-auth] [version][models-lib]

A refresh is attempted only for Codex-backend auth or a provider with command
auth. When the current auth uses the Codex backend, a non-empty remote catalog
containing at least one `visibility: "list"` entry replaces the entire bundled
catalog. Otherwise
(including command/API-key auth), Codex starts from the bundle, replaces each
matching slug wholesale, and appends new slugs; an empty response retains the
bundle. Picker candidates are sorted by ascending numeric priority, API-key
mode filters out `supported_in_api: false`, `visibility: "list"` controls picker
display, and the first picker-visible result becomes the default. At the audited
baseline, visible priority-1 `gpt-6-astra` is the bundled default. An explicitly
configured model is preserved by the dynamic manager. If no model is
picker-visible, the first model is the default fallback. [Manager][manager]
[manager tests][manager-tests] [model conversion][model-types]

At the audited baseline, command-auth providers represent the external bearer
token as API-key auth. The provider's default Guardian reviewer is therefore
`gpt-5.6-luna` (ChatGPT auth still defaults to `codex-auto-review`). A main
model's non-null `auto_review_model_override` takes precedence; Codex resolves
that slug from the available catalog and otherwise falls back to the override
slug/main model as documented in the Guardian selection code. The audited
catalog contains both reviewer slugs, each with complete metadata; both are
`supported_in_api: true`, `visibility: "hide"` for `codex-auto-review` and
`"list"` for Luna. [Provider defaults][provider]
[command auth][command-auth] [Guardian selection][guardian]
[catalog][catalog]

## Executable client check

`TestLatestCodexBinaryAcceptsUnknownRemoteCatalogModel`,
`TestLatestCodexBinaryReplacesBundledRemoteCatalogModel`, and
`TestLatestCodexBinaryKeepsBundledSourceAheadOfMatchingAlias` are opt-in
black-box checks. With `CODEX_CATALOG_AUDIT_BINARY` pointing at the pinned
executable, they run Codex against isolated local command-auth providers. The
first asserts the real request's `client_version`, bearer/default headers,
unknown-slug `ModelsResponse` acceptance, picker selection, and unchanged
Responses model. The second retains the original audit of wholesale in-place
replacement for a known bundled slug. The third returns only an unknown alias
cloned from the untouched bundled default source and asserts that equal display,
priority, and visibility leave the bundled source selected. A negative control
establishes that explicit `--model` forwards an arbitrary unknown slug even with
an empty remote catalog, so explicit selection is not treated as merge evidence.
The audit instead queries the pinned app-server's `model/list`: the empty-catalog
control omits the alias while the alias-catalog run includes it, independently
witnessing the merge before `/responses` forwarding is checked. The 2026-09-05
audit ran them against the pinned [`rust-v0.153.4` executable
asset][audit-codex-binary], after verifying archive SHA-256
`c485e889611b73ff5c3cc11fb5cea7551ef504465ad8675163766b9b1a9ec84a`; the
resulting executable had SHA-256
`56ef98ab4032d317ab26e9b5e5a175650717351edb16ed9cde0cb6d1734d62da`.

For current and future audits, the contract test reads the expected executable
digest and stable release tag from `release.json`, derives the CLI version from
that tag, and enforces both before exercising the client. It discovers the
command-auth `printf` executable from `PATH` and skips with an explicit
prerequisite message when unavailable. The downloaded Codex executable was
temporary and is not stored in the repository.

## Manual release bump checklist

1. Audit the latest stable release, peel its tag to a commit, review the Codex
   schema/merge/selection/default behavior, and verify the official executable.
2. Update `release.json` and `models.json` together. Confirm the recorded audited
   bundled default from upstream behavior. Compare `LICENSE` and `NOTICE`, and
   replace them only if their upstream bytes changed.
3. Normally leave the runtime loader/cache, synthetic fixture versions,
   independent Serde-required-field list, `CONFIGURATION.md`, `CONTEXT.md`, and
   ADRs unchanged. Edit them only when the audit finds a real contract, config,
   policy, or legal change.
4. Run `nix fmt`, focused `nix develop -c go test ./internal/catalog -count=1`,
   and the opt-in executable audit with `CODEX_CATALOG_AUDIT_BINARY` pointing at
   the exact recorded executable. Run the repository's complete verification at
   the final feature tip.

[audit-release]: https://api.github.com/repos/openai/codex/releases/383061770
[audit-tag-object]: https://api.github.com/repos/openai/codex/git/tags/042fb41b7c813ac7999105e886b2b7aa715b5081
[audit-commit]: https://github.com/openai/codex/commit/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a
[audit-catalog-object]: https://api.github.com/repos/openai/codex/git/blobs/698da6fb7a825cd3ede1696e4ce8579ef5c42c02
[audit-catalog-raw]: https://raw.githubusercontent.com/openai/codex/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/models.json
[audit-checksums]: https://api.github.com/repos/openai/codex/releases/assets/545043391
[audit-source-tar]: https://api.github.com/repos/openai/codex/tarball/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a
[audit-source-zip]: https://api.github.com/repos/openai/codex/zipball/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a
[audit-codex-binary]: https://github.com/openai/codex/releases/download/rust-v0.153.4/codex-x86_64-unknown-linux-musl.zst
[catalog]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/models.json
[model-types]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/protocol/src/openai_models.rs#L1-L941
[model-messages]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/protocol/src/openai_models.rs#L539-L682
[guardian-v2]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/protocol/src/openai_models/guardian_v2.rs
[instruction-promotion]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/protocol/src/openai_models.rs#L750-L850
[model-tests]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/protocol/src/openai_models.rs#L942-L2074
[model-runtime]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/src/model_info.rs
[models-client]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/codex-api/src/endpoint/models.rs
[models-endpoint]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/model-provider/src/models_endpoint.rs
[default-headers]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/login/src/auth/default_client.rs#L278-L350
[command-auth]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/login/src/auth/external_bearer.rs
[models-lib]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/src/lib.rs
[manager]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/src/manager.rs#L120-L677
[manager-tests]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/models-manager/src/manager_tests.rs#L831-L1045
[provider]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/model-provider/src/provider.rs#L120-L380
[guardian]: https://github.com/openai/codex/blob/3d2ee51ca2d5db578f328aa75e20aa22c0197c9a/codex-rs/core/src/guardian/review.rs#L861-L975
