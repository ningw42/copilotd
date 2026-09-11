// Package modelmatch resolves a Reported model to one original-provider
// Pricing-model identity. It is a pure, immutable index: it has no pricing,
// Catalog, Surface, persistence, or I/O dependency, and a constructed Matcher is
// safe for concurrent Resolve calls.
//
// New accepts literal provider/model candidate pairs. It indexes only the
// initial original-provider namespaces openai, anthropic, google, and xai, but it
// includes every supplied identity in those namespaces regardless of whether a
// caller has rates for it. Construction is contextual so a report can include
// indexing in its existing work and cancellation budget. Candidate storage is
// detached, duplicate identical pairs collapse to one identity, and no candidate
// ordering is used as a tie-breaker.
//
// Resolve applies these stages in order:
//
//  1. literal model ID;
//  2. ASCII-case normalization plus numeric dotted-to-hyphenated version
//     normalization only for IDs beginning claude-;
//  3. reviewed explicit aliases;
//  4. removal of at most one terminal -fast (ASCII-case-insensitive) and at
//     most one valid terminal calendar date, in either order; and
//  5. an undated Reported stem to a unique dated candidate.
//
// The explicit-alias stage is intentionally empty. Neither the observed
// gpt-5.6-sol-fast/gpt-5.6-sol relationship nor the observed dotted/hyphenated,
// dated Claude relationship needs an alias: the constrained suffix,
// normalization, and dated rules explain them. Their evidence is retained in
// docs/research/2026-09-05-native-usage-shapes.md and
// docs/research/2026-07-26-anthropic-model-id-aliases.md. Any future alias must
// add linked source or observational evidence and a literal matcher-interface
// acceptance case in the same change; aliases are not operator-configurable.
//
// A recognized provider qualifier restricts its namespace. Other prefixes stay
// in the model bytes. Suffix lookup ranks fewer removals first, then a retained
// literal spelling before its normalized spelling. Because the two removable
// suffix forms are disjoint and terminal, each removal count has at most one
// retained stem; it is therefore also the longest normalized retained stem at
// that rank. The final dated stage applies the same literal-before-normalized and
// fewer-transformations ordering. A supplied date disables that final stage, so
// it cannot jump to another date; -fast is never stripped from candidates.
//
// Every lookup bucket stores only one identity plus an ambiguity bit. Multiple
// distinct identities at a reached stage and rank therefore return Ambiguous
// immediately, without exposing a candidate list or falling through to a weaker
// match. Matched resolutions return the candidate's original provider and model
// spelling. Resolve never rewrites or returns replacement bytes for the caller's
// Reported model.
package modelmatch
