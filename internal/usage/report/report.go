// Package report owns independent calendar aggregates of persisted native Turns.
// It performs no logging, writer coordination, or upstream work.
package report

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/ningw42/copilotd/internal/usage/modelmatch"
	"github.com/ningw42/copilotd/internal/usage/pricing"
)

// Query selects daemon-owned calendar aggregation in a named timezone.
// Empty Period selects day. Model selects an exact Reported model; nil selects all.
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

// UnpricedCoverage partitions unpriceable stored Turns by the first applicable
// reason. Together with Cost.PricedTurns these counts always equal Total.Turns.
type UnpricedCoverage struct {
	UnknownModel      int64
	AmbiguousModel    int64
	MissingRate       int64
	MissingUsage      int64
	InconsistentUsage int64
}

// Cost is the exact priced-Turn subtotal and complete valuation coverage for one
// aggregate. Amount is nil only for a nonempty aggregate with no priceable
// Turns. Empty aggregates and priceable free Turns carry a non-nil exact zero.
type Cost struct {
	Amount      *pricing.Amount
	PricedTurns int64
	Unpriced    UnpricedCoverage
}

// PricingMatchStatus describes whether one Reported model selected a Pricing
// model identity.
type PricingMatchStatus string

const (
	PricingMatchMatched   PricingMatchStatus = "matched"
	PricingMatchUnknown   PricingMatchStatus = "unknown"
	PricingMatchAmbiguous PricingMatchStatus = "ambiguous"
)

// PricingMatchMethod identifies the strongest matching policy that selected a
// Pricing model.
type PricingMatchMethod string

const (
	PricingMatchByExact      PricingMatchMethod = "exact"
	PricingMatchByNormalized PricingMatchMethod = "normalized"
	PricingMatchByAlias      PricingMatchMethod = "alias"
	PricingMatchBySuffix     PricingMatchMethod = "suffix"
	PricingMatchByDated      PricingMatchMethod = "dated"
)

// PricingMatch is one Reported model's report-wide immutable resolution.
type PricingMatch struct {
	Status   PricingMatchStatus
	Provider string
	Model    string
	Method   PricingMatchMethod
}

type Total struct {
	Turns int64             `json:"turns,string"`
	Usage map[string]Metric `json:"usage"`
	Cost  Cost              `json:"-"`
}
type ModelTotal struct {
	Model        string       `json:"model"`
	PricingMatch PricingMatch `json:"-"`
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

// PricingProvenance identifies the original-provider tariff benchmark captured
// once for a report. LastSuccess is a UTC copy of the successful content-fetch
// time, not a tariff effective date; the captured source status is not mutated.
type PricingProvenance struct {
	Dataset          string
	Currency         string
	Basis            string
	ContextPolicy    string
	CacheWritePolicy string
	Version          string
	Source           string
	LastSuccess      *time.Time
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
	// Pricing is always non-nil for Reporter.Query results. A nil value is
	// reserved for a client's validated all-absent response from an older daemon.
	Pricing   *PricingProvenance `json:"-"`
	Anthropic *Section           `json:"anthropic,omitempty"`
	OpenAI    *Section           `json:"openai,omitempty"`
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

// Reporter captures the daemon's absolute path and immutable read policy. New
// does not touch files.
type Reporter struct {
	path              string
	pricingSource     pricing.Source
	now               func() time.Time
	closeDB           func(*sql.DB) error
	limits            readLimits
	afterExaminedTurn func(int)
}

func New(databasePath string, pricingSource pricing.Source) *Reporter {
	return &Reporter{path: databasePath, pricingSource: pricingSource, now: time.Now, closeDB: (*sql.DB).Close, limits: productionReadLimits()}
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
	budget := readBudget{limits: r.limits, afterExaminedTurn: r.afterExaminedTurn, identities: map[string]string{}}
	valuation, status, captureErr := r.capturePricing(ctx, &budget)
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
	if (q.Surface != "all" && q.Surface != "openai" && q.Surface != "anthropic") || (q.Period != "day" && q.Period != "week" && q.Period != "month" && q.Period != "year") {
		return Report{}, invalid("Select a supported Surface and day/week/month/year period.")
	}
	if q.Model != nil && (*q.Model == "" || !utf8.ValidString(*q.Model)) {
		return Report{}, invalid("Supply a non-empty valid UTF-8 Reported model.")
	}
	// Direct callers do not have the HTTP adapter's raw-query cap. Bounding the
	// filter also ensures the lazy equality predicate cannot read a larger model.
	if q.Model != nil && len(*q.Model) > r.limits.maxModelBytes {
		return Report{}, tooLarge()
	}
	buckets, start, end, err := calendar(ctx, &q, now)
	if err != nil {
		return Report{}, err
	}
	if captureErr != nil {
		if errors.Is(captureErr, pricing.ErrProjectionLimit) || errors.Is(captureErr, modelmatch.ErrRetentionLimit) {
			return Report{}, tooLarge()
		}
		return Report{}, captureErr
	}
	result = Report{SchemaVersion: 1, GeneratedAt: now, Timezone: q.Timezone, Period: q.Period, Since: q.Since, Until: q.Until, Surface: q.Surface, Model: q.Model, WindowStart: start, WindowEnd: end, Scope: "configured_database", Collection: "best_effort", Buckets: buckets, Pricing: pricingProvenance(status)}
	sections, err := r.read(ctx, result.Buckets, q.Surface, q.Model, &budget, valuation)
	if err != nil {
		return Report{}, err
	}
	result.Anthropic, result.OpenAI = sections["anthropic"], sections["openai"]
	return result, ctx.Err()
}

type capturedPricing struct {
	snapshot *pricing.Snapshot
	matcher  *modelmatch.Matcher
	memo     map[string]modelPricing
}

type modelPricing struct {
	match  PricingMatch
	tariff pricing.Tariff
}

func (r *Reporter) capturePricing(ctx context.Context, budget *readBudget) (*capturedPricing, pricing.SnapshotStatus, error) {
	if r.pricingSource == nil {
		return nil, pricing.SnapshotStatus{}, errors.New("pricing source is unavailable")
	}
	snapshot, status, err := r.pricingSource.Current(ctx, pricing.ProjectionLimit{MaxIdentityBytes: budget.remainingIdentityBytes()})
	if err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if snapshot == nil {
		return nil, pricing.SnapshotStatus{}, errors.New("pricing source returned no snapshot")
	}
	if err := budget.retainIdentityBytes(snapshot.IdentityBytes()); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	identities := snapshot.Identities()
	candidates := make([]modelmatch.Identity, 0, len(identities))
	for _, identity := range identities {
		if err := ctx.Err(); err != nil {
			return nil, pricing.SnapshotStatus{}, err
		}
		candidates = append(candidates, modelmatch.Identity{Provider: identity.Provider, Model: identity.Model})
	}
	matcher, err := modelmatch.New(ctx, candidates, modelmatch.RetentionLimit{MaxIdentityBytes: budget.remainingIdentityBytes()})
	if err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if err := budget.retainIdentityBytes(matcher.RetainedIdentityBytes()); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, pricing.SnapshotStatus{}, err
	}
	return &capturedPricing{snapshot: snapshot, matcher: matcher, memo: make(map[string]modelPricing)}, status, nil
}

func (c *capturedPricing) model(ctx context.Context, reported string) (modelPricing, error) {
	if resolved, ok := c.memo[reported]; ok {
		return resolved, nil
	}
	if err := ctx.Err(); err != nil {
		return modelPricing{}, err
	}
	resolution := c.matcher.Resolve(reported)
	selected := modelPricing{match: reportPricingMatch(resolution)}
	if resolution.Status == modelmatch.StatusMatched {
		selected.tariff, _ = c.snapshot.Tariff(pricing.Identity{
			Provider: resolution.Identity.Provider,
			Model:    resolution.Identity.Model,
		})
	}
	if err := ctx.Err(); err != nil {
		return modelPricing{}, err
	}
	c.memo[reported] = selected
	return selected, nil
}

func reportPricingMatch(resolution modelmatch.Resolution) PricingMatch {
	match := PricingMatch{Status: PricingMatchStatus(resolution.Status)}
	if resolution.Status == modelmatch.StatusMatched {
		match.Provider = resolution.Identity.Provider
		match.Model = resolution.Identity.Model
		match.Method = PricingMatchMethod(resolution.Method)
	}
	return match
}

func pricingProvenance(status pricing.SnapshotStatus) *PricingProvenance {
	provenance := &PricingProvenance{
		Dataset:          "models.dev/api.json",
		Currency:         "USD",
		Basis:            "original_provider",
		ContextPolicy:    "highest_tier",
		CacheWritePolicy: "single_rate",
		Version:          status.Version,
		Source:           status.Source,
	}
	if status.LastSuccess != nil {
		lastSuccess := status.LastSuccess.UTC()
		provenance.LastSuccess = &lastSuccess
	}
	return provenance
}

func strictDate(value string) (time.Time, error) {
	date, err := time.Parse(time.DateOnly, value)
	if err != nil || date.Format(time.DateOnly) != value || value < "1970-01-01" || value > "9999-01-01" {
		return time.Time{}, invalid("Supply since/until dates as YYYY-MM-DD in 1970-01-01 through 9999-01-01.")
	}
	return date, nil
}

func emptyTotal(names []string) Total {
	zeroAmount := pricing.Amount{}
	total := Total{Usage: map[string]Metric{}, Cost: Cost{Amount: &zeroAmount}}
	for _, name := range names {
		total.Usage[name] = Metric{}
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		zero := int64(0)
		total.Usage[name] = Metric{Sum: &zero}
	}
	return total
}
