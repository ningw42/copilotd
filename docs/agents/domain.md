# Domain Docs

This is a single-context repository.

## Before exploring

- Read the root `CONTEXT.md` for the project's canonical language.
- Read relevant decisions under `docs/adr/` before changing an affected area.
- If either source is absent, proceed silently; domain-modeling workflows create
  them only when a term or durable decision needs to be recorded.

## Use the glossary's vocabulary

Use terms as defined in `CONTEXT.md` in issue titles, design documents,
hypotheses, tests, and implementation notes. Avoid synonyms that the glossary
explicitly rejects.

If a needed concept is absent, reconsider whether it is project-specific. Use
the domain-modeling workflow to add it only when a canonical project term is
actually resolved.

## Respect architectural decisions

Surface any conflict with a relevant ADR explicitly rather than silently
overriding it. New ADRs are reserved for decisions that are hard to reverse,
surprising without context, and the result of a real trade-off.
