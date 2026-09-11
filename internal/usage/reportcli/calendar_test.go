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
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

type calendarPricingSource struct {
	snapshot *pricing.Snapshot
}

func (s calendarPricingSource) Current(ctx context.Context, limit pricing.ProjectionLimit) (*pricing.Snapshot, pricing.SnapshotStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if s.snapshot.IdentityBytes() > limit.MaxIdentityBytes {
		return nil, pricing.SnapshotStatus{}, pricing.ErrProjectionLimit
	}
	return s.snapshot, pricing.SnapshotStatus{Source: "fallback", Version: "sha256:empty-test-prices"}, nil
}

func calendarPricing(t *testing.T) pricing.Source {
	t.Helper()
	snapshot, err := pricing.ParseSnapshot(context.Background(), []byte(`{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return calendarPricingSource{snapshot: snapshot}
}

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
	server := httptest.NewServer(reporthttp.Handler(report.New(path, calendarPricing(t)).Query))
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
			heading := strings.ToUpper(tc.period[:1]) + tc.period[1:]
			for _, want := range []string{"Timezone: Europe/Berlin", "Range: 2020-12-31 to 2021-01-05 (exclusive)", "Period: " + tc.period, " Anthropic \n", " OpenAI \n", tc.first, tc.second, "m"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
			// Each native table derives a period Total before the model breakdown
			// without rendering the whole-range total as another row.
			if strings.Count(text, "  18 ") != 4 || strings.Count(text, "  13 ") != 4 || strings.Count(text, "│ Total") != 4 {
				t.Fatalf("period totals and model groups did not reach terminal: %s", text)
			}
			assertTextExcludes(t, text, "  31 ", `"m"`, `├─ "`, `└─ "`, "│ All ", "Section total", "│ Range")
			if strings.Count(text, "\n│ "+heading) != 2 {
				t.Fatalf("period-specific headings missing: %s", text)
			}
			if strings.Count(text, "\n├") != 8 {
				t.Fatalf("missing total and period separators: %s", text)
			}
			assertTextExcludes(t, text, "[clipped]", "[in progress]")
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
