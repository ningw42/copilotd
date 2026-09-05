# OpenAI Responses management operations

copilotd does not serve the response-ID management lifecycle from the OpenAI
Responses API: retrieve, delete, cancel, and input-item listing. It also rejects
HTTP response creation with `background:true`.

The supported Responses boundary is:

- `POST /openai/v1/responses` for foreground JSON or SSE creation;
- WebSocket `GET /openai/v1/responses` for the Responses WebSocket transport;
- no `/openai/v1/responses/{response_id}` management subpaths; and
- no HTTP background-response lifecycle.

The current implementation enforces this boundary in two ways. The response-ID
management paths are not mounted, so the router does not accept them.
`background:true` is rejected explicitly with an OpenAI-shaped `400`, preventing
copilotd from creating an asynchronous response that its clients cannot later
manage.

## Why this is out of scope

copilotd forwards each Surface only to matching GitHub Copilot Routes and does
not promise the full API surface of the corresponding direct provider. Adding a
Responses lifecycle therefore requires evidence that GitHub Copilot supports
the exact upstream operations and that a real client needs them; direct OpenAI
API support alone is insufficient.

The available public GitHub Copilot documentation and first-party client source
establish foreground Responses creation, streaming, WebSocket transport, and
`previous_response_id` continuation. They do not establish retrieve, delete,
cancel, input-item listing, or background creation against GitHub Copilot. The
current VS Code Copilot path uses foreground streaming with `store:false`, so it
does not provide a management-lifecycle use case.

Speculatively relaxing the background guard could strand upstream responses.
Implementing a lifecycle locally would also violate the raw-forwarding boundary
by introducing a response database or invented semantics. The existing behavior
is therefore the deliberate support boundary, not an incomplete promise of full
OpenAI Responses parity.

## Reconsideration criteria

Reconsider this decision only when both are available:

- an authorized capture proving the supported GitHub Copilot methods, paths,
  fidelity, failures, limits, and cancellation behavior; and
- a concrete client use case that requires the management lifecycle.

Any accepted design must continue to raw-forward a matching upstream Surface;
it must not add local response storage or cross-family translation without a
separate architectural decision.

## Prior requests

- [#190 — Evaluate OpenAI Responses management operations](https://github.com/ningw42/copilotd/issues/190)
