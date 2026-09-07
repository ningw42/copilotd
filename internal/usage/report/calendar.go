package report

import (
	"context"
	"math"
	"strings"
	"time"
)

// LoadTimezone validates a named selection's grammar and loads the rules visible
// to this process. It does not certify operator-provided data as pristine IANA
// data, or force Go's embedded fallback ahead of ZONEINFO/platform data.
func LoadTimezone(name string) (*time.Location, error) {
	if name != "UTC" {
		parts := strings.Split(name, "/")
		if len(parts) < 2 {
			return nil, invalid("Supply a named timezone such as Area/City or UTC.")
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return nil, invalid("Supply a named timezone such as Area/City or UTC.")
			}
			for _, c := range part {
				if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '+' || c == '.') {
					return nil, invalid("Supply a named timezone such as Area/City or UTC.")
				}
			}
		}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, invalid("Timezone is not loadable; supply a known named timezone such as Area/City or UTC.")
	}
	return loc, nil
}

// Nominal dates are represented at UTC midnight only for Gregorian arithmetic;
// they are not actual local instants. No time.Date call with loc chooses a fold.
func calendar(ctx context.Context, q *Query, now time.Time) ([]Bucket, time.Time, time.Time, error) {
	loc, err := LoadTimezone(q.Timezone)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	localNow := now.In(loc)
	month := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	if q.Since == "" {
		q.Since = month.Format(time.DateOnly)
	}
	if q.Until == "" {
		q.Until = month.AddDate(0, 1, 0).Format(time.DateOnly)
	}
	start, err := strictDate(q.Since)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	end, err := strictDate(q.Until)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	if !start.Before(end) {
		return nil, time.Time{}, time.Time{}, invalid("since must precede until.")
	}
	// Date-only UTC epoch seconds stay exact across the supported years;
	// time.Time.Sub would saturate for malicious multi-century ranges.
	if (end.Unix()-start.Unix())/86400 > MaxDates {
		return nil, time.Time{}, time.Time{}, tooLarge()
	}
	edge, days, months := start, 1, 0
	switch q.Period {
	case "week":
		edge = edge.AddDate(0, 0, -(int(edge.Weekday())+6)%7)
		days = 7
	case "month":
		edge = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
		days, months = 0, 1
	case "year":
		edge = time.Date(start.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		days, months = 0, 12
	}
	resolver := dateResolver{ctx: ctx, loc: loc, starts: map[time.Time]time.Time{}, cursor: edge.Add(-time.Duration(math.MaxInt32) * time.Second)}
	lo, err := resolver.start(start)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	hi, err := resolver.start(end)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	if lo.IsZero() || hi.IsZero() {
		return nil, time.Time{}, time.Time{}, invalid("Selected calendar date does not exist in this timezone.")
	}
	// An operator's extreme TZif offset can push otherwise valid nominal
	// dates beyond RFC3339's four-digit UTC year. Reject before any SQL.
	if lo.Year() > 9999 || hi.Year() > 9999 {
		return nil, time.Time{}, time.Time{}, invalid("Timezone places the report window outside supported UTC years.")
	}
	if !lo.Before(hi) {
		return nil, time.Time{}, time.Time{}, invalid("Timezone has non-monotonic calendar edges.")
	}
	buckets := []Bucket{}
	for ; edge.Before(end); edge = edge.AddDate(0, months, days) {
		if err := ctx.Err(); err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		next := edge.AddDate(0, months, days)
		a, err := resolver.edge(edge)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		b, err := resolver.edge(next)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}
		if a.After(b) {
			return nil, time.Time{}, time.Time{}, invalid("Timezone has non-monotonic calendar edges.")
		}
		if a.Equal(b) {
			continue
		}
		clippedA, clippedB := a, b
		if clippedA.Before(lo) {
			clippedA = lo
		}
		if clippedB.After(hi) {
			clippedB = hi
		}
		previous := lo
		if len(buckets) > 0 {
			previous = buckets[len(buckets)-1].RangeEnd
		}
		if !clippedA.Before(clippedB) || !clippedA.Equal(previous) {
			return nil, time.Time{}, time.Time{}, invalid("Timezone has non-monotonic calendar edges.")
		}
		if len(buckets) >= MaxBuckets {
			return nil, time.Time{}, time.Time{}, tooLarge()
		}
		buckets = append(buckets, Bucket{StartDate: edge.Format(time.DateOnly), UntilDate: next.Format(time.DateOnly), RangeStart: clippedA, RangeEnd: clippedB, RangePartial: !a.Equal(clippedA) || !b.Equal(clippedB), InProgress: !now.Before(a) && now.Before(b)})
	}
	if len(buckets) == 0 || !buckets[len(buckets)-1].RangeEnd.Equal(hi) {
		return nil, time.Time{}, time.Time{}, invalid("Timezone has non-monotonic calendar edges.")
	}
	return buckets, lo, hi, nil
}

// TZif offsets are signed 32-bit seconds. Search the entire possible UTC
// preimage of a date, not a guessed +/-24h window or today's offset. Intersect
// each actual constant-offset interval with that preimage, keeping the earliest
// occurrence even if a later transition returns the displayed date backwards.
// Bound interval work as well as checking cancellation: custom TZif need not
// have IANA's sparse, well-behaved transitions.
const maxCalendarSteps = 4000000
const maxZoneIntervals = 16384

type zoneInterval struct {
	start, end time.Time
	offset     int
}

type dateResolver struct {
	ctx       context.Context
	loc       *time.Location
	starts    map[time.Time]time.Time
	steps     int
	cursor    time.Time
	intervals []zoneInterval
}

func (r *dateResolver) edge(date time.Time) (time.Time, error) {
	for skipped := 0; skipped <= MaxDates; skipped++ {
		if err := r.ctx.Err(); err != nil {
			return time.Time{}, err
		}
		start, err := r.start(date)
		if err != nil || !start.IsZero() {
			return start, err
		}
		date = date.AddDate(0, 0, 1)
	}
	return time.Time{}, tooLarge()
}

func (r *dateResolver) start(date time.Time) (time.Time, error) {
	if start, ok := r.starts[date]; ok {
		return start, nil
	}
	next := date.AddDate(0, 0, 1)
	limit := next.Add(-time.Duration(math.MinInt32) * time.Second)
	var earliest time.Time
	for i := 0; ; i++ {
		if err := r.check(); err != nil {
			return time.Time{}, err
		}
		if i == len(r.intervals) {
			if !r.cursor.Before(limit) {
				break
			}
			interval, err := r.nextInterval(limit)
			if err != nil {
				return time.Time{}, err
			}
			if len(r.intervals) >= maxZoneIntervals {
				return time.Time{}, tooLarge()
			}
			r.intervals = append(r.intervals, interval)
			r.cursor = interval.end
		}
		interval := r.intervals[i]
		if !interval.start.Before(limit) {
			break
		}
		a := date.Add(-time.Duration(interval.offset) * time.Second)
		b := next.Add(-time.Duration(interval.offset) * time.Second)
		if a.Before(interval.start) {
			a = interval.start
		}
		if b.After(interval.end) {
			b = interval.end
		}
		if a.Before(b) {
			earliest = a.UTC()
			break
		}
	}
	r.starts[date] = earliest
	return earliest, nil
}

func (r *dateResolver) check() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.steps++
	if r.steps > maxCalendarSteps {
		return tooLarge()
	}
	return nil
}

func (r *dateResolver) nextInterval(limit time.Time) (zoneInterval, error) {
	cursor := r.cursor
	_, offset := cursor.In(r.loc).Zone()
	interval := zoneInterval{start: cursor, offset: offset}
	if offset < math.MinInt32 || offset > math.MaxInt32 {
		return interval, invalid("Unsupported timezone offset.")
	}
	for {
		if err := r.check(); err != nil {
			return interval, err
		}
		local := cursor.In(r.loc)
		_, current := local.Zone()
		if current != offset {
			interval.end = cursor
			return interval, nil
		}
		start, end := local.ZoneBounds()
		if !start.IsZero() && start.After(cursor) {
			return interval, invalid("Timezone has unsupported transition bounds.")
		}
		if end.IsZero() || end.After(limit) {
			end = limit
		}
		if end.After(cursor) {
			interval.end = end
			return interval, nil
		}
		// Go's POSIX-extension lookup can return a stale end on the last UTC
		// day of a leap year (tzset uses 365 days). Do not invent a constant
		// interval up to January 1: inspect EVERY TZif-resolution second until
		// an offset change or a usable bound. Cache the resulting intervals so
		// thousands of selected dates don't repeat this bounded fallback work.
		cursor = cursor.Add(time.Second)
		if !cursor.Before(limit) {
			interval.end = limit
			return interval, nil
		}
	}
}
