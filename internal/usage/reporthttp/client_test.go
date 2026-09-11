package reporthttp_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

const emptyJSON = `{"schema_version":1,"generated_at":"2026-09-07T12:00:00Z","timezone":"UTC","period":"day","since":"2026-09-01","until":"2026-09-02","window_start":"2026-09-01T00:00:00Z","window_end":"2026-09-02T00:00:00Z","scope":"configured_database","collection":"best_effort","surface":"openai","model":null,"buckets":[{"start_date":"2026-09-01","until_date":"2026-09-02","range_start":"2026-09-01T00:00:00Z","range_end":"2026-09-02T00:00:00Z","range_partial":false,"in_progress":false}],"openai":{"rows":[],"models":[],"total":{"turns":"0","usage":{"input_tokens":{"sum":"0","reported_turns":"0"},"output_tokens":{"sum":"0","reported_turns":"0"},"cached_tokens":{"sum":null,"reported_turns":"0"},"cache_write_tokens":{"sum":null,"reported_turns":"0"},"reasoning_tokens":{"sum":null,"reported_turns":"0"},"total_tokens":{"sum":null,"reported_turns":"0"}}}},"future":{"additive":true}}`

const emptyAnthropicSection = `{"rows":[],"models":[],"total":{"turns":"0","usage":{"input_tokens":{"sum":"0","reported_turns":"0"},"output_tokens":{"sum":"0","reported_turns":"0"},"cache_creation_input_tokens":{"sum":null,"reported_turns":"0"},"cache_read_input_tokens":{"sum":null,"reported_turns":"0"},"ephemeral_5m_input_tokens":{"sum":null,"reported_turns":"0"},"ephemeral_1h_input_tokens":{"sum":null,"reported_turns":"0"},"thinking_tokens":{"sum":null,"reported_turns":"0"}}}}`

func TestHandlerClientPreserveSelectedNativeSections(t *testing.T) {
	for _, surface := range []string{"anthropic", "all", ""} {
		t.Run("surface="+surface, func(t *testing.T) {
			var result report.Report
			if err := json.Unmarshal([]byte(emptyJSON), &result); err != nil {
				t.Fatal(err)
			}
			result.Surface = surface
			if surface == "" {
				result.Surface = "all"
			}
			var section report.Section
			if err := json.Unmarshal([]byte(emptyAnthropicSection), &section); err != nil {
				t.Fatal(err)
			}
			result.Anthropic = &section
			if surface == "anthropic" {
				result.OpenAI = nil
			}
			server := httptest.NewServer(reporthttp.Handler(func(_ context.Context, q report.Query) (report.Report, error) {
				if q.Surface != surface {
					t.Errorf("selection changed: %+v", q)
				}
				return result, nil
			}))
			defer server.Close()
			client, _ := reporthttp.NewClient(server.URL)
			q := clientQuery()
			q.Surface = surface
			got, err := client.Query(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			a := got.Report.Anthropic
			if a == nil || a.Rows == nil || a.Models == nil || a.Total.Turns != 0 || len(a.Total.Usage) != 7 || *a.Total.Usage["input_tokens"].Sum != 0 || a.Total.Usage["thinking_tokens"].Sum != nil {
				t.Fatalf("selected empty native section: %+v", a)
			}
			if (got.Report.OpenAI != nil) != (surface != "anthropic") {
				t.Fatalf("unselected section present: %+v", got.Report)
			}
			if !strings.Contains(string(got.JSON), `"anthropic":`) {
				t.Fatal("original native JSON lost")
			}
		})
	}
}

func TestHandlerClientKeepQueryPricingFieldsOffLegacyWire(t *testing.T) {
	var result report.Report
	if err := json.Unmarshal([]byte(emptyJSON), &result); err != nil {
		t.Fatal(err)
	}
	zero := pricing.Amount{}
	result.Pricing = report.PricingProvenance{
		Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider", ContextPolicy: "highest_tier", CacheWritePolicy: "single_rate",
		Version: "sha256:internal-only", Source: "fallback",
	}
	result.OpenAI.Total.Cost = report.Cost{Amount: &zero}

	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) {
		return result, nil
	}))
	t.Cleanup(server.Close)
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Query(context.Background(), clientQuery())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(emptyJSON, `,"future":{"additive":true}`, "", 1)
	want = strings.Replace(want,
		`"input_tokens":{"sum":"0","reported_turns":"0"},"output_tokens":{"sum":"0","reported_turns":"0"},"cached_tokens":{"sum":null,"reported_turns":"0"},"cache_write_tokens":{"sum":null,"reported_turns":"0"}`,
		`"cache_write_tokens":{"sum":null,"reported_turns":"0"},"cached_tokens":{"sum":null,"reported_turns":"0"},"input_tokens":{"sum":"0","reported_turns":"0"},"output_tokens":{"sum":"0","reported_turns":"0"}`, 1)
	if string(got.JSON) != want || strings.Contains(string(got.JSON), `"pricing"`) || strings.Contains(string(got.JSON), `"cost"`) || strings.Contains(string(got.JSON), `"pricing_match"`) {
		t.Fatalf("staged pricing changed legacy wire:\n got %s\nwant %s", got.JSON, want)
	}
	if got.Report.Pricing != (report.PricingProvenance{}) || got.Report.OpenAI.Total.Cost.Amount != nil {
		t.Fatalf("legacy client unexpectedly reconstructed hidden pricing: %+v", got.Report)
	}
}

func clientQuery() report.Query {
	return report.Query{Surface: "openai", Period: "day", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02"}
}

func TestClientDefaultTLSRejectsUntrustedCertificateBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, emptyJSON)
	}))
	t.Cleanup(server.Close)
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(context.Background(), clientQuery())
	if err == nil || !strings.Contains(err.Error(), "report request failed") || !strings.Contains(err.Error(), "failed to verify certificate") {
		t.Fatalf("default TLS certificate verification: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("untrusted TLS reached HTTP handler %d times", got)
	}
}

func TestClientRejectsUnsafeEndpointsAndBoundsTransport(t *testing.T) {
	for _, endpoint := range []string{"file:///tmp/db", "ftp://example.test", "http:///missing", "http://user:secret@example.test", "http://example.test?", "http://example.test#", "http://example.test:0", "http://example.test:65536", "http://example.test:bad", "http://example.test:", "http://:8080"} {
		if _, err := reporthttp.NewClient(endpoint); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, emptyJSON)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	client, _ := reporthttp.NewClient(redirect.URL)
	if _, err := client.Query(context.Background(), clientQuery()); err == nil || followed.Load() {
		t.Errorf("followed redirect or accepted it: %v", err)
	}
	for _, kind := range []string{"wrong media", "chunked", "gzip", "html error", "structured error", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "wrong media":
					w.Header().Set("Content-Type", "text/plain")
					_, _ = io.WriteString(w, emptyJSON)
				case "chunked":
					w.Header().Set("Content-Type", "application/json")
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, strings.Repeat(" ", 8<<20)+emptyJSON)
				case "gzip":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Encoding", "gzip")
					z := gzip.NewWriter(w)
					_, _ = io.WriteString(z, strings.Repeat(" ", 8<<20)+emptyJSON)
					_ = z.Close()
				case "html error":
					w.WriteHeader(404)
					_, _ = io.WriteString(w, "<html>do not print secrets here</html>")
				case "structured error":
					w.Header().Set("X-Request-Id", "request-123")
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"schema_version":1,"error":{"code":"usage_unavailable","message":"bad\u001b[31m\ntext"}}`)
				case "timeout":
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			client, _ := reporthttp.NewClient(server.URL)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := client.Query(ctx, clientQuery())
			if err == nil {
				t.Fatal("accepted unsafe response")
			}
			if strings.Contains(err.Error(), "secrets") || strings.ContainsAny(err.Error(), "\x1b\n") {
				t.Fatalf("unsafe diagnostic: %q", err)
			}
			if kind == "structured error" && (!strings.Contains(err.Error(), "usage_unavailable") || !strings.Contains(err.Error(), "request-123")) {
				t.Fatalf("lost bounded structured diagnostic: %v", err)
			}
		})
	}
}

func TestClientRequestsPrefixSafelyAndAcceptsCompleteEmptyReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilotd/usage/v1/report" || r.Method != "GET" || r.URL.Query().Get("surface") != "openai" || r.URL.Query().Get("timezone") != "UTC" {
			t.Errorf("request: %s %s", r.Method, r.URL)
		}
		for _, header := range []string{"Authorization", "x-api-key", "Cookie", "Editor-Version", "Copilot-Integration-Id"} {
			if r.Header.Get(header) != "" {
				t.Errorf("credential/impersonation header %s", header)
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(emptyJSON))
	}))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL + "/copilotd/")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Query(context.Background(), clientQuery())
	if err != nil {
		t.Fatal(err)
	}
	if result.Report.OpenAI == nil || result.Report.OpenAI.Rows == nil || result.Report.OpenAI.Total.Turns != 0 || result.Report.OpenAI.Total.Usage["cached_tokens"].Sum != nil || *result.Report.OpenAI.Total.Usage["input_tokens"].Sum != 0 {
		t.Fatalf("report: %+v", result.Report)
	}
	if string(result.JSON) != emptyJSON {
		t.Error("additive original bytes lost")
	}
}
