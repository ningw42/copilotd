# Phase 1 — Core forward path, both surfaces (non-streaming) — Design

Status: approved design (refined via brainstorming session), pending implementation plan
Date: 2026-07-14
Roadmap reference: `ROADMAP.md` §7 "Phase 1 — Core forward path, both surfaces (non-streaming)"
Builds on: `docs/design/2026-07-13-phase-0-foundations-design.md`

## 1. Goal & outcome

Make the first real call. Phase 0 gave us an observable binary that serves
`/healthz`. Phase 1 adds the three workstreams the roadmap bundles — inbound
auth, GitHub↔Copilot identity, and the raw forwarder — because none of them
produces a real call alone.

**Outcome:** non-streaming JSON requests round-trip end to end against GitHub
Copilot on **both** surfaces — `POST /anthropic/v1/messages` (+ `count_tokens`)
and `POST /openai/v1/responses` — authenticated by a required API key, forwarded
with an impersonated GitHub Copilot credential that is minted and refreshed
automatically.

Streaming is explicitly **not** in Phase 1 (Phase 2 owns the SSE engine); a
`stream:true` request is cleanly rejected, not half-supported.

### 1.1 The onion, this phase

```
 client ──(API key)──► requestID → accessLog → recover → [/anthropic,/openai only:] auth → readiness → forward ──(Copilot bearer)──► GitHub Copilot
          INBOUND gate                                                                                        OUTBOUND credential
```

The API key is the inbound gate (a secret the operator invents, presented by
clients). The Copilot bearer token is the short-lived outbound credential, minted
by the identity manager. They are opposite ends of the onion and never conflated.

## 2. Scope

**In scope (Phase 1):**

- **CLI restructure** to a subcommand tree: `serve` (the daemon), `login` (device
  flow), `help`, `version`. The bare `copilotd` prints help.
- **Inbound auth** — a single required API key, constant-time validated, accepted
  as `Authorization: Bearer` or `x-api-key`, gating the provider routes only.
- **GitHub↔Copilot identity** — OAuth device-flow login (`copilotd login`) **or**
  an injected token; the `copilot_internal/v2/token` exchange; scheduled
  single-flight refresh with a strict single-in-flight invariant; owner-only
  OAuth-token file; the impersonation header set.
- **Raw forwarder** — hand-rolled dumb passthrough on the three provider routes,
  with header impersonation, denylist header policy, per-request body bounding,
  and verbatim upstream response/error passthrough.
- **Non-streaming boundary** — `stream:true` rejected with a provider-shaped error.
- **Provider-shaped error bodies** (`internal/apierror`) for proxy-originated
  errors, Anthropic-shaped on `/anthropic`, OpenAI-shaped on `/openai`.
- **Readiness split** — `GET /readyz` reflecting identity health; `/healthz`
  stays pure liveness.
- Configuration, structured logging, and metric *seams* for all of the above.
- TDD unit + integration + end-to-end smoke tests.

**Out of scope (deferred — see §12):** the SSE streaming engine (Phase 2); the
middleware/onion framework (Phase 3); every seed shim — self-heal 401 retry,
unsupported-param/beta stripping, model-name mapping, stable Responses item-ids
(Phase 4); `/models` and other support endpoints (Phase 4/5); the Responses
management sub-paths (`GET/DELETE /responses/{id}`, cancel, input_items);
multi-account pooling; cross-compilation and service install (Phase 6).

### 2.1 Scope boundary: identity refresh vs. self-heal retry

Two token-refresh behaviors sound similar and must not be conflated:

- **Proactive scheduled refresh** (Phase 1, this design): the identity manager
  re-mints the Copilot token *before* it expires, on a timer.
- **Reactive self-heal retry** (Phase 4 shim): re-minting *in response to* a `401`
  from the upstream inference call and replaying the request.

Phase 1 builds only the former. The forwarder stays dumb: an upstream `401`
passes straight back to the client, exactly like any other upstream status.

## 3. Guiding decisions & rationale

| Decision | Choice | Rationale |
| --- | --- | --- |
| Forwarder implementation | Hand-rolled, not `httputil.ReverseProxy` | ~40 focused lines; total control over exactly which headers cross; deserializes nothing (raw passthrough, principle #1); avoids ReverseProxy's streaming machinery that Phase 2's *custom* SSE engine won't reuse. |
| Header policy | Denylist / passthrough | Forward every inbound header except an explicit strip-set. Faithful to raw passthrough and future-proof — a new protocol header (`anthropic-beta`, …) flows through without a code change; header *stripping* stays a Phase 4 shim. |
| Body inspection | Full-buffer (bounded) + shallow `stream` peek + forward original bytes | Reads only the `stream` field to enforce the non-streaming boundary, then forwards the *original* bytes — no re-serialization (dodges the SDK-re-serialization `400`s observed upstream). |
| Upstream errors | Pass through verbatim | Copilot's `/v1/messages` and `/responses` already return correctly Anthropic-/OpenAI-shaped errors. Only proxy-*originated* errors are synthesized. |
| API key requirement | Always required (fail-fast) | copilotd forwards every accepted request with the operator's real Copilot credential. A bare, unauthenticated endpoint is never allowed, even on loopback (loopback does not isolate local users). |
| API-key compare | SHA-256 both sides → `subtle.ConstantTimeCompare` | Fixed-length constant-time compare with no length leak. |
| OAuth token persistence | Only the long-lived GitHub OAuth token, raw, in an owner-only file | Minimizes secrets at rest ("single owner-only credential file"). The short-lived Copilot token lives in memory only and is cheap to re-mint. |
| Identity startup posture | Hybrid: fail-fast on no credential, else degrade | Distinguishes "nothing to try" (misconfig → exit) from "GitHub is flaky right now" (transient → serve degraded, `/readyz` not-ready, background retry). Robust long-running-daemon behavior. |
| Refresh concurrency | One `singleflight` key for background *and* on-demand refresh | Guarantees **at most one exchange in flight globally**; a request burst that finds an expired cache collapses to a single exchange. |
| CLI shape | Subcommand tree via `ff/v4` `ff.Command` + `ffhelp` | `serve`/`login` need genuinely different flags; a git-style tree with `help <sub>` is the modern, discoverable interface and reuses the dependency Phase 0 already pulls in. |
| Outbound timeout | Per-request context deadline (default `600s`), no `http.Client.Timeout` | A blunt client timeout would kill a legitimately slow completion; the context deadline also gives client-cancel propagation for free. |
| New dependency | `golang.org/x/sync/singleflight` only | The Go team's own module; everything else stays stdlib. |

## 4. Module layout & package boundaries

Extending Phase 0's conventions — small, single-purpose, dependency-injected units.

```
copilotd/
├── cmd/copilotd/main.go     # composition root + ff.Command tree (serve/login/help/version)
└── internal/
    ├── config/              # ServeConfig + LoginConfig + Load*/validate (+ LogValue redaction)
    ├── logging/             # unchanged, reused
    ├── build/               # unchanged, reused
    ├── apierror/    [NEW]    # provider-shaped error bodies (Anthropic + OpenAI), one leaf
    ├── identity/    [NEW]    # device flow · exchange · single-flight refresh · token file · headers
    ├── forward/     [NEW]    # the dumb forwarder: path map · header build · body bound · Do · copy-back
    └── server/              # + auth middleware · readiness gate · provider routes · /readyz
```

Each new unit — *what it does · how it is used · what it depends on*:

- **`internal/apierror`** — a leaf with no copilotd dependencies. `Write(w,
  provider, kind, msg)` renders the correct status + JSON body per provider. Used
  by `server` (auth `401`, readiness `503`) and `forward` (stream reject, `413`,
  `502`, `504`). Depends on `net/http` + `encoding/json`.

- **`internal/identity`** — owns all Copilot-credential concerns. Exposes a
  `Credential` snapshot (base URL + token + impersonation headers), a `Ready()`
  predicate, a background `Run(ctx)` refresh loop, and a `login` entry point.
  Used by `forward` (per-request `Current`), `server` (readiness), and `main`
  (`login`, `Run`). Depends on `net/http`, `encoding/json`, `crypto`,
  `golang.org/x/sync/singleflight`, `config`, `logging`. Knows nothing about the
  inbound HTTP surface.

- **`internal/forward`** — the dumb upstream client. Given a route's upstream
  path + provider tag and an `identity` handle, it bounds/reads the body, peeks
  `stream`, builds the outbound request, calls it, and copies the response back.
  Used by `server` (route handlers). Depends on `net/http`, `identity`,
  `apierror`, `config`. Knows nothing about Copilot credentials beyond the
  `Credential` seam.

- **`internal/server`** — gains the auth middleware, the readiness gate, provider
  route registration, and `/readyz`. Depends on `net/http`, `forward`,
  `identity`, `apierror`, `logging`, `config`, `build`.

**Key boundaries:** `identity` holds all Copilot-specificity; `forward` is
Copilot-agnostic (it only sees the `Credential` value). `apierror` is the single
definition of the two error shapes, so no other package hand-rolls them.

## 5. CLI / command tree

The entrypoint becomes a git-style subcommand tree, assembled with `ff/v4`'s
`ff.Command` and rendered with `ffhelp`:

```
copilotd                    → prints general help (identical to `copilotd help`)
copilotd help               → general help (root usage + subcommand list)
copilotd help serve         → serve's help
copilotd help login         → login's help
copilotd serve  [flags]     → run the HTTP daemon (the forward path)
copilotd login  [flags]     → device flow → write the OAuth-token file
copilotd version            → build info (`--version` retained as an alias)
```

- **Root** carries the inherited flags — `--log-level`, `--log-format`,
  `--log-file`, `--config`, `--github-oauth-token-file` — so both `serve` and
  `login` share logging, the config file, and the token-file path. Root's `Exec`
  prints help.
- **`help`** is a small hand-rolled subcommand: with no argument it prints root
  usage; `help <name>` looks the subcommand up in the tree and renders its
  `ffhelp`. (ff's built-in help is the `-h`/`--help` flag; the `help <sub>` verb
  is the ~15-line piece we add — an implementation detail to confirm, not a
  design fork.)
- The testable `run(args, env, stdout, stderr) int` entrypoint from Phase 0 is
  preserved; it now drives `rootCmd.ParseAndRun` and returns the exit code.
- `serve` reuses the **entire** Phase 0 lifecycle unchanged (config load → build
  logger + `slog.SetDefault` → bind listener → signal-aware context → graceful
  shutdown with second-signal hard-kill). It is the same server, relocated under
  a verb, with the Phase 1 routes and middleware added.

## 6. Identity manager (`internal/identity`)

### 6.1 The seam

```go
// Credential is an immutable snapshot the forwarder applies to an outbound request.
type Credential struct {
    BaseURL string      // from the exchange response's endpoints.api (fallback: api.githubcopilot.com)
    Token   string      // short-lived Copilot bearer token (secret)
    Headers http.Header // static impersonation set (integration-id, editor-version, ...)
}

func (m *Manager) Current(ctx context.Context) (Credential, error) // fast path: cached token; single-flight refresh if missing/expired
func (m *Manager) Ready() bool                                     // for /readyz and the provider readiness gate
func (m *Manager) Run(ctx context.Context)                         // background single-flight refresh loop
```

`forward` builds `outURL = cred.BaseURL + upstreamPath`, sets `Authorization:
Bearer <cred.Token>`, and copies `cred.Headers`. No Copilot knowledge leaks into
`forward`.

### 6.2 Token exchange

`GET https://api.github.com/copilot_internal/v2/token` with `Authorization:
token <oauth>` **plus the editor/integration impersonation headers** (the token
endpoint enforces a client/user-agent allowlist check). The JSON response is
parsed into an internal `copilotToken{raw, expiresAt, refreshIn, baseURL}`:

- `token` → `raw` (treated as **opaque**; passed verbatim, never parsed for auth
  logic).
- `expires_at` → `expiresAt` (hard expiry).
- `refresh_in` → `refreshIn` (intended re-mint interval, shorter than expiry).
- `endpoints.api` → `baseURL` (per-account host; the `--upstream-base` override,
  if set, wins).

### 6.3 Failure classification

Exchange failures are classified so the daemon's degraded state is legible:

- **Auth-class** (`401`/`403`/`404`): the OAuth token is invalid/revoked or the
  account is not entitled to Copilot. **Permanent** — not fixable by retrying.
  Logged at `error` with a distinct "not transient — check the Copilot
  subscription" message plus the status and response body. The daemon still
  degrades rather than exits (consistent with the hybrid posture: we had a token
  to try), but the operator is not left guessing.
- **Transient** (`429`/`5xx`/network/timeout): retried with capped backoff.

### 6.4 Refresh & the single-in-flight invariant

A single background goroutine (`Run`) sleeps until `refreshIn` elapses (minus a
small jitter), re-exchanges, and reschedules from the new `refreshIn`. On failure
it keeps the current token and retries with capped backoff until `expiresAt`;
once past hard expiry with no fresh token, `Ready()` returns false
(serve-stale-until-hard-expiry).

**Invariant: at most one exchange is in flight globally, at any moment.** Both the
background loop and any request-triggered emergency refresh (a request arriving
with an expired cache before the background loop caught up — e.g. just after
startup or a long idle) route through **one** `singleflight.Group.Do("refresh",
…)` key. They therefore cannot both fire, and a burst of requests collapses to a
single exchange whose result all callers share.

### 6.5 OAuth-token file & source precedence

- **File:** the raw GitHub OAuth token, stored at
  `<os.UserConfigDir()>/copilotd/github-oauth-token` by default
  (`~/.config/…` on Linux, `~/Library/Application Support/…` on macOS,
  `%AppData%\…` on Windows), overridable via `--github-oauth-token-file`.
  Written **atomically** (temp file `0600` in the same dir → rename). On read,
  Unix permissions broader than `0600` cause a **refusal** with a clear error
  (fail-closed for a secret, ssh-style). Windows' permission model differs, so
  enforcement there is best-effort via the per-user profile directory, with a
  documented caveat (mirrors Phase 0's Darwin static-binary caveat).
- **Source precedence:** inline `--github-oauth-token` / `COPILOTD_GITHUB_OAUTH_TOKEN`
  / config-file `github-oauth-token` **>** the token file. If *neither* yields a
  token, the daemon **fails fast** with a "run `copilotd login`" message. Once any
  OAuth token is present, the daemon starts and degrades-on-exchange-failure —
  fail-fast fires **only** when there is nothing to try.
- Auto-reading VS Code's `~/.config/github-copilot/apps.json` is deliberately
  **not** done (explicit secret sources only); noted as a possible later
  convenience.

### 6.6 `copilotd login` (device flow)

`login` obtains **only** the GitHub OAuth token; it performs no verifying
exchange (entitlement failures surface at first `serve` exchange, classified per
§6.3). Flow:

1. `POST https://github.com/login/device/code` with `{client_id, scope}`
   (defaults `Iv1.b507a08c87ecfe98`, `read:user`; both configurable — the
   client id override supports GitHub Enterprise Server).
2. Print the returned `verification_uri` and `user_code`.
3. Poll `POST https://github.com/login/oauth/access_token` at the returned
   `interval`, handling `authorization_pending`, `slow_down` (back off),
   `expired_token`, and `access_denied`.
4. On success, fetch `GET https://api.github.com/user` and print the
   authenticated login as confirmation (not persisted), then write the raw token
   to the OAuth-token file (`0600`, atomic) and print its path.

### 6.7 Impersonation header set

Config values, seeded with research-confirmed working defaults, each a knob
because they are version-sensitive. Applied to both the exchange request (§6.2)
and every inference request.

| Header | Default | Notes |
| --- | --- | --- |
| `Copilot-Integration-Id` | `vscode-chat` | Essential; the universally-working id |
| `Editor-Version` | `vscode/1.104.1` | Presence-checked |
| `Editor-Plugin-Version` | `copilot-chat/0.26.7` | |
| `User-Agent` | `GitHubCopilotChat/0.26.7` | Token endpoint enforces a UA check |
| `X-GitHub-Api-Version` | `2025-04-01` | Version-sensitive knob |

### 6.8 Observability

Structured logs for: each login step; each refresh (outcome, next-refresh
interval); degraded↔ready transitions. The OAuth and Copilot tokens are **never**
logged (Phase 0's redaction discipline). Metric seam: token-refresh events and
outcomes (the roadmap's named signal).

## 7. Forward path (`internal/forward`)

### 7.1 Route → upstream path map

Explicit per-route mapping (not a blind prefix-strip — note the `/v1` asymmetry):

| Inbound route | Upstream path | Provider tag |
| --- | --- | --- |
| `POST /anthropic/v1/messages` | `/v1/messages` | anthropic |
| `POST /anthropic/v1/messages/count_tokens` | `/v1/messages/count_tokens` | anthropic |
| `POST /openai/v1/responses` | `/responses` | openai |

Each route is a thin handler closure carrying its upstream path + provider tag;
the provider tag selects the error shape for any proxy-originated error. The
upstream host comes from `Credential.BaseURL`.

### 7.2 Body bounding & the `stream` peek

Wrap `r.Body` in `http.MaxBytesReader(w, r.Body, maxRequestBytes)` (default
32 MiB — a safety rail against pathological bodies, generous enough for
multi-image base64). Read the bounded body **fully into memory**, then
`json.Unmarshal` into `struct{ Stream *bool }` — reading *only* the `stream`
field — and forward the **original bytes** (`bytes.NewReader(body)`), never a
re-serialized version.

- `stream:true` → reject with a provider-shaped error (`StreamUnsupported`).
- `stream:false`, absent, or a non-JSON body → treat as non-streaming and forward
  (we are not a JSON validator; malformed bodies get Copilot's own `400`).
- `count_tokens` never streams, so the peek is a harmless no-op there.
- Exceeding `maxRequestBytes` → `413` (`PayloadTooLarge`).

### 7.3 Inbound → outbound header policy (denylist / passthrough)

Forward **every** inbound header except an explicit strip-set; then set ours.

- **Strip:** `Authorization` and `X-Api-Key` (our API key — must never leak
  upstream), `Host`, `Content-Length` (recomputed), and hop-by-hop headers
  (`Connection`, `Keep-Alive`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`,
  `Proxy-Authenticate`, `Proxy-Authorization`, plus any header named in the
  inbound `Connection` header).
- **Set (ours, replacing any client value):** `Authorization: Bearer
  <cred.Token>` and the impersonation set from `identity`.
- **Pass through:** everything else (`Content-Type`, `Accept`,
  `anthropic-version`, `anthropic-beta`, …). Header *stripping/normalization* is a
  Phase 4 shim; Phase 1 forwards faithfully and lets any resulting upstream `400`
  pass back.

### 7.4 Outbound client

A dedicated `*http.Client` (separate from identity's exchange client — different
timeout profile) with a tuned `Transport`: connection pooling
(`MaxIdleConns`/`MaxIdleConnsPerHost`/`IdleConnTimeout`),
`http.ProxyFromEnvironment` (corporate-proxy friendly), default TLS verification.

- **No `http.Client.Timeout`** (a blunt cap would kill a legitimately slow
  completion). Instead each request uses `ctx, cancel :=
  context.WithTimeout(r.Context(), outboundTimeout)` (default `600s`).
- Deriving from `r.Context()` gives **client-cancel propagation** for free: a
  client disconnect cancels the upstream call.

### 7.5 Response copy-back & the error rule

- **Upstream responded (any status, including `400`/`429`/`500`):** copy the
  status code and all response headers (minus hop-by-hop), then `io.Copy` the
  body downstream — **verbatim**. Copilot's error bodies are already correctly
  Anthropic-/OpenAI-shaped.
- **Proxy-originated error (around the call):** synthesized via `apierror` with
  the route's provider tag — `stream:true` reject, body-too-large (`413`),
  dial/unreachable (`502` `BadGateway`), outbound timeout (`504`
  `GatewayTimeout`).

## 8. Inbound auth, readiness & error shapes (`internal/server`)

### 8.1 Auth middleware

Mounted on the `/anthropic/*` and `/openai/*` subtrees only (never `/healthz` or
`/readyz`). Extracts the presented key from `Authorization: Bearer <k>` **or**
`x-api-key: <k>`, then compares constant-time: **SHA-256 both sides →
`subtle.ConstantTimeCompare`** (fixed-length, so no length leak). A missing or
mismatched key → provider-shaped `401` (`Unauthorized`). The key is never logged.
The API key is **required**: if unset, `serve` fails fast at startup (no bare
endpoint is ever served).

### 8.2 Readiness gate & endpoints

- A readiness gate wraps the same provider subtrees, **after** auth — so an
  unauthenticated caller gets `401`, never a `503` that would leak readiness
  state. When `identity.Ready()` is false → provider-shaped `503` (`NotReady`).
- **`GET /readyz`** (unauthenticated): `200 {"status":"ready"}` when
  `identity.Ready()`, else `503 {"status":"not ready"}`.
- **`GET /healthz`** stays pure liveness (Phase 0, unchanged): `200` while the
  process is up.

Order within the provider subtree: `requestID → accessLog → recover → auth →
readiness → forward`.

### 8.3 Provider-shaped errors (`internal/apierror`)

One leaf defines both shapes so nothing else hand-rolls them:

- **Anthropic:** `{"type":"error","error":{"type":"authentication_error |
  invalid_request_error | api_error","message":"…"}}`
- **OpenAI:** `{"error":{"message":"…","type":"…","code":"…","param":null}}`

API: `apierror.Write(w, provider, kind, msg)` over a small `Kind` set —
`Unauthorized` (`401`), `NotReady` (`503`), `StreamUnsupported` (`400`),
`PayloadTooLarge` (`413`), `BadGateway` (`502`), `GatewayTimeout` (`504`) — each
mapped to its HTTP status and the provider's error `type`.

## 9. Configuration

`ff/v4`-backed, extending Phase 0. Env prefix `COPILOTD_`; precedence **flags >
env > TOML file > default**; env-var names follow `COPILOTD_` + upper(flag,
`-`→`_`). `config` defines `ServeConfig` and `LoginConfig` with all validation
(pure, table-testable); the `ff.Command` tree binds each subcommand's flags into
the appropriate typed config.

**Secret redaction:** `apikey` and `github-oauth-token` are secrets and are
**omitted from `Config.LogValue`** (redaction by construction — Phase 0's
pattern). The Phase 0 `LogValue` test is extended to assert neither ever appears
in log output.

### 9.1 Global (root — inherited by `serve` and `login`)

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| Log level | `log-level` | `--log-level` | `COPILOTD_LOG_LEVEL` | `info` | `debug`\|`info`\|`warn`\|`error` |
| Log format | `log-format` | `--log-format` | `COPILOTD_LOG_FORMAT` | `text` | `text`\|`json` |
| Log file | `log-file` | `--log-file` | `COPILOTD_LOG_FILE` | *(empty)* | Empty = stderr |
| Config file | — | `--config` | `COPILOTD_CONFIG` | *(empty)* | Path to this TOML file |
| OAuth token file | `github-oauth-token-file` | `--github-oauth-token-file` | `COPILOTD_GITHUB_OAUTH_TOKEN_FILE` | `<UserConfigDir>/copilotd/github-oauth-token` | Owner-only `0600`, atomic; `login` writes, `serve` reads; holds the raw token |

### 9.2 `copilotd serve`

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| Bind address | `addr` | `--addr` | `COPILOTD_ADDR` | `127.0.0.1:8080` | Loopback default |
| Shutdown timeout | `shutdown-timeout` | `--shutdown-timeout` | `COPILOTD_SHUTDOWN_TIMEOUT` | `10s` | Graceful-shutdown grace |
| **API key** | `apikey` | `--apikey` | `COPILOTD_APIKEY` | *(required)* | **Secret**, redacted; fail-fast if unset; client presents via `Authorization: Bearer` or `x-api-key`. Flag form is `ps`-visible (documented caveat) |
| **Injected OAuth token** | `github-oauth-token` | `--github-oauth-token` | `COPILOTD_GITHUB_OAUTH_TOKEN` | *(empty)* | **Secret**; inline value; precedence over the token file |
| Upstream base override | `upstream-base` | `--upstream-base` | `COPILOTD_UPSTREAM_BASE` | *(empty)* | Empty ⇒ use exchange response's `endpoints.api` |
| Outbound timeout | `outbound-timeout` | `--outbound-timeout` | `COPILOTD_OUTBOUND_TIMEOUT` | `600s` | Per-request ctx deadline (not a blunt client timeout) |
| Max request bytes | `max-request-bytes` | `--max-request-bytes` | `COPILOTD_MAX_REQUEST_BYTES` | `33554432` (32 MiB) | Safety rail; over-limit ⇒ `413` |
| Integration id | `copilot-integration-id` | `--copilot-integration-id` | `COPILOTD_COPILOT_INTEGRATION_ID` | `vscode-chat` | Impersonation; essential value |
| Editor version | `editor-version` | `--editor-version` | `COPILOTD_EDITOR_VERSION` | `vscode/1.104.1` | Impersonation; presence-checked |
| Editor plugin version | `editor-plugin-version` | `--editor-plugin-version` | `COPILOTD_EDITOR_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Impersonation |
| User agent | `copilot-user-agent` | `--copilot-user-agent` | `COPILOTD_COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Impersonation; UA-checked upstream |
| GitHub API version | `github-api-version` | `--github-api-version` | `COPILOTD_GITHUB_API_VERSION` | `2025-04-01` | Version-sensitive knob |

### 9.3 `copilotd login`

| Setting | TOML | Flag | Env | Default | Remarks |
| --- | --- | --- | --- | --- | --- |
| GitHub client id | `github-client-id` | `--github-client-id` | `COPILOTD_GITHUB_CLIENT_ID` | `Iv1.b507a08c87ecfe98` | Device-flow OAuth app; override for GHES |
| GitHub scope | `github-scope` | `--github-scope` | `COPILOTD_GITHUB_SCOPE` | `read:user` | Device-flow scope |

*(plus the inherited `--github-oauth-token-file` write target and the global
logging/config flags.)*

### 9.4 Validation

`serve`: `addr` a valid `host:port` (Phase 0); `apikey` non-empty (fail-fast);
`outbound-timeout` > 0; `max-request-bytes` > 0; log level/format in-set (Phase
0). `login`: `github-client-id` and `github-scope` non-empty. Invalid config
yields a clear error and a non-zero exit **before** binding the listener or
starting a device flow.

## 10. Observability

Phase 0's structured logging + request-id + route-template access logging carry
forward and now cover the provider routes. Additions:

- **Logs:** upstream call at `debug` (method, upstream path, status, duration —
  never bodies or secrets); auth failures at `info`/`warn` without the presented
  key; identity events per §6.8.
- **Metric seams** (named, not necessarily built — Phase 0 posture): upstream
  outcome (status class) by route template; token-refresh events + outcomes;
  degraded↔ready transitions; request counts/latency by route (already seeded).
- **Redaction discipline** (Phase 0) inherited wholesale: no full header/body
  dumps; secrets never logged; config logged through `LogValue`.

## 11. Testing strategy

TDD throughout (red → green → refactor), `-race`, stdlib `testing` +
`net/http/httptest`. GitHub and Copilot are stubbed with `httptest`; refresh uses
an injected clock. Every unit is testable because dependencies are injected.

- **`identity`** — exchange response parsing (`token`/`expires_at`/`refresh_in`/
  `endpoints.api`); auth-vs-transient failure classification; the
  **single-in-flight invariant** (concurrent `Current` calls trigger exactly one
  exchange, asserted by counting upstream hits); serve-stale-until-hard-expiry and
  reschedule-from-`refresh_in`; token-file round-trip, `0600` enforcement,
  refuse-on-too-open (Unix), atomic write; device-flow polling
  (`authorization_pending`/`slow_down`/`expired_token`/`access_denied`); source
  precedence (inline > file) and fail-fast on no source; impersonation headers
  present on the exchange request.
- **`forward`** — path map incl. the `/v1` asymmetry; the `stream` peek (reject
  vs forward vs non-JSON); header denylist (API key stripped, impersonation set,
  hop-by-hop stripped, other client headers passed through); `413` on over-limit;
  **verbatim upstream error passthrough** (upstream `400`/`429`/`500` body copied
  unchanged); proxy-origin `502`/`504`; client-cancel propagation. Driven with a
  stubbed upstream + a fake `Credential`.
- **`server`** — auth (valid Bearer and valid `x-api-key` pass; missing/wrong ⇒
  per-surface `401`; key never logged); readiness gate + **auth-before-readiness**
  ordering (unauthenticated caller gets `401` even when not ready); `/readyz`
  reflects `identity.Ready()`; `/healthz` unchanged.
- **`apierror`** — each `(provider, kind)` emits the correct status + JSON shape.
- **`config`** — new-field precedence + validation; `LogValue` **omits** `apikey`
  and `github-oauth-token` (extends the Phase 0 redaction test).
- **CLI (`run`)** — dispatch: bare `copilotd` ⇒ help; `help serve`/`help login`;
  `version`/`--version`; unknown subcommand ⇒ error + non-zero exit.
- **End-to-end smoke** (Phase 1's outcome as an automated test) — server + API
  key + a **stubbed identity** (static `Credential` → an `httptest` "Copilot") →
  `POST /anthropic/v1/messages` with a valid key → `200` + verbatim body
  round-trip; and the same with a wrong key → `401`; and while not-ready → `503`.

## 12. Deferrals mapped to phases

| Deferred item | Lands in |
| --- | --- |
| SSE streaming engine (parse → re-emit, keepalive, terminal enforcement, client-disconnect) | Phase 2 |
| Middleware / onion framework (request/stream transform) | Phase 3 |
| Self-heal 401 retry; unsupported-param/beta stripping; model-name mapping; stable Responses item-ids | Phase 4 |
| Responses management sub-paths (`GET/DELETE /responses/{id}`, cancel, input_items) | Phase 4 (or when a client needs them) |
| `/models` (provider-agnostic, then provider-shaped) | Phase 4 / Phase 5 |
| Cross-compilation to the four targets, service install | Phase 6 |
| Metrics build-out (Prometheus/OTel) beyond the named seams | Later phase (Phase 0 §2.1 deviation) |
| Auto-discovery of VS Code's `apps.json` as a token source | If/when a real need appears |

## 13. Notes & open items

- **New dependency (Phase 1):** `golang.org/x/sync/singleflight`. Everything else
  is stdlib beyond the Phase 0 set (`ff/v4`, `google/uuid`, TOML parser).
- **Facts to confirm at implementation (not design forks):**
  1. Exact `ff/v4` `ff.Command`/`ffhelp` API and the ~15-line `help <sub>` verb.
  2. The current upstream paths remain `/v1/messages`, `/v1/messages/count_tokens`
     (Anthropic) and `/responses` (OpenAI Responses) on the resolved
     `endpoints.api` host; confirm against a live account (individual first;
     business/enterprise hosts may differ).
  3. The minimal-yet-sufficient impersonation header set and current acceptable
     `X-GitHub-Api-Version` value (version-sensitive; treated as knobs).
  4. Device-flow scope (`read:user` vs alternatives) against a live login;
     entitlement, not scope, gates Copilot access.
  5. Windows owner-only file semantics for the OAuth-token file (best-effort;
     documented caveat).
- **Drift sensitivity (ROADMAP §8):** the impersonation headers, the token
  exchange, and the upstream paths are the drift-exposed surfaces. Keeping them
  configurable (headers, base override) and the forwarder dumb limits blast
  radius; the identity layer is where upstream change will most likely bite.
```
