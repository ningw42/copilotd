# Provenance and upstream contract

## Vendored snapshot identity

- Source repository: <https://github.com/openai/codex>
- Tag: `rust-v0.152.1`
- Commit: [`5adb68a49933ae446bf11935662c83dba55a0804`][commit]
- Source path:
  [`codex-rs/models-manager/models.json`][catalog]
- Git blob: [`0c4137ad9560e1ac7b9baf1adc95dbc7051e2b6c`][catalog-object]
- Size: `424117` bytes
- SHA-256: `eb0d7b9a5dcaf103895c5f8a14c16b269df46e039b375a55ba97f6238542d2ed`
- Immutable raw source: [raw `models.json`][catalog-raw]

`models.json` is vendored without modification from the source above. `LICENSE`
and `NOTICE` are copied from the repository root at the same commit. All three
files are byte-identical to their `rust-v0.151.0` counterparts; this bump
advances the release identity and accepted schema contract without changing
the embedded bytes.

The round-trip test computes Git's `blob <length>\0<content>` object identity
locally and requires the blob ID above, in addition to the SHA-256 check.

## Stable release audit

The identity above was audited on 2026-09-02. GitHub's first-party
`/repos/openai/codex/releases/latest` endpoint returned release ID `380862087`,
published `2026-09-01T22:33:02Z`, with `draft: false` and `prerelease: false`,
and named the pinned tag. The annotated [tag object][tag-object]
`3c6cfbab81e44218c729dc8c6b304cb760d1b8a1` peels to the pinned commit; that
commit, rather than the release's `target_commitish: "main"`, is the source pin.
[Release][release]

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

The release also publishes `codex-package_SHA256SUMS`; GitHub records the
manifest asset itself as
`sha256:6c287edb8ec9b153febd7e385d00fd1cc5ccf0cce6cc6405d7263dad4a1595ef`.
GitHub's generated [commit tarball][source-tar] and [commit ZIP][source-zip]
have no API-provided digest, so they are not used as the vendored snapshot
identity. [Manifest metadata][checksums]

## Deserialization contract and test grounding

`TestVendoredCodexCatalogRoundTripFidelity` proves that the exact vendored
snapshot passes validation, cache acceptance, handler rendering, reviewer
resolution, a derived limits overlay, and field-for-field preservation. The
unchanged blob is not a regression witness for the nested message fields added
in `rust-v0.152.1`, because none of those fields is populated. Synthetic
contract tests exercise their optional/default forms and malformed values.

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

The pinned catalog supplies both instruction forms for all ten entries. Six
entries explicitly set `instructions_variables` to `null`; the other four use
an object. Newer sibling sections (`auto_review`, `collaboration_modes`,
`guardian_v2`, `multi_agent`, `permissions`, and `token_budget`) are present as
objects or explicit `null` and must survive copilotd rendering unchanged unless
an ADR-governed overlay names the field. The `rust-v0.152.1` additions under
`tools`, `auto_review`, `multi_agent`, and `token_budget` are absent from the
unchanged catalog blob. [Catalog][catalog]

The vendored snapshot does not positively exercise every mirrored
optional/default field: `effective_context_window_percent` and
`multi_agent_reasoning_effort` are absent from all ten entries;
`availability_nux` is null throughout; `persistent_instructions`,
`confirmation_policies`, and `tools` are absent; and the new
`auto_review.node_repl_policy`, `multi_agent.mode.proactive`,
`token_budget.enabled`, and `token_budget.use_history_notes_extension` fields
are absent. Synthetic positive and malformed-shape tests cover the corresponding
validator paths.

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
cloned from the untouched bundled default source and asserts that equal display,
priority, and visibility leave the bundled source selected. A negative control
establishes that explicit `--model` forwards an arbitrary unknown slug even with
an empty remote catalog, so explicit selection is not treated as merge evidence.
The audit instead queries the pinned app-server's `model/list`: the empty-catalog
control omits the alias while the alias-catalog run includes it, independently
witnessing the merge before `/responses` forwarding is checked. The audit ran
them against the official
[`codex-x86_64-unknown-linux-musl.zst`][codex-binary] asset after verifying
SHA-256
`343c79bd7f979d1650cb41d3756bd5524bddf7edcc2f6796594c50ae5db994c2`;
the resulting executable has SHA-256
`b82018241214a4a7c6b97b198585192d1dbc3aab1ddcdc640f04d8dee8c606f9`.
The contract test enforces that executable digest and `codex-cli 0.152.1`
before exercising the client. It discovers the command-auth `printf` executable
from `PATH` and skips with an explicit prerequisite message when unavailable.
The downloaded Codex executable was temporary and is not stored in the
repository.

[release]: https://api.github.com/repos/openai/codex/releases/380862087
[tag-object]: https://api.github.com/repos/openai/codex/git/tags/3c6cfbab81e44218c729dc8c6b304cb760d1b8a1
[commit]: https://github.com/openai/codex/commit/5adb68a49933ae446bf11935662c83dba55a0804
[catalog]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/models.json
[catalog-object]: https://api.github.com/repos/openai/codex/git/blobs/0c4137ad9560e1ac7b9baf1adc95dbc7051e2b6c
[catalog-raw]: https://raw.githubusercontent.com/openai/codex/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/models.json
[checksums]: https://api.github.com/repos/openai/codex/releases/assets/540234309
[source-tar]: https://api.github.com/repos/openai/codex/tarball/5adb68a49933ae446bf11935662c83dba55a0804
[source-zip]: https://api.github.com/repos/openai/codex/zipball/5adb68a49933ae446bf11935662c83dba55a0804
[model-types]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/protocol/src/openai_models.rs#L1-L941
[model-messages]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/protocol/src/openai_models.rs#L544-L682
[guardian-v2]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/protocol/src/openai_models/guardian_v2.rs
[instruction-promotion]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/protocol/src/openai_models.rs#L745-L850
[model-tests]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/protocol/src/openai_models.rs#L942-L2089
[model-runtime]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/src/model_info.rs
[models-client]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/codex-api/src/endpoint/models.rs
[models-endpoint]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/model-provider/src/models_endpoint.rs
[default-headers]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/login/src/auth/default_client.rs#L278-L350
[command-auth]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/login/src/auth/external_bearer.rs
[models-lib]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/src/lib.rs
[manager]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/src/manager.rs#L120-L680
[manager-tests]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/models-manager/src/manager_tests.rs#L831-L1045
[provider]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/model-provider/src/provider.rs#L120-L380
[guardian]: https://github.com/openai/codex/blob/5adb68a49933ae446bf11935662c83dba55a0804/codex-rs/core/src/guardian/review.rs#L831-L940
[codex-binary]: https://github.com/openai/codex/releases/download/rust-v0.152.1/codex-x86_64-unknown-linux-musl.zst
