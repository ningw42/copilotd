// Package apierror renders copilotd-originated signals in the inbound
// surface's selected dialect — Anthropic-shaped on the Anthropic and GitHub
// Copilot surfaces for now, OpenAI-shaped on the OpenAI surface — from a single
// table, so no other package hand-rolls an error body. Upstream (Copilot) error
// responses are passed through verbatim by the forwarder and never routed
// through here; apierror covers only the errors copilotd itself originates: auth
// (401), unavailable credentials/readiness (503), the unsupported
// background-mode and shim rejects (400), body bounding (413), unexpected shim
// failures (500), copilotd-originated 502/504, and native terminal SSE errors
// after a streaming response is committed.
package apierror

import (
	"encoding/json"
	"net/http"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// Error carries a deliberate surface-native pre-commit rejection from a shim.
type Error struct {
	Kind Kind
	Msg  string
}

// Error implements error.
func (e *Error) Error() string { return e.Msg }

// Reject constructs a deliberate surface-native pre-commit rejection.
func Reject(kind Kind, msg string) *Error { return &Error{Kind: kind, Msg: msg} }

// StreamReason identifies why copilotd must originate a terminal SSE error
// after the HTTP response has already been committed.
type StreamReason int

const (
	StreamEnded StreamReason = iota
	StreamFailed
	StreamStalled
	StreamShimFailed
)

var streamMessages = map[StreamReason]string{
	StreamEnded:      "copilotd: upstream stream ended before a terminal event",
	StreamFailed:     "copilotd: upstream stream failed",
	StreamStalled:    "copilotd: upstream stream stalled",
	StreamShimFailed: "copilotd: shim failed",
}

// Kind enumerates every copilotd-originated error condition, each mapped by the
// table below to an HTTP status and the per-surface error type.
type Kind int

const (
	Unauthorized          Kind = iota // 401 — missing or invalid API key
	NotReady                          // 503 — identity cannot supply a credential or local prerequisites are absent
	BackgroundUnsupported             // 400 — background:true (OpenAI surface only)
	NotAWebSocketUpgrade              // 426 — OpenAI Responses Route called without a WebSocket upgrade
	PayloadTooLarge                   // 413 — inbound body over the cap
	BadGateway                        // 502 — could not reach the upstream
	GatewayTimeout                    // 504 — upstream call exceeded the deadline
	ShimError                         // 500 — unexpected pre-commit shim failure
	InvalidRequest                    // 400 — a shim's deliberate invalid request
)

// entry is one row of the mapping: the HTTP status and each surface's error
// type, plus the OpenAI error code (empty ⇒ rendered as JSON null).
type entry struct {
	status        int
	anthropicType string
	openaiType    string
	openaiCode    string
}

// table is the single source of truth for status + per-surface type/code. The
// Anthropic type is one of authentication_error|invalid_request_error|api_error;
// the OpenAI type mirrors it with a nullable code. BackgroundUnsupported is
// OpenAI-only in practice (rejected before any Anthropic route can raise it), but
// carries a well-formed Anthropic row so the table is total.
var table = map[Kind]entry{
	Unauthorized:          {http.StatusUnauthorized, "authentication_error", "invalid_request_error", "invalid_api_key"},
	NotReady:              {http.StatusServiceUnavailable, "api_error", "api_error", ""},
	BackgroundUnsupported: {http.StatusBadRequest, "invalid_request_error", "invalid_request_error", ""},
	NotAWebSocketUpgrade:  {http.StatusUpgradeRequired, "invalid_request_error", "invalid_request_error", ""},
	PayloadTooLarge:       {http.StatusRequestEntityTooLarge, "invalid_request_error", "invalid_request_error", ""},
	BadGateway:            {http.StatusBadGateway, "api_error", "api_error", ""},
	GatewayTimeout:        {http.StatusGatewayTimeout, "api_error", "api_error", ""},
	ShimError:             {http.StatusInternalServerError, "api_error", "api_error", ""},
	InvalidRequest:        {http.StatusBadRequest, "invalid_request_error", "invalid_request_error", ""},
}

// Write renders kind for surface as the mapped status plus a JSON body, setting
// Content-Type: application/json. msg is the human-readable message.
func Write(w http.ResponseWriter, surface endpoint.Surface, kind Kind, msg string) {
	e := table[kind]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_, _ = w.Write(body(surface, e, msg))
}

// WriteStreamError writes and flushes one native-shaped terminal SSE error.
func WriteStreamError(w http.ResponseWriter, surface endpoint.Surface, reason StreamReason) error {
	var payload []byte
	if surface == endpoint.OpenAI {
		var b openaiStreamError
		b.Type = "error"
		b.Message = streamMessages[reason]
		payload, _ = json.Marshal(b)
	} else {
		payload = body(endpoint.Anthropic, entry{anthropicType: "api_error"}, streamMessages[reason])
	}
	if _, err := w.Write(append(append([]byte("event: error\ndata: "), payload...), '\n', '\n')); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// openaiStreamError is the Responses API's bare terminal error event. Code and
// Param stay nil so the native nullable fields serialize as JSON null.
type openaiStreamError struct {
	Type    string  `json:"type"`
	Code    *string `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

func body(surface endpoint.Surface, e entry, msg string) []byte {
	switch surface {
	case endpoint.OpenAI:
		var b openaiError
		b.Error.Message = msg
		b.Error.Type = e.openaiType
		if e.openaiCode != "" {
			code := e.openaiCode
			b.Error.Code = &code
		}
		out, _ := json.Marshal(b)
		return out
	case endpoint.GitHubCopilot:
		// GitHub Copilot is a distinct Surface even though its local failures
		// deliberately reuse the Anthropic envelope for now.
		return body(endpoint.Anthropic, e, msg)
	}
	var b anthropicError
	b.Type = "error"
	b.Error.Type = e.anthropicType
	b.Error.Message = msg
	out, _ := json.Marshal(b)
	return out
}

// anthropicError renders {"type":"error","error":{"type":"…","message":"…"}}.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// openaiError renders {"error":{"message":"…","type":"…","code":…,"param":null}}.
// Code and Param are pointers so they serialize to null when unset.
type openaiError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    *string `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}
