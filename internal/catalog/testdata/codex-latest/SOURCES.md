# Upstream provenance and contract

## Stable release pin

This fixture was audited on 2026-08-31. GitHub's first-party
`/repos/openai/codex/releases/latest` endpoint returned release ID `378941035`,
tag `rust-v0.151.0`, published `2026-08-29T09:55:39Z`, with `draft: false` and
`prerelease: false`. The annotated tag object
`d8673cb68e349c208659b986697773d3145dbb14` peels to commit
`78c290807ce710180111df227df3b7a4fe845452`; that commit, rather than the
release's `target_commitish: "main"`, is the source pin. [Release][release]
[tag object][tag-object] [commit][commit]

GitHub reports the release as `immutable: false`, and the tag is unsigned.
The commit and individual Git blob IDs are therefore the durable identities;
the release/tag names establish which stable release was current at the audit
cutoff. [Release][release] [tag object][tag-object]

The runtime cached value keeps the stable tag as its cheap version label, then
resolves a changed tag through GitHub's commit endpoint and requests
`models.json` with `ref=<40-hex commit>`. This closes the tag-movement window
between resolution and content retrieval, and an unchanged tag cannot silently
replace last-good bytes. Commit resolution requests the 40-byte GitHub SHA media
type, and every response on this credential-free edge is capped at 8 MiB.

## Fixture identity

- Repository/path: `openai/codex`,
  `codex-rs/models-manager/models.json` at the pinned commit. [Source][catalog]
- Git blob: [`0c4137ad9560e1ac7b9baf1adc95dbc7051e2b6c`][catalog-object].
- Local file: `models.json`, copied byte-for-byte from that blob.
- Size: `424117` bytes.
- SHA-256: `eb0d7b9a5dcaf103895c5f8a14c16b269df46e039b375a55ba97f6238542d2ed`.
- Immutable raw source: [raw `models.json`][catalog-raw].

The round-trip test computes Git's `blob <length>\0<content>` object identity
locally and requires the blob ID above, in addition to the SHA-256 check.

The release also publishes `codex-package_SHA256SUMS`; GitHub records the
manifest asset itself as
`sha256:197e852956ef6fcd48d9959c6ab3df8eb81ce0dbe7f5cc472215554bfbd2b1d5`.
GitHub's generated [commit tarball][source-tar] and [commit ZIP][source-zip]
have no API-provided digest, so they are not used as the fixture identity.
[Manifest metadata][checksums]

## Deserialization contract and test grounding

`TestLatestStableCodexCatalogRoundTripFidelity` proves that the exact pinned
catalog passes validation, cache acceptance, handler rendering, reviewer
resolution, a derived limits overlay, and field-for-field preservation. It is
not the regression witness for the relaxed acceptance rules: parent commit
`931b566` also accepts this exact 0.151 fixture because every entry still
carries both instruction forms, the two removed legacy booleans, and non-empty
reasoning levels. The synthetic contract tests exercise those Serde-valid
alternatives and fail against the parent implementation.

`ModelsResponse` requires a non-null `models` array. Each element is decoded
as `ModelInfo`; unknown object members are accepted because the type does not
deny unknown fields. The current bundled file has ten entries and contains
legacy/transport members not present in `ModelInfo` (including
`available_in_plans`, `minimal_client_version`, `prefer_websockets`,
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
variables, approval/collaboration/auto-review/permission/multi-agent sections,
persistent instructions, token-budget settings, Guardian V2 settings, and
confirmation policies are all optional and accept missing or `null`. When one
of the nested objects is present, its non-optional leaves remain required (for
example every `ModelTokenBudgetConfig` field); option-valued leaves preserve
the distinction between a missing/null value and an empty string. Guardian V2
and its transcript contain only optional leaves. [Messages][model-messages]
[Guardian V2][guardian-v2] [message tests][model-tests]

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

The pinned catalog supplies both instruction forms for all ten entries. Six
entries explicitly set `instructions_variables` to `null`; the other four use
an object. Newer sibling sections (`auto_review`, `collaboration_modes`,
`guardian_v2`, `multi_agent`, `permissions`, and `token_budget`) are present as
objects or explicit `null` and must survive copilotd rendering unchanged unless
an ADR-governed overlay names the field. [Catalog][catalog]

The fixture does not positively exercise every mirrored optional/default field:
`effective_context_window_percent` and `multi_agent_reasoning_effort` are absent
from all ten entries; `availability_nux` is null throughout; and
`persistent_instructions` and `confirmation_policies` are absent. Synthetic
positive and malformed-shape tests cover the corresponding validator paths.

Reasoning levels are an array of `{effort, description}` objects: both members
are required, descriptions are strings, and effort is any non-empty string
(known values currently include `none`, `minimal`, `low`, `medium`, `high`,
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
display, and the first picker-visible result becomes the default. An explicitly
configured model is preserved by the dynamic manager. If no model is
picker-visible, the first model is the default fallback. [Manager][manager]
[manager tests][manager-tests] [model conversion][model-types]

For current command-auth providers, the external bearer token is represented
as API-key auth. The provider's default Guardian reviewer is therefore
`gpt-5.6-luna` (ChatGPT auth still defaults to `codex-auto-review`). A main
model's non-null `auto_review_model_override` takes precedence; Codex resolves
that slug from the available catalog and otherwise falls back to the override
slug/main model as documented in the Guardian selection code. The pinned
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
cloned from the untouched bundled default source; it asserts that equal display,
priority, and visibility leave the bundled source selected, then explicitly
selects the alias and observes it on `/responses` as an independent merge
witness. The audit ran them against the official
[`codex-x86_64-unknown-linux-musl.zst`][codex-binary] asset after verifying
SHA-256
`4041e6a1b600a20420505984ecc534d56d9c10fddcdeaf03736b22a0b3308c1a`;
the resulting executable has SHA-256
`9739cbc928b9c573be83256acd46668f5dd4f119d2d09e05246895ca2aaf0c9a`.
The contract test enforces that executable digest and `codex-cli 0.151.0`
before exercising the client. It discovers the command-auth `printf` executable
from `PATH` and skips with an explicit prerequisite message when unavailable.
The downloaded Codex executable was temporary and is not stored in the
repository.

[release]: https://api.github.com/repos/openai/codex/releases/378941035
[tag-object]: https://api.github.com/repos/openai/codex/git/tags/d8673cb68e349c208659b986697773d3145dbb14
[commit]: https://github.com/openai/codex/commit/78c290807ce710180111df227df3b7a4fe845452
[catalog]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/models.json
[catalog-object]: https://api.github.com/repos/openai/codex/git/blobs/0c4137ad9560e1ac7b9baf1adc95dbc7051e2b6c
[catalog-raw]: https://raw.githubusercontent.com/openai/codex/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/models.json
[checksums]: https://api.github.com/repos/openai/codex/releases/assets/535048190
[source-tar]: https://api.github.com/repos/openai/codex/tarball/78c290807ce710180111df227df3b7a4fe845452
[source-zip]: https://api.github.com/repos/openai/codex/zipball/78c290807ce710180111df227df3b7a4fe845452
[model-types]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/protocol/src/openai_models.rs#L1-L910
[model-messages]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/protocol/src/openai_models.rs#L533-L678
[guardian-v2]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/protocol/src/openai_models/guardian_v2.rs
[instruction-promotion]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/protocol/src/openai_models.rs#L713-L816
[model-tests]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/protocol/src/openai_models.rs#L910-L1680
[model-runtime]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/src/model_info.rs
[models-client]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/codex-api/src/endpoint/models.rs
[models-endpoint]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/model-provider/src/models_endpoint.rs
[default-headers]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/login/src/auth/default_client.rs#L278-L350
[command-auth]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/login/src/auth/external_bearer.rs
[models-lib]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/src/lib.rs
[manager]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/src/manager.rs#L120-L680
[manager-tests]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/models-manager/src/manager_tests.rs#L831-L1045
[provider]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/model-provider/src/provider.rs#L120-L370
[guardian]: https://github.com/openai/codex/blob/78c290807ce710180111df227df3b7a4fe845452/codex-rs/core/src/guardian/review.rs#L831-L931
[codex-binary]: https://github.com/openai/codex/releases/download/rust-v0.151.0/codex-x86_64-unknown-linux-musl.zst
