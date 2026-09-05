# Generic unsupported-parameter handling

copilotd does not maintain a general whitelist or sanitizer for request parameters accepted by GitHub Copilot.

## Why this is out of scope

The inference path is raw-passthrough-first: unknown fields are preserved, and Copilot remains authoritative for model- and Route-specific validation. Parameter support changes with the selected model, Surface, Route, client, and upstream rollout, so a static copilotd whitelist would duplicate mutable upstream policy and quickly become stale.

Silently removing a field is also not generally safe. A parameter can express requested semantics—such as a service tier or reasoning behavior—and forwarding a request after discarding that intent can be more misleading than returning Copilot's rejection.

This decision does not rule out a narrowly scoped parity shim. A future request may proceed when it includes a reproducible copilotd compatibility failure, identifies the exact Surface, Route, model scope, and transport behavior, and justifies pass-through, local rejection, or an explicit opt-in alteration. Such work should be tracked as its own evidence-backed issue rather than expanding a generic parameter catalog.

## Prior requests

- [#189 — Evaluate unsupported-parameter handling shims](https://github.com/ningw42/copilotd/issues/189)
