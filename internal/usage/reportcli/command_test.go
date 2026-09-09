package reportcli_test

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func number(n int64) *int64 { return &n }

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

func commandReport() report.Report {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	counts := map[string]report.Metric{"input_tokens": {Sum: number(9007199254740993), ReportedTurns: 2}, "output_tokens": {Sum: number(12), ReportedTurns: 2}, "cached_tokens": {Sum: number(6000), ReportedTurns: 1}, "cache_write_tokens": {}, "reasoning_tokens": {}, "total_tokens": {}}
	total := report.Total{Turns: 2, Usage: counts}
	model := report.ModelTotal{Model: "evil\x1b[31m\n\u202e", Total: total}
	return report.Report{SchemaVersion: 1, GeneratedAt: start, Timezone: "UTC", Period: "day", Since: "2026-09-01", Until: "2026-09-02", WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Surface: "openai", Buckets: []report.Bucket{{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: start, RangeEnd: end}}, OpenAI: &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: model}}, Periods: []report.PeriodTotal{{BucketStart: "2026-09-01", Total: total}}, Models: []report.ModelTotal{model}, Total: total}}
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
	result.Anthropic = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: a}, {BucketStart: "2026-09-01", ModelTotal: z}}, Periods: []report.PeriodTotal{{BucketStart: "2026-09-01", Total: total}}, Models: []report.ModelTotal{a, z}, Total: total}
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
	anthropic, openai, found := strings.Cut(text, "OpenAI\n")
	if !found || !strings.Contains(anthropic, "Anthropic\n") {
		t.Fatalf("section ordering: %s", text)
	}
	for _, want := range []string{"Uncached input", "Output", "Cache create", "Cache read", "2,000*", "0*", "—", "cache create: 1/2 stored Turns", "cache read: 1/2 stored Turns", "cache read: 1/3 stored Turns", "9,007,199,254,741,005", `"a\x1b\n\u202e"`, "╭", "╯"} {
		if !strings.Contains(anthropic, want) {
			t.Errorf("missing %q: %s", want, anthropic)
		}
	}
	for _, want := range [][]string{
		{"2026-09-01", "All", "3", "9,007,199,254,741,005", "12", "2,000*", "0*"},
		{"", `├─ "a\x1b\n\u202e"`, "2", "12", "9", "2,000*", "0*"},
		{"", `└─ "z"`, "1", "9,007,199,254,740,993", "3", "—", "—"},
	} {
		if !hasTableRow(anthropic, want...) {
			t.Errorf("missing hierarchical row %q: %s", want, anthropic)
		}
	}
	if strings.ContainsAny(text, "\x1b\u202e") || strings.Contains(anthropic, "Cache write") || strings.Contains(openai, "Uncached input") || strings.Contains(text, "Grand total") || strings.Contains(text, "[clipped]") || strings.Contains(text, "[in progress]") {
		t.Fatalf("unsafe or cross-Surface presentation: %s", text)
	}
	if !strings.Contains(openai, "6,000*") || !strings.Contains(openai, "Persisted successful Turns") {
		t.Fatal("OpenAI/caveat regressed")
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
	for _, want := range []string{server.URL, "UTC", "2026-09-01", "OpenAI", "All", "└─", "9,007,199,254,740,993", "6,000*", "cache read: 1/2 stored Turns", "—", `"evil\x1b[31m\n\u202e"`, "Persisted successful Turns observed by the Usage meter; best-effort", "╭", "╯"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\u202e") || strings.Contains(text, "Model totals") || strings.Contains(text, "Section total") || strings.Contains(text, "│ Range") {
		t.Fatal("unsafe identity or range totals")
	}
	if err := reportcli.Run(context.Background(), client, options, brokenOutput{}); err == nil {
		t.Fatal("output failure succeeded")
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
