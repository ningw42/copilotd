// Package apierror renders proxy-originated error responses in the inbound
// surface's own dialect — Anthropic-shaped on the Anthropic surface, OpenAI-shaped
// on the OpenAI surface — from a single table, so no other package hand-rolls an
// error body. Upstream (Copilot) error responses are passed through verbatim by
// the forwarder and never routed through here; apierror covers only the errors
// copilotd itself originates: auth (401), readiness (503), the synchronous-only
// rejects (400), body bounding (413), and the proxy-origin 502/504.
package apierror

import (
	"encoding/json"
	"net/http"
)

// Surface selects the error dialect. It is also the forwarder's per-route
// surface tag, so a route's tag drives both its peek behavior and its error
// shape. (CONTEXT.md reserves "provider" for the credential provider;
// the inbound API dialect is a Surface.)
type Surface int

const (
	Anthropic Surface = iota
	OpenAI
)

// Kind enumerates every proxy-originated error condition, each mapped by the
// table below to an HTTP status and the per-surface error type.
type Kind int

const (
	Unauthorized          Kind = iota // 401 — missing or invalid API key
	NotReady                          // 503 — identity has no working credential yet
	StreamUnsupported                 // 400 — stream:true (Phase 1 is synchronous only)
	BackgroundUnsupported             // 400 — background:true (OpenAI surface only)
	PayloadTooLarge                   // 413 — inbound body over the cap
	BadGateway                        // 502 — could not reach the upstream
	GatewayTimeout                    // 504 — upstream call exceeded the deadline
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
	StreamUnsupported:     {http.StatusBadRequest, "invalid_request_error", "invalid_request_error", ""},
	BackgroundUnsupported: {http.StatusBadRequest, "invalid_request_error", "invalid_request_error", ""},
	PayloadTooLarge:       {http.StatusRequestEntityTooLarge, "invalid_request_error", "invalid_request_error", ""},
	BadGateway:            {http.StatusBadGateway, "api_error", "api_error", ""},
	GatewayTimeout:        {http.StatusGatewayTimeout, "api_error", "api_error", ""},
}

// Write renders kind for surface as the mapped status plus a JSON body, setting
// Content-Type: application/json. msg is the human-readable message.
func Write(w http.ResponseWriter, surface Surface, kind Kind, msg string) {
	e := table[kind]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_, _ = w.Write(body(surface, e, msg))
}

func body(surface Surface, e entry, msg string) []byte {
	if surface == OpenAI {
		var b openaiError
		b.Error.Message = msg
		b.Error.Type = e.openaiType
		if e.openaiCode != "" {
			code := e.openaiCode
			b.Error.Code = &code
		}
		out, _ := json.Marshal(b)
		return out
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
