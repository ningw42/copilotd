# Parse the SSE stream into frames, but stay payload-opaque

**Status:** accepted

copilotd must stream GitHub Copilot's Server-Sent Events to clients while staying
a faithful passthrough. The streaming engine splits the upstream byte stream into
SSE frames and classifies each frame by its `event:` line — falling back to a
minimal decode of only the `data` JSON `type` field when that line is absent — but
it never deserializes the event payload. It re-emits the original frame bytes
verbatim.

## Considered options

- **Byte-level flushing passthrough** (`io.Copy` + flush): rejected — it cannot
  inject keepalive at event boundaries, cannot detect terminal events to enforce a
  clean stream end, and would force Phase 3's middleware to retrofit framing.
- **Full parse into typed events**: rejected — re-serializing through typed structs
  silently drops unknown fields and breaks on new event types, violating
  raw-passthrough principle #1. Both APIs warn new event types will appear.
- **Frame-aware, payload-opaque** (chosen): parse frames and identify by the event
  line; forward the payload bytes verbatim. Pays for identification and nothing
  more, and yields terminal detection, keepalive injection, and the Phase 3
  per-event seam for free.

## Consequences

- Identification is `event:`-line-first. This is grounded: Anthropic Messages
  normatively guarantees the event name (and that it matches the data `type`);
  OpenAI Responses only shows the event line in examples; the WHATWG SSE standard
  makes it optional. So a minimal `data.type` fallback runs when the line is
  absent, and a **fallback-fired counter** turns a Copilot regression that drops the
  event line into an observable signal rather than a silent misclassification.
- The engine stays payload-opaque: unknown fields and unknown event types pass
  through untouched, and a future middleware pays the full
  unmarshal/mutate/re-marshal cost only for the specific events it rewrites.

See `docs/design/2026-07-15-phase-2-sse-streaming-engine-design.md` §§1, 3, 5.
