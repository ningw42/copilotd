# copilotd — Roadmap

Run the **Anthropic Messages API** and the **OpenAI Responses API** off a GitHub
Copilot subscription, behind a managed API token.

This document is a **project-level roadmap**: it describes the high-level
components and the order we build them in. It deliberately does *not* specify how
any individual feature is implemented — those decisions belong to per-feature
design docs written as we reach each phase.

---

## 1. What this is

`copilotd` is a single-binary proxy. On the **inbound** side it exposes two
inference Surfaces that clients already know how to speak:

- **Anthropic Messages API**, mounted under `HOST/anthropic`
- **OpenAI Responses API**, mounted under `HOST/openai`

It also exposes the native **GitHub Copilot Surface** for support data at
unprefixed paths, beginning with `GET/HEAD HOST/models`.

On the **outbound** side it talks to GitHub Copilot's **raw Anthropic Messages
API**, **raw OpenAI Responses API**, and native support Routes, using a managed
GitHub Copilot credential.

It is **not** a translation engine. Anthropic requests are forwarded to Copilot's
Anthropic endpoint; Responses requests are forwarded to Copilot's Responses
endpoint. There is never any cross-family translation.

### Explicitly out of scope (non-goals)

- The Chat Completions API — in **either** direction (we neither accept it from
  clients nor call it upstream).
- Cross-family translation (Anthropic ⟷ Responses).
- Multi-tenant token minting, per-token quotas, or billing. Inbound auth is a
  single managed token (or a small static set).
- Multi-account pooling / rotation of GitHub Copilot subscriptions. One account.
- Embeddings. (Could later drop into the GitHub Copilot-native support tier if ever
  wanted, but it is not a goal.)

---

## 2. Guiding principles

These constrain every component below.

1. **Raw passthrough is the substrate.** The forward path moves request and
   response bodies with as little re-interpretation as possible. The moment we
   deserialize into strict typed structs and re-serialize, we silently drop
   unknown fields and fall *out* of parity with the real APIs. The hot path
   favors schema-agnostic passthrough; typed handling is reserved for the
   specific shims that need it.

2. **The onion is the only extension mechanism.** The core forwarder stays dumb.
   All feature parity lives in composable **shims** wrapped around it. A
   shim can do exactly two things:
   - rewrite the **inbound request** before it reaches the forwarder, and
   - transform the **outbound response** on the way back to the client.

   No shim ever talks to the upstream directly.

3. **No cross-family translation.** Anthropic-in → Anthropic-upstream;
   Responses-in → Responses-upstream. Full stop.

4. **Single static binary, four native targets, no services at rest.**
   Runs natively on `x86_64-linux`, `x86_64-windows`, `arm64-windows`, and
   `aarch64-darwin`. No database; state at rest is a single owner-only
   credential file.

### A note on the Go SDKs

Go's official `anthropic-sdk-go` and `openai-go` are useful for the **auth /
device-flow** work and for **typed logic inside individual shims**. They are
deliberately **not** used on the raw forward path — wrapping the core in an SDK
would re-serialize bodies through lossy types and defeat principle #1.

---

## 3. Architecture

### The onion

```
                inbound request                        outbound response
client  ─────────────────────────►  ┌───────────────┐ ◄─────────────────────────  client
   │                                │     shim A     │                                ▲
   │        rewrite request  ──────►│     shim B     │◄──────  transform response     │
   │                                │      ...       │         (JSON or SSE stream)   │
   ▼                                │ ┌───────────┐  │                                │
 inbound auth  ───────────────────► │ │   dumb    │  │ ──► GitHub Copilot upstream ───┘
 (managed token)                    │ │ forwarder │  │     (raw Anthropic / Responses)
                                    │ └───────────┘  │
                                    └───────────────┘
                       (SSE streaming engine underpins the forwarder
                        and every shim's response half)
```

### Component map

| Group            | Component                     | Responsibility                                                                                               |
| ---------------- | ----------------------------- | ------------------------------------------------------------------------------------------------------------ |
| **Edge**         | HTTP server & router          | Inbound listener; provider-namespaced routes; health                                                         |
| **Edge**         | Inbound auth                  | Constant-time validation of the managed token (`Authorization: Bearer` / `x-api-key`)                        |
| **Identity**     | GitHub↔Copilot identity mgr   | Device-flow login **or** injected token; `copilot_internal` token exchange; scheduled, single-flight refresh; owner-only credential file; header impersonation |
| **Core**         | Raw forwarder                 | Dumb upstream client: attach Copilot credential + impersonation headers, send to Copilot, return body untouched |
| **Core**         | SSE streaming engine          | Parse upstream SSE → re-emit downstream; keepalive pings; terminal-event enforcement; client-cancel propagation |
| **Parity**       | Shim framework                | The onion contract: request-transform + response/stream-transform; ordering; per-shim toggle                 |
| **Parity**       | Shim catalog                  | Individual shims (see §5). Grows over time.                                                                   |
| **Support**      | GitHub Copilot support        | GitHub Copilot-native endpoints plus deferred provider/client-shaped views (see §4.2)                        |
| **Cross-cutting**| Observability                 | Structured logging, request-id, metrics — present from Phase 0 (see §6)                                      |
| **Cross-cutting**| Configuration                 | Flags + env (+ optional file): bind address, managed token, timeouts, shim toggles                           |
| **Cross-cutting**| Build & distribution          | Native build per target; single binary; optional service install                                            |

---

## 4. Inbound surface

### 4.1 Provider-namespaced mounts

Each API surface is mounted under its provider prefix, so a client only has to
set its base URL and every path beneath it is native.

| Client base URL  | Endpoints                                                                         |
| ---------------- | --------------------------------------------------------------------------------- |
| `HOST/anthropic` | `POST /anthropic/v1/messages`, `POST /anthropic/v1/messages/count_tokens`          |
| `HOST/openai`    | `POST /openai/v1/responses` (+ response sub-paths: `GET/DELETE /responses/{id}`, cancel, input_items, …) |

### 4.2 Support endpoints — two tiers

GitHub Copilot support data follows a two-tier pattern:

- **GitHub Copilot-native tier** — the raw Copilot response exposed on its native
  Surface, e.g. `(GitHubCopilot, /models)` at `HOST/models` as a straight
  passthrough. Cheap; lands early.
- **Provider/client-shaped tier** — the same data reshaped to match what a real
  provider or a specific client expects, e.g. `HOST/anthropic/v1/models`,
  `HOST/openai/v1/models`, or Codex model-catalog metadata, with **best-effort
  sanitization**. Explicitly **deferred** to a later, lower-priority band.

---

## 5. The shim model

The shim contract is the heart of the parity story. Each shim is one layer
in the onion, individually toggleable.

- **Inbound half — easy.** The request is a single body, so a shim's
  request side is just `f(request) → request'`.
- **Outbound half — the hard half.** The response is usually an **SSE event
  stream, not a value**. So a shim's response side is a **stream
  transformer** `f(eventStream) → eventStream'`, which must be able to
  pass-through, edit, drop, inject, or coalesce events.

Design problems the shim framework has to answer (named here, solved in
the Phase 3 design doc):

- **Stateful transforms** across a whole stream (e.g. stable Responses item-IDs:
  remember the first id per `output_index` and rewrite every later event).
- **Pass-through vs. buffering.** Some shims work event-by-event (cheap, keeps
  the stream live); others must see more before emitting (buffering adds latency
  and partly surrenders streaming). The contract must let each shim choose and
  make the cost explicit.
- **Composition with the stream concerns the core already owns** — keepalive
  pings, terminal-event enforcement (`message_stop` / `response.completed`), and
  client-cancel propagation. Shims must nest inside these without breaking
  them.

### Seed shim catalog

Starting set; the catalog grows as parity gaps surface.

- **Stable Responses item-IDs** — Copilot emits a different opaque `id` on every
  Responses SSE event for the same item, violating OpenAI's stable-id contract;
  pin the first-seen id per `output_index`.
- **Model-name mapping** — normalize client-facing model aliases to Copilot's
  names on the way in, preserve the client's requested name on the way out.
- **Unsupported-param handling** — sanitize / reject params that Copilot's
  endpoints don't accept, with clear errors.

---

## 6. Cross-cutting: observability from day one

Observability is **not** a finishing phase. If it isn't wired into the walking
skeleton it never gets retrofitted cleanly. It is established in Phase 0 and each
subsequent component emits its own signals as it lands:

- **Structured logging** with a per-request request-id, propagated end to end.
  Logs never echo secrets (managed token, GitHub token, Copilot token).
- **Metrics** scaffolding (request counts/latency by route template, upstream
  outcomes, token-refresh events, stream terminal outcomes).

We *account for* observability from the beginning even where we don't build out
every metric yet — the seams are there from Phase 0.

---

## 7. Phased roadmap

Horizontal layering: each inference capability is built across **both inference
Surfaces** before we move up a layer. Observability (§6) threads through all
phases.

### Phase 0 — Foundations / walking skeleton
Module layout, HTTP server + router, configuration (flags/env), **structured
logging + request-id + metrics scaffolding**, health endpoint, native build.
_Outcome: the binary runs and is observable._

### Phase 1 — Core forward path, both inference Surfaces (non-streaming)
Three workstreams that must all land together to make the first real call:
- **Inbound auth** — managed-token validation.
- **GitHub↔Copilot identity** — device-flow login or injected token; token
  exchange; scheduled, single-flight refresh; owner-only credential file.
- **Raw forwarder** — dumb passthrough with header impersonation, on the
  provider-namespaced routes.

_Outcome: JSON round-trips work end-to-end against Copilot on both inference
Surfaces._

> Note: identity and the forwarder are merged into one band because a forward
> cannot reach Copilot without the outbound credential; they have to land
> together to produce the first real call.

### Phase 2 — SSE streaming engine, both inference Surfaces
Upstream SSE parse → downstream emit; keepalive pings; terminal-event
enforcement; client-cancel/disconnect propagation.
_Outcome: genuinely usable with real streaming clients (Claude Code, Codex, SDKs)._

### Phase 3 — Shim framework
The onion contract: request-transform + response/stream-transform, ordering, and
per-shim toggling — proven with a no-op passthrough shim. No shims yet,
just the mechanism.
_Outcome: a place to hang parity features without touching the core._

### Phase 4 — GitHub Copilot support endpoint
Implement the GitHub Copilot-native support tier: `(GitHubCopilot, /models)` at
`HOST/models` as a raw Copilot passthrough.
_Outcome: raw Copilot support data is available through its native endpoint._

### Phase 5 — Shim catalog (seed)
Implement the seed shims from §5. This band is expected to keep growing after
the roadmap's other phases are "done."
_Outcome: real parity gaps are closed; the shim catalog is open-ended._

### Phase 6 — Provider/client-shaped support endpoints (deferred)
`HOST/anthropic/v1/models` and `HOST/openai/v1/models` with best-effort sanitization to
match the real providers, plus a Codex catalog view that advertises configurable
reviewer routing through `auto_review_model_override`. Low priority.
_Outcome: support endpoints look native to their provider or client._

> Dependency note: Phases 4 and 5 are independent. Phase 6 builds on Phase 4's
> upstream support-data plumbing, but does not depend on Phase 5.

### Phase 7 — Distribution & service management
Optional daemon / OS-service install (systemd, launchd, Windows SCM), packaging
across the four native targets (cross-compilation is a cheap opt-in with Go, not
a requirement), and documentation.
_Outcome: easy to run and keep running on every target._

---

## 8. Risks & constraints

- **Copilot ToS / abuse detection.** The approach depends on impersonating the
  VS Code Copilot client (integration-id and editor headers). Automated or bulk
  use can trip GitHub's abuse detection. This is inherently unsupported and
  fragile.
- **Upstream API drift.** Copilot can change endpoints, headers, model names, or
  streaming behavior at any time; the raw-passthrough core limits our exposure
  but the shims and identity layer are drift-sensitive.
- **Streaming shim complexity.** The pass-through-vs-buffering tension
  (§5) is the sharpest design risk; getting the stream-transformer contract
  right early keeps the shim catalog cheap to grow.

---

## 9. Target platforms

Native single binary on:

- `x86_64-linux`
- `x86_64-windows`
- `arm64-windows`
- `aarch64-darwin`

Go covers all four natively; cross-compilation is available cheaply if desired
but is not a hard requirement of the project.
