// Package report owns independent calendar aggregates of persisted native Turns.
// It performs no logging, writer coordination, or upstream work.
package report

import (
	"context"
	"database/sql"
	"time"
)

// Query selects daemon-owned calendar aggregation in a named timezone.
// Empty Period selects day. Model filters remain a follow-on capability.
type Query struct {
	Period   string
	Since    string
	Until    string
	Timezone string
	Surface  string
	Model    *string
}

// Metric retains the known native sum and its coverage within stored Turns.
// Required metrics alone have a known zero in an empty section; optional sums
// remain nil until at least one Turn reports them, including a reported zero.
type Metric struct {
	Sum           *int64 `json:"sum,string"`
	ReportedTurns int64  `json:"reported_turns,string"`
}

type Total struct {
	Turns int64             `json:"turns,string"`
	Usage map[string]Metric `json:"usage"`
}
type ModelTotal struct {
	Model string `json:"model"`
	Total
}
type Row struct {
	BucketStart string `json:"bucket_start"`
	ModelTotal
}
type Section struct {
	Rows   []Row        `json:"rows"`
	Models []ModelTotal `json:"models"`
	Total  Total        `json:"total"`
}
type Bucket struct {
	StartDate    string    `json:"start_date"`
	UntilDate    string    `json:"until_date"`
	RangeStart   time.Time `json:"range_start"`
	RangeEnd     time.Time `json:"range_end"`
	RangePartial bool      `json:"range_partial"`
	InProgress   bool      `json:"in_progress"`
}

// Report is an independent materialized snapshot. Generation time is the query
// clock, not a watermark or a promise that queued observations were persisted.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Timezone      string    `json:"timezone"`
	Period        string    `json:"period"`
	Since         string    `json:"since"`
	Until         string    `json:"until"`
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	Scope         string    `json:"scope"`
	Collection    string    `json:"collection"`
	Surface       string    `json:"surface"`
	Model         *string   `json:"model"`
	Buckets       []Bucket  `json:"buckets"`
	Anthropic     *Section  `json:"anthropic,omitempty"`
	OpenAI        *Section  `json:"openai,omitempty"`
}

// OpenAIMetrics names the frozen native projection; subsets are never added to
// their containing counts. The returned list is independent of internal state.
func OpenAIMetrics() []string {
	return []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens"}
}

// AnthropicMetrics names the frozen native projection. Input is the uncached
// remainder; TTL counts are cache-creation subsets and thinking is inside output.
func AnthropicMetrics() []string {
	return []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens", "thinking_tokens"}
}

// Reporter captures only the daemon's absolute path. New does not touch files.
type Reporter struct {
	path    string
	now     func() time.Time
	closeDB func(*sql.DB) error
}

func New(databasePath string) *Reporter {
	return &Reporter{path: databasePath, now: time.Now, closeDB: (*sql.DB).Close}
}

func (r *Reporter) Query(ctx context.Context, q Query) (result Report, err error) {
	defer func() {
		if err != nil {
			result = Report{}
			err = publicError(ctx, err)
		}
	}()
	if err = ctx.Err(); err != nil {
		return Report{}, err
	}
	now := r.now().UTC()
	if q.Period == "" {
		q.Period = "day"
	}
	if q.Surface == "" {
		q.Surface = "all"
	}
	if (q.Surface != "all" && q.Surface != "openai" && q.Surface != "anthropic") || (q.Period != "day" && q.Period != "week" && q.Period != "month" && q.Period != "year") || q.Model != nil {
		return Report{}, invalid("Select a supported Surface and day/week/month/year period; model filters are not yet supported.")
	}
	buckets, start, end, err := calendar(ctx, &q, now)
	if err != nil {
		return Report{}, err
	}
	result = Report{SchemaVersion: 1, GeneratedAt: now, Timezone: q.Timezone, Period: q.Period, Since: q.Since, Until: q.Until, Surface: q.Surface, Model: q.Model, WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Buckets: buckets}
	sections, err := r.read(ctx, result.Buckets, q.Surface)
	if err != nil {
		return Report{}, err
	}
	result.Anthropic, result.OpenAI = sections["anthropic"], sections["openai"]
	return result, ctx.Err()
}
func strictDate(value string) (time.Time, error) {
	date, err := time.Parse(time.DateOnly, value)
	if err != nil || date.Format(time.DateOnly) != value || value < "1970-01-01" || value > "9999-01-01" {
		return time.Time{}, invalid("Supply since/until dates as YYYY-MM-DD in 1970-01-01 through 9999-01-01.")
	}
	return date, nil
}

func emptyTotal(names []string) Total {
	total := Total{Usage: map[string]Metric{}}
	for _, name := range names {
		total.Usage[name] = Metric{}
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		zero := int64(0)
		total.Usage[name] = Metric{Sum: &zero}
	}
	return total
}
