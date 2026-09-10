package reportcli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func number(n int64) *int64 { return &n }

func assertTextExcludes(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Errorf("unexpected fragment %q in:\n%s", fragment, text)
		}
	}
}

func hasTableRow(text string, want ...string) bool {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
			continue
		}
		cells := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "│"), "│"), "│")
		if len(cells) != len(want) {
			continue
		}
		matches := true
		for i := range cells {
			if strings.TrimSpace(cells[i]) != want[i] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func commandOutput(t *testing.T, r report.Report, details bool) string {
	t.Helper()
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return r, nil }))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	utc := "UTC"
	var out bytes.Buffer
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Period: r.Period, Since: r.Since, Until: r.Until}, Details: details, Timeout: time.Second}
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func openAICommandTotal(turns, cachedCoverage, reasoningCoverage int64) report.Total {
	input, output := 10*turns, turns
	usage := map[string]report.Metric{
		"input_tokens":       {Sum: &input, ReportedTurns: turns},
		"output_tokens":      {Sum: &output, ReportedTurns: turns},
		"cached_tokens":      {},
		"cache_write_tokens": {},
		"reasoning_tokens":   {},
		"total_tokens":       {},
	}
	if cachedCoverage > 0 {
		cached := cachedCoverage
		usage["cached_tokens"] = report.Metric{Sum: &cached, ReportedTurns: cachedCoverage}
	}
	if reasoningCoverage > 0 {
		reasoning := reasoningCoverage
		usage["reasoning_tokens"] = report.Metric{Sum: &reasoning, ReportedTurns: reasoningCoverage}
	}
	return report.Total{Turns: turns, Usage: usage}
}

func multiPeriodCommandReport(names []string, days int, turns int64, coverage func(day, model int) int64) report.Report {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, days)
	section := &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}}
	modelCoverage := make([]int64, len(names))
	var totalCoverage int64
	for day := range days {
		from := start.AddDate(0, 0, day)
		bucket := from.Format(time.DateOnly)
		for model, name := range names {
			reported := coverage(day, model)
			modelCoverage[model] += reported
			totalCoverage += reported
			section.Rows = append(section.Rows, report.Row{BucketStart: bucket, ModelTotal: report.ModelTotal{Model: name, Total: openAICommandTotal(turns, reported, reported)}})
		}
	}
	for model, name := range names {
		section.Models = append(section.Models, report.ModelTotal{Model: name, Total: openAICommandTotal(turns*int64(days), modelCoverage[model], modelCoverage[model])})
	}
	section.Total = openAICommandTotal(turns*int64(days*len(names)), totalCoverage, totalCoverage)
	r := report.Report{SchemaVersion: 1, GeneratedAt: end, Timezone: "UTC", Period: "day", Since: start.Format(time.DateOnly), Until: end.Format(time.DateOnly), WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Surface: "openai", OpenAI: section}
	for day := range days {
		from, until := start.AddDate(0, 0, day), start.AddDate(0, 0, day+1)
		r.Buckets = append(r.Buckets, report.Bucket{StartDate: from.Format(time.DateOnly), UntilDate: until.Format(time.DateOnly), RangeStart: from, RangeEnd: until})
	}
	return r
}

func TestCommandEscapesBoundarySpacesWithoutModelCollisions(t *testing.T) {
	names := []string{" ", " m", "m", "m ", `m\x20`}
	r := multiPeriodCommandReport(names, 1, 1, func(int, int) int64 { return 0 })
	text := commandOutput(t, r, false)
	var got []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "│") {
			continue
		}
		cells := strings.Split(line, "│")
		if len(cells) == 9 && strings.TrimSpace(cells[2]) != "Model(s)" {
			got = append(got, strings.TrimSpace(cells[2]))
		}
	}
	want := []string{`\x20`, `\x20m`, "m", `m\x20`, `m\\x20`}
	if !slices.Equal(got, want) {
		t.Fatalf("model cells = %q, want distinct escaped identities %q\n%s", got, want, text)
	}
}

func TestCommandCoverageNotesIdentifyPeriodAndModel(t *testing.T) {
	names := []string{"alpha ", "beta"}
	r := multiPeriodCommandReport(names, 2, 3, func(day, model int) int64 { return int64(1 + (day+model)%2) })
	for _, details := range []bool{false, true} {
		t.Run(fmt.Sprintf("details=%t", details), func(t *testing.T) {
			text := commandOutput(t, r, details)
			for day := range 2 {
				bucket := time.Date(2026, 9, 1+day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
				for model, name := range []string{`alpha\x20`, "beta"} {
					fraction := 1 + (day+model)%2
					for _, metric := range []string{"cache read", "reasoning"} {
						want := fmt.Sprintf("%s / %s — %s: %d/3 stored Turns", bucket, name, metric, fraction)
						if metric == "reasoning" && !details {
							if strings.Contains(text, want) {
								t.Fatalf("compact output contains secondary coverage %q", want)
							}
							continue
						}
						if strings.Count(text, want) != 1 {
							t.Errorf("coverage context %q count != 1:\n%s", want, text)
						}
					}
				}
			}
			for _, line := range strings.Split(text, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "cache read:") || strings.HasPrefix(trimmed, "reasoning:") {
					t.Errorf("detached coverage note %q", trimmed)
				}
			}
		})
	}
}

func commandReport() report.Report {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	counts := map[string]report.Metric{"input_tokens": {Sum: number(9007199254740993), ReportedTurns: 2}, "output_tokens": {Sum: number(12), ReportedTurns: 2}, "cached_tokens": {Sum: number(6000), ReportedTurns: 1}, "cache_write_tokens": {}, "reasoning_tokens": {}, "total_tokens": {}}
	total := report.Total{Turns: 2, Usage: counts}
	model := report.ModelTotal{Model: "evil\x1b[31m\n\u202e", Total: total}
	return report.Report{SchemaVersion: 1, GeneratedAt: start, Timezone: "UTC", Period: "day", Since: "2026-09-01", Until: "2026-09-02", WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Surface: "openai", Buckets: []report.Bucket{{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: start, RangeEnd: end}}, OpenAI: &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: model}}, Models: []report.ModelTotal{model}, Total: total}}
}
func TestCommandValidatesExplicitNamedZonesBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(reporthttp.Handler(func(_ context.Context, q report.Query) (report.Report, error) {
		calls.Add(1)
		r := commandReport()
		r.Timezone = q.Timezone
		return r, nil
	}))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	for _, zone := range []string{"", "Local", "local", "GMT", "CET", "Japan", "EST5EDT", "+05:00", "UTC+5", "EST5EDT,M3.2.0,M11.1.0", "/Europe/Berlin", "Europe/Berlin/", "Europe//Berlin", "Europe/./Berlin", "Europe/../Berlin", `Europe\Berlin`, "Europe/Ber lin", "Europe/Berlín", "Europe/Berlin\x00", "NoSuch/Zone", ":Europe/Berlin"} {
		options := reportcli.Options{Endpoint: server.URL, Timezone: &zone, Query: report.Query{Surface: "openai", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
		var out bytes.Buffer
		if err := reportcli.Run(context.Background(), client, options, &out); err == nil || out.Len() != 0 {
			t.Errorf("invalid zone %q: %v stdout=%s", zone, err, out.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid named zones sent %d requests", calls.Load())
	}
	for _, zone := range []string{"UTC", "US/Eastern", "Etc/UTC", "Etc/GMT+5", "Europe/Berlin"} {
		options := reportcli.Options{Endpoint: server.URL, Timezone: &zone, Query: report.Query{Surface: "openai", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
		var out bytes.Buffer
		if err := reportcli.Run(context.Background(), client, options, &out); err != nil || !strings.Contains(out.String(), zone) {
			t.Errorf("accepted name %q: %v stdout=%s", zone, err, out.String())
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("accepted names sent %d requests", calls.Load())
	}
}

func TestCommandRejectsInvalidSelectionsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { calls.Add(1); return commandReport(), nil }))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	base := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Period: "day", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
	for _, change := range []func(*reportcli.Options){func(o *reportcli.Options) { empty := ""; o.Timezone = &empty }, func(o *reportcli.Options) { v := ""; o.Query.Model = &v }, func(o *reportcli.Options) { v := "invalid\xff"; o.Query.Model = &v }, func(o *reportcli.Options) { o.Timeout = 0 }} {
		options := base
		change(&options)
		if err := reportcli.Run(context.Background(), client, options, io.Discard); err == nil {
			t.Errorf("invalid selection succeeded: %+v", options)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid local selections made %d HTTP requests", calls.Load())
	}
}

func TestCommandRendersAnthropicNativeCoverageWithoutPeriodAnnotations(t *testing.T) {
	result := commandReport()
	result.Surface = "all"
	result.Buckets[0].RangePartial, result.Buckets[0].InProgress = true, true
	first := report.Total{Turns: 2, Usage: map[string]report.Metric{
		"input_tokens": {Sum: number(12), ReportedTurns: 2}, "output_tokens": {Sum: number(9), ReportedTurns: 2},
		"cache_creation_input_tokens": {Sum: number(2000), ReportedTurns: 1}, "cache_read_input_tokens": {Sum: number(0), ReportedTurns: 1},
		"ephemeral_5m_input_tokens": {}, "ephemeral_1h_input_tokens": {}, "thinking_tokens": {},
	}}
	second := report.Total{Turns: 1, Usage: map[string]report.Metric{
		"input_tokens": {Sum: number(9007199254740993), ReportedTurns: 1}, "output_tokens": {Sum: number(3), ReportedTurns: 1},
		"cache_creation_input_tokens": {}, "cache_read_input_tokens": {}, "ephemeral_5m_input_tokens": {}, "ephemeral_1h_input_tokens": {}, "thinking_tokens": {},
	}}
	total := report.Total{Turns: 3, Usage: map[string]report.Metric{
		"input_tokens": {Sum: number(9007199254741005), ReportedTurns: 3}, "output_tokens": {Sum: number(12), ReportedTurns: 3},
		"cache_creation_input_tokens": {Sum: number(2000), ReportedTurns: 1}, "cache_read_input_tokens": {Sum: number(0), ReportedTurns: 1},
		"ephemeral_5m_input_tokens": {}, "ephemeral_1h_input_tokens": {}, "thinking_tokens": {},
	}}
	a, z := report.ModelTotal{Model: "a\x1b\n\u202e", Total: first}, report.ModelTotal{Model: "z", Total: second}
	result.Anthropic = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: a}, {BucketStart: "2026-09-01", ModelTotal: z}}, Models: []report.ModelTotal{a, z}, Total: total}
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return result, nil }))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	var out bytes.Buffer
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "all", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	anthropic, openai, found := strings.Cut(text, " OpenAI \n")
	if !found || !strings.Contains(anthropic, " Anthropic \n") {
		t.Fatalf("section ordering: %s", text)
	}
	for _, want := range []string{"Day", "Uncached input", "Output", "Cache create", "Cache read", "2,000*", "0*", "—", "cache create: 1/2 stored Turns", "cache read: 1/2 stored Turns", "9,007,199,254,740,993", `a\x1b\n\u202e`, "╭", "╯"} {
		if !strings.Contains(anthropic, want) {
			t.Errorf("missing %q: %s", want, anthropic)
		}
	}
	for _, want := range [][]string{
		{"2026-09-01", `a\x1b\n\u202e`, "2", "12", "9", "2,000*", "0*"},
		{"", "z", "1", "9,007,199,254,740,993", "3", "—", "—"},
	} {
		if !hasTableRow(anthropic, want...) {
			t.Errorf("missing grouped row %q: %s", want, anthropic)
		}
	}
	if strings.Count(anthropic, "\n├") != 1 {
		t.Fatalf("models within one period were separated: %s", anthropic)
	}
	assertTextExcludes(t, text, "\x1b", "\u202e", "Grand total", `├─ "`, `└─ "`, "│ All ", "│ Period ", "[clipped]", "[in progress]")
	assertTextExcludes(t, anthropic, `"a\x1b\n\u202e"`, `"z"`, "Cache write")
	assertTextExcludes(t, openai, "Uncached input")
	if !strings.Contains(openai, "6,000*") {
		t.Fatal("OpenAI section regressed")
	}
}

func TestCommandRendersServerValuesSafelyAndReturnsOutputFailures(t *testing.T) {
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return commandReport(), nil }))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	utc := "UTC"
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Period: "day", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
	var out bytes.Buffer
	if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{server.URL, "Timezone: UTC", "2026-09-01", "OpenAI", "9,007,199,254,740,993", "6,000*", "cache read: 1/2 stored Turns", "—", `evil\x1b[31m\n\u202e`, "╭", "╯"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	assertTextExcludes(t, text, "\x1b", "\u202e", `Timezone: "UTC"`, `"evil\x1b[31m\n\u202e"`, `├─ "`, `└─ "`, "│ All ", "Model totals", "Section total", "│ Range", "Persisted successful Turns observed by the Usage meter", "Optional-count coverage refers only to stored Turns")
	if err := reportcli.Run(context.Background(), client, options, brokenOutput{}); err == nil {
		t.Fatal("output failure succeeded")
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
