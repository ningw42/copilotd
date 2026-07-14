package apierror

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestWriteShapesAndStatus asserts every (provider, kind) pair emits the right
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
		{StreamUnsupported, 400, "invalid_request_error", "invalid_request_error", ""},
		{BackgroundUnsupported, 400, "invalid_request_error", "invalid_request_error", ""},
		{PayloadTooLarge, 413, "invalid_request_error", "invalid_request_error", ""},
		{BadGateway, 502, "api_error", "api_error", ""},
		{GatewayTimeout, 504, "api_error", "api_error", ""},
	}

	for _, tc := range tests {
		t.Run("anthropic", func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, Anthropic, tc.kind, "boom")
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
			Write(rec, OpenAI, tc.kind, "boom")
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
