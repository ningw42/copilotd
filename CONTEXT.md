# copilotd

copilotd is a single-binary proxy that serves the Anthropic Messages API and the
OpenAI Responses API off a GitHub Copilot subscription. This glossary fixes the
vocabulary — above all the credential-like things, whose confusion is the
project's chief hazard.

## Language

### Credentials

**API key**:
The inbound secret an operator invents and clients present to copilotd (as
`Authorization: Bearer` or `x-api-key`). Gates copilotd's own front door; never
sent upstream.
_Avoid_: managed token, token (unqualified)

**GitHub OAuth token**:
The long-lived GitHub user-to-server token obtained via login (or injected).
Durable — no timed expiry; dies only on revocation. Stored raw in the owner-only
GitHub OAuth token file; the sole input to the exchange.
_Avoid_: credential, oauth, gho token

**GitHub OAuth token file**:
The owner-only durable file that stores the raw GitHub OAuth token. Login writes
it atomically; serve reads it as a token source.
_Avoid_: token file, credential file

**Copilot token**:
The short-lived (~25 min) bearer token the exchange mints from the GitHub OAuth
token. Held in memory only, re-minted continuously, and sent as `Authorization:
Bearer` on every authenticated upstream Copilot call.
_Avoid_: access token, session token, token (unqualified)

### Identity lifecycle

**Exchange**:
The call to `GET api.github.com/copilot_internal/v2/token` that trades a GitHub
OAuth token for a Copilot token (plus its `expires_at` / `refresh_in`).
_Avoid_: auth, token swap

**Mint**:
To produce a Copilot token via a successful exchange.

**Startup mint**:
The single mint at boot — asynchronous, retried a bounded number of times on
transient failure — that warms readiness and surfaces a bad credential early.

**On-demand mint**:
Minting a Copilot token inside a request's path when the cached one is missing or
stale (within a safety margin of expiry). The only ongoing mint trigger — there
is no scheduled refresh.
_Avoid_: refresh, scheduled refresh, background refresh

**Resolve**:
Reading the GitHub OAuth token from its source (inline value, then GitHub OAuth
token file) at startup. A local read, not a network call.

**Login**:
The `copilotd login` device flow that obtains a GitHub OAuth token and writes it
to the GitHub OAuth token file. Obtains the OAuth token only; performs no
exchange.

### Surfaces & forwarding

**Surface**:
One of the three inbound API dialects copilotd serves — the Anthropic surface
(`/anthropic/...`), the OpenAI surface (`/openai/...`), and the GitHub Copilot
surface (initially `/models`). Each endpoint forwards only to its matching
upstream Surface and Route; never cross-translated.
The `Surface` type lives in `internal/endpoint`; error rendering (`apierror`)
and the other consumers depend on it, not the reverse.
_Avoid_: provider, endpoint (unqualified)

**Route**:
The registered upstream path a Surface exposes — `/v1/messages`,
`/v1/messages/count_tokens`, `/responses`, or `/models`. Unique within a Surface,
not assumed globally unique (a later Surface may reuse a path).
Modeled as the single `endpoint.Route` type, shared by HTTP forwarding, catalog
required-route membership, and shim dispatch; the earlier separate `shim.Route`
and `catalog.Route` types are removed.
_Avoid_: endpoint (unqualified), path (unqualified)

**Endpoint**:
How copilotd serves one operation — an inbound binding paired with an upstream
(outbound) dependency, modeled as a typed served *contract* (one of: HTTP
forward, WebSocket forward, raw passthrough, or Catalog). A route with an
inbound side but no outbound dependency (`/healthz`, `/readyz`) is not an
Endpoint. An Endpoint owns its Surface, so Surface-level facts (the terminal
event, the error dialect) are governed through it; a fact sits directly on the
Endpoint only when it can differ between two endpoints of the same Surface (for
example, whether it may stream). Lives in `internal/endpoint`; rendering,
handlers, authentication, clients, and logging are kept out. Replaces the
earlier `(Surface, Route)`-pair sense.
_Avoid_: "valid Endpoint identities", "valid (Surface, Route) pair"

**`/models` Endpoint note**:
`/models` is one upstream path serving three Endpoints: the raw passthrough plus
both catalogs' outbound source — the same upstream dependency, but three
different served contracts.

**Catalog**:
A provider-shaped model list served on a Surface's `/models` — Copilot's raw
`/models` fetched once, filtered to the models that Surface can forward, and
re-rendered in the real provider's `GET /v1/models` schema. Carries the provider's
*schema* with Copilot's *values*, not value-level provider parity. Distinct from
the GitHub Copilot Surface's raw `/models` passthrough, which reshapes nothing.
This is the **provider-shaped** catalog; the **Codex catalog** is the client-shaped
counterpart.
_Avoid_: model list (unqualified); models endpoint (that is the raw passthrough)

**Forwarder**:
The dumb core that moves a request to Copilot and the response back with minimal
re-interpretation (raw passthrough) — deserializing nothing beyond a shallow peek.
The approved WebSocket transport retains `wsforward.Proxy` as its exported Go
identifier; outside code references, call it the **WebSocket forwarder**.
_Avoid_: proxy (unqualified), router

**Impersonation**:
Presenting the request to Copilot as the VS Code Copilot client via a header set,
so upstream client checks pass. The two version-bearing headers (`Editor-Version`;
`Editor-Plugin-Version` / `User-Agent`) are **discovered at runtime** and kept
current, with a static **fallback** when discovery has not succeeded;
`Copilot-Integration-Id` and `X-GitHub-Api-Version` are fixed.

**Discovery**:
Fetching the current VS Code and Copilot Chat versions at runtime from their
public Microsoft release endpoints, to keep the impersonated version headers
current. Best-effort: the static fallback covers failure, and
`--impersonation-refresh-interval=0` disables it (air-gapped / locked-egress).
_Avoid_: refresh (reserved for the token-mint sense).

**Shim**:
A composable middleware layer that closes one specific parity gap (Phase 3+). Not
present in Phase 1.
_Avoid_: middleware as the *name* of the mechanism — call it a shim (nested via the
onion); "middleware" stays reserved for the `http.Handler` request pipeline. Also
plugin, filter.

**Prelude**:
The response envelope — status line plus headers — treated as a unit distinct from
the body. Its shim transform runs once per response, before the body, on both the
buffered and streaming paths (Phase 3+).

### Codex catalog & auto-review

**Codex catalog**:
The client-shaped model list served on the OpenAI Surface's `/models` when a
request carries Codex's `?client_version=` and the feature is enabled — Codex's own
`ModelInfo` entries (from a vendored snapshot) re-emitted field-for-field with a
reviewer override injected. Carries *Codex's* schema and values, not Copilot's.
Contrast the provider-shaped **Catalog**.
_Avoid_: model list (unqualified).

**Reviewer model**:
The real, forwardable model copilotd routes Codex's guardian auto-review to via
`auto_review_model_override`, replacing Codex's unforwardable default
`codex-auto-review`.
_Avoid_: auto-reviewer, guardian model.

**Command-auth provider**:
The Codex `[model_providers.NAME.auth]` configuration whose `command` prints
copilotd's API key to stdout — the only condition (`has_command_auth()`) under
which Codex fetches a self-hosted proxy's model catalog.
_Avoid_: auth provider (unqualified).

**Vendored snapshot**:
The pinned copy of Codex's bundled `models.json` (`rust-v0.144.5`) embedded in
copilotd, carried with Apache-2.0 `LICENSE`/`NOTICE` and a `PROVENANCE` record —
the only faithful source for the `ModelInfo` fields Copilot never returns.
_Avoid_: snapshot (unqualified) where ambiguous.

### Streaming

**Terminal event**:
The event that legitimately ends an SSE stream — Anthropic `message_stop`; OpenAI
`response.completed` / `response.failed` / `response.incomplete` (an upstream
`error` event also ends it). copilotd detects it to tell a clean end from a
truncated one.
_Avoid_: end event, stop event, final event

**copilotd-originated signal**:
Any response copilotd itself produces rather than forwards from Copilot — the
auth/readiness/limit errors and the synthesized stream terminals. The proxy's only
divergence from a genuine first-party endpoint; enumerated exhaustively (the
"divergence ledger") and identified off-band (request-id, logs), never by a field
on the wire.
_Avoid_: proxy error, internal error

**Synthesized terminal**:
A terminal error event copilotd originates when an upstream stream on a Route
whose contract includes SSE semantics dies without one, so a client's SSE parser
never hangs; a raw passthrough Route does not acquire SSE semantics from a
`Content-Type` value alone. It is a copilotd-originated signal, never conflated
with a forwarded upstream terminal.
_Avoid_: fake terminal, injected error (unqualified)

### Runtime state

**Ready / Not-ready**:
copilotd is *ready* when its last mint attempt succeeded — it stays ready across
idle token expiry (the next request re-mints) and flips *not-ready* only when a
mint fails. Surfaced at `/readyz`; when not-ready, Surface endpoints return `503`.
_Avoid_: healthy (that is liveness, `/healthz`)

**Degraded**:
Running but not-ready — serving `/healthz` and refusing Surface endpoints with
`503` — because no mint has yet succeeded (or the last one failed).
