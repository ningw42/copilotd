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
func commandReport() report.Report {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	counts := map[string]report.Metric{"input_tokens": {Sum: number(9007199254740993), ReportedTurns: 2}, "output_tokens": {Sum: number(12), ReportedTurns: 2}, "cached_tokens": {Sum: number(6000), ReportedTurns: 1}, "cache_write_tokens": {}, "reasoning_tokens": {}, "total_tokens": {}}
	total := report.Total{Turns: 2, Usage: counts}
	model := report.ModelTotal{Model: "evil\x1b[31m\n\u202e", Total: total}
	return report.Report{SchemaVersion: 1, GeneratedAt: start, Timezone: "UTC", Period: "day", Since: "2026-09-01", Until: "2026-09-02", WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Surface: "openai", Buckets: []report.Bucket{{StartDate: "2026-09-01", UntilDate: "2026-09-02", RangeStart: start, RangeEnd: end}}, OpenAI: &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: model}}, Models: []report.ModelTotal{model}, Total: total}}
}
func TestCommandRejectsUnfinishedSelectionsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { calls.Add(1); return commandReport(), nil }))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	base := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Period: "day", Since: "2026-09-01", Until: "2026-09-02"}, Timeout: time.Second}
	for _, change := range []func(*reportcli.Options){func(o *reportcli.Options) { o.Timezone = nil }, func(o *reportcli.Options) { empty := ""; o.Timezone = &empty }, func(o *reportcli.Options) { o.Details = true }, func(o *reportcli.Options) { o.JSON = true }, func(o *reportcli.Options) { v := "x"; o.Query.Model = &v }} {
		options := base
		change(&options)
		if err := reportcli.Run(context.Background(), client, options, io.Discard); err == nil {
			t.Errorf("unfinished selection succeeded: %+v", options)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid local selections made %d HTTP requests", calls.Load())
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
	for _, want := range []string{server.URL, "UTC", "2026-09-01", "OpenAI", "Model totals", "Section total", "9,007,199,254,740,993", "6,000*", "cache read: 1/2 stored Turns", "—", `"evil\x1b[31m\n\u202e"`, "Persisted successful Turns observed by the Usage meter; best-effort"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\u202e") {
		t.Fatal("unsafe terminal identity")
	}
	if err := reportcli.Run(context.Background(), client, options, brokenOutput{}); err == nil {
		t.Fatal("output failure succeeded")
	}
}

type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
