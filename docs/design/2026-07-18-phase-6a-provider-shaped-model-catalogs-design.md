# Phase 6a — Provider-shaped model catalogs (`/anthropic/v1/models`, `/openai/v1/models`) — Design

Status: proposed design (polished via brainstorming), pending final written-spec review
Date: 2026-07-18
Roadmap reference: `ROADMAP.md` §4.2 and §7 "Phase 6 — Provider/client-shaped support endpoints"
Builds on: `docs/design/2026-07-18-phase-4-github-copilot-support-endpoint-design.md`,
`docs/design/2026-07-16-phase-3-middleware-framework-design.md`,
`docs/design/2026-07-16-forwarding-fidelity-and-sse-identity-design.md`

## 1. Goal and outcome

Phase 6a adds two client-facing model catalogs — `GET/HEAD HOST/anthropic/v1/models`
and `GET/HEAD HOST/openai/v1/models` — that fetch GitHub Copilot's raw `/models`
once, keep only the models each Surface can actually forward, and re-render them
in the native shape of the real provider's `GET /v1/models`. A client that has
set its base URL to `HOST/anthropic` or `HOST/openai` can list models and receive
a response whose envelope and per-model **schema** are the genuine provider's,
**populated with Copilot's own values** — which may differ from the genuine
provider's published numbers and capability tree (§5.5).

**Outcome:** the two inference Surfaces gain a native, provider-shaped model list
built entirely from real Copilot data, with best-effort sanitization. Unlike the
Phase 4 raw `/models` passthrough, these endpoints **parse and reconstruct** the
response, so they own their wire shape and their own failure modes.

This is the **provider-shaped** half of ROADMAP Phase 6. The **client-shaped**
half — the Codex catalog (`/v1/models?client_version=…`) and its
`auto_review_model_override` reviewer routing — is a larger, externally-dependent
effort split out to a separate follow-on spec (**Phase 6b**, §12).

### 1.1 Grounding (rule: nothing is invented without a real Copilot response)

Every field mapping below is derived from a **captured** GitHub Copilot `/models`
response (the raw response from GitHub Copilot, obtained through the Phase 4
`/models` passthrough). The Anthropic
and OpenAI target shapes are taken from the live provider docs
(`GET /v1/models`). The reasoning about a shim alternative (§6.4) is checked
against the real `internal/shim` and `internal/forward` source. Where Copilot
supplies no basis for a provider field, that field is dropped, except the
timestamp stub called out in §5. The full set of resulting divergences from the
genuine provider is enumerated in §5.5 and recorded in ADR 0004.

## 2. Scope

### 2.1 In scope

- Two provider-shaped list endpoints, each with an explicit `GET` and `HEAD`
  registration:
  - `GET/HEAD /anthropic/v1/models` → Anthropic `GET /v1/models` shape.
  - `GET/HEAD /openai/v1/models` → OpenAI `GET /v1/models` shape.
- The existing API-key gate followed by the existing readiness gate (the same
  `guard` order as every provider route).
- One credentialed upstream `GET /models` fetch per request, buffered and decoded.
- **Endpoint-based filtering**: list only models whose Copilot capabilities show
  the Surface's upstream Route is served (§4).
- Copilot→provider field mapping with **enrichment where honestly derivable**
  (§5), and a single epoch-0 timestamp stub for the absent creation date.
- A new pure, I/O-free `internal/catalog` package for decode + filter + render,
  plus a focused dumb `Forwarder` fetch method and `internal/server` wiring (§6).
- Surface-shaped errors: `/anthropic/v1/models` speaks the Anthropic error dialect,
  `/openai/v1/models` the OpenAI dialect (§8).
- Automated unit, boundary, and real-listener tests using a stub upstream, seeded
  from the real capture.

### 2.2 Out of scope

- **The Codex catalog** (`/v1/models?client_version=…`), the Codex `ModelInfo`
  shape, and `auto_review_model_override` reviewer routing — all Phase 6b.
- **Retrieve-by-id** (`GET /{surface}/models/{id}`). The ROADMAP scopes a
  "catalog" (list); a single-model lookup is a trivial later addition and is not
  needed to populate a model picker.
- **Caching, refresh jobs, conditional-response synthesis, or state at rest.**
  Each request issues its own upstream fetch; two calls cause two fetches.
- **Cursor pagination.** The catalog is < 20 models; a single unpaginated page is
  returned (§5.3). Unknown pagination query params are accepted and ignored.
- **Any reshape as a shim.** Rejected with reasons in §6.4; these are first-party
  support endpoints, not onion parity layers.
- **Cross-family translation, chat-completions, embeddings, WebSocket transports.**
  Unchanged project non-goals; a natural consequence is that endpoint-less,
  chat-only, and embedding models drop out of both catalogs (§4).

## 3. Decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Which models each endpoint lists | `model_picker_enabled == true` **and** `supported_endpoints` contains the Surface's upstream Route (`/v1/messages` for Anthropic, `/responses` for OpenAI) | Advertises only models copilotd can actually forward — correctness, not arbitrary sanitization. Endpoint-less / chat-only / embedding models drop out because copilotd cannot serve them. |
| Object richness | Provider core fields, **enriched only where Copilot supplies a basis**; drop the rest | Best-effort match to the real provider's *schema* without fabricating capability claims. The emitted `capabilities` tree is therefore a **subset** of the genuine Anthropic tree (§5.5). Enrichment materializes only on the Anthropic object; the OpenAI object schema has no capability slots. |
| Missing creation date | Stub epoch 0 (`created: 0` / `created_at: "1970-01-01T00:00:00Z"`) | Copilot returns no timestamp on any model. For **Anthropic** this is provider-sanctioned — the live docs state `created_at` "may be set to an epoch value if the release date is unknown" — so it is a native "unknown" sentinel, not a divergence. For **OpenAI**, `created: 0` is a genuine stub (OpenAI documents no unknown-sentinel), kept so strict SDKs that require the field still deserialize. |
| Response handling | Parse Copilot's body and reconstruct the provider shape | These endpoints own their wire shape; that is the entire point. This is the deliberate departure from Phase 4's byte-verbatim passthrough. |
| Implementation vehicle | A **standalone handler** composing a dumb forwarder fetch with a pure `internal/catalog` renderer | A first-party support endpoint may construct its own representation; a shim may not (no-fabrication invariant), the inference path hardcodes POST, and Phase 4 kept `/models` off the shim path. See §6.4. |
| Package boundary | New pure `internal/catalog`; forwarder gains a focused dumb `FetchModels`; server wires the handler | Keeps the raw forwarder Copilot-agnostic; isolates the typed transform in one deep, unit-testable unit; matches the repo's dependency-inversion posture. |
| Error dialect | Each route renders copilotd-originated failures in its own Surface's envelope | Resolves what Phase 4 §11 deferred; a provider-shaped endpoint must also produce provider-shaped errors. |
| Upstream non-2xx | Render a Surface-shaped **502** (do not pass Copilot's body through) | A non-catalog upstream body cannot be reshaped; leaking Copilot's error shape on a provider-shaped endpoint would break the contract. |
| Freshness | No cache; one upstream `/models` fetch per request | Consistent with Phase 4 and the "no services / no state at rest" principle. |

## 4. The filter — endpoint-based sanitization

copilotd's Anthropic Surface forwards inference to Copilot `/v1/messages`; its
OpenAI Surface forwards to Copilot `/responses`. Every model in the capture is
tagged with the exact upstream Routes it supports, so the correct catalog for a
Surface is precisely the models that advertise that Surface's Route.

A Copilot model `M` is listed on Surface `S` **iff both** hold:

1. `M.model_picker_enabled == true` — a **defensive** gate against Copilot's
   hidden/deprecated/internal entries. On the current capture it is **redundant**:
   every `model_picker_enabled:false` entry (old `gpt-4o-*`, `gpt-3.5-turbo`,
   `gpt-41-copilot`, embeddings, `trajectory-compaction`, …) already fails
   predicate 2. Its only live effect is on a hypothetical future model that is
   *forwardable but hidden* (picker-disabled yet serving the Route), which we
   deliberately keep out of the catalog.
2. `M.supported_endpoints` is present and contains `S`'s upstream Route:
   - `/anthropic/v1/models` requires `"/v1/messages" ∈ M.supported_endpoints`.
   - `/openai/v1/models` requires `"/responses" ∈ M.supported_endpoints`.

Notes:

- A model with **no** `supported_endpoints` is **not** listed: copilotd cannot
  confirm the Surface serves it.
- The websocket variant `"ws:/responses"` is **not** `"/responses"`; the exact
  HTTP Route string is required (the WebSocket transport is a non-goal).
- **Membership is keyed on the wire-Surface, not the vendor.** `/openai/v1/models`
  lists every model forwardable on the OpenAI (Responses) Surface, including
  non-OpenAI-vendor models: `mai-code-1-flash-picker` (`vendor: "Microsoft"`) and
  `gpt-5-mini` (`vendor: "Azure OpenAI"`) both serve `/responses` and are callable
  through `HOST/openai`, so both are listed. Vendor-gating would make the catalog
  under-report what a client can actually call.
- Applied to the capture, the predicate yields the 7 current Claude models on
  `/anthropic/v1/models` (`claude-opus-4.6/4.7/4.8`, `claude-sonnet-4.6/5/4.5`,
  `claude-haiku-4.5`) and 9 Responses-capable models on `/openai/v1/models`
  (`gpt-5.3-codex`, `gpt-5.4`, `gpt-5.4-mini`, `gpt-5.5`,
  `gpt-5.6-luna/sol/terra`, `mai-code-1-flash-picker`, `gpt-5-mini`).

Upstream `data[]` order is preserved through the filter; the rendered list keeps
that order.

## 5. Field mappings

All source fields below name Copilot response fields verbatim. Every mapping is
**defensive**: when a source field is absent, the target key is omitted (never
emitted as a false value), except the timestamp stub.

### 5.1 OpenAI object (`/openai/v1/models`)

The OpenAI `Model` object is exactly four fields; its schema has no capability or
limit slots, so "enrichment" does not apply here.

| OpenAI field | Source | Value |
| --- | --- | --- |
| `id` | `M.id` | e.g. `"gpt-5.6-luna"` |
| `object` | constant | `"model"` |
| `created` | — (no Copilot basis) | `0` (epoch-0 stub) |
| `owned_by` | `M.vendor` | Copilot's vendor string verbatim, e.g. `"OpenAI"`, `"Azure OpenAI"`, `"Microsoft"` — not normalized to OpenAI's `"openai"`/`"system"` convention (§5.5) |

Envelope: `{ "object": "list", "data": [ … ] }` (the OpenAI models list is
unpaginated).

### 5.2 Anthropic object (`/anthropic/v1/models`)

The Anthropic model object has token-limit and capability slots, so enrichment
materializes here.

| Anthropic field | Source | Notes |
| --- | --- | --- |
| `id` | `M.id` | |
| `type` | constant | `"model"` |
| `display_name` | `M.name` | e.g. `"Claude Opus 4.6"` |
| `created_at` | — (no Copilot basis) | `"1970-01-01T00:00:00Z"` (RFC 3339). Provider-sanctioned: Anthropic's docs allow an epoch value when the release date is unknown, so this is a native sentinel, not a divergence. |
| `max_input_tokens` | `M.capabilities.limits.max_prompt_tokens` | Copilot's forwardable prompt budget; may sit below the provider's published context window (§5.5). Omit if absent. |
| `max_tokens` | `M.capabilities.limits.max_output_tokens` | Copilot's output-parameter ceiling; may sit below the provider's published max output (§5.5). Omit if absent. |
| `capabilities.structured_outputs.supported` | `M.capabilities.supports.structured_outputs` | omit key if absent |
| `capabilities.image_input.supported` | `M.capabilities.supports.vision` | omit key if absent |
| `capabilities.pdf_input.supported` | `"application/pdf" ∈ M.capabilities.limits.vision.supported_media_types` | omit key if no vision limits |
| `capabilities.effort` | derived from `M.capabilities.supports.reasoning_effort` | see §5.2.1; omit whole block if the array is absent/empty |
| `capabilities.thinking` | derived from Copilot's thinking signals | see §5.2.2; omit whole block if no signal |

Capability keys Anthropic defines but Copilot gives **no** basis for — `batch`,
`citations`, `code_execution`, `context_management` — are **omitted**. Emitting
`supported: false` would falsely claim non-support; emitting `true` would invent.

#### 5.2.1 `effort` mapping (exact)

Anthropic enumerates effort levels `low`, `medium`, `high`, `xhigh`, `max`.
Copilot's `reasoning_effort` array may also contain `none` and `minimal`, which
have no Anthropic slot.

- Emit the `capabilities.effort` block **iff** `reasoning_effort` is present and
  non-empty.
- `capabilities.effort.supported = true`.
- For each level `L` in {`low`, `medium`, `high`, `xhigh`, `max`}:
  `capabilities.effort.<L>.supported = (L ∈ reasoning_effort)`.
- `none` and `minimal` are dropped (no Anthropic slot).

#### 5.2.2 `thinking` mapping (exact)

Copilot exposes three thinking signals under `capabilities.supports`:
`adaptive_thinking` (bool), `min_thinking_budget`, `max_thinking_budget` (ints).
Anthropic's `thinking` object is booleans only —
`{ supported, types: { adaptive: {supported}, enabled: {supported} } }` — with no
numeric budget slot. Rules:

- Emit the `capabilities.thinking` block **iff** any of the three signals is
  present.
- `capabilities.thinking.types.adaptive.supported = (M.capabilities.supports.adaptive_thinking == true)`.
  This is the only source for adaptive; it is independent of the budgets.
- `capabilities.thinking.types.enabled.supported =` **an explicit budget is
  advertised**, i.e. `min_thinking_budget` **or** `max_thinking_budget` is
  present. This is the single inferential step in the whole mapping, and it is a
  **Copilot-derived signal that can diverge from the genuine Anthropic API**:
  Copilot advertises a budget range even for adaptive-only models that in fact
  *reject* `budget_tokens` upstream, and the models whose real `enabled` answer
  differs are indistinguishable in Copilot's data (worked example below), so no
  mapping can reproduce it faithfully. We keep the inference under the
  provider-*shaped* contract (§5.5) — it reports Copilot's advertised budget —
  while accepting that it reads `true` where the genuine API returns `false`.
  Stated here so the divergence is a conscious, reviewable choice.
- `capabilities.thinking.supported = adaptive.supported || enabled.supported`.
- The budget **magnitudes** (e.g. `1024` / `32000`) are surfaced **nowhere** —
  Anthropic's model schema has no field for them; they serve only as the boolean
  signal above.

Worked examples from the capture (the first two match the genuine API; the third
is the documented divergence):

- `claude-opus-4.6`: `adaptive_thinking:true`, budgets present →
  `thinking = { supported:true, types:{ adaptive:{supported:true}, enabled:{supported:true} } }`;
  `effort` present with `low/medium/high/max` supported. **Matches** the genuine
  Anthropic object (opus-4.6 still honors `budget_tokens`).
- `claude-sonnet-4.5`, `claude-haiku-4.5`: **no** `adaptive_thinking`, budgets
  present, **no** `reasoning_effort` →
  `thinking = { supported:true, types:{ adaptive:{supported:false}, enabled:{supported:true} } }`;
  `effort` block omitted. **Matches** the genuine API.
- `claude-opus-4.8` (**divergence**): Copilot's thinking signals are *identical* to
  opus-4.6 (`adaptive_thinking:true`, `min/max_thinking_budget` present), so we emit
  `enabled:{supported:true}` — but the genuine Anthropic API returns
  `enabled:{supported:false}` (4.7+ rejected `budget_tokens`; adaptive-only). The
  discriminating fact is absent from Copilot's data, so no mapping could reproduce
  it — the §5.5 divergence made concrete.

### 5.3 Envelopes and pagination

- **OpenAI:** `{ "object": "list", "data": [ … ] }`. No pagination fields.
- **Anthropic:** `{ "data": [ … ], "has_more": false, "first_id": <data[0].id>, "last_id": <data[-1].id> }`.
  For an empty `data`, `first_id` and `last_id` are `null`. The full filtered list
  is returned as a single page; `limit`/`after_id`/`before_id` are accepted and
  ignored (best-effort). Honoring `limit` is a trivial later addition if a client
  needs it.

### 5.4 Sanitization side-effect

Because the reshape reads only the mapped fields, the provider-shaped output
**naturally drops** Copilot-internal metadata: `policy.terms`, `warning_message`
(the billing notice on every entry in the capture), `model_picker_category`,
`capabilities.family`, `tokenizer`, `version`, and `supported_endpoints`. The
catalogs therefore do not leak Copilot's billing warnings or policy text — a
benefit of reconstructing rather than passing through.

### 5.5 Fidelity contract — provider-shaped, not provider-identical

These catalogs promise the genuine provider's **envelope and per-model schema**,
populated with **Copilot's own values**. They do **not** promise value-level
parity with the real provider — copilotd only knows Copilot's view of each model,
and that view differs from the provider's published data in enumerable ways. The
complete set of accepted divergences:

- **Token limits are Copilot's forwardable budgets, not the provider's published
  ceilings.** `max_input_tokens ← max_prompt_tokens` and `max_tokens ←
  max_output_tokens` report what Copilot will actually accept/emit, which can be
  lower than the provider's headline numbers (e.g. `claude-opus-4.6`: 936K/64K
  here vs Anthropic's published 1M/128K).
- **The `capabilities` tree is a subset.** Leaves Copilot gives no basis for
  (`batch`, `citations`, `code_execution`, `context_management`) are omitted
  rather than fabricated. A client doing unguarded bracket access into an omitted
  leaf — which Anthropic's own SDK guidance encourages, since the genuine API
  always returns the full tree — will not find it.
- **`enabled` thinking support is inferred and can be wrong.** Per §5.2.2 it reads
  `true` for adaptive-only models (`opus-4.7/4.8`, `sonnet-5`) where the genuine
  API returns `false`.
- **`owned_by` is Copilot's `vendor` verbatim** (e.g. `"Azure OpenAI"`,
  `"Microsoft"`), not normalized to OpenAI's `"openai"`/`"system"` convention.
- **List order is Copilot's `data[]` order**, not the provider's "most recently
  released first" convention.

Each divergence is a deliberate consequence of the no-fabrication rule: where a
faithful provider-shaped value is not derivable from Copilot's data, copilotd
reports Copilot's value (or omits the field) rather than inventing the provider's.
This contract is recorded in ADR 0004.

## 6. Architecture and package boundaries

Two new units and thin wiring; the raw forwarder stays dumb.

```
copilotd/
└── internal/
    ├── catalog/   [NEW]  Pure decode + filter + render. Minimal Copilot decode types (only the read
    │                     fields) · Filter(models, requiredEndpoint) · RenderOpenAI / RenderAnthropic ·
    │                     Handler(desc, Fetcher) http.HandlerFunc built on a narrow Fetcher interface.
    │                     I/O-free transform; depends on encoding/json, net/http, apierror.
    ├── forward/   [CHG]  + FetchModels(ctx): one credentialed, buffered GET /models with identity
    │                     encoding; typed errors; no reshaping, no shims, no SSE. Implements catalog.Fetcher.
    └── server/    [CHG]  Registers GET/HEAD /anthropic/v1/models and /openai/v1/models under the existing
                          guard, each bound to its Surface descriptor (tag, required endpoint, renderer).
```

### 6.1 `internal/catalog` (pure)

The deep, isolated transform unit — no network, no credentials, fully
unit-testable off a fixture derived from the real capture.

- **Decode types** that unmarshal only the fields §4/§5 read (`id`, `name`,
  `vendor`, `model_picker_enabled`, `supported_endpoints`, and the relevant
  `capabilities.limits.*` / `capabilities.supports.*`). Unknown fields are ignored
  by `encoding/json` — no dependency on the full Copilot shape.
- `Decode(body []byte) ([]Model, error)` — reads the `{ "data": [...] }` envelope.
- `Filter(models []Model, requiredEndpoint string) []Model` — the §4 predicate.
- `RenderOpenAI(models []Model) ([]byte, error)` and
  `RenderAnthropic(models []Model) ([]byte, error)` — build the §5 envelopes and
  objects, preserving upstream order.
- `Handler(desc Descriptor, fetcher Fetcher) http.HandlerFunc` — the endpoint
  orchestration (§7), depending on a narrow interface rather than on `forward`:

  ```go
  type Fetcher interface {
      // FetchModels issues one credentialed GET to the upstream /models route
      // and returns its buffered status and body, or a typed copilotd error.
      FetchModels(ctx context.Context) (status int, body []byte, err error)
  }

  type Descriptor struct {
      Surface          apierror.Surface        // Anthropic or OpenAI (error dialect + auth tag)
      RequiredEndpoint string                  // "/v1/messages" or "/responses"
      Render           func([]Model) ([]byte, error)
  }
  ```

  Dependency inversion keeps `catalog` decoupled from `forward`, mirroring how
  `internal/sse` owns `FrameTransformer` and `internal/shim` implements it.

### 6.2 `internal/forward` (`FetchModels`)

A focused, dumb addition that reuses the forwarder's existing credential,
impersonation, transport, and timeout machinery (the same primitives
`PassthroughHandler` uses): `provider.Current` → build `GET cred.BaseURL+"/models"`
with `authenticatedOutboundHeaders` and `Accept-Encoding: identity` → `client.Do`
bounded by `outboundTimeout` → log any differing upstream `X-Request-Id` via the
existing upstream-correlation line → read the body under the buffered-response cap →
return `(status, body, err)`. It performs **no** reshaping, runs **no** shim
chain, and does **no** SSE classification.

Failures return **typed** errors so the caller maps them to its Surface dialect
(the error dialect is the endpoint's concern, not the forwarder's):
`ErrNoCredential`, `ErrBuildUpstream`, `ErrUpstreamUnreachable`,
`ErrUpstreamTimeout`, `ErrUpstreamRead`. Client cancellation is **not** a distinct
sentinel: a canceled context surfaces as an unreachable/read failure, which the
handler discards by checking the request context before writing (§7 step 3).

### 6.3 `internal/server` (wiring)

`newHandler` registers four routes under the existing `guard` (auth → readiness),
each an independent explicit method registration (mirroring Phase 4's explicit
`GET`/`HEAD` pattern so both are visible and testable):

```go
anthropicModels := catalog.Handler(
    catalog.Descriptor{Surface: apierror.Anthropic, RequiredEndpoint: "/v1/messages", Render: catalog.RenderAnthropic},
    fwd)
mux.Handle("GET /anthropic/v1/models",  guard(apierror.Anthropic, anthropicModels))
mux.Handle("HEAD /anthropic/v1/models", guard(apierror.Anthropic, anthropicModels))

openaiModels := catalog.Handler(
    catalog.Descriptor{Surface: apierror.OpenAI, RequiredEndpoint: "/responses", Render: catalog.RenderOpenAI},
    fwd)
mux.Handle("GET /openai/v1/models",  guard(apierror.OpenAI, openaiModels))
mux.Handle("HEAD /openai/v1/models", guard(apierror.OpenAI, openaiModels))
```

`POST` and other methods receive `net/http`'s normal 405. No wildcard subtree is
introduced. The existing raw `GET/HEAD /models` (Phase 4) is untouched.

### 6.4 Alternatives considered — reshape as a shim (rejected)

A `BufferedTransformer` shim can mechanically replace a whole JSON body
(`shim.Chain.RunBuffered` hands it `*Body{Bytes}`; `HasBufferedTransformer()`
triggers the buffered path in `forward.go`). Routing `/anthropic/v1/models` through
the shim-bearing `Handler` was considered and rejected on grounds confirmed
against the real source:

- **No-fabrication invariant** (`internal/shim` package doc; Phase 3 §10.2): a
  shim "must not fabricate information without an upstream basis." The
  whole-envelope synthesis (`object:"list"`, `has_more`, `first_id`/`last_id`,
  constant `type`) has no upstream basis — it is §10.2's "structural repair,"
  explicitly *not pre-authorized* and requiring a named divergence-ledger
  amendment. (The epoch `created_at` is *not* the offending part — Anthropic
  sanctions an epoch value for an unknown date, §5.2; the synthesized list
  envelope is.) A first-party support endpoint is *expected* to construct its
  representation; Phase 4 already established support endpoints as first-party,
  not shim-transformed.
- **The inference path hardcodes POST.** `forward.forward()` builds the upstream
  request with `http.MethodPost`; `/models` is a `GET` Route. A shim would force a
  method seam into the dumb core the onion exists to keep untouched.
- **`Handler` carries inference-only machinery** — the request-body cap, the
  OpenAI `peekBackground` reject, and `isEventStream` branching — which Phase 4
  deliberately kept `/models` away from. Re-routing re-introduces that coupling.
- **Shims are not route-scoped.** One global registry instantiates every shim on
  every inference request; a models-reshape shim would fire on `/v1/messages` and
  `/responses` and must no-op there, entangling a support concern into the
  inference hot path.
- **Prelude precedes body.** On an upstream 401/500 the body is a Copilot error,
  not a catalog; the status commits from upstream before the buffered pass, so a
  shim cannot cleanly emit a Surface-shaped error. A handler branches on status
  trivially.
- **Toggle mismatch.** Shims are individually `--shim-<name>-enabled` optional
  parity layers; reshaping *is* this endpoint's reason to exist — disabling it
  would serve raw Copilot bytes on an Anthropic-advertised route.

The reusable plumbing a shim would provide (credential, impersonation, transport,
buffering) is reused just as well by the standalone handler via the
`PassthroughHandler` pattern, and the pure transform (`internal/catalog`) is
identical either way. The shim buys nothing and costs a contract amendment plus
core changes. This is not a violation of ROADMAP principle #2 ("the onion is the
only extension mechanism"): that principle governs *parity on forwarded inference
routes*; support endpoints are their own architecture component.

## 7. End-to-end flow

For any of the four registered routes:

1. Request-ID, access-log, and recovery middleware wrap the request as on every
   route; API-key auth then readiness run via `guard`.
2. The catalog handler calls `fetcher.FetchModels(ctx)` — one credentialed GET to
   Copilot `/models` (the provider may perform its normal on-demand mint).
3. On a typed fetch error, first short-circuit client cancellation: if the request
   context is canceled (the client disconnected), return without writing — the
   caller has already left — mirroring `forward.go`'s existing check. Otherwise
   render the Surface dialect: `ErrNoCredential` → 503 `NotReady`; `ErrBuildUpstream`
   / `ErrUpstreamUnreachable` / `ErrUpstreamRead` → 502 `BadGateway`;
   `ErrUpstreamTimeout` → 504 `GatewayTimeout`.
4. On `status != 200`, render a Surface-shaped 502 `BadGateway` ("upstream models
   request failed"); Copilot's body is not forwarded.
5. `catalog.Decode(body)`; a decode error → 502 `BadGateway`.
6. `catalog.Filter(models, desc.RequiredEndpoint)` then `desc.Render(filtered)`.
7. Write `200` with `Content-Type: application/json` and `Content-Length`. For
   `GET`, write the body; for `HEAD`, compute the same body to set an accurate
   `Content-Length` but suppress the body write.

There is no cache and no single-flight; two client calls cause two upstream
`/models` fetches. `X-Request-Id` correlation and the strip of any upstream
`X-Request-Id` follow the existing global policy.

## 8. Errors, timeout, and cancellation

- Every copilotd-originated failure renders in the **route's own Surface dialect**
  (`apierror.Anthropic` for `/anthropic/v1/models`, `apierror.OpenAI` for
  `/openai/v1/models`) — resolving what Phase 4 §11 deferred for these paths.
- **Upstream non-2xx and unparseable 2xx bodies** both render a Surface-shaped
  502; copilotd never reshapes a non-catalog body or leaks Copilot's error shape.
- `ResponseHeaderTimeout` bounds time-to-headers; `OutboundTimeout` bounds the
  buffered read. No SSE idle/keepalive timers apply (this is not a stream).
- `HEAD` returns the same status and headers as `GET` (including `Content-Length`)
  with no body.
- Client cancellation propagates to the upstream fetch; the handler detects the
  canceled request context (§7 step 3) and returns without writing, so a caller
  that has already left is never sent a mapped 502/504 — no additional downstream
  signal.
- Router-level 404/405 and panic recovery remain generic server signals, as
  elsewhere.

## 9. Observability and security

- Access logging records one line per request with the explicit route template
  (`GET /anthropic/v1/models`, `HEAD /openai/v1/models`, …), status, byte count,
  duration, and the resolved correlation ID.
- The one upstream fetch reuses the existing single-correlation-ID invariant; a
  different upstream `X-Request-Id` is suppressed downstream (and logged only per
  the existing upstream-correlation log line).
- The API key, GitHub OAuth token, and Copilot token are never logged. Model data,
  query values, and bodies are not logged.
- No new metrics, configuration keys, background work, cache, or durable state.
- The endpoints are protected (auth + readiness) because they perform an
  account-authorized Copilot operation and expose account-specific model data.
- The reshape drops Copilot billing/policy metadata (§5.4), so the provider-shaped
  output carries no `warning_message` or `policy.terms`.

## 10. Test design

All automated tests use `httptest` upstreams and a stubbed identity; none need a
GitHub account or network. Fixtures are derived from the raw response from GitHub
Copilot (the Phase 4 `/models` passthrough).

> **Implementation note:** this capture is not committed to the repo. The
> implementing agent must obtain a current `/models` response through the Phase 4
> passthrough and land it as a checked-in fixture (e.g. `internal/catalog/testdata`);
> if no live Copilot credential is available in the environment, ask the human
> operator to capture one.

### 10.1 `internal/catalog` unit tests (pure)

- **Filter:** the Anthropic predicate selects exactly the 7 Claude models; the
  OpenAI predicate exactly the 9 Responses models; endpoint-less, chat-only,
  `ws:/responses`-only, and `model_picker_enabled:false` models are excluded.
- **OpenAI render:** four fields per object, `object:"model"`, `created:0`,
  `owned_by` = vendor verbatim; envelope `{object:"list", data}`; order preserved.
- **Anthropic render — enrichment:** `max_input_tokens`←`max_prompt_tokens`,
  `max_tokens`←`max_output_tokens`; `structured_outputs`/`image_input`/`pdf_input`
  from the right sources; `effort` per §5.2.1; `thinking` per §5.2.2 with the three
  worked examples asserted exactly (`opus-4.6` adaptive+enabled+effort;
  `sonnet-4.5`/`haiku-4.5` enabled-only, no adaptive, no effort; `opus-4.8` the
  §5.5 divergence — `enabled:true` emitted despite the genuine API's `false`, since
  the rule follows Copilot's advertised budget); envelope with `has_more:false` and
  correct `first_id`/`last_id`; empty list → `null` ids.
- **Defensive omit:** a model missing vision limits → no `pdf_input`/`image_input`
  keys; missing `reasoning_effort` → no `effort` block; missing all thinking
  signals → no `thinking` block; missing a limit → that token field omitted.
- **Decode tolerance:** unknown/extra Copilot fields are ignored; the epoch-0
  stub is present on every object.

### 10.2 `internal/forward.FetchModels` unit tests

- Issues one upstream `GET /models` with the credential, impersonation headers,
  resolved `X-Request-Id`, and `Accept-Encoding: identity`; buffers the body.
- Each failure surface returns its typed error (`ErrNoCredential`,
  `ErrUpstreamUnreachable`, `ErrUpstreamTimeout`, `ErrUpstreamRead`,
  `ErrBuildUpstream`).

### 10.3 Server boundary and real-listener tests

- Both API-key forms authorize all four routes; invalid auth → 401 before a
  not-ready identity can return 503.
- Not-ready identity → the route's Surface-shaped 503.
- Upstream non-2xx and malformed-JSON both → the route's Surface-shaped 502; the
  Anthropic route emits the Anthropic error envelope and the OpenAI route the
  OpenAI envelope.
- `HEAD` returns headers (incl. `Content-Length`) with no wire body, proven over a
  real listener.
- Two client calls cause exactly two upstream `/models` fetches.
- The single-correlation-ID invariant holds; a deliberately different upstream
  `X-Request-Id` is suppressed.
- Unregistered methods (`POST /anthropic/v1/models`) receive the standard 405 and
  never reach Copilot.

### 10.4 Regression

- Phase 4 `GET/HEAD /models` passthrough and the inference routes are unchanged.
- `go test ./...` and `go test -race ./...`.

## 11. Acceptance criteria

Phase 6a is complete when all hold:

1. `GET/HEAD /anthropic/v1/models` and `GET/HEAD /openai/v1/models` are explicit,
   authenticated, readiness-gated endpoints.
2. Each request issues exactly one upstream `/models` fetch; no model response is
   cached, retried, or refreshed.
3. Each endpoint lists exactly the models passing the §4 predicate for its
   Surface.
4. The OpenAI object matches §5.1 and the Anthropic object matches §5.2 (including
   the §5.2.1 effort and §5.2.2 thinking rules and the epoch-0 stub), with
   defensive omission of unsupported keys, and the documented §5.5 divergences
   hold as specified (no attempt at value-level provider parity).
5. Envelopes and pagination match §5.3.
6. copilotd-originated failures render in the route's own Surface dialect;
   upstream non-2xx and unparseable bodies render a Surface-shaped 502.
7. `HEAD` returns GET-equivalent status and headers with no body.
8. The raw forwarder stays dumb; the transform lives in the pure `internal/catalog`
   unit; no reshape runs as a shim.
9. No new configuration, metric, background task, cache, or durable state is
   introduced.
10. The automated suite and race detector pass.

## 12. Phase 6b handoff

Phase 6b (the Codex catalog + `auto_review_model_override`) reuses this phase's
seams: `catalog.Decode` for the Copilot model list and `forward.FetchModels` for
the credentialed fetch. It adds `client_version` query gating, the Codex
`ModelInfo` shape (sourced from `openai/codex`'s own `models.json`, which is the
only real basis for the required fields Copilot never returns —
`base_instructions`, `truncation_policy`, `supported_reasoning_levels`, …), and
config-driven reviewer routing that patches `auto_review_model_override` into the
advertised entries. That work carries an external dependency and a heavier
ToS/fragility surface, which is why it is a separate spec. Phase 6a promises 6b a
reusable decode/fetch seam, not that its handler or `Descriptor` is 6b's final
abstraction.
