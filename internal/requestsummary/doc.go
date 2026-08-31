// Package requestsummary accumulates bounded completion facts and prepares the
// non-emitting publication plan for copilotd's terminal request summary: the
// whole-request access record produced after a handler returns. That qualified
// term is distinct from an SSE Terminal event, which legitimately ends only an
// SSE stream.
//
// Immutable request scope describes facts that are true while work executes and
// remains carried by logging contexts. Completion facts are learned while the
// handler runs, cross its return boundary through the opaque Summary handle
// installed by Begin, and are consumed only after the handler returns. The
// package does not emit log records or provide a general attribute or
// request-state store.
package requestsummary
