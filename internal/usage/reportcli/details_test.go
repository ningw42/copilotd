package reportcli_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestCommandDetailsGroupsAnthropicNativeSubsetsByPeriod(t *testing.T) {
	r := commandReport()
	r.Surface = "all"
	counts := map[string]report.Metric{"input_tokens": {Sum: number(12), ReportedTurns: 2}, "output_tokens": {Sum: number(9), ReportedTurns: 2}, "cache_creation_input_tokens": {Sum: number(2000), ReportedTurns: 1}, "cache_read_input_tokens": {}, "thinking_tokens": {Sum: number(4), ReportedTurns: 1}, "ephemeral_5m_input_tokens": {Sum: number(0), ReportedTurns: 1}, "ephemeral_1h_input_tokens": {}}
	model := report.ModelTotal{Model: "模型\t\u2066\n", Total: report.Total{Turns: 2, Usage: counts}}
	r.Anthropic = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: model}}, Models: []report.ModelTotal{model}, Total: model.Total}
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return r, nil }))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "all", Since: "2026-09-01", Until: "2026-09-02"}, Details: true, Timeout: time.Second}
	var out bytes.Buffer
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	text, openai, found := strings.Cut(out.String(), "OpenAI\n")
	if !found || !strings.Contains(text, "Anthropic\n") || !strings.Contains(openai, "Reasoning") {
		t.Fatal(out.String())
	}
	for _, want := range []string{"Thinking", "Cache create 5m", "Cache create 1h", "thinking: 1/2 stored Turns", "cache create 5m: 1/2 stored Turns"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	for _, want := range [][]string{
		{"2026-09-01", `\u6a21\u578b\t\u2066\n`, "2", "4*", "0*", "—"},
	} {
		if !hasTableRow(text, want...) {
			t.Errorf("missing secondary row %q: %s", want, text)
		}
	}
	assertTextExcludes(t, text, "模型", "\u2066", "Reasoning", "8,012", "Grand total")
}

func TestCommandDetailsGroupsOpenAIReportedSecondaryValuesByPeriod(t *testing.T) {
	r := commandReport()
	r.Buckets[0].RangePartial, r.Buckets[0].InProgress = true, true
	// Deliberately independent server totals: validation is not aggregation.
	for i, total := range []*report.Total{&r.OpenAI.Rows[0].Total, &r.OpenAI.Models[0].Total, &r.OpenAI.Total} {
		metrics := map[string]report.Metric{}
		for k, v := range total.Usage {
			metrics[k] = v
		}
		total.Usage = metrics
		switch i {
		case 0:
			metrics["reasoning_tokens"] = report.Metric{Sum: number(0), ReportedTurns: 1}
		case 1:
			metrics["reasoning_tokens"] = report.Metric{Sum: number(4), ReportedTurns: 2}
			metrics["total_tokens"] = report.Metric{Sum: number(9007199254740993), ReportedTurns: 1}
		case 2:
			metrics["reasoning_tokens"] = report.Metric{Sum: number(6), ReportedTurns: 2}
			metrics["total_tokens"] = report.Metric{Sum: number(9223372036854775807), ReportedTurns: 2}
		}
	}
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return r, nil }))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Since: "2026-09-01", Until: "2026-09-02"}, Details: true, Timeout: time.Second}
	var out bytes.Buffer
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Reasoning", "Reported total", "reasoning: 1/2 stored Turns", "Persisted successful Turns"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	for _, want := range [][]string{
		{"2026-09-01", `evil\x1b[31m\n\u202e`, "2", "0*", "—"},
	} {
		if !hasTableRow(text, want...) {
			t.Errorf("missing secondary row %q: %s", want, text)
		}
	}
	assertTextExcludes(t, text, "\x1b", "\u202e", `"evil\x1b[31m\n\u202e"`, `├─ "`, `└─ "`, "│ All ", "[clipped]", "[in progress]", "Model totals", "Section total", "│ Range", "reported total: 1/2 stored Turns", "9,223,372,036,854,775,807")
	options.Details = false
	out.Reset()
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Reasoning") || strings.Contains(out.String(), "Reported total") {
		t.Fatal("secondary table in compact output")
	}
}
