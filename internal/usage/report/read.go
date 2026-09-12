package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

const (
	MaxModelBytes         = 1 << 20
	MaxRows               = 1000000
	MaxGroups             = 10000
	MaxDistinctModelBytes = 1 << 20
	NativeBusyWait        = 100 * time.Millisecond
)

type readLimits struct {
	maxModelBytes, maxRows, maxGroups, maxDistinctModelBytes int
}

func productionReadLimits() readLimits {
	return readLimits{
		maxModelBytes:         MaxModelBytes,
		maxRows:               MaxRows,
		maxGroups:             MaxGroups,
		maxDistinctModelBytes: MaxDistinctModelBytes,
	}
}

// Each call owns exactly one read-only connection and one snapshot, including
// compatibility checks. No resource survives materialization, even on failure.
func (r *Reporter) read(ctx context.Context, buckets []Bucket, surface string, model *string, budget *readBudget, valuation *capturedPricing) (_ map[string]*Section, err error) {
	if !filepath.IsAbs(r.path) {
		return nil, errors.New("database path is not absolute")
	}
	uri := sqlitestore.LiteralFileURL(r.path)
	values := uri.Query()
	values.Set("mode", "ro")
	values.Set("_busy_timeout", strconv.FormatInt(remainingBusyMS(ctx), 10))
	uri.RawQuery = values.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, r.closeDB(db)) }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if err = capBusy(ctx, conn); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return nil, err
	}
	if err = capBusy(ctx, conn); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	defer func() {
		// A read rollback does not acquire a writer lock. Remove native waiting
		// before cleanup even after the work context is canceled.
		_, capErr := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=0")
		_, closeErr := conn.ExecContext(context.Background(), "ROLLBACK")
		err = errors.Join(err, capErr, closeErr)
	}()
	var version int
	var encoding string
	if err = capBusy(ctx, conn); err != nil {
		return nil, err
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if err = capBusy(ctx, conn); err != nil {
		return nil, err
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA encoding").Scan(&encoding); err != nil {
		return nil, err
	}
	if version != sqlitestore.SchemaVersion() || encoding != "UTF-8" {
		return nil, errors.New("incompatible usage schema or encoding")
	}
	// Version alone is insufficient for a damaged or partially replaced schema.
	for _, statement := range []string{
		`SELECT at_ms,model,requested_model,service_tier,input_tokens,output_tokens,cached_tokens,cache_write_tokens,reasoning_tokens,total_tokens FROM openai_turn LIMIT 0`,
		`SELECT at_ms,model,requested_model,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,ephemeral_5m_input_tokens,ephemeral_1h_input_tokens,thinking_tokens FROM anthropic_turn LIMIT 0`,
	} {
		if err = capBusy(ctx, conn); err != nil {
			return nil, err
		}
		rows, e := conn.QueryContext(ctx, statement)
		if e != nil {
			return nil, e
		}
		if e = errors.Join(rows.Err(), rows.Close()); e != nil {
			return nil, e
		}
	}
	sections := map[string]*Section{}
	for _, native := range []string{"anthropic", "openai"} {
		if surface != "all" && surface != native {
			continue
		}
		// Close each table's Rows before the next query, retaining the same
		// transaction and request-wide budgets for both native sections.
		sections[native], err = readSection(ctx, conn, buckets, native, model, budget, valuation)
		if err != nil {
			return nil, err
		}
	}
	return sections, ctx.Err()
}

type readBudget struct {
	limits                          readLimits
	afterExaminedTurn               func(int)
	identities                      map[string]string
	retainedBytes, examined, groups int
}

func (b *readBudget) remainingIdentityBytes() int {
	return max(0, b.limits.maxDistinctModelBytes-b.retainedBytes)
}

func (b *readBudget) retainIdentityBytes(size int) error {
	if size < 0 || size > b.remainingIdentityBytes() {
		return tooLarge()
	}
	b.retainedBytes += size
	return nil
}

func readSection(ctx context.Context, conn *sql.Conn, buckets []Bucket, surface string, model *string, budget *readBudget, valuation *capturedPricing) (_ *Section, err error) {
	if err = capBusy(ctx, conn); err != nil {
		return nil, err
	}
	names, table := OpenAIMetrics(), "openai_turn"
	if surface == "anthropic" {
		names, table = AnthropicMetrics(), "anthropic_turn"
	}
	// Table and columns are exclusively the frozen native projection, never
	// caller-provided SQL. Preserve timestamp-only indexed ordering and the
	// lazy byte-length guard before transferring either Surface's identity.
	predicate := ""
	args := []any{budget.limits.maxModelBytes, buckets[0].RangeStart.UnixMilli(), buckets[len(buckets)-1].RangeEnd.UnixMilli()}
	if model != nil {
		// CASE, not ordinary AND evaluation order, guards identity comparison.
		predicate = ` AND CASE WHEN octet_length(model)=? THEN model=? COLLATE BINARY ELSE 0 END`
		args = append(args, len(*model), *model)
	}
	rows, err := conn.QueryContext(ctx, `SELECT at_ms,octet_length(model),CASE WHEN octet_length(model)<=? THEN model ELSE NULL END,`+strings.Join(names, ",")+` FROM `+table+` WHERE at_ms>=? AND at_ms<?`+predicate+` ORDER BY at_ms`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	section := Section{Rows: []Row{}, Models: []ModelTotal{}, Total: emptyTotal(names)}
	models := map[string]*Total{}
	type groupKey struct{ bucket, model string }
	groups := map[groupKey]*Total{}
	// Reuse scan storage for the section, not one destination allocation per
	// Turn. Accumulation copies numeric values and interns model identities.
	var at, modelBytes int64
	var safeModel sql.NullString
	counts := make([]sql.NullInt64, len(names))
	dest := []any{&at, &modelBytes, &safeModel}
	for i := range counts {
		dest = append(dest, &counts[i])
	}
	bucket := 0
	for rows.Next() {
		budget.examined++
		if budget.examined > budget.limits.maxRows {
			return nil, tooLarge()
		}
		if budget.afterExaminedTurn != nil {
			budget.afterExaminedTurn(budget.examined)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if modelBytes > int64(budget.limits.maxModelBytes) {
			return nil, tooLarge()
		}
		if !safeModel.Valid || !utf8.ValidString(safeModel.String) || modelBytes != int64(len(safeModel.String)) {
			return nil, errors.New("invalid stored model")
		}
		if !counts[0].Valid || !counts[1].Valid {
			return nil, errors.New("missing required count")
		}
		for _, count := range counts {
			if count.Valid && count.Int64 < 0 {
				return nil, errors.New("negative stored count")
			}
		}
		model, interned := budget.identities[safeModel.String]
		if !interned {
			if err := budget.retainIdentityBytes(len(safeModel.String)); err != nil {
				return nil, err
			}
			model = safeModel.String
			budget.identities[model] = model
		}
		selectedPricing, err := valuation.model(ctx, model)
		if err != nil {
			return nil, err
		}
		contribution, err := valueTurn(surface, counts, selectedPricing)
		if err != nil {
			if errors.Is(err, pricing.ErrOverflow) {
				return nil, overflow()
			}
			return nil, err
		}
		// Timestamp-ordered rows belong to the authoritative half-open UTC
		// intervals, even when a historical clock reversal displays yesterday.
		for bucket < len(buckets) && at >= buckets[bucket].RangeEnd.UnixMilli() {
			bucket++
		}
		if bucket == len(buckets) || at < buckets[bucket].RangeStart.UnixMilli() {
			return nil, errors.New("timestamp outside report intervals")
		}
		key := groupKey{buckets[bucket].StartDate, model}
		if groups[key] == nil {
			// Surface is implicit in this section's map, but its groups count
			// against the one request-wide limit.
			budget.groups++
			if budget.groups > budget.limits.maxGroups {
				return nil, tooLarge()
			}
			total := emptyTotal(names)
			groups[key] = &total
		}
		if models[model] == nil {
			total := emptyTotal(names)
			models[model] = &total
		}
		for _, total := range []*Total{groups[key], models[model], &section.Total} {
			if total.Turns == math.MaxInt64 {
				return nil, overflow()
			}
			total.Turns++
			for i, name := range names {
				if counts[i].Valid {
					m := total.Usage[name]
					sum := counts[i].Int64
					if m.Sum != nil {
						if sum > math.MaxInt64-*m.Sum {
							return nil, overflow()
						}
						sum += *m.Sum
					}
					if m.ReportedTurns == math.MaxInt64 {
						return nil, overflow()
					}
					m.Sum = &sum
					m.ReportedTurns++
					total.Usage[name] = m
				}
			}
			if err := addCost(&total.Cost, contribution); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for key, total := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		selected, ok := valuation.memo[key.model]
		if !ok {
			return nil, errors.New("missing memoized pricing resolution")
		}
		section.Rows = append(section.Rows, Row{BucketStart: key.bucket, ModelTotal: ModelTotal{Model: key.model, Total: *total, PricingMatch: selected.match}})
	}
	for model, total := range models {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		selected, ok := valuation.memo[model]
		if !ok {
			return nil, errors.New("missing memoized pricing resolution")
		}
		section.Models = append(section.Models, ModelTotal{Model: model, Total: *total, PricingMatch: selected.match})
	}
	sort.Slice(section.Rows, func(i, j int) bool {
		a, b := section.Rows[i], section.Rows[j]
		return a.BucketStart < b.BucketStart || a.BucketStart == b.BucketStart && a.Model < b.Model
	})
	sort.Slice(section.Models, func(i, j int) bool { return section.Models[i].Model < section.Models[j].Model })
	return &section, ctx.Err()
}

type turnContribution struct {
	amount pricing.Amount
	reason string
}

func valueTurn(surface string, counts []sql.NullInt64, selected modelPricing) (turnContribution, error) {
	switch selected.match.Status {
	case PricingMatchUnknown:
		return turnContribution{reason: "unknown_model"}, nil
	case PricingMatchAmbiguous:
		return turnContribution{reason: "ambiguous_model"}, nil
	case PricingMatchMatched:
	default:
		return turnContribution{}, errors.New("invalid pricing resolution")
	}

	var contribution pricing.Contribution
	var err error
	if surface == "anthropic" {
		contribution, err = selected.tariff.CalculateAnthropic(usage.AnthropicUsage{
			InputTokens:              counts[0].Int64,
			OutputTokens:             counts[1].Int64,
			CacheCreationInputTokens: nullableCount(counts[2]),
			CacheReadInputTokens:     nullableCount(counts[3]),
			Ephemeral5mInputTokens:   nullableCount(counts[4]),
			Ephemeral1hInputTokens:   nullableCount(counts[5]),
			ThinkingTokens:           nullableCount(counts[6]),
		})
	} else {
		contribution, err = selected.tariff.CalculateOpenAI(usage.OpenAIUsage{
			InputTokens:      counts[0].Int64,
			OutputTokens:     counts[1].Int64,
			CachedTokens:     nullableCount(counts[2]),
			CacheWriteTokens: nullableCount(counts[3]),
			ReasoningTokens:  nullableCount(counts[4]),
			TotalTokens:      nullableCount(counts[5]),
		})
	}
	if err != nil {
		return turnContribution{}, err
	}
	return turnContribution{amount: contribution.Amount, reason: string(contribution.Reason)}, nil
}

func nullableCount(count sql.NullInt64) *int64 {
	if !count.Valid {
		return nil
	}
	value := count.Int64
	return &value
}

func addCost(cost *Cost, contribution turnContribution) error {
	if contribution.reason != "" {
		if cost.PricedTurns == 0 {
			cost.Amount = nil
		}
		var count *int64
		switch contribution.reason {
		case "unknown_model":
			count = &cost.Unpriced.UnknownModel
		case "ambiguous_model":
			count = &cost.Unpriced.AmbiguousModel
		case string(pricing.ExclusionMissingRate):
			count = &cost.Unpriced.MissingRate
		case string(pricing.ExclusionMissingUsage):
			count = &cost.Unpriced.MissingUsage
		case string(pricing.ExclusionInconsistentUsage):
			count = &cost.Unpriced.InconsistentUsage
		default:
			return errors.New("invalid pricing exclusion")
		}
		if *count == math.MaxInt64 {
			return overflow()
		}
		*count = *count + 1
		return nil
	}
	if cost.PricedTurns == math.MaxInt64 {
		return overflow()
	}
	var current pricing.Amount
	if cost.Amount != nil {
		current = *cost.Amount
	}
	total, err := current.Add(contribution.amount)
	if err != nil {
		if errors.Is(err, pricing.ErrOverflow) {
			return overflow()
		}
		return err
	}
	cost.PricedTurns++
	cost.Amount = &total
	return nil
}

// Floor, rather than round up: a sub-millisecond remainder means no native
// wait. Conn acquisition uses the same cap in the escaped DSN before any SQL.
func remainingBusyMS(ctx context.Context) int64 {
	remaining := NativeBusyWait
	if deadline, ok := ctx.Deadline(); ok {
		remaining = min(remaining, time.Until(deadline))
	}
	return max(0, remaining.Milliseconds())
}
func capBusy(ctx context.Context, conn *sql.Conn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", remainingBusyMS(ctx)))
	return err
}

func overflow() error {
	return &Error{Code: Overflow, Message: "Exact aggregation exceeds supported numeric bounds; narrow the date range or model/Surface selection."}
}
