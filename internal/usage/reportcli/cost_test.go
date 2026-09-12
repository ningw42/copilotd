package reportcli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

const costLayoutWire = `{
  "schema_version": 1,
  "generated_at": "2026-09-11T12:00:00Z",
  "timezone": "UTC",
  "period": "day",
  "since": "2026-09-01",
  "until": "2026-09-02",
  "window_start": "2026-09-01T00:00:00Z",
  "window_end": "2026-09-02T00:00:00Z",
  "scope": "configured_database",
  "collection": "best_effort",
  "surface": "all",
  "model": null,
  "buckets": [{"start_date":"2026-09-01","until_date":"2026-09-02","range_start":"2026-09-01T00:00:00Z","range_end":"2026-09-02T00:00:00Z","range_partial":false,"in_progress":false}],
  "pricing": {"dataset":"models.dev/api.json","currency":"USD","basis":"original_provider","context_policy":"highest_tier","cache_write_policy":"single_rate","version":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","source":"fetched","last_success":"2026-09-11T11:59:00Z"},
  "anthropic": {
    "rows": [{"bucket_start":"2026-09-01","model":"claude-observed","turns":"1","usage":{"input_tokens":{"sum":"12","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cache_creation_input_tokens":{"sum":"2000","reported_turns":"1"},"cache_read_input_tokens":{"sum":"6000","reported_turns":"1"},"ephemeral_5m_input_tokens":{"sum":null,"reported_turns":"0"},"ephemeral_1h_input_tokens":{"sum":null,"reported_turns":"0"},"thinking_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0.003157","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}},"pricing_match":{"status":"matched","provider":"anthropic","model":"claude-pricing","method":"exact"}}],
    "models": [{"model":"claude-observed","turns":"1","usage":{"input_tokens":{"sum":"12","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cache_creation_input_tokens":{"sum":"2000","reported_turns":"1"},"cache_read_input_tokens":{"sum":"6000","reported_turns":"1"},"ephemeral_5m_input_tokens":{"sum":null,"reported_turns":"0"},"ephemeral_1h_input_tokens":{"sum":null,"reported_turns":"0"},"thinking_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0.003157","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}},"pricing_match":{"status":"matched","provider":"anthropic","model":"claude-pricing","method":"exact"}}],
    "total": {"turns":"1","usage":{"input_tokens":{"sum":"12","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cache_creation_input_tokens":{"sum":"2000","reported_turns":"1"},"cache_read_input_tokens":{"sum":"6000","reported_turns":"1"},"ephemeral_5m_input_tokens":{"sum":null,"reported_turns":"0"},"ephemeral_1h_input_tokens":{"sum":null,"reported_turns":"0"},"thinking_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0.003157","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}}}
  },
  "openai": {
    "rows": [{"bucket_start":"2026-09-01","model":"gpt-observed","turns":"1","usage":{"input_tokens":{"sum":"8012","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cached_tokens":{"sum":"6000","reported_turns":"1"},"cache_write_tokens":{"sum":"2000","reported_turns":"1"},"reasoning_tokens":{"sum":null,"reported_turns":"0"},"total_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}},"pricing_match":{"status":"matched","provider":"openai","model":"gpt-pricing","method":"normalized"}}],
    "models": [{"model":"gpt-observed","turns":"1","usage":{"input_tokens":{"sum":"8012","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cached_tokens":{"sum":"6000","reported_turns":"1"},"cache_write_tokens":{"sum":"2000","reported_turns":"1"},"reasoning_tokens":{"sum":null,"reported_turns":"0"},"total_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}},"pricing_match":{"status":"matched","provider":"openai","model":"gpt-pricing","method":"normalized"}}],
    "total": {"turns":"1","usage":{"input_tokens":{"sum":"8012","reported_turns":"1"},"output_tokens":{"sum":"9","reported_turns":"1"},"cached_tokens":{"sum":"6000","reported_turns":"1"},"cache_write_tokens":{"sum":"2000","reported_turns":"1"},"reasoning_tokens":{"sum":null,"reported_turns":"0"},"total_tokens":{"sum":null,"reported_turns":"0"}},"cost":{"amount":"0","priced_turns":"1","unpriced":{"unknown_model":"0","ambiguous_model":"0","missing_rate":"0","missing_usage":"0","inconsistent_usage":"0"}}}
  }
}`

func runCostWireCommand(t *testing.T, body string, details, jsonMode bool, surface string) (string, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	utc := "UTC"
	var out bytes.Buffer
	err = reportcli.Run(context.Background(), client, reportcli.Options{
		Endpoint: server.URL,
		Timezone: &utc,
		Query:    report.Query{Period: "day", Since: "2026-09-01", Until: "2026-09-02", Surface: surface},
		Details:  details,
		JSON:     jsonMode,
		Timeout:  time.Second,
	}, &out)
	return out.String(), err
}

func TestCommandRoundsExactUSDHalfUpToThreeFractionalDigits(t *testing.T) {
	for _, tc := range []struct {
		name, amount, want string
	}{
		{"additional precision", "0.003157", "0.003"},
		{"half cent-thousandth", "0.0035", "0.004"},
		{"small positive", "0.0004", "0.000"},
		{"small halfway", "0.0005", "0.001"},
		{"whole carry", "0.9995", "1.000"},
		{"exact zero", "0", "0.000"},
		{"large carry", "999999999999999999999.9995", "1000000000000000000000.000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.ReplaceAll(costLayoutWire, `"amount":"0.003157"`, `"amount":"`+tc.amount+`"`)
			text, err := runCostWireCommand(t, body, false, false, "all")
			if err != nil {
				t.Fatal(err)
			}
			if !hasTableRow(text, "", "claude-observed", tc.want, "1", "12", "9", "2,000", "6,000") {
				t.Fatalf("amount %s did not render as %s:\n%s", tc.amount, tc.want, text)
			}
		})
	}
}

func TestCommandSumsExactServerAmountsBeforeRoundingPeriodTotals(t *testing.T) {
	for _, tc := range []struct {
		name, amount, total string
	}{
		{"small positives carry", "0.0004", "0.001"},
		{"halfway cells do not sum", "0.0035", "0.007"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pricedOpenAICommandReport(t, []string{tc.amount, tc.amount}, "0")
			text := commandOutput(t, r, false)
			if !hasTableRow(text, "2026-09-01", "Total", tc.total, "2", "2", "2", "—", "—") {
				t.Fatalf("exact period subtotal missing for two %s cells:\n%s", tc.amount, text)
			}
		})
	}
}

func pricedOpenAICommandReport(t *testing.T, amounts []string, sectionAmount string) report.Report {
	t.Helper()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	provenance := &report.PricingProvenance{
		Dataset: "models.dev/api.json", Currency: "USD", Basis: "original_provider",
		CacheWritePolicy: "single_rate",
		Version:          "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd", Source: "fallback",
	}
	section := &report.Section{}
	for index, raw := range amounts {
		amount, err := pricing.ParseAmount(raw)
		if err != nil {
			t.Fatal(err)
		}
		input, output := int64(1), int64(1)
		model := report.ModelTotal{
			Model:        string(rune('a' + index)),
			PricingMatch: report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "priced", Method: report.PricingMatchByExact},
			Total: report.Total{Turns: 1, Usage: map[string]report.Metric{
				"input_tokens": {Sum: &input, ReportedTurns: 1}, "output_tokens": {Sum: &output, ReportedTurns: 1},
				"cached_tokens": {}, "cache_write_tokens": {}, "reasoning_tokens": {}, "total_tokens": {},
			}, Cost: report.Cost{Amount: &amount, PricedTurns: 1}},
		}
		section.Rows = append(section.Rows, report.Row{BucketStart: "2026-09-01", ModelTotal: model})
		section.Models = append(section.Models, model)
	}
	input, output := int64(len(amounts)), int64(len(amounts))
	serverAmount, err := pricing.ParseAmount(sectionAmount)
	if err != nil {
		t.Fatal(err)
	}
	section.Total = report.Total{Turns: int64(len(amounts)), Usage: map[string]report.Metric{
		"input_tokens": {Sum: &input, ReportedTurns: int64(len(amounts))}, "output_tokens": {Sum: &output, ReportedTurns: int64(len(amounts))},
		"cached_tokens": {}, "cache_write_tokens": {}, "reasoning_tokens": {}, "total_tokens": {},
	}, Cost: report.Cost{Amount: &serverAmount, PricedTurns: int64(len(amounts))}}
	return report.Report{
		SchemaVersion: 1, GeneratedAt: end, Timezone: "UTC", Period: "day", Since: "2026-09-01", Until: "2026-09-02",
		WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Surface: "openai",
		Pricing: provenance, Buckets: []report.Bucket{{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: start, RangeEnd: end}}, OpenAI: section,
	}
}

func TestCommandMarksAndExplainsPartialAndEntirelyUnpricedCosts(t *testing.T) {
	r := pricedOpenAICommandReport(t, []string{"0.0035", "0.25"}, "0.0035")
	partial := report.Cost{Amount: r.OpenAI.Rows[0].Cost.Amount, PricedTurns: 1, Unpriced: report.UnpricedCoverage{UnknownModel: 1, MissingUsage: 1, InconsistentUsage: 1}}
	unpriced := report.Cost{Unpriced: report.UnpricedCoverage{AmbiguousModel: 1, MissingRate: 1}}
	setCostFixtureTotal(&r.OpenAI.Rows[0].Total, 4, partial)
	setCostFixtureTotal(&r.OpenAI.Rows[1].Total, 2, unpriced)
	setCostFixtureTotal(&r.OpenAI.Models[0].Total, 4, partial)
	setCostFixtureTotal(&r.OpenAI.Models[1].Total, 2, unpriced)
	setCostFixtureTotal(&r.OpenAI.Total, 6, report.Cost{Amount: partial.Amount, PricedTurns: 1, Unpriced: report.UnpricedCoverage{UnknownModel: 1, AmbiguousModel: 1, MissingRate: 1, MissingUsage: 1, InconsistentUsage: 1}})
	r.OpenAI.Rows[1].PricingMatch = report.PricingMatch{Status: report.PricingMatchAmbiguous}
	r.OpenAI.Models[1].PricingMatch = report.PricingMatch{Status: report.PricingMatchAmbiguous}

	text := commandOutput(t, r, false)
	for _, want := range [][]string{
		{"2026-09-01", "Total", "0.004*", "6", "2", "2", "—", "—"},
		{"", "a", "0.004*", "4", "1", "1", "—", "—"},
		{"", "b", "—", "2", "1", "1", "—", "—"},
	} {
		if !hasTableRow(text, want...) {
			t.Errorf("missing cost coverage row %q:\n%s", want, text)
		}
	}
	for _, want := range []string{
		"2026-09-01 / Total — estimated cost: 1/6 priced stored Turns; unknown_model=1; ambiguous_model=1; missing_rate=1; missing_usage=1; inconsistent_usage=1",
		"2026-09-01 / a — estimated cost: 1/4 priced stored Turns; unknown_model=1; missing_usage=1; inconsistent_usage=1",
		"2026-09-01 / b — estimated cost: 0/2 priced stored Turns; ambiguous_model=1; missing_rate=1",
	} {
		if strings.Count(text, want) != 1 {
			t.Errorf("cost coverage note %q count != 1:\n%s", want, text)
		}
	}
}

func setCostFixtureTotal(total *report.Total, turns int64, cost report.Cost) {
	total.Turns = turns
	total.Cost = cost
	for _, name := range []string{"input_tokens", "output_tokens"} {
		metric := total.Usage[name]
		metric.ReportedTurns = turns
		total.Usage[name] = metric
	}
}

func TestCommandShowsPricingProvenanceAndEstimateCaveat(t *testing.T) {
	for _, tc := range []struct {
		name, body, source string
		lastSuccess        bool
	}{
		{"fetched", costLayoutWire, "fetched", true},
		{"fallback after successful fetch", strings.Replace(costLayoutWire, `"source":"fetched"`, `"source":"fallback"`, 1), "fallback", true},
		{"cold fallback", strings.Replace(strings.Replace(costLayoutWire, `"source":"fetched"`, `"source":"fallback"`, 1), `"last_success":"2026-09-11T11:59:00Z"`, `"last_success":null`, 1), "fallback", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := runCostWireCommand(t, tc.body, false, false, "all")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"Pricing: original-provider / per-Turn context tiers / single cache-write rates",
				"snapshot: " + tc.source + " (sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef)",
				"Estimated original-provider cost for persisted best-effort observations using current accepted per-Turn context-tier selection and single cache-write rates; not a Copilot bill; may exclude unpriceable Turns.",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("missing pricing provenance/caveat %q:\n%s", want, text)
				}
			}
			fetchText := "last successful fetch: 2026-09-11T11:59:00Z"
			if tc.lastSuccess && !strings.Contains(text, fetchText) {
				t.Errorf("missing successful fetch time:\n%s", text)
			}
			if !tc.lastSuccess && strings.Contains(text, "last successful fetch:") {
				t.Errorf("cold fallback invented a successful fetch time:\n%s", text)
			}
		})
	}
}

func TestCommandPresentsPricingEnabledEmptySectionWithoutInventedRows(t *testing.T) {
	text := commandOutput(t, pricedOpenAICommandReport(t, nil, "0"), true)
	if strings.Count(text, "No stored Turns in the selected range.") != 1 || !strings.Contains(text, "Pricing: original-provider / per-Turn context tiers / single cache-write rates") || !strings.Contains(text, "Estimated original-provider cost") {
		t.Fatalf("pricing-enabled empty presentation is incomplete:\n%s", text)
	}
	assertTextExcludes(t, text, "Est. USD", "estimated cost:", "Pricing model resolutions", "unknown_model=", "missing_usage=")
}

func TestCommandExplainsOlderDaemonWithoutInventingCostCoverage(t *testing.T) {
	text := commandOutput(t, commandReport(), true)
	if strings.Count(text, "Estimated cost unavailable (daemon does not provide prices)") != 1 {
		t.Fatalf("missing exact older-daemon explanation:\n%s", text)
	}
	for _, want := range [][]string{
		{"Day", "Model(s)", "Est. USD", "Turns", "Input", "Output", "Cache write", "Cache read"},
		{"2026-09-01", "Total", "—", "2", "9,007,199,254,740,993", "12", "—", "6,000*"},
		{"Day", "Model(s)", "Turns", "Reasoning", "Reported total"},
	} {
		if !hasTableRow(text, want...) {
			t.Errorf("older-daemon native row missing %q:\n%s", want, text)
		}
	}
	assertTextExcludes(t, text, "estimated cost:", "unknown_model=", "ambiguous_model=", "missing_rate=", "missing_usage=", "inconsistent_usage=", "Pricing: original-provider")
}

func TestCommandDetailsListsDistinctTerminalSafePricingResolutions(t *testing.T) {
	r := pricedOpenAICommandReport(t, []string{"0.001", "0.25", "0.5"}, "0.001")
	reportedMatched := " reported\x1b\n\u202e "
	r.OpenAI.Rows[0].Model, r.OpenAI.Models[0].Model = reportedMatched, reportedMatched
	r.OpenAI.Rows[0].PricingMatch = report.PricingMatch{Status: report.PricingMatchMatched, Provider: " openai\x1b ", Model: " priced模型\n\x00 ", Method: report.PricingMatchBySuffix}
	r.OpenAI.Models[0].PricingMatch = report.PricingMatch{Status: report.PricingMatchMatched, Provider: "openai", Model: "range-pricing", Method: report.PricingMatchByNormalized}
	r.OpenAI.Rows[1].Model, r.OpenAI.Models[1].Model = "ambiguous", "ambiguous"
	r.OpenAI.Rows[1].PricingMatch, r.OpenAI.Models[1].PricingMatch = report.PricingMatch{Status: report.PricingMatchAmbiguous}, report.PricingMatch{Status: report.PricingMatchAmbiguous}
	setCostFixtureTotal(&r.OpenAI.Rows[1].Total, 1, report.Cost{Unpriced: report.UnpricedCoverage{AmbiguousModel: 1}})
	setCostFixtureTotal(&r.OpenAI.Models[1].Total, 1, report.Cost{Unpriced: report.UnpricedCoverage{AmbiguousModel: 1}})
	r.OpenAI.Rows[2].Model, r.OpenAI.Models[2].Model = "unknown", "unknown"
	r.OpenAI.Rows[2].PricingMatch, r.OpenAI.Models[2].PricingMatch = report.PricingMatch{Status: report.PricingMatchUnknown}, report.PricingMatch{Status: report.PricingMatchUnknown}
	setCostFixtureTotal(&r.OpenAI.Rows[2].Total, 1, report.Cost{Unpriced: report.UnpricedCoverage{UnknownModel: 1}})
	setCostFixtureTotal(&r.OpenAI.Models[2].Total, 1, report.Cost{Unpriced: report.UnpricedCoverage{UnknownModel: 1}})
	setCostFixtureTotal(&r.OpenAI.Total, 3, report.Cost{Amount: r.OpenAI.Rows[0].Cost.Amount, PricedTurns: 1, Unpriced: report.UnpricedCoverage{AmbiguousModel: 1, UnknownModel: 1}})

	second := make([]report.Row, len(r.OpenAI.Rows))
	copy(second, r.OpenAI.Rows)
	for index := range second {
		second[index].BucketStart = "2026-09-02"
	}
	r.OpenAI.Rows = append(r.OpenAI.Rows, second...)
	r.Until = "2026-09-03"
	r.WindowEnd = r.WindowEnd.AddDate(0, 0, 1)
	r.Buckets = append(r.Buckets, report.Bucket{StartDate: "2026-09-02", UntilDate: "2026-09-03", RangeStart: r.Buckets[0].RangeEnd, RangeEnd: r.WindowEnd})

	text := commandOutput(t, r, true)
	for _, want := range []string{
		"Pricing model resolutions (Reported → Pricing)",
		`  \x20reported\x1b\n\u202e\x20 → \x20openai\x1b\x20/\x20priced\u6a21\u578b\n\x00\x20 (suffix)`,
		`  \x20reported\x1b\n\u202e\x20 → openai/range-pricing (normalized)`,
		"  ambiguous → ambiguous",
		"  unknown → unknown",
		"A matched Pricing model does not guarantee priceability; rates and native usage must also be complete.",
	} {
		if strings.Count(text, want) != 1 {
			t.Errorf("resolution detail %q count != 1:\n%s", want, text)
		}
	}
	assertTextExcludes(t, text, "\x00", "\x1b", "\u202e", "模型")

	compact := commandOutput(t, r, false)
	assertTextExcludes(t, compact, "Pricing model resolutions", "does not guarantee priceability")
}

func TestCommandPricingJSONPreservesLiteralWireBytesIndependentOfDetails(t *testing.T) {
	for _, details := range []bool{false, true} {
		text, err := runCostWireCommand(t, costLayoutWire, details, true, "all")
		if err != nil {
			t.Fatal(err)
		}
		if text != costLayoutWire+"\n" {
			t.Fatalf("details=%t changed original pricing JSON", details)
		}
		assertTextExcludes(t, text, "Est. USD", "Pricing: original-provider", "Estimated original-provider cost", "Pricing model resolutions")
	}
}

func TestCommandCostSubtotalOverflowEmitsNothingWhileJSONStaysOriginal(t *testing.T) {
	r := pricedOpenAICommandReport(t, []string{strings.Repeat("9", 128), strings.Repeat("9", 128)}, "0")
	for _, details := range []bool{false, true} {
		text, err := runCostReportCommand(t, r, details, false)
		if err == nil || !strings.Contains(err.Error(), "terminal text period cost subtotal overflow") || !strings.Contains(err.Error(), pricing.ErrOverflow.Error()) {
			t.Fatalf("details=%t cost overflow error = %v", details, err)
		}
		if text != "" {
			t.Fatalf("details=%t cost overflow emitted stdout:\n%s", details, text)
		}
	}
	for _, details := range []bool{false, true} {
		text, err := runCostReportCommand(t, r, details, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(text, strings.Repeat("9", 128)) != 4 || !strings.HasSuffix(text, "\n") || strings.Contains(text, "Pricing: original-provider") || strings.Contains(text, "Est. USD") {
			t.Fatalf("details=%t JSON did not bypass terminal subtotal/decorations: %s", details, text)
		}
	}
}

func runCostReportCommand(t *testing.T, r report.Report, details, jsonMode bool) (string, error) {
	t.Helper()
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return r, nil }))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	utc := "UTC"
	var out bytes.Buffer
	err = reportcli.Run(context.Background(), client, reportcli.Options{
		Endpoint: server.URL, Timezone: &utc,
		Query:   report.Query{Period: r.Period, Since: r.Since, Until: r.Until, Surface: r.Surface},
		Details: details, JSON: jsonMode, Timeout: time.Second,
	}, &out)
	return out.String(), err
}

func TestCommandPlacesEstimatedUSDOnlyInBothPrimarySurfaceTables(t *testing.T) {
	text, err := runCostWireCommand(t, costLayoutWire, true, false, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{
		{"Day", "Model(s)", "Est. USD", "Turns", "Uncached input", "Output", "Cache create", "Cache read"},
		{"2026-09-01", "Total", "0.003", "1", "12", "9", "2,000", "6,000"},
		{"", "claude-observed", "0.003", "1", "12", "9", "2,000", "6,000"},
		{"Day", "Model(s)", "Turns", "Thinking", "Cache create 5m", "Cache create 1h"},
		{"Day", "Model(s)", "Est. USD", "Turns", "Input", "Output", "Cache write", "Cache read"},
		{"2026-09-01", "Total", "0.000", "1", "8,012", "9", "2,000", "6,000"},
		{"", "gpt-observed", "0.000", "1", "8,012", "9", "2,000", "6,000"},
		{"Day", "Model(s)", "Turns", "Reasoning", "Reported total"},
	} {
		if !hasTableRow(text, want...) {
			t.Errorf("missing table row %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "Est. USD"); got != 2 {
		t.Fatalf("Est. USD headers = %d, want one per primary Surface table:\n%s", got, text)
	}
}
