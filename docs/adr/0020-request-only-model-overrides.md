# Admit explicit request-only model overrides

**Status:** accepted and implemented in
[#297](https://github.com/ningw42/copilotd/issues/297) as the `model-override`
Shim; operator documentation is in
[CONFIGURATION.md](../../CONFIGURATION.md#--model-overrides).

copilotd admits an opt-in Model override Shim that substitutes an explicitly
configured model ID in outbound inference requests across its supported
inference Surfaces and transports. An operator may deliberately choose a
different upstream model without demonstrating a client compatibility failure.
The motivating deployment selects `gpt-5.6-sol` consistently across gateways
while routing Copilot requests to `gpt-5.6-sol-fast`.

The override is authoritative even when its source ID is itself a live upstream
model. Shadowing that original selection is an accepted operator-controlled
limitation, not a catalog collision to repair. There is no target-availability
lookup, upstream call from the Shim, fallback, retry, or cross-Surface protocol
translation. Copilot remains authoritative for target acceptance.

This is a request-only Alteration. It leaves Catalog membership, metadata,
limits, and reviewer fields unchanged; configuring a rule neither advertises
its source ID nor hides its target. It does not restore the source ID in
responses or alter Reported model, Reported service tier, native usage, or
Estimated-cost policy. Existing HTTP Requested-model observation remains
post-request-Shim; WebSocket Requested-model attribution remains unavailable.

## Matching and transport contract

One shared map uses a single exact, case-sensitive lookup of the decoded
client-supplied model ID. For `A=B,B=C`, an incoming `A` becomes `B`, not `C`.
Reciprocal rules do not recurse; self-mappings and unmapped IDs are byte-verbatim
no-ops. Rules are not wildcards, patterns, or per-Surface maps, and do not infer
model-family equivalences. An incompatible target is the operator's
responsibility; changing its ID never translates the request protocol.

The Shim replaces only a unique top-level string `model` value in these inputs:

- Anthropic Messages and Count Tokens HTTP request bodies;
- OpenAI Responses HTTP request bodies, regardless of response streaming;
- OpenAI Responses WebSocket Text Messages with an unambiguous top-level
  `type: "response.create"`.

All payload bytes outside the replaced value remain unchanged. In particular,
nested model fields, prompts, unknown fields, numeric spellings, and whitespace
are not re-encoded. Missing, non-string, duplicate, malformed, or lossily
decodable model evidence declines by passthrough. Malformed JSON, ambiguous
WebSocket type fields, other event types, and Binary Messages also remain
unchanged. No model is inserted when absent; there is no per-turn inheritance
or session state. The same uncertainty policy applies to HTTP and WebSocket:
no new local rejection, drop, retry, or session termination. This convenience
transform is deliberately not a policy-enforcement boundary.

Count Tokens does carry a top-level `model`, as shown by the official Anthropic
SDK's [MessageCountTokensParams and countTokens implementation](https://github.com/anthropics/anthropic-sdk-typescript/blob/main/src/resources/messages/messages.ts).
This corrects the contrary historical seed-shim note. Existing
[WebSocket protocol research](../research/2026-07-19-responses-websocket-mode.md)
records `response.create` as text JSON with the same top-level request fields
as HTTP Responses. These sources establish payload shape, not live target
availability or cross-Surface model compatibility.

## Configuration and integration

The stable Shim registration is `model-override`, controlled by
`shim-model-override-enabled` (default `false`) and a separate `model-overrides`
map (default empty):

```toml
shim-model-override-enabled = true
model-overrides = "gpt-5.6-sol=gpt-5.6-sol-fast"
```

Use the existing string-map configuration conventions: comma-separated
`source=target` entries, trimmed configuration whitespace, nonempty source and
target, and duplicate source IDs rejected at startup. The implementation also
rejects IDs that are not valid UTF-8, so a target is sent exactly as configured
rather than with replacement characters. Empty entries follow the existing
parser convention. Self-mappings are accepted no-ops. Configuration
follows flags > environment > TOML > default, replacing rather than merging
maps across layers. Invalid configuration fails before binding, even when the
Shim is disabled. A disabled Shim or an empty map changes no traffic.

The canonical registry orders this request transform before the Usage meter.
The existing request and client-Message hooks own the transform; the Forwarder
stays payload-opaque outside its existing seams. There are no response hooks
for this Shim and no changes to usage persistence or valuation.

The request-only Alteration must be recorded in the divergence ledger when
implemented. No output claims that the configured target was accepted or that
a particular service tier was delivered.

## Trade-off and prior rejection

Client-only configuration would preserve entirely verbatim forwarding, but
would repeat the deployment choice in every agent selection and task route.
Coupling the override to catalog resolution would add availability, limits, and
metadata policy unrelated to the requested outbound substitution. We instead
accept an explicit, off-by-default request transform with unchanged catalogs
and responses; operators own any resulting mismatch between advertised
metadata and their chosen target.

This decision narrows the rejection in #188: generic request-and-response model
name normalization remains out of scope, but request-only operator overrides
no longer require a demonstrated compatibility failure. Existing Codex catalog
Metadata-source aliases keep their current meaning and do not themselves
rewrite inference requests.
