package reporthttp_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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

func TestHandlerClientPublishCompleteEmptyPricingExtension(t *testing.T) {
	var result report.Report
	if err := json.Unmarshal([]byte(emptyJSON), &result); err != nil {
		t.Fatal(err)
	}
	zero := pricing.Amount{}
	result.Pricing = &report.PricingProvenance{
		Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider", CacheWritePolicy: "single_rate",
		Version: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Source: "fallback",
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
	if !strings.Contains(string(got.JSON), `"pricing":{"dataset":"models.dev/api.json","currency":"USD","basis":"original_provider","cache_write_policy":"single_rate","version":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","source":"fallback","last_success":null}`) ||
		!strings.Contains(string(got.JSON), `"cost":{"amount":"0","priced_turns":"0","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}}`) {
		t.Fatalf("complete empty pricing extension missing: %s", got.JSON)
	}
	if got.Report.Pricing.Version != result.Pricing.Version || got.Report.OpenAI.Total.Cost.Amount == nil || got.Report.OpenAI.Total.Cost.Amount.String() != "0" {
		t.Fatalf("complete empty pricing extension not decoded: %+v", got.Report)
	}
	request, err := http.NewRequest(http.MethodHead, server.URL+reporthttp.Path+"?timezone=UTC&surface=openai&period=day&since=2026-09-01&until=2026-09-02", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(body) != 0 || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("complete extension HEAD response: status=%d headers=%v body=%q", response.StatusCode, response.Header, body)
	}
}

func TestHandlerClientPublishCompletePricingAtEveryAggregateLevel(t *testing.T) {
	amount := func(value string) *pricing.Amount {
		parsed, err := pricing.ParseAmount(value)
		if err != nil {
			t.Fatal(err)
		}
		return &parsed
	}
	nativeTotal := func(names []string, turns int64, cost report.Cost) report.Total {
		metrics := make(map[string]report.Metric, len(names))
		for _, name := range names {
			metrics[name] = report.Metric{}
		}
		input, output := turns*10, turns*2
		metrics["input_tokens"] = report.Metric{Sum: &input, ReportedTurns: turns}
		metrics["output_tokens"] = report.Metric{Sum: &output, ReportedTurns: turns}
		return report.Total{Turns: turns, Usage: metrics, Cost: cost}
	}
	models := []struct {
		name   string
		match  report.PricingMatch
		cost   report.Cost
		native string
	}{
		{"ambiguous", report.PricingMatch{Status: report.PricingMatchAmbiguous}, report.Cost{Unpriced: report.UnpricedCoverage{AmbiguousModel: 1}}, "anthropic"},
		{"dated", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "anthropic", Model: "dated-20260901", Method: report.PricingMatchByDated}, report.Cost{Amount: amount("0.03"), PricedTurns: 1, Unpriced: report.UnpricedCoverage{MissingUsage: 1}}, "anthropic"},
		{"unpriced", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "anthropic", Model: "unpriced", Method: report.PricingMatchByAlias}, report.Cost{Unpriced: report.UnpricedCoverage{MissingRate: 1}}, "anthropic"},
		{"exact", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "exact", Method: report.PricingMatchByExact}, report.Cost{Amount: amount("0.01"), PricedTurns: 1}, "openai"},
		{"free", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "free", Method: report.PricingMatchByNormalized}, report.Cost{Amount: amount("0"), PricedTurns: 1}, "openai"},
		{"suffix", report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "suffix-base", Method: report.PricingMatchBySuffix}, report.Cost{Unpriced: report.UnpricedCoverage{InconsistentUsage: 1}}, "openai"},
		{"unknown", report.PricingMatch{Status: report.PricingMatchUnknown}, report.Cost{Unpriced: report.UnpricedCoverage{UnknownModel: 1}}, "openai"},
	}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	lastSuccess := start.Add(-time.Hour)
	result := report.Report{
		SchemaVersion: 1, GeneratedAt: start.Add(12 * time.Hour), Timezone: "UTC", Period: "day",
		Since: "2026-09-01", Until: "2026-09-02", WindowStart: start, WindowEnd: start.AddDate(0, 0, 1),
		Scope: "configured_database", Collection: "best_effort", Surface: "all",
		Buckets: []report.Bucket{{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: start, RangeEnd: start.AddDate(0, 0, 1)}},
		Pricing: &report.PricingProvenance{
			Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider", CacheWritePolicy: "single_rate",
			Version: "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd", Source: "fallback", LastSuccess: &lastSuccess,
		},
		Anthropic: &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}},
		OpenAI:    &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}},
	}
	for _, fixture := range models {
		turns := int64(1)
		if fixture.name == "dated" {
			turns = 2
		}
		total := nativeTotal(report.OpenAIMetrics(), turns, fixture.cost)
		section := result.OpenAI
		if fixture.native == "anthropic" {
			total = nativeTotal(report.AnthropicMetrics(), turns, fixture.cost)
			section = result.Anthropic
		}
		model := report.ModelTotal{Model: fixture.name, PricingMatch: fixture.match, Total: total}
		section.Rows = append(section.Rows, report.Row{BucketStart: "2026-09-01", ModelTotal: model})
		section.Models = append(section.Models, model)
	}
	result.Anthropic.Total = nativeTotal(report.AnthropicMetrics(), 4, report.Cost{Amount: amount("0.03"), PricedTurns: 1, Unpriced: report.UnpricedCoverage{AmbiguousModel: 1, MissingRate: 1, MissingUsage: 1}})
	result.OpenAI.Total = nativeTotal(report.OpenAIMetrics(), 4, report.Cost{Amount: amount("0.01"), PricedTurns: 2, Unpriced: report.UnpricedCoverage{UnknownModel: 1, InconsistentUsage: 1}})

	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return result, nil }))
	t.Cleanup(server.Close)
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := clientQuery()
	query.Surface = "all"
	got, err := client.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Report.Pricing == nil || got.Report.Pricing.Source != "fallback" || got.Report.Pricing.LastSuccess == nil || !got.Report.Pricing.LastSuccess.Equal(lastSuccess) {
		t.Fatalf("fallback successful-fetch provenance = %+v", got.Report.Pricing)
	}
	type expectedModel struct {
		name          string
		turns         int64
		match         report.PricingMatch
		amount        string
		amountPresent bool
		pricedTurns   int64
		unpriced      report.UnpricedCoverage
	}
	type expectedSection struct {
		name        string
		got         *report.Section
		amount      string
		pricedTurns int64
		unpriced    report.UnpricedCoverage
		models      []expectedModel
	}
	expected := []expectedSection{
		{
			name: "anthropic", got: got.Report.Anthropic, amount: "0.03", pricedTurns: 1,
			unpriced: report.UnpricedCoverage{AmbiguousModel: 1, MissingRate: 1, MissingUsage: 1},
			models: []expectedModel{
				{name: "ambiguous", turns: 1, match: report.PricingMatch{Status: report.PricingMatchAmbiguous}, unpriced: report.UnpricedCoverage{AmbiguousModel: 1}},
				{name: "dated", turns: 2, match: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "anthropic", Model: "dated-20260901", Method: report.PricingMatchByDated}, amount: "0.03", amountPresent: true, pricedTurns: 1, unpriced: report.UnpricedCoverage{MissingUsage: 1}},
				{name: "unpriced", turns: 1, match: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "anthropic", Model: "unpriced", Method: report.PricingMatchByAlias}, unpriced: report.UnpricedCoverage{MissingRate: 1}},
			},
		},
		{
			name: "openai", got: got.Report.OpenAI, amount: "0.01", pricedTurns: 2,
			unpriced: report.UnpricedCoverage{UnknownModel: 1, InconsistentUsage: 1},
			models: []expectedModel{
				{name: "exact", turns: 1, match: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "exact", Method: report.PricingMatchByExact}, amount: "0.01", amountPresent: true, pricedTurns: 1},
				{name: "free", turns: 1, match: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "free", Method: report.PricingMatchByNormalized}, amount: "0", amountPresent: true, pricedTurns: 1},
				{name: "suffix", turns: 1, match: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "suffix-base", Method: report.PricingMatchBySuffix}, unpriced: report.UnpricedCoverage{InconsistentUsage: 1}},
				{name: "unknown", turns: 1, match: report.PricingMatch{Status: report.PricingMatchUnknown}, unpriced: report.UnpricedCoverage{UnknownModel: 1}},
			},
		},
	}
	assertCost := func(label string, cost report.Cost, amountValue string, amountPresent bool, pricedTurns int64, unpriced report.UnpricedCoverage) {
		t.Helper()
		if cost.PricedTurns != pricedTurns || cost.Unpriced != unpriced || (cost.Amount != nil) != amountPresent {
			t.Fatalf("%s cost = %+v, want amount %q present=%t priced=%d unpriced=%+v", label, cost, amountValue, amountPresent, pricedTurns, unpriced)
		}
		if amountPresent && cost.Amount.String() != amountValue {
			t.Fatalf("%s amount = %q, want %q", label, cost.Amount.String(), amountValue)
		}
	}
	for _, section := range expected {
		if section.got == nil || len(section.got.Rows) != len(section.models) || len(section.got.Models) != len(section.models) || section.got.Total.Turns != 4 {
			t.Fatalf("%s aggregate shape = %+v", section.name, section.got)
		}
		assertCost(section.name+" total", section.got.Total.Cost, section.amount, true, section.pricedTurns, section.unpriced)
		for index, want := range section.models {
			observed := []struct {
				label string
				value report.ModelTotal
			}{
				{label: "row", value: section.got.Rows[index].ModelTotal},
				{label: "model", value: section.got.Models[index]},
			}
			for _, gotModel := range observed {
				label := section.name + " " + gotModel.label + " " + want.name
				if gotModel.value.Model != want.name || gotModel.value.Turns != want.turns || gotModel.value.PricingMatch != want.match {
					t.Fatalf("%s = %+v, want model=%q turns=%d match=%+v", label, gotModel.value, want.name, want.turns, want.match)
				}
				assertCost(label, gotModel.value.Cost, want.amount, want.amountPresent, want.pricedTurns, want.unpriced)
			}
		}
	}
	for _, fragment := range []string{`"pricing_match":{"status":"matched"`, `"pricing_match":{"status":"unknown"}`, `"pricing_match":{"status":"ambiguous"}`, `"method":"exact"`, `"method":"normalized"`, `"method":"alias"`, `"method":"suffix"`, `"method":"dated"`, `"amount":null`, `"amount":"0"`, `"inconsistent_usage":"1"`} {
		if !strings.Contains(string(got.JSON), fragment) {
			t.Errorf("wire missing %s: %s", fragment, got.JSON)
		}
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
