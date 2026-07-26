# Anthropic model-ID aliases through GitHub Copilot

**Observed:** 2026-07-26

## Question

GitHub Copilot's `/models` catalog advertised `claude-opus-4.8`, while Anthropic's
model-ID convention and inference responses use `claude-opus-4-8`. Before changing
the provider-shaped Anthropic catalog, we needed to establish whether GitHub
Copilot accepts both spellings and which spelling it returns.

## Probe path

The requests were sent to the deployed copilotd Anthropic Surface:

```text
POST https://copilotd.ningw.net/anthropic/v1/messages
anthropic-version: 2023-06-01
content-type: application/json
x-api-key: <redacted>
```

copilotd forwards this request body to GitHub Copilot's `/v1/messages` Route
without model-ID translation. The response body is likewise forwarded verbatim.
Each probe used this payload, varying only `model`:

```json
{
  "model": "<probe>",
  "max_tokens": 1,
  "messages": [{"role": "user", "content": "Hi"}]
}
```

## Observations

Both the dotted catalog spelling and its proposed hyphenated alias were probed
for every dotted Claude ID in the captured Copilot catalog:

| Catalog ID | Dotted request | Hyphenated request | Successful response `model` |
| --- | --- | --- | --- |
| `claude-opus-4.6` | accepted | accepted | `claude-opus-4-6` |
| `claude-opus-4.7` | accepted | accepted | `claude-opus-4-7` |
| `claude-opus-4.8` | accepted | accepted | `claude-opus-4-8` |
| `claude-sonnet-4.6` | accepted | accepted | `claude-sonnet-4-6` |
| `claude-sonnet-4.5` | `400: The requested model is not supported.` | `400: The requested model is not supported.` | — |
| `claude-haiku-4.5` | accepted | accepted | `claude-haiku-4-5-20251001` |

Successful one-token responses had `type:"message"`, `stop_reason:"max_tokens"`,
and no error. Haiku's returned ID is dated, but the successful hyphenated request
still establishes `claude-haiku-4-5` as an accepted Copilot alias.

In the same session, `GET /anthropic/v1/models` returned
`claude-opus-4.8`, confirming that the spelling mismatch was isolated to the
provider-shaped catalog.

## Conclusion and limits

The successful probes demonstrate Copilot's dotted-catalog/hyphenated-provider
spelling convention across the current Opus, Sonnet, and Haiku families. The
normalizer therefore applies the convention by rule: replace dots only when the
catalog model has `vendor:"Anthropic"` and an ID beginning `claude-`. Other
vendors and non-Claude Anthropic IDs remain verbatim.

The production rule deliberately does not materialize this observed model set as
an allowlist. Copilot's catalog and supported Claude families evolve independently
of copilotd releases; freezing the IDs observed here would silently stop
normalizing future models. Fixed IDs belong in this evidence record and in tests,
not in feature code.

Sonnet 4.5 also shows that catalog membership and live callability can drift: both
spellings were rejected even though `/models` advertised the Messages Route. The
normalizer governs spelling only; it does not claim that every catalog member is
currently callable.

This is a dated observation of external behavior, not a permanent protocol
guarantee. The normalization remains disabled by default.
