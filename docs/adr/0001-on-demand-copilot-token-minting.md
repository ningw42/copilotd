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

## Consequences

- The mint is triggered by traffic, so the exchange pattern correlates with real
  use rather than an idle heartbeat.
- The request that crosses a token-expiry boundary pays one extra GitHub
  round-trip (mitigated by a small safety margin so we re-mint just *before*
  expiry, never mid-call). On-demand mints are single-attempt; the startup mint
  retries transient failures up to `--startup-mint-retries`.
- `/readyz` cannot mean "holds an unexpired token" (an idle daemon would flap
  not-ready every ~25 min). Readiness instead tracks the **last mint outcome**:
  ready after the first success, not-ready only when a mint fails.
- A single `singleflight` key across the startup and on-demand mints preserves the
  invariant of at most one exchange in flight globally.

See `docs/design/2026-07-14-phase-1-core-forward-path-design.md` §§2.1, 6.3–6.4,
8.2 for the full treatment.
