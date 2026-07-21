# Mint the Copilot token on demand, not on a schedule

**Status:** accepted

copilotd needs a short-lived Copilot token (minted from the long-lived GitHub
OAuth token via the `copilot_internal/v2/token` exchange) to talk to Copilot.
That token lives ~25–30 min and must be re-minted. We mint it **on demand** — a
request that finds the in-memory token missing or stale mints a fresh one in its
own path — plus **one asynchronous warm-up mint at startup**. There is no
scheduled/background refresh loop.

## Considered options

- **Proactive scheduled refresh** (what most Copilot proxies do — litellm et al.):
  a timer re-mints every `refresh_in` (~25 min). Rejected: a perfectly regular
  exchange call every 25 min, 24/7, *even while completely idle*, is a textbook
  automation signature — exactly the abuse-detection risk called out in
  ROADMAP §8. It also requires a timer goroutine, jitter, and reschedule logic.
- **On-demand + startup warm-up** (chosen): exchange calls track real inference
  traffic, which looks like an actual editor session, and the timer disappears.
- **Use the last mint outcome as readiness**: rejected. A failed on-demand mint
  would make the readiness middleware reject every later request before it could
  reach the only recovery trigger, `Provider.Current`, turning a request-scoped
  network failure into a permanent admission latch.

## Consequences

- The mint is triggered by traffic, so the exchange pattern correlates with real
  use rather than an idle heartbeat.
- The request that crosses a token-expiry boundary pays one extra GitHub
  round-trip (mitigated by a small safety margin so we re-mint just *before*
  expiry, never mid-call). On-demand mints are single-attempt; the startup mint
  retries transient failures up to `--startup-mint-retries`. Each exchange uses
  a non-replayable bodyless request and does not follow redirects, so a logical
  attempt is exactly one wire request.
- Startup mint is warm-up only. Before it completes or after it fails, an
  authenticated request still reaches `Provider.Current` and can mint on demand.
- A failed on-demand mint returns a Surface-shaped `503` for that request. The
  next request performs a new single-attempt mint; permanent auth-class failures
  also remain request-scoped so a later manual request can recover if the same
  GitHub OAuth token/account becomes usable again, without restarting copilotd.
- `/readyz` reports local prerequisites needed to attempt service, not possession
  of an unexpired Copilot token or the last exchange result. The serve lifecycle
  resolves configuration and the GitHub OAuth token before binding, so remote
  exchange failures never flip a running server to not-ready.
- A single `singleflight` key across the startup and on-demand mints preserves the
  invariant of at most one exchange in flight globally.
- Trigger and outcome remain observable in logs, but tokens, raw exchange bodies,
  and underlying errors are omitted.

See `docs/design/2026-07-14-phase-1-core-forward-path-design.md` §§2.1, 6.3–6.4,
8.2 for the full treatment.
