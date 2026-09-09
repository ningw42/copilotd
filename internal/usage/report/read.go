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

	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

const (
	MaxModelBytes         = 1 << 20
	MaxRows               = 1000000
	MaxGroups             = 10000
	MaxDistinctModelBytes = 1 << 20
	NativeBusyWait        = 100 * time.Millisecond
)

// Each call owns exactly one read-only connection and one snapshot, including
// compatibility checks. No resource survives materialization, even on failure.
func (r *Reporter) read(ctx context.Context, buckets []Bucket, surface string, model *string) (_ map[string]*Section, err error) {
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
		`SELECT at_ms,model,requested_model,input_tokens,output_tokens,cached_tokens,cache_write_tokens,reasoning_tokens,total_tokens FROM openai_turn LIMIT 0`,
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
	budget := readBudget{identities: map[string]string{}}
	for _, native := range []string{"anthropic", "openai"} {
		if surface != "all" && surface != native {
			continue
		}
		// Close each table's Rows before the next query, retaining the same
		// transaction and request-wide budgets for both native sections.
		sections[native], err = readSection(ctx, conn, buckets, native, model, &budget)
		if err != nil {
			return nil, err
		}
	}
	return sections, ctx.Err()
}

type readBudget struct {
	identities                      map[string]string
	retainedBytes, examined, groups int
}

func readSection(ctx context.Context, conn *sql.Conn, buckets []Bucket, surface string, model *string, budget *readBudget) (_ *Section, err error) {
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
	args := []any{MaxModelBytes, buckets[0].RangeStart.UnixMilli(), buckets[len(buckets)-1].RangeEnd.UnixMilli()}
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
		if budget.examined > MaxRows {
			return nil, tooLarge()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if modelBytes > MaxModelBytes {
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
			budget.retainedBytes += len(safeModel.String)
			if budget.retainedBytes > MaxDistinctModelBytes {
				return nil, tooLarge()
			}
			model = safeModel.String
			budget.identities[model] = model
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
			if budget.groups > MaxGroups {
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
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for key, total := range groups {
		section.Rows = append(section.Rows, Row{BucketStart: key.bucket, ModelTotal: ModelTotal{Model: key.model, Total: *total}})
	}
	for model, total := range models {
		section.Models = append(section.Models, ModelTotal{Model: model, Total: *total})
	}
	sort.Slice(section.Rows, func(i, j int) bool {
		a, b := section.Rows[i], section.Rows[j]
		return a.BucketStart < b.BucketStart || a.BucketStart == b.BucketStart && a.Model < b.Model
	})
	sort.Slice(section.Models, func(i, j int) bool { return section.Models[i].Model < section.Models[j].Model })
	return &section, ctx.Err()
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
	return &Error{Code: Overflow, Message: "Exact aggregate exceeds int64; narrow the date range or model/Surface selection."}
}
