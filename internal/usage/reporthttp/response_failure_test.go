package reporthttp_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestClientNon2xxBodyFailuresRetainMetadataWithoutDecoding(t *testing.T) {
	const structured = `{"schema_version":1,"error":{"code":"DO_NOT_DECODE","message":"BODY_SECRET"}}`
	for _, tc := range []struct {
		name, requestID, reason string
		body                    string
		contentLength           int
	}{
		{
			name:          "truncated complete JSON prefix",
			requestID:     "truncated-request",
			reason:        "report response read failed or was canceled",
			body:          structured,
			contentLength: len(structured) + 20,
		},
		{
			name:      "oversized complete JSON prefix",
			requestID: "oversized-request",
			reason:    "report response exceeds 8 MiB",
			body:      structured + strings.Repeat(" ", reporthttp.MaxBodyBytes+1-len(structured)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", tc.requestID)
				if tc.contentLength != 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tc.contentLength))
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client, err := reporthttp.NewClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Query(context.Background(), report.Query{Timezone: "UTC"})
			if err == nil {
				t.Fatal("failed response accepted")
			}
			if len(result.JSON) != 0 {
				t.Fatal("failed response returned JSON")
			}
			message := err.Error()
			for _, want := range []string{"report HTTP status 503", tc.reason, tc.requestID} {
				if !strings.Contains(message, want) {
					t.Errorf("error %q does not contain %q", message, want)
				}
			}
			for _, secret := range []string{"DO_NOT_DECODE", "BODY_SECRET"} {
				if strings.Contains(message, secret) {
					t.Fatalf("interpreted failed body: %q", message)
				}
			}
			if requests.Load() != 1 {
				t.Fatalf("made %d requests", requests.Load())
			}
		})
	}
}
