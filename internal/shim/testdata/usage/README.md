# Native usage fixtures

These fixtures support the evidence in
[`docs/research/2026-09-05-native-usage-shapes.md`](../../../../docs/research/2026-09-05-native-usage-shapes.md).
They contain no request text, generated text, credentials, headers, or raw
response bodies.

## Recorded Copilot projections

The three `*.recorded.*` fixtures are sanitized projections of successful
GitHub Copilot Responses completions observed through an existing local
copilotd daemon on 2026-09-05. `recorded-capture-metadata.json` records the UTC
capture time, transport, requested and returned model, raw byte count, and the
redaction procedure.

- Response IDs are replaced by deterministic, non-empty placeholders.
- Model, completed status, event type, sequence number, and every field/value
  below each `usage` object are preserved exactly.
- All output items and other response fields are omitted. This removes all
  generated content and every output-item ID.
- The SSE fixture retains the captured `event:`/`data:` framing and blank frame
  separator for the retained `response.completed` event. The WebSocket fixture
  retains one server Message as one JSONL record.

The running daemon had `responses-item-id-stabilizer` enabled. That transform
can rewrite only output-item IDs; it does not rewrite the response object's
`id`, `model`, `status`, or `usage` fields. Those are exactly the retained
fields. The buffered path does not invoke that transform. See
[`responses_item_id.go`](../../responses_item_id.go).

No recorded Anthropic fixture is present. The same capture session found zero
raw Copilot catalog entries advertising `/v1/messages`, the provider-shaped
Anthropic catalog was empty, and one bounded request using the previously known
`claude-opus-4.8` ID returned HTTP 400. A streaming request was not made after
those two availability checks. That dated observation remains historical; it no
longer blocks the schema decision. The user approved the exact official
[Messages Create](https://platform.claude.com/docs/en/api/messages/create)
contract plus generated fixtures, with live Copilot Anthropic compatibility
explicitly unverified. Treating any synthetic fixture below as a recorded
Copilot capture would be false provenance.

## Generated synthetic contract variants

Every `*.synthetic.*` file is a hand-authored generated fixture. These files are
parser-contract or worked-example inputs, not captured behavior and not proof
that Copilot emits a field.

- `anthropic-messages-buffered.synthetic.json` is the accepted buffered
  eligibility shape and includes all current stable Anthropic scalar token
  fields, including `output_tokens_details.thinking_tokens`.
- `anthropic-messages-sse-cumulative.synthetic.sse` covers cumulative
  last-value updates, explicit-null preservation, a genuine reported zero,
  and `message_stop` completion.
- `anthropic-messages-sse-late-usage.synthetic.sse` covers a start with no
  `usage` object that is later completed by numeric zero reports.
- `openai-responses-inclusive.synthetic.json` is the worked inclusive-input
  example; cached and cache-write tokens are subsets of `input_tokens`.
- `openai-responses-null-details.synthetic.json` covers the design's nullable
  optional projection. Current official OpenAI schema and all three recorded
  captures report numeric detail values, so null is not claimed as provider
  behavior.
- `invalid-count-cases.synthetic.json` labels wrong-type, negative, and int64
  overflow candidates that the planned parsers must reject without changing
  their wire payload.

## Primary schema pins

Schema interpretation is pinned in the research note to:

- OpenAI's official Go SDK, package version 3.56.0 at commit
  `65785ca59ffea26f592920b5aae7bbe302cf30cc` (2026-09-05).
- Anthropic's official Go SDK v1.71.0 at commit
  `de6914c544629b14a67c0695ce147edae6a291e0` (2026-09-04).
- The official OpenAI prompt-caching guide and Anthropic's exact
  [Messages Create](https://platform.claude.com/docs/en/api/messages/create),
  streaming, and prompt-caching documentation, accessed 2026-09-05.

The accepted projection and evidence substitution are recorded in
[ADR-0018](../../../../docs/adr/0018-store-per-surface-native-usage.md).
