// Package shim defines copilotd's composable parity-extension contract.
//
// Shims may alter, drop, hold, or coalesce information derived from a request or
// upstream response, but must not fabricate information without an upstream
// basis. A hook must not access Copilot or drive an upstream retry. Both rules
// are policy invariants enforced by review rather than by the type system.
//
// SSE stream hooks run synchronously in the SSE pump and therefore must be
// prompt and non-blocking: CPU-bound transformation only, with no I/O or
// waiting. A shim that holds content must also hold its terminal and release
// both together in order at Finalize. Terminal position is an author
// obligation; the framework prevents a second synthesized terminal but does
// not police content emitted after a terminal.
//
// WebSocket message hooks are opt-in through ClientMessageTransformer and
// ServerMessageTransformer. They run synchronously in their respective pumps
// and carry the same prompt, non-blocking obligation. A transform maps one
// message to at most one message, or drops it; the framework never holds an
// emission for later release.
//
// A shim instance is per-request on the HTTP path but per-session on the
// long-lived, multi-turn WebSocket path. A shim that spans both transports must
// not assume a request-scoped lifetime and must bound any per-turn accumulation.
// Registrations may declaratively restrict construction to selected
// Surface/Route pairs with Scope; a nil Scope includes every pair.
//
// The Responses item-id stabilizer is registered as
// responses-item-id-stabilizer and is governed by the
// --shim-responses-item-id-stabilizer-enabled flag, represented in
// configuration as ShimResponsesItemIDStabilizerEnabled. It is opt-in, off by
// default, and scoped to OpenAI /responses. On both SSE and server-to-client
// WebSocket paths, it pins the first genuine upstream item id per output index
// and rewrites later item-id surfaces to that value. This is an Alteration, not
// Fabrication: it never mints an id.
package shim

import (
	"context"
	"net/http"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/sse"
)

// Request carries the mutable inbound payload. Query is deliberately private:
// hooks may inspect it with Query but cannot replace the core-owned query.
type Request struct {
	Header http.Header
	Body   []byte
	query  string
}

// Query returns the inbound query exactly as received.
func (r *Request) Query() string { return r.query }

// Prelude carries the mutable response envelope before it is committed.
type Prelude struct {
	Status int
	Header http.Header
}

// Body carries a mutable buffered response body.
type Body struct {
	Bytes []byte
}

// MessageKind is the transport-neutral WebSocket message kind.
type MessageKind int

const (
	MessageText MessageKind = iota
	MessageBinary
)

// Message carries one mutable, reassembled WebSocket message. A transformer
// mutates Kind and/or Data in place.
type Message struct {
	Kind MessageKind
	Data []byte
}

// RequestTransformer transforms one inbound request before forwarding.
// Implementations should include a compile-time guard such as:
//
//	var _ shim.RequestTransformer = (*myShim)(nil)
type RequestTransformer interface {
	TransformRequest(context.Context, *Request) error
}

// PreludeTransformer transforms one response envelope before commit.
type PreludeTransformer interface {
	TransformPrelude(context.Context, *Prelude) error
}

// BufferedTransformer transforms one complete buffered response body.
type BufferedTransformer interface {
	TransformBuffered(context.Context, *Body) error
}

// ClientMessageTransformer transforms one client-to-upstream WebSocket
// message. It runs synchronously and must be prompt and non-blocking. Return
// emit=false to drop the message. A transformer that cannot interpret a message
// declines by leaving it unchanged and returning emit=true.
type ClientMessageTransformer interface {
	TransformClientMessage(context.Context, *Message) (emit bool)
}

// ServerMessageTransformer transforms one upstream-to-client WebSocket
// message under the same rules as ClientMessageTransformer.
type ServerMessageTransformer interface {
	TransformServerMessage(context.Context, *Message) (emit bool)
}

// MessageTransform folds the enabled directional hooks for one direction into
// one call. Nil means no shim participates in that direction.
type MessageTransform func(context.Context, *Message) (emit bool)

// EventTransformer transforms one upstream SSE frame into zero or more frames.
// It runs synchronously in the SSE pump and must be prompt and non-blocking
// (CPU-bound transformation only; no I/O or waiting). A transformer that cannot
// interpret a frame declines by returning that frame unchanged. A transformer
// that holds content must also hold its terminal so Finalize can release both in
// order; terminal position is the shim author's obligation, not framework
// policing.
type EventTransformer interface {
	TransformEvent(context.Context, sse.Frame) []sse.Frame
}

// StreamFinalizer releases frames held by a stream shim at stream end. It runs
// synchronously in the SSE pump and must be prompt and non-blocking. Held
// content and its terminal must be released together in valid wire order;
// terminal position is the shim author's obligation.
type StreamFinalizer interface {
	Finalize(context.Context) []sse.Frame
}

// Registration describes one ordered shim and its instance factory. HTTP
// forwarding creates one instance per request; WebSocket forwarding creates
// one instance per session.
type Registration struct {
	Name    string
	Enabled bool
	// Scope reports whether this shim participates in a Surface/Route pair.
	// Nil includes every pair.
	Scope func(endpoint.Surface, endpoint.Route) bool
	New   func(context.Context, endpoint.Surface, endpoint.Route) any
}

// Registry is an ordered set of shim registrations. Registration order is
// onion order.
type Registry []Registration

// namedInstance keeps the stable registration identity beside the per-request
// or per-session implementation. Post-commit adapters preserve this association
// through each individual hook call boundary.
type namedInstance struct {
	name     string
	instance any
}

// Chain holds the enabled shim instances for one HTTP request or WebSocket
// session.
type Chain struct {
	instances []namedInstance
	overruns  *requestsummary.HookOverrunRecorder
}

// NewChain constructs each enabled shim once in registration order for the
// caller's request or session lifetime.
func (r Registry) NewChain(ctx context.Context, surface endpoint.Surface, route endpoint.Route) *Chain {
	chain := &Chain{overruns: requestsummary.NewHookOverrunRecorder(ctx)}
	for _, registration := range r {
		if registration.Enabled && (registration.Scope == nil || registration.Scope(surface, route)) {
			chain.instances = append(chain.instances, namedInstance{
				name:     registration.Name,
				instance: registration.New(ctx, surface, route),
			})
		}
	}
	return chain
}

// RunRequest applies the request half of the onion.
func (c *Chain) RunRequest(ctx context.Context, query string, header http.Header, body []byte) (http.Header, []byte, error) {
	request := &Request{Header: header, Body: body, query: query}
	for _, registered := range c.instances {
		if transformer, ok := registered.instance.(RequestTransformer); ok {
			if err := transformer.TransformRequest(ctx, request); err != nil {
				return request.Header, request.Body, err
			}
		}
	}
	return request.Header, request.Body, nil
}

// RunPrelude applies the response-envelope half of the onion.
func (c *Chain) RunPrelude(ctx context.Context, status int, header http.Header) (int, http.Header, error) {
	prelude := &Prelude{Status: status, Header: header}
	for i := len(c.instances) - 1; i >= 0; i-- {
		if transformer, ok := c.instances[i].instance.(PreludeTransformer); ok {
			if err := transformer.TransformPrelude(ctx, prelude); err != nil {
				return prelude.Status, prelude.Header, err
			}
		}
	}
	return prelude.Status, prelude.Header, nil
}

// RunBuffered applies the complete buffered response half of the onion.
func (c *Chain) RunBuffered(ctx context.Context, body []byte) ([]byte, error) {
	buffered := &Body{Bytes: body}
	for i := len(c.instances) - 1; i >= 0; i-- {
		if transformer, ok := c.instances[i].instance.(BufferedTransformer); ok {
			if err := transformer.TransformBuffered(ctx, buffered); err != nil {
				return buffered.Bytes, err
			}
		}
	}
	return buffered.Bytes, nil
}

// HasBufferedTransformer reports whether hook presence opts this response into
// whole-body buffering.
func (c *Chain) HasBufferedTransformer() bool {
	for _, registered := range c.instances {
		if _, ok := registered.instance.(BufferedTransformer); ok {
			return true
		}
	}
	return false
}

// StreamAdapter composes the enabled stream hooks into the SSE engine's single
// transformer seam. Nil selects the engine's byte-verbatim fast path.
func (c *Chain) StreamAdapter() sse.FrameTransformer {
	var streamInstances []namedInstance
	for _, registered := range c.instances {
		_, transforms := registered.instance.(EventTransformer)
		_, finalizes := registered.instance.(StreamFinalizer)
		if transforms || finalizes {
			streamInstances = append(streamInstances, registered)
		}
	}
	if len(streamInstances) == 0 {
		return nil
	}
	return &sseAdapter{instances: streamInstances}
}

type namedClientMessageTransformer struct {
	name        string
	transformer ClientMessageTransformer
}

type namedServerMessageTransformer struct {
	name        string
	transformer ServerMessageTransformer
}

func (c *Chain) clientMessageParticipants() []namedClientMessageTransformer {
	var participants []namedClientMessageTransformer
	for _, registered := range c.instances {
		if transformer, ok := registered.instance.(ClientMessageTransformer); ok {
			participants = append(participants, namedClientMessageTransformer{name: registered.name, transformer: transformer})
		}
	}
	return participants
}

func (c *Chain) serverMessageParticipants() []namedServerMessageTransformer {
	var participants []namedServerMessageTransformer
	for _, registered := range c.instances {
		if transformer, ok := registered.instance.(ServerMessageTransformer); ok {
			participants = append(participants, namedServerMessageTransformer{name: registered.name, transformer: transformer})
		}
	}
	return participants
}

// WSClientAdapter composes enabled client-to-upstream WebSocket message hooks.
func (c *Chain) WSClientAdapter() MessageTransform {
	participants := c.clientMessageParticipants()
	if len(participants) == 0 {
		return nil
	}
	return func(ctx context.Context, message *Message) bool {
		for _, participant := range participants {
			if !participant.transformer.TransformClientMessage(ctx, message) {
				return false
			}
		}
		return true
	}
}

// WSServerAdapter composes enabled upstream-to-client WebSocket message hooks.
func (c *Chain) WSServerAdapter() MessageTransform {
	participants := c.serverMessageParticipants()
	if len(participants) == 0 {
		return nil
	}
	return func(ctx context.Context, message *Message) bool {
		for i := len(participants) - 1; i >= 0; i-- {
			if !participants[i].transformer.TransformServerMessage(ctx, message) {
				return false
			}
		}
		return true
	}
}

type sseAdapter struct {
	instances []namedInstance
}

var _ sse.FrameTransformer = (*sseAdapter)(nil)

// Transform folds one frame from the innermost shim to the outermost. Each
// output from one hook is independently fed to the next outer hook.
func (a *sseAdapter) Transform(ctx context.Context, frame sse.Frame) []sse.Frame {
	frames := []sse.Frame{frame}
	for i := len(a.instances) - 1; i >= 0; i-- {
		transformer, ok := a.instances[i].instance.(EventTransformer)
		if !ok {
			continue
		}
		frames = transformFrames(ctx, transformer, frames)
	}
	return frames
}

// Finalize sweeps inner to outer. Pending output from inner finalizers passes
// through each outer event hook before that outer shim appends its own finalizer
// output, so every emitted frame completes the whole onion.
func (a *sseAdapter) Finalize(ctx context.Context) []sse.Frame {
	var pending []sse.Frame
	for i := len(a.instances) - 1; i >= 0; i-- {
		if transformer, ok := a.instances[i].instance.(EventTransformer); ok {
			pending = transformFrames(ctx, transformer, pending)
		}
		if finalizer, ok := a.instances[i].instance.(StreamFinalizer); ok {
			pending = append(pending, finalizer.Finalize(ctx)...)
		}
	}
	return pending
}

func transformFrames(ctx context.Context, transformer EventTransformer, frames []sse.Frame) []sse.Frame {
	var transformed []sse.Frame
	for _, frame := range frames {
		transformed = append(transformed, transformer.TransformEvent(ctx, frame)...)
	}
	return transformed
}

// NopShim is the canonical no-op. It intentionally implements no hook.
type NopShim struct{}

// CanonicalRegistry returns a fresh copy of the canonical registration order.
func CanonicalRegistry() Registry {
	return Registry{
		{
			Name:    "nop",
			Enabled: false,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return NopShim{}
			},
		},
		{
			Name:    "responses-item-id-stabilizer",
			Enabled: false,
			Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
				return surface == endpoint.OpenAI && route == endpoint.RouteOpenAIResponses
			},
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return newResponsesItemIDStabilizer()
			},
		},
	}
}
