package report_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestQueryMonthsIncludeLeapDayAndEmptyFutureMetadata(t *testing.T) {
	path := stored(t, turn("2024-02-29T23:59:59Z", "m", usage.OpenAIUsage{InputTokens: 17}), turn("2024-03-01T00:00:00Z", "m", usage.OpenAIUsage{InputTokens: 19}))
	got, err := report.New(path).Query(context.Background(), report.Query{Period: "month", Timezone: "UTC", Since: "2024-02-29", Until: "2024-04-02", Surface: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{
		{StartDate: "2024-02-01", UntilDate: "2024-03-01", RangeStart: instant("2024-02-29T00:00:00Z"), RangeEnd: instant("2024-03-01T00:00:00Z"), RangePartial: true},
		{StartDate: "2024-03-01", UntilDate: "2024-04-01", RangeStart: instant("2024-03-01T00:00:00Z"), RangeEnd: instant("2024-04-01T00:00:00Z")},
		{StartDate: "2024-04-01", UntilDate: "2024-05-01", RangeStart: instant("2024-04-01T00:00:00Z"), RangeEnd: instant("2024-04-02T00:00:00Z"), RangePartial: true},
	}
	if !reflect.DeepEqual(got.Buckets, want) || len(got.OpenAI.Rows) != 2 || got.OpenAI.Rows[0].BucketStart != "2024-02-01" || got.OpenAI.Rows[1].BucketStart != "2024-03-01" || *got.OpenAI.Total.Usage["input_tokens"].Sum != 36 {
		t.Fatalf("month report: %+v %+v", got, got.OpenAI)
	}
	future, err := report.New(path).Query(context.Background(), report.Query{Period: "month", Timezone: "UTC", Since: "9998-12-01", Until: "9999-01-01"})
	if err != nil || len(future.Buckets) != 1 || future.Buckets[0].UntilDate != "9999-01-01" || future.Buckets[0].InProgress || len(future.OpenAI.Rows) != 0 || len(future.Anthropic.Rows) != 0 {
		t.Fatalf("future report: %+v %v", future, err)
	}
}

func TestQueryYearsRetainJanuaryLabelsAtFourDigitBoundary(t *testing.T) {
	path := stored(t, turn("9998-12-31T23:59:59Z", "m", usage.OpenAIUsage{InputTokens: 23}))
	got, err := report.New(path).Query(context.Background(), report.Query{Period: "year", Timezone: "UTC", Since: "9997-12-31", Until: "9999-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{
		{StartDate: "9997-01-01", UntilDate: "9998-01-01", RangeStart: instant("9997-12-31T00:00:00Z"), RangeEnd: instant("9998-01-01T00:00:00Z"), RangePartial: true},
		{StartDate: "9998-01-01", UntilDate: "9999-01-01", RangeStart: instant("9998-01-01T00:00:00Z"), RangeEnd: instant("9999-01-01T00:00:00Z")},
	}
	if !reflect.DeepEqual(got.Buckets, want) || len(got.OpenAI.Rows) != 1 || got.OpenAI.Rows[0].BucketStart != "9998-01-01" || *got.OpenAI.Total.Usage["input_tokens"].Sum != 23 {
		t.Fatalf("year report: %+v %+v", got, got.OpenAI)
	}
}

func TestQueryGooseBayReversalUsesEarliestUTCIntervalMembership(t *testing.T) {
	path := stored(t,
		turn("1988-10-30T01:59:59Z", "before", usage.OpenAIUsage{InputTokens: 1}),
		turn("1988-10-30T02:00:00Z", "m", usage.OpenAIUsage{InputTokens: 7}),
		turn("1988-10-30T02:01:00Z", "m", usage.OpenAIUsage{InputTokens: 11}),
		turn("1988-10-30T04:00:00Z", "m", usage.OpenAIUsage{InputTokens: 13}),
		turn("1988-10-31T04:00:00Z", "after", usage.OpenAIUsage{InputTokens: 999}))
	q := report.Query{Timezone: "America/Goose_Bay", Since: "1988-10-30", Until: "1988-10-31", Surface: "openai"}
	got, err := report.New(path).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{{StartDate: "1988-10-30", UntilDate: "1988-10-31", RangeStart: instant("1988-10-30T02:00:00Z"), RangeEnd: instant("1988-10-31T04:00:00Z")}}
	if !reflect.DeepEqual(got.Buckets, want) || got.OpenAI.Total.Turns != 3 || len(got.OpenAI.Rows) != 1 || got.OpenAI.Rows[0].BucketStart != "1988-10-30" || *got.OpenAI.Rows[0].Usage["input_tokens"].Sum != 31 {
		t.Fatalf("reversal membership: %+v %+v", got, got.OpenAI)
	}
	q.Since, q.Until = "1988-10-29", "1988-10-30"
	got, err = report.New(path).Query(context.Background(), q)
	if err != nil || !got.WindowEnd.Equal(instant("1988-10-30T02:00:00Z")) || got.OpenAI.Total.Turns != 1 || got.OpenAI.Rows[0].Model != "before" {
		t.Fatalf("exclusive end must exclude returned October29 clocks: %+v %v", got, err)
	}
}

func TestQuerySkippedDateRejectsExplicitBoundsButOmitsCoincidentDailyBucket(t *testing.T) {
	path := stored(t, turn("2011-12-30T09:59:59Z", "m", usage.OpenAIUsage{InputTokens: 7}), turn("2011-12-30T10:00:00Z", "m", usage.OpenAIUsage{InputTokens: 11}))
	q := report.Query{Timezone: "Pacific/Apia", Since: "2011-12-29", Until: "2012-01-01"}
	got, err := report.New(path).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{
		{StartDate: "2011-12-29", UntilDate: "2011-12-30", RangeStart: instant("2011-12-29T10:00:00Z"), RangeEnd: instant("2011-12-30T10:00:00Z")},
		{StartDate: "2011-12-31", UntilDate: "2012-01-01", RangeStart: instant("2011-12-30T10:00:00Z"), RangeEnd: instant("2011-12-31T10:00:00Z")},
	}
	if !reflect.DeepEqual(got.Buckets, want) || len(got.OpenAI.Rows) != 2 || got.OpenAI.Rows[0].BucketStart != "2011-12-29" || got.OpenAI.Rows[1].BucketStart != "2011-12-31" {
		t.Fatalf("skipped internal date: %+v %+v", got, got.OpenAI)
	}
	missing := filepath.Join(t.TempDir(), "absent", "usage.db")
	for _, q := range []report.Query{{Timezone: "Pacific/Apia", Since: "2011-12-30", Until: "2012-01-01"}, {Timezone: "Pacific/Apia", Since: "2011-12-29", Until: "2011-12-30"}} {
		_, err := report.New(missing).Query(context.Background(), q)
		var e *report.Error
		if !errors.As(err, &e) || e.Code != report.InvalidQuery {
			t.Fatalf("skipped explicit bound: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("semantic failure touched missing DB: %v", err)
	}
}

func TestQueryFutureNamedZoneRetainsRulesBeyondExplicitTZifTransitions(t *testing.T) {
	got, err := report.New(stored(t)).Query(context.Background(), report.Query{Period: "year", Timezone: "Europe/Berlin", Since: "9998-12-31", Until: "9999-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{{StartDate: "9998-01-01", UntilDate: "9999-01-01", RangeStart: instant("9998-12-30T23:00:00Z"), RangeEnd: instant("9998-12-31T23:00:00Z"), RangePartial: true}}
	if !reflect.DeepEqual(got.Buckets, want) || got.OpenAI.Total.Turns != 0 {
		t.Fatalf("future zone rules: %+v", got)
	}
}

// Regression evidence: the transition-based tracer already supports these.
func TestQueryDateStartsAcrossDSTAndMidnightChanges(t *testing.T) {
	for _, tc := range []struct{ zone, since, until, start, end string }{
		{"America/New_York", "2024-03-10", "2024-03-11", "2024-03-10T05:00:00Z", "2024-03-11T04:00:00Z"},
		{"America/New_York", "2024-11-03", "2024-11-04", "2024-11-03T04:00:00Z", "2024-11-04T05:00:00Z"},
		{"America/Sao_Paulo", "2018-11-04", "2018-11-05", "2018-11-04T03:00:00Z", "2018-11-05T02:00:00Z"},
		{"America/Havana", "2020-11-01", "2020-11-02", "2020-11-01T04:00:00Z", "2020-11-02T05:00:00Z"},
	} {
		t.Run(tc.zone+tc.since, func(t *testing.T) {
			path := stored(t, turn(tc.start, "m", usage.OpenAIUsage{InputTokens: 7}), turn(tc.end, "excluded", usage.OpenAIUsage{InputTokens: 999}))
			got, err := report.New(path).Query(context.Background(), report.Query{Timezone: tc.zone, Since: tc.since, Until: tc.until})
			if err != nil || len(got.Buckets) != 1 || !got.WindowStart.Equal(instant(tc.start)) || !got.WindowEnd.Equal(instant(tc.end)) || got.OpenAI.Total.Turns != 1 || *got.OpenAI.Total.Usage["input_tokens"].Sum != 7 {
				t.Fatalf("transition interval: %+v %v", got, err)
			}
		})
	}
}

// Regression evidence for the shared grammar/loading policy through Query.
func TestQueryValidatesNamedTimezoneBeforeOpeningMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "usage.db")
	for _, zone := range []string{"", "Local", "local", "GMT", "CET", "Japan", "EST5EDT", "+05:00", "UTC+5", "EST5EDT,M3.2.0,M11.1.0", "/Europe/Berlin", "Europe/Berlin/", "Europe//Berlin", "Europe/./Berlin", "Europe/../Berlin", `Europe\Berlin`, "Europe/Ber lin", "Europe/Berlín", "Europe/Berlin\x00", "NoSuch/Zone", ":Europe/Berlin"} {
		_, err := report.New(path).Query(context.Background(), report.Query{Timezone: zone, Since: "2024-01-01", Until: "2024-01-02"})
		var failure *report.Error
		if !errors.As(err, &failure) || failure.Code != report.InvalidQuery {
			t.Errorf("zone %q: %v", zone, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("invalid zone touched missing DB: %v", err)
	}
	path = stored(t)
	for _, tc := range []struct{ zone, start string }{{"UTC", "2024-01-01T00:00:00Z"}, {"US/Eastern", "2024-01-01T05:00:00Z"}, {"Etc/UTC", "2024-01-01T00:00:00Z"}, {"Etc/GMT+5", "2024-01-01T05:00:00Z"}, {"Europe/Berlin", "2023-12-31T23:00:00Z"}} {
		got, err := report.New(path).Query(context.Background(), report.Query{Timezone: tc.zone, Since: "2024-01-01", Until: "2024-01-02"})
		if err != nil || got.Timezone != tc.zone || got.WindowStart.Format(time.RFC3339) != tc.start {
			t.Errorf("named zone %s: %+v %v", tc.zone, got, err)
		}
	}
}

func TestQueryCalendarLimitsRemainBoundedForEveryPeriodAndNamedZone(t *testing.T) {
	path := stored(t)
	missing := filepath.Join(t.TempDir(), "absent", "usage.db")
	for _, zone := range []string{"UTC", "Europe/Berlin", "US/Eastern", "Pacific/Apia"} {
		for _, tc := range []struct {
			period  string
			buckets int
		}{{"day", 3660}, {"week", 524}, {"month", 121}, {"year", 11}} {
			q := report.Query{Timezone: zone, Period: tc.period, Since: "1970-01-01", Until: "1980-01-09"}
			got, err := report.New(path).Query(context.Background(), q)
			if err != nil || len(got.Buckets) != tc.buckets || len(got.Buckets) > report.MaxBuckets || len(got.OpenAI.Rows) != 0 || len(got.Anthropic.Rows) != 0 {
				t.Fatalf("exact date budget %s/%s: buckets=%d %v", zone, tc.period, len(got.Buckets), err)
			}
			for _, until := range []string{"1980-01-10", "9999-01-01"} {
				q.Until = until
				got, err := report.New(missing).Query(context.Background(), q)
				var failure *report.Error
				if !errors.As(err, &failure) || failure.Code != report.TooLarge || got.OpenAI != nil {
					t.Fatalf("excessive nominal dates %s/%s/%s: %v", zone, tc.period, until, err)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatalf("excessive calendar opened missing database: %v", err)
	}
}

func instant(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestQueryWeeksUseMondayLabelsAcrossYearsAndClipBothEnds(t *testing.T) {
	path := stored(t,
		turn("2020-12-31T00:00:00Z", "m", usage.OpenAIUsage{InputTokens: 7}),
		turn("2021-01-03T23:59:59Z", "m", usage.OpenAIUsage{InputTokens: 11}),
		turn("2021-01-04T00:00:00Z", "m", usage.OpenAIUsage{InputTokens: 13}),
		turn("2021-01-05T00:00:00Z", "excluded", usage.OpenAIUsage{InputTokens: 999}))
	got, err := report.New(path).Query(context.Background(), report.Query{Period: "week", Timezone: "UTC", Since: "2020-12-31", Until: "2021-01-05", Surface: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	want := []report.Bucket{
		{StartDate: "2020-12-28", UntilDate: "2021-01-04", RangeStart: instant("2020-12-31T00:00:00Z"), RangeEnd: instant("2021-01-04T00:00:00Z"), RangePartial: true},
		{StartDate: "2021-01-04", UntilDate: "2021-01-11", RangeStart: instant("2021-01-04T00:00:00Z"), RangeEnd: instant("2021-01-05T00:00:00Z"), RangePartial: true},
	}
	if !reflect.DeepEqual(got.Buckets, want) {
		t.Fatalf("buckets: %+v", got.Buckets)
	}
	s := got.OpenAI
	if len(s.Rows) != 2 || s.Rows[0].BucketStart != "2020-12-28" || s.Rows[0].Turns != 2 || *s.Rows[0].Usage["input_tokens"].Sum != 18 || s.Rows[1].BucketStart != "2021-01-04" || *s.Rows[1].Usage["input_tokens"].Sum != 13 || s.Total.Turns != 3 || *s.Total.Usage["input_tokens"].Sum != 31 {
		t.Fatalf("weekly counts: %+v", s)
	}
}
