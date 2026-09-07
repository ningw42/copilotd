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

func TestClientValidatesCanonicalNativeSelectionAndEveryAnthropicMetric(t *testing.T) {
	for _, tc := range []struct {
		name, selection, effective, old, replacement string
		valid                                        bool
	}{
		{"default cannot select one Surface", "", "openai", "", "", false},
		{"unknown effective Surface", "all", "unknown", "", "", false},
		{"all missing Anthropic", "all", "all", `"anthropic":`, `"Anthropic":`, false},
		{"all missing OpenAI", "all", "all", `"openai":`, `"OpenAI":`, false},
		{"all NULL Anthropic", "all", "all", emptyAnthropicSection, `null`, false},
		{"unselected known NULL", "anthropic", "anthropic", `"future":`, `"openai":null,"future":`, false},
		{"unselected known empty", "openai", "openai", `"future":`, `"anthropic":` + emptyAnthropicSection + `,"future":`, false},
		{"unknown additive native case", "anthropic", "anthropic", `"future":`, `"OpenAI":null,"future":`, true},
		{"optional sum requires coverage", "all", "all", `"thinking_tokens":{"sum":null`, `"thinking_tokens":{"sum":"0"`, false},
		{"coverage requires sum", "all", "all", `"thinking_tokens":{"sum":null,"reported_turns":"0"}`, `"thinking_tokens":{"sum":null,"reported_turns":"1"}`, false},
		{"missing native cache metric", "all", "all", `"cache_creation_input_tokens":`, `"CACHE_CREATION_INPUT_TOKENS":`, false},
		{"duplicate escaped metric", "all", "all", `"thinking_tokens":`, `"thinking_tokens":{},"thinking_\u0074okens":`, false},
		{"unknown metric remains additive", "anthropic", "anthropic", `"thinking_tokens":`, `"future_native_tokens":{"sum":"999"},"thinking_tokens":`, true},
		{"unpaired surrogate additive field", "all", "all", `"additive":true`, `"additive":"\udc00"`, false},
		{"NULL native models", "anthropic", "anthropic", `"models":[]`, `"models":null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root map[string]json.RawMessage
			if err := json.Unmarshal([]byte(emptyJSON), &root); err != nil {
				t.Fatal(err)
			}
			root["surface"] = json.RawMessage(`"` + tc.effective + `"`)
			if tc.effective == "all" || tc.effective == "anthropic" {
				root["anthropic"] = json.RawMessage(emptyAnthropicSection)
			}
			if tc.effective == "anthropic" {
				delete(root, "openai")
			}
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if tc.old != "" {
				if !strings.Contains(body, tc.old) {
					t.Fatal("invalid mutation fixture")
				}
				body = strings.Replace(body, tc.old, tc.replacement, 1)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client, _ := reporthttp.NewClient(server.URL)
			q := clientQuery()
			q.Surface = tc.selection
			got, err := client.Query(context.Background(), q)
			if (err == nil) != tc.valid {
				t.Fatalf("protocol valid=%t: %v", tc.valid, err)
			}
			if tc.valid && string(got.JSON) != body {
				t.Fatal("original additive bytes lost")
			}
		})
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens", "thinking_tokens"} {
		t.Run("required native metric "+name, func(t *testing.T) {
			body := strings.Replace(emptyJSON, `"surface":"openai"`, `"surface":"all"`, 1)
			section := strings.Replace(emptyAnthropicSection, `"`+name+`":`, `"unknown":`, 1)
			body = strings.Replace(body, `"openai":`, `"anthropic":`+section+`,"openai":`, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client, _ := reporthttp.NewClient(server.URL)
			q := clientQuery()
			q.Surface = "all"
			if _, err := client.Query(context.Background(), q); err == nil {
				t.Fatalf("accepted missing native metric %s", name)
			}
		})
	}
}

func clientQuery() report.Query {
	return report.Query{Surface: "openai", Period: "day", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02"}
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

func TestClientRejectsAmbiguousOrContradictorySuccessJSON(t *testing.T) {
	cases := []struct{ name, old, replacement string }{
		{"version", "\"schema_version\":1", "\"schema_version\":2"},
		{"case is not required", "\"timezone\":\"UTC\"", "\"TIMEZONE\":\"UTC\""},
		{"duplicate", "\"timezone\":\"UTC\"", "\"timezone\":\"UTC\",\"timezone\":\"UTC\""},
		{"escaped duplicate", "\"timezone\":\"UTC\"", "\"timezone\":\"UTC\",\"time\\u007aone\":\"UTC\""},
		{"wrong selection", "\"timezone\":\"UTC\"", "\"timezone\":\"Etc/UTC\""},
		{"surface", "\"surface\":\"openai\"", "\"surface\":\"all\""},
		{"filter", "\"model\":null", "\"model\":\"x\""},
		{"period", "\"period\":\"day\"", "\"period\":\"week\""},
		{"bound", "\"since\":\"2026-09-01\"", "\"since\":\"2026-08-01\""},
		{"null array", "\"rows\":[]", "\"rows\":null"},
		{"missing section", "\"openai\":", "\"not_openai\":"},
		{"missing metric", "\"cached_tokens\":", "\"unknown_tokens\":"},
		{"null required", "\"input_tokens\":{\"sum\":\"0\"", "\"input_tokens\":{\"sum\":null"},
		{"optional zero uncovered", "\"cached_tokens\":{\"sum\":null", "\"cached_tokens\":{\"sum\":\"0\""},
		{"coverage", "\"reported_turns\":\"0\"", "\"reported_turns\":\"1\""},
		{"negative", "\"turns\":\"0\"", "\"turns\":\"-1\""},
		{"range", "\"turns\":\"0\"", "\"turns\":\"9223372036854775808\""},
		{"leading zero", "\"turns\":\"0\"", "\"turns\":\"00\""},
		{"number not string", "\"turns\":\"0\"", "\"turns\":0"},
		{"surrogate", "\"additive\":true", "\"additive\":\"\\ud800\""},
		{"invalid utf8", "\"additive\":true", "\"additive\":\"\xff\""},
		{"trailing", "\"additive\":true}}", "\"additive\":true}} {}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(emptyJSON, tc.old, tc.replacement, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client, err := reporthttp.NewClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Query(context.Background(), clientQuery()); err == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
	t.Run("case variants remain additive", func(t *testing.T) {
		body := strings.Replace(emptyJSON, `"future":`, `"TIMEZONE":"not the effective zone","future":`, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()
		client, _ := reporthttp.NewClient(server.URL)
		result, err := client.Query(context.Background(), clientQuery())
		if err != nil || result.Report.Timezone != "UTC" {
			t.Fatalf("additive case variant: %+v %v", result, err)
		}
	})
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
