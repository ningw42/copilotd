# Catalog fixture provenance

`codex-latest/` pins the latest stable Codex catalog audited by the executable
compatibility test. Its stable release, immutable commit/blob, checksum,
schema, and runtime-contract sources are recorded in `codex-latest/SOURCES.md`.

## Raw Copilot `/models` fixture

`copilot-models-2026-07-18.json` is a focused projection of a real GitHub
Copilot `GET /models` response captured through copilotd's raw `/models`
passthrough on 2026-07-18. It preserves the source order and all source fields
used by the Phase 6a filter and representative render mappings, plus real
chat-only, Route-less, and picker-disabled entries.

The full operator capture remains an untracked local artifact at the repository
root. This fixture deliberately omits credentials (the response contained none),
billing/policy prose except one shortened ignored-field sample, and unrelated
Copilot metadata.
