# Copilot model fixture provenance

`copilot-models-2026-07-18.json` is a focused projection of a real GitHub
Copilot `GET /models` response captured through copilotd's raw `/models`
passthrough on 2026-07-18. It preserves the source order and all source fields
used by the Phase 6a filter and representative render mappings, plus real
chat-only, Route-less, and picker-disabled entries.

The full operator capture remains an untracked local artifact at the repository
root. This fixture deliberately omits credentials (the response contained none),
billing/policy prose except one shortened ignored-field sample, and unrelated
Copilot metadata.
