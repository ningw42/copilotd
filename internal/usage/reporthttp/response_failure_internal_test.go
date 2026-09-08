package reporthttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
)

type syntheticRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip syntheticRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type syntheticCloseErrorBody struct {
	io.Reader
	closes int
}

func (body *syntheticCloseErrorBody) Close() error {
	body.closes++
	return errors.New("CLOSE_SECRET")
}

// This synthetic private transport isolates the close-error branch; it does not
// claim that a real net/http response body naturally returns this close error.
func TestClientNon2xxSyntheticCloseFailureRetainsSafeMetadata(t *testing.T) {
	longControlID := "close-request\x1b[31m\n\u202e" + strings.Repeat("x", 300)
	for _, tc := range []struct {
		name, requestID string
	}{
		{"missing request ID", ""},
		{"ordinary request ID", "close-request"},
		{"long control request ID", longControlID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &syntheticCloseErrorBody{Reader: strings.NewReader(`{"schema_version":1,"error":{"code":"DO_NOT_DECODE","message":"BODY_SECRET"}}`)}
			client, err := NewClient("http://unused.invalid")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			client.http.Transport = syntheticRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Status:     "503 REASON_SECRET",
					Header:     http.Header{"X-Request-Id": []string{tc.requestID}},
					Body:       body,
				}, nil
			})
			result, err := client.Query(context.Background(), report.Query{Timezone: "UTC"})
			if err == nil {
				t.Fatal("failed response accepted")
			}
			if len(result.JSON) != 0 || calls != 1 || body.closes != 1 {
				t.Fatalf("JSON bytes=%d calls=%d closes=%d", len(result.JSON), calls, body.closes)
			}
			message := err.Error()
			if !strings.Contains(message, "report HTTP status 503") || !strings.Contains(message, "report response read failed or was canceled") {
				t.Fatalf("lost status or failure reason: %q", message)
			}
			if tc.requestID == "" && strings.Contains(message, "request ID") {
				t.Fatalf("invented request ID: %q", message)
			}
			if tc.requestID != "" && !strings.Contains(message, "close-request") {
				t.Fatalf("lost request ID: %q", message)
			}
			for _, unsafe := range []string{"\x1b", "\n", "\u202e", "SECRET", "DO_NOT_DECODE"} {
				if strings.Contains(message, unsafe) {
					t.Fatalf("unsafe diagnostic %q contains %q", message, unsafe)
				}
			}
			if tc.name == "long control request ID" && (!strings.Contains(message, `\x1b`) || !strings.Contains(message, `\n`) || !strings.Contains(message, `\u202e`) || !strings.Contains(message, "...")) {
				t.Fatalf("request ID was not bounded and ASCII escaped: %q", message)
			}
			if len(message) > 1000 {
				t.Fatalf("unbounded diagnostic length %d", len(message))
			}
		})
	}
}
