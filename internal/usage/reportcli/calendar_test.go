package reportcli_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestCommandAllPeriodsFromSQLiteThroughHTTPInExplicitNamedZone(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("daemon", 9*3600)
	t.Cleanup(func() { time.Local = previous })
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at    string
		input int64
	}{{"2020-12-30T22:59:59Z", 999}, {"2020-12-30T23:00:00Z", 7}, {"2020-12-31T00:30:00Z", 11}, {"2021-01-03T23:00:00Z", 13}, {"2021-01-04T23:00:00Z", 999}} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		for _, counts := range []usage.Usage{usage.AnthropicUsage{InputTokens: tc.input, OutputTokens: 1}, usage.OpenAIUsage{InputTokens: tc.input, OutputTokens: 1}} {
			store.Record(usage.Turn{At: at, Model: "m", Transport: usage.TransportBuffered, Usage: counts})
		}
	}
	if result := store.Close(context.Background()); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("fixture: %+v", result)
	}
	server := httptest.NewServer(reporthttp.Handler(report.New(path).Query))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	zone := "Europe/Berlin"
	for _, tc := range []struct {
		period, first, second string
		rows                  int
	}{{"day", "2020-12-31", "2021-01-04", 5}, {"week", "2020-12-28", "2021-01-04", 2}, {"month", "2020-12-01", "2021-01-01", 2}, {"year", "2020-01-01", "2021-01-01", 2}} {
		t.Run(tc.period, func(t *testing.T) {
			q := report.Query{Period: tc.period, Timezone: zone, Since: "2020-12-31", Until: "2021-01-05"}
			var out bytes.Buffer
			if err := reportcli.Run(context.Background(), client, reportcli.Options{Endpoint: server.URL, Query: q, Timezone: &zone, Timeout: time.Second * 5}, &out); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			for _, want := range []string{`Timezone: "Europe/Berlin"`, "Range: 2020-12-31 to 2021-01-05 (exclusive)", "Period: " + tc.period, "Anthropic\n", "OpenAI\n", tc.first, tc.second, "Section total", "Persisted successful Turns"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			// Literal native counts reach the terminal, not just the separate
			// wire assertions below: two period rows per Surface and both kinds
			// of range total, without renderer-side regrouping.
			if strings.Count(text, "  18 ") != 2 || strings.Count(text, "  13 ") != 2 || strings.Count(text, "  31 ") != 4 {
				t.Fatalf("period and range counts did not reach terminal: %s", text)
			}
			if strings.Contains(text, "[clipped]") || strings.Contains(text, "[in progress]") {
				t.Fatalf("rendered period annotations: %s", text)
			}
			result, err := client.Query(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			r := result.Report
			if len(r.Buckets) != tc.rows || r.WindowStart.Format(time.RFC3339) != "2020-12-30T23:00:00Z" || r.WindowEnd.Format(time.RFC3339) != "2021-01-04T23:00:00Z" {
				t.Fatalf("calendar: %+v", r)
			}
			for _, s := range []*report.Section{r.Anthropic, r.OpenAI} {
				if len(s.Rows) != 2 || s.Rows[0].BucketStart != tc.first || s.Rows[1].BucketStart != tc.second || s.Rows[0].Turns != 2 || *s.Rows[0].Usage["input_tokens"].Sum != 18 || s.Total.Turns != 3 || *s.Total.Usage["input_tokens"].Sum != 31 || s.Models[0].Turns != 3 {
					t.Fatalf("shared interval native aggregates: %+v", s)
				}
			}
		})
	}
}
