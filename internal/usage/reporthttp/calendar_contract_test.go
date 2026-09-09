package reporthttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func reportBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(emptyJSON), &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func periodReportBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	return reportBody(t, func(root map[string]any) {
		section := root["openai"].(map[string]any)
		total := section["total"].(map[string]any)
		total["turns"] = "1"
		usage := total["usage"].(map[string]any)
		for _, name := range []string{"input_tokens", "output_tokens"} {
			usage[name].(map[string]any)["reported_turns"] = "1"
		}
		section["rows"] = []any{map[string]any{"bucket_start": "2026-09-01", "model": "example", "turns": "1", "usage": usage}}
		section["periods"] = []any{map[string]any{"bucket_start": "2026-09-01", "turns": "1", "usage": usage}}
		section["models"] = []any{map[string]any{"model": "example", "turns": "1", "usage": usage}}
		if mutate != nil {
			mutate(section)
		}
	})
}

func runPresentations(t *testing.T, body string, query report.Query, valid bool) {
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
	zone := query.Timezone
	if zone == "" {
		zone = "UTC"
	}
	for _, jsonMode := range []bool{false, true} {
		name := "text"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := reportcli.Run(context.Background(), client, reportcli.Options{
				Endpoint: server.URL,
				Timezone: &zone,
				Query:    query,
				JSON:     jsonMode,
				Timeout:  time.Second,
			}, &output)
			if valid && err != nil {
				t.Fatalf("valid report rejected: %v", err)
			}
			if !valid && err == nil {
				t.Fatal("invalid report accepted")
			}
			if !valid && output.Len() != 0 {
				t.Fatal("invalid report emitted output")
			}
			if valid && output.Len() == 0 {
				t.Fatal("valid report emitted no output")
			}
			if valid && jsonMode && output.String() != body+"\n" {
				t.Fatal("original JSON bytes changed")
			}
		})
	}
}

func TestClientValidatesOptionalServerPeriodTotals(t *testing.T) {
	runPresentations(t, periodReportBody(t, nil), report.Query{Surface: "openai", Period: "day"}, true)
	runPresentations(t, periodReportBody(t, func(section map[string]any) {
		delete(section, "periods")
	}), report.Query{Surface: "openai", Period: "day"}, true)

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"null periods", func(section map[string]any) { section["periods"] = nil }},
		{"empty periods with rows", func(section map[string]any) { section["periods"] = []any{} }},
		{"unknown bucket", func(section map[string]any) {
			section["periods"].([]any)[0].(map[string]any)["bucket_start"] = "2026-09-02"
		}},
		{"duplicate bucket", func(section map[string]any) {
			period := section["periods"].([]any)[0]
			section["periods"] = append(section["periods"].([]any), period)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runPresentations(t, periodReportBody(t, tc.mutate), report.Query{Surface: "openai", Period: "day"}, false)
		})
	}
}

func TestClientRejectsMalformedInstantSpellingsBeforePresentation(t *testing.T) {
	for _, field := range []string{"generated_at", "window_start", "window_end", "range_start", "range_end"} {
		for _, separator := range []string{"one digit hour", "comma fraction"} {
			t.Run(field+"/"+separator, func(t *testing.T) {
				body := reportBody(t, func(root map[string]any) {
					object := root
					if strings.HasPrefix(field, "range_") {
						object = root["buckets"].([]any)[0].(map[string]any)
					}
					stamp := object[field].(string)
					if separator == "one digit hour" {
						stamp = stamp[:11] + stamp[12:]
					} else {
						stamp = strings.TrimSuffix(stamp, "Z") + ",0Z"
					}
					object[field] = stamp
				})
				runPresentations(t, body, report.Query{Surface: "openai", Period: "day"}, false)
			})
		}
	}
}

func TestClientAcceptsZeroOffsetAndFractionInstantSpellings(t *testing.T) {
	for _, suffix := range []string{"Z", "+00:00", "-00:00", ".0Z", ".1000Z", ".0000000000Z", ".123456789012Z", ".1000+00:00", ".1000-00:00"} {
		t.Run(suffix, func(t *testing.T) {
			body := reportBody(t, func(root map[string]any) {
				root["generated_at"] = "2026-09-07T12:00:00" + suffix
			})
			runPresentations(t, body, report.Query{Surface: "openai", Period: "day"}, true)
		})
	}
}

func setReportRange(root map[string]any, since, until, label, next, start, end, period, zone string) {
	root["since"] = since
	root["until"] = until
	root["window_start"] = start
	root["window_end"] = end
	root["period"] = period
	root["timezone"] = zone
	bucket := root["buckets"].([]any)[0].(map[string]any)
	bucket["start_date"] = label
	bucket["until_date"] = next
	bucket["range_start"] = start
	bucket["range_end"] = end
	bucket["range_partial"] = since != label || until != next
}

func TestClientRejectsOutOfRangeEffectiveSelectedDates(t *testing.T) {
	for _, tc := range []struct {
		name, since, until string
		query              report.Query
	}{
		{"omitted since", "1969-12-31", "1970-01-02", report.Query{Surface: "openai", Until: "1970-01-02"}},
		{"omitted until", "9998-12-31", "9999-01-02", report.Query{Surface: "openai", Since: "9998-12-31"}},
		{"both omitted below", "1969-12-30", "1969-12-31", report.Query{Surface: "openai"}},
		{"both omitted above", "9999-01-02", "9999-01-03", report.Query{Surface: "openai"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := reportBody(t, func(root map[string]any) {
				if tc.name == "both omitted below" {
					setReportRange(root, tc.since, tc.until, "1969-01-01", "1970-01-01", tc.since+"T00:00:00Z", tc.until+"T00:00:00Z", "year", "UTC")
					return
				}
				root["since"] = tc.since
				root["until"] = tc.until
				root["window_start"] = tc.since + "T00:00:00Z"
				root["window_end"] = tc.until + "T00:00:00Z"
				root["buckets"] = []any{}
				first, err := time.Parse(time.DateOnly, tc.since)
				if err != nil {
					t.Fatal(err)
				}
				last, err := time.Parse(time.DateOnly, tc.until)
				if err != nil {
					t.Fatal(err)
				}
				for date := first; date.Before(last); date = date.AddDate(0, 0, 1) {
					next := date.AddDate(0, 0, 1)
					root["buckets"] = append(root["buckets"].([]any), map[string]any{
						"start_date":    date.Format(time.DateOnly),
						"until_date":    next.Format(time.DateOnly),
						"range_start":   date.Format(time.RFC3339),
						"range_end":     next.Format(time.RFC3339),
						"range_partial": false,
						"in_progress":   false,
					})
				}
			})
			runPresentations(t, body, tc.query, false)
		})
	}
}

func TestClientSelectedDateLimitsDoNotConstrainLabelsOrInstants(t *testing.T) {
	for _, tc := range []struct {
		name, since, until, label, next, start, end, period, zone string
	}{
		{"lower weekly label", "1970-01-01", "1970-01-02", "1969-12-29", "1970-01-05", "1970-01-01T00:00:00Z", "1970-01-02T00:00:00Z", "week", "UTC"},
		{"upper weekly label", "9998-12-31", "9999-01-01", "9998-12-28", "9999-01-04", "9998-12-31T00:00:00Z", "9999-01-01T00:00:00Z", "week", "UTC"},
		{"lower local date UTC spillover", "1970-01-01", "1970-01-02", "1970-01-01", "1970-01-02", "1969-12-31T23:00:00Z", "1970-01-01T23:00:00Z", "day", "Europe/Berlin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := reportBody(t, func(root map[string]any) {
				setReportRange(root, tc.since, tc.until, tc.label, tc.next, tc.start, tc.end, tc.period, tc.zone)
				usage := root["openai"].(map[string]any)["total"].(map[string]any)["usage"].(map[string]any)
				for _, name := range []string{"input_tokens", "output_tokens"} {
					usage[name].(map[string]any)["reported_turns"] = "1"
				}
				root["openai"] = map[string]any{
					"rows":    []any{map[string]any{"bucket_start": tc.label, "model": "example", "turns": "1", "usage": usage}},
					"periods": []any{map[string]any{"bucket_start": tc.label, "turns": "1", "usage": usage}},
					"models":  []any{map[string]any{"model": "example", "turns": "1", "usage": usage}},
					"total":   map[string]any{"turns": "1", "usage": usage},
				}
			})
			runPresentations(t, body, report.Query{Surface: "openai", Period: tc.period, Timezone: tc.zone, Since: tc.since, Until: tc.until}, true)
		})
	}
}
