package apierror

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// TestWriteShapesAndStatus asserts every (Surface, kind) pair emits the right
// HTTP status, Content-Type, and JSON body shape from the single mapping table.
func TestWriteShapesAndStatus(t *testing.T) {
	tests := []struct {
		kind       Kind
		wantStatus int
		anthropic  string // Anthropic error.type
		openai     string // OpenAI error.type
		openaiCode string // OpenAI error.code ("" ⇒ expect JSON null)
	}{
		{Unauthorized, 401, "authentication_error", "invalid_request_error", "invalid_api_key"},
		{NotReady, 503, "api_error", "api_error", ""},
		{BackgroundUnsupported, 400, "invalid_request_error", "invalid_request_error", ""},
		{NotAWebSocketUpgrade, 426, "invalid_request_error", "invalid_request_error", ""},
		{PayloadTooLarge, 413, "invalid_request_error", "invalid_request_error", ""},
		{BadGateway, 502, "api_error", "api_error", ""},
		{GatewayTimeout, 504, "api_error", "api_error", ""},
		{ShimError, 500, "api_error", "api_error", ""},
		{InvalidRequest, 400, "invalid_request_error", "invalid_request_error", ""},
	}

	for _, tc := range tests {
		t.Run("anthropic", func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, endpoint.Anthropic, tc.kind, "boom")
			if rec.Code != tc.wantStatus {
				t.Errorf("kind %d: status = %d, want %d", tc.kind, rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("kind %d: Content-Type = %q, want application/json", tc.kind, ct)
			}
			var got struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("kind %d: body not JSON: %v (%s)", tc.kind, err, rec.Body.String())
			}
			if got.Type != "error" {
				t.Errorf("kind %d: top type = %q, want error", tc.kind, got.Type)
			}
			if got.Error.Type != tc.anthropic {
				t.Errorf("kind %d: error.type = %q, want %q", tc.kind, got.Error.Type, tc.anthropic)
			}
			if got.Error.Message != "boom" {
				t.Errorf("kind %d: error.message = %q, want boom", tc.kind, got.Error.Message)
			}
		})

		t.Run("openai", func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, endpoint.OpenAI, tc.kind, "boom")
			if rec.Code != tc.wantStatus {
				t.Errorf("kind %d: status = %d, want %d", tc.kind, rec.Code, tc.wantStatus)
			}
			// Decode into a raw map so a null code/param is observable.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("kind %d: body not JSON: %v (%s)", tc.kind, err, rec.Body.String())
			}
			errObj := struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Code    *string `json:"code"`
				Param   *string `json:"param"`
			}{}
			if err := json.Unmarshal(raw["error"], &errObj); err != nil {
				t.Fatalf("kind %d: error object not JSON: %v", tc.kind, err)
			}
			if errObj.Type != tc.openai {
				t.Errorf("kind %d: error.type = %q, want %q", tc.kind, errObj.Type, tc.openai)
			}
			if errObj.Message != "boom" {
				t.Errorf("kind %d: error.message = %q, want boom", tc.kind, errObj.Message)
			}
			if errObj.Param != nil {
				t.Errorf("kind %d: error.param = %v, want null", tc.kind, *errObj.Param)
			}
			switch {
			case tc.openaiCode == "" && errObj.Code != nil:
				t.Errorf("kind %d: error.code = %q, want null", tc.kind, *errObj.Code)
			case tc.openaiCode != "" && (errObj.Code == nil || *errObj.Code != tc.openaiCode):
				t.Errorf("kind %d: error.code = %v, want %q", tc.kind, errObj.Code, tc.openaiCode)
			}
		})
	}
}

func TestGitHubCopilotWriteMatchesAnthropicForEveryKind(t *testing.T) {
	for kind := range table {
		t.Run(kindName(kind), func(t *testing.T) {
			anthropic := httptest.NewRecorder()
			Write(anthropic, endpoint.Anthropic, kind, "same message")
			githubCopilot := httptest.NewRecorder()
			Write(githubCopilot, endpoint.GitHubCopilot, kind, "same message")

			if githubCopilot.Code != anthropic.Code {
				t.Errorf("status = %d, want Anthropic status %d", githubCopilot.Code, anthropic.Code)
			}
			if !reflect.DeepEqual(githubCopilot.Header(), anthropic.Header()) {
				t.Errorf("headers = %v, want Anthropic headers %v", githubCopilot.Header(), anthropic.Header())
			}
			if !bytes.Equal(githubCopilot.Body.Bytes(), anthropic.Body.Bytes()) {
				t.Errorf("body = %s, want Anthropic body %s", githubCopilot.Body.Bytes(), anthropic.Body.Bytes())
			}
		})
	}
}

func kindName(kind Kind) string {
	return map[Kind]string{
		Unauthorized:          "Unauthorized",
		NotReady:              "NotReady",
		BackgroundUnsupported: "BackgroundUnsupported",
		NotAWebSocketUpgrade:  "NotAWebSocketUpgrade",
		PayloadTooLarge:       "PayloadTooLarge",
		BadGateway:            "BadGateway",
		GatewayTimeout:        "GatewayTimeout",
		ShimError:             "ShimError",
		InvalidRequest:        "InvalidRequest",
	}[kind]
}

func TestRejectCarriesKindAndMessageAsAnError(t *testing.T) {
	err := Reject(InvalidRequest, "unsupported option")
	if err.Kind != InvalidRequest {
		t.Errorf("Kind = %v, want InvalidRequest", err.Kind)
	}
	if err.Msg != "unsupported option" || err.Error() != "unsupported option" {
		t.Errorf("message = %q / %q, want unsupported option", err.Msg, err.Error())
	}
}

func TestWriteStreamErrorAnthropicEnded(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteStreamError(rec, endpoint.Anthropic, StreamEnded)

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream ended before a terminal event\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("stream error was not flushed")
	}
}

func TestWriteStreamErrorAnthropicFailed(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteStreamError(rec, endpoint.Anthropic, StreamFailed)

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream failed\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("stream error was not flushed")
	}
}

func TestWriteStreamErrorAnthropicStalled(t *testing.T) {
	rec := httptest.NewRecorder()

	if err := WriteStreamError(rec, endpoint.Anthropic, StreamStalled); err != nil {
		t.Fatalf("WriteStreamError() error = %v", err)
	}

	const want = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: upstream stream stalled\"}}\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if !rec.Flushed {
		t.Error("stream error was not flushed")
	}
}

func TestWriteStreamErrorShimFailureUsesNativeShape(t *testing.T) {
	tests := []struct {
		name    string
		surface endpoint.Surface
		want    string
	}{
		{
			name:    "Anthropic",
			surface: endpoint.Anthropic,
			want:    "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: shim failed\"}}\n\n",
		},
		{
			name:    "OpenAI",
			surface: endpoint.OpenAI,
			want:    "event: error\ndata: {\"type\":\"error\",\"code\":null,\"message\":\"copilotd: shim failed\",\"param\":null}\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := WriteStreamError(rec, tc.surface, StreamShimFailed); err != nil {
				t.Fatalf("WriteStreamError: %v", err)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if !rec.Flushed {
				t.Error("stream shim error was not flushed")
			}
		})
	}
}

func TestWriteStreamErrorOpenAIUsesBareNativeShape(t *testing.T) {
	tests := []struct {
		name   string
		reason StreamReason
		want   string
	}{
		{
			name:   "ended",
			reason: StreamEnded,
			want:   "event: error\ndata: {\"type\":\"error\",\"code\":null,\"message\":\"copilotd: upstream stream ended before a terminal event\",\"param\":null}\n\n",
		},
		{
			name:   "stalled",
			reason: StreamStalled,
			want:   "event: error\ndata: {\"type\":\"error\",\"code\":null,\"message\":\"copilotd: upstream stream stalled\",\"param\":null}\n\n",
		},
		{
			name:   "failed",
			reason: StreamFailed,
			want:   "event: error\ndata: {\"type\":\"error\",\"code\":null,\"message\":\"copilotd: upstream stream failed\",\"param\":null}\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := WriteStreamError(rec, endpoint.OpenAI, tc.reason); err != nil {
				t.Fatalf("WriteStreamError() error = %v", err)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if !rec.Flushed {
				t.Error("stream error was not flushed")
			}
		})
	}
}
