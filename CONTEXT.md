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
Bearer` on every upstream inference call.
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
One of the two inbound API dialects copilotd serves — the Anthropic surface
(`/anthropic/...`) and the OpenAI surface (`/openai/...`). Each forwards only to
its matching upstream dialect; never cross-translated.
_Avoid_: provider, endpoint (unqualified)

**Route**:
The registered upstream path a Surface exposes — `/v1/messages`,
`/v1/messages/count_tokens`, or `/responses`. Unique within a Surface, not assumed
globally unique (a later Surface may reuse a path).
_Avoid_: endpoint (unqualified), path (unqualified)

**Endpoint**:
A specific served entry point, identified by the `(Surface, Route)` pair — the
qualified sense in which "endpoint" is allowed (bare "endpoint" is still avoided for
Surface and Route).

**Forwarder**:
The dumb core that moves a request to Copilot and the response back with minimal
re-interpretation (raw passthrough) — deserializing nothing beyond a shallow peek.
_Avoid_: proxy (unqualified), router

**Impersonation**:
Presenting the request to Copilot as the VS Code Copilot client, via a fixed
header set (integration-id, editor-version, …), so upstream client checks pass.

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
A terminal error event copilotd originates when an upstream stream dies without
one, so a client's SSE parser never hangs. A copilotd-originated signal — never
conflated with a forwarded upstream terminal.
_Avoid_: fake terminal, injected error (unqualified)

### Runtime state

**Ready / Not-ready**:
copilotd is *ready* when its last mint attempt succeeded — it stays ready across
idle token expiry (the next request re-mints) and flips *not-ready* only when a
mint fails. Surfaced at `/readyz`; when not-ready, provider routes return `503`.
_Avoid_: healthy (that is liveness, `/healthz`)

**Degraded**:
Running but not-ready — serving `/healthz` and refusing provider routes with
`503` — because no mint has yet succeeded (or the last one failed).
