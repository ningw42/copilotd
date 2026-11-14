package shim

import (
	"context"
	"encoding/json/jsontext"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// responseCreateEvent is the only client-to-upstream WebSocket event whose
// model the Model override substitutes.
const responseCreateEvent = "response.create"

// modelOverride is the request-only Model override approved by ADR-0020. One
// exact, case-sensitive lookup of the unique top-level request model selects a
// configured upstream model ID; only that JSON value's bytes are replaced. The
// lookup is never repeated on its own result, so rules cannot chain or loop.
//
// An instance holds no per-request or per-session state: every HTTP request and
// WebSocket Message is interpreted independently. Instances share the validated
// rules map, which nothing modifies while serving. It has no response hooks.
type modelOverride struct {
	rules map[string]string
}

var (
	_ RequestTransformer       = (*modelOverride)(nil)
	_ ClientMessageTransformer = (*modelOverride)(nil)
)

// modelOverrideScope selects the supported inference request inputs: the two
// Anthropic HTTP operations and OpenAI Responses over HTTP and WebSocket.
func modelOverrideScope(surface endpoint.Surface, route endpoint.Route) bool {
	switch surface {
	case endpoint.Anthropic:
		return route == endpoint.RouteAnthropicMessages || route == endpoint.RouteAnthropicCountTokens
	case endpoint.OpenAI:
		return route == endpoint.RouteOpenAIResponses
	default:
		return false
	}
}

// TransformRequest substitutes the request model when a rule matches. Input it
// cannot confidently interpret is forwarded byte-verbatim; it never rejects.
func (o *modelOverride) TransformRequest(_ context.Context, request *Request) error {
	request.Body = o.rewrite(request.Body, false)
	return nil
}

// TransformClientMessage substitutes the model of a response.create Text
// Message. Every Message keeps its kind and is emitted, so the override can
// neither drop a Message nor end a session.
func (o *modelOverride) TransformClientMessage(_ context.Context, message *Message) bool {
	if message.Kind == MessageText {
		message.Data = o.rewrite(message.Data, true)
	}
	return true
}

// rewrite returns payload with its unique top-level string model replaced by a
// mapped target. Any uncertainty returns payload itself, unchanged.
func (o *modelOverride) rewrite(payload []byte, requireResponseCreate bool) []byte {
	if len(o.rules) == 0 {
		return payload
	}
	model := topLevelMember{name: "model"}
	eventType := topLevelMember{name: "type"}
	if !scanTopLevelMembers(payload, &model, &eventType) {
		return payload
	}
	if requireResponseCreate {
		event, ok := eventType.uniqueString(payload)
		if !ok || event != responseCreateEvent {
			return payload
		}
	}
	source, ok := model.uniqueString(payload)
	if !ok {
		return payload
	}
	target, mapped := o.rules[source]
	if !mapped || target == source {
		// A self-mapping keeps the client's exact spelling of the model.
		return payload
	}
	rewritten := make([]byte, 0, len(payload))
	rewritten = append(rewritten, payload[:model.start]...)
	// Configuration accepts only valid UTF-8 IDs, so quoting cannot fail.
	rewritten, _ = jsontext.AppendQuote(rewritten, target)
	return append(rewritten, payload[model.end:]...)
}
