package shim

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"io"
	"unicode/utf8"

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
// WebSocket Message is interpreted independently. Instances share one immutable
// target map built when the registry is constructed. It has no response hooks.
type modelOverride struct {
	targets modelOverrideTargets
}

// modelOverrideTargets maps each source model ID to its target as an encoded
// JSON string. Self-mappings are omitted so a matching source remains
// byte-verbatim. It is never mutated after construction.
type modelOverrideTargets map[string][]byte

var (
	_ RequestTransformer       = (*modelOverride)(nil)
	_ ClientMessageTransformer = (*modelOverride)(nil)
)

// newModelOverrideTargets copies and pre-encodes rules once per registry.
func newModelOverrideTargets(rules map[string]string) modelOverrideTargets {
	targets := make(modelOverrideTargets, len(rules))
	for source, target := range rules {
		if source == target {
			continue
		}
		encoded, err := jsontext.AppendQuote(nil, target)
		if err != nil {
			// Configuration rejects IDs that are not valid UTF-8. Never send a
			// replacement-character spelling of an operator's target.
			continue
		}
		targets[source] = encoded
	}
	return targets
}

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
	if len(o.targets) == 0 {
		return payload
	}
	members, ok := scanModelOverrideMembers(payload)
	if !ok {
		return payload
	}
	if requireResponseCreate {
		eventType, ok := members.eventType.decode(payload)
		if !ok || eventType != responseCreateEvent {
			return payload
		}
	}
	model, ok := members.model.decode(payload)
	if !ok {
		return payload
	}
	target, mapped := o.targets[model]
	if !mapped {
		return payload
	}
	span := members.model
	rewritten := make([]byte, 0, len(payload)-(span.end-span.start)+len(target))
	rewritten = append(rewritten, payload[:span.start]...)
	rewritten = append(rewritten, target...)
	return append(rewritten, payload[span.end:]...)
}

// modelOverrideMember records how often one top-level member name occurred
// and the exact byte span of its last raw value.
type modelOverrideMember struct {
	occurrences int
	start, end  int
}

// decode returns the member's string value only when it occurs exactly once,
// is a JSON string, and decodes losslessly. AppendUnquote rejects every other
// value kind as well as lone surrogate escapes.
func (m modelOverrideMember) decode(payload []byte) (string, bool) {
	if m.occurrences != 1 {
		return "", false
	}
	decoded, err := jsontext.AppendUnquote(nil, payload[m.start:m.end])
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

type modelOverrideMembers struct {
	model, eventType modelOverrideMember
}

// scanModelOverrideMembers walks exactly one JSON object followed only by
// whitespace, recording the top-level model and type members by their decoded
// names. Unrelated values are validated but never decoded or re-encoded, and
// unrelated duplicate names are allowed. A payload that is not valid UTF-8 is
// malformed JSON text.
func scanModelOverrideMembers(payload []byte) (modelOverrideMembers, bool) {
	var members modelOverrideMembers
	if !utf8.Valid(payload) {
		return members, false
	}
	// Strings in unrelated members may carry lone surrogate escapes, which are
	// grammatical JSON; the selected members are decoded strictly above.
	decoder := jsontext.NewDecoder(bytes.NewReader(payload),
		jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '{' {
		return members, false
	}
	for decoder.PeekKind() != '}' {
		name, err := decoder.ReadToken()
		if err != nil {
			return members, false
		}
		// A Token is voided by the next read, so resolve its name first.
		var member *modelOverrideMember
		switch name.String() {
		case "model":
			member = &members.model
		case "type":
			member = &members.eventType
		}
		value, err := decoder.ReadValue()
		if err != nil {
			return members, false
		}
		if member != nil {
			member.occurrences++
			member.end = int(decoder.InputOffset())
			member.start = member.end - len(value)
		}
	}
	if token, err := decoder.ReadToken(); err != nil || token.Kind() != '}' {
		return members, false
	}
	// The decoder accepts a stream of values; anything but end of input after
	// the object, including a second value, is trailing content.
	if _, err := decoder.ReadToken(); err != io.EOF {
		return members, false
	}
	return members, true
}
