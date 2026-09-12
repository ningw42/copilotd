package report

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func calendarReader(t *testing.T, now string, turns ...usage.Turn) *Reporter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		store.Record(turn)
	}
	if result := store.Close(context.Background()); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("fixture: %+v", result)
	}
	r := newReporterForTest(path)
	at, err := time.Parse(time.RFC3339, now)
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { captured := at; at = at.AddDate(0, 1, 0); return captured }
	return r
}

// Regression evidence: flags use the captured UTC interval, not local date.
func TestQueryProgressUsesUnclippedUTCIntervalEvenWhenClockReturnsYesterday(t *testing.T) {
	for _, tc := range []struct {
		now, since, until, period string
		want                      bool
	}{
		{"1988-10-30T02:00:00Z", "1988-10-30", "1988-10-31", "day", true},
		{"1988-10-30T02:30:00Z", "1988-10-30", "1988-10-31", "day", true},
		{"1988-10-30T01:59:59Z", "1988-10-30", "1988-10-31", "day", false},
		{"1988-10-31T04:00:00Z", "1988-10-30", "1988-10-31", "day", false},
		{"2024-03-15T12:00:00Z", "2024-03-01", "2024-03-02", "month", true},
	} {
		got, err := calendarReader(t, tc.now).Query(context.Background(), Query{Timezone: "America/Goose_Bay", Since: tc.since, Until: tc.until, Period: tc.period})
		if err != nil || len(got.Buckets) != 1 || got.Buckets[0].InProgress != tc.want {
			t.Fatalf("progress at %s: %+v %v", tc.now, got, err)
		}
	}
}

func TestQueryDefaultsEachBoundFromOneCapturedRequestedZoneMonth(t *testing.T) {
	previous := time.Local
	time.Local = time.FixedZone("daemon", -8*3600)
	t.Cleanup(func() { time.Local = previous })
	for _, period := range []string{"day", "week", "month", "year"} {
		t.Run(period, func(t *testing.T) {
			for _, tc := range []struct {
				since, until, wantSince, wantUntil string
				invalid                            bool
			}{
				{"", "", "2024-03-01", "2024-04-01", false},
				{"2024-02-29", "", "2024-02-29", "2024-04-01", false},
				{"", "2024-03-03", "2024-03-01", "2024-03-03", false},
				{"", "2024-02-01", "", "", true},
				{"2024-05-01", "", "", "", true},
			} {
				r := calendarReader(t, "2024-02-29T23:30:00Z") // Berlin is already in March; daemon is not.
				got, err := r.Query(context.Background(), Query{Period: period, Timezone: "Europe/Berlin", Since: tc.since, Until: tc.until})
				if tc.invalid {
					var e *Error
					if !errors.As(err, &e) || e.Code != InvalidQuery {
						t.Fatalf("independent defaults must not infer range: %+v %v", got, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.Since != tc.wantSince || got.Until != tc.wantUntil || got.GeneratedAt.Format(time.RFC3339) != "2024-02-29T23:30:00Z" {
					t.Fatalf("defaults: %+v", got)
				}
				if tc.since == "" && tc.until == "" {
					if got.WindowStart.Format(time.RFC3339) != "2024-02-29T23:00:00Z" || got.WindowEnd.Format(time.RFC3339) != "2024-03-31T22:00:00Z" {
						t.Fatalf("month window: %+v", got)
					}
					progress := 0
					for _, b := range got.Buckets {
						if b.InProgress {
							progress++
						}
					}
					if progress != 1 {
						t.Fatalf("captured progress: %+v", got.Buckets)
					}
				}
			}
		})
	}
}
