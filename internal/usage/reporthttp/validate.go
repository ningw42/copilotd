package reporthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
)

var errProtocol = errors.New("invalid Usage report protocol")

// Validate only the wire contract, never aggregate totals or reconstruct the
// daemon's calendar. Each known member is looked up case-sensitively; additive
// fields cannot override it through encoding/json's case-insensitive structs.
func decodeReport(ctx context.Context, body []byte, q report.Query) (report.Report, error) {
	if err := unambiguousJSON(ctx, body); err != nil {
		return report.Report{}, err
	}
	d := wireDecoder{ctx: ctx}
	root := d.object(body)
	r := report.Report{SchemaVersion: d.integer(root, "schema_version"), GeneratedAt: d.instant(root, "generated_at"), Timezone: d.text(root, "timezone"), Period: d.text(root, "period"), Since: d.date(root, "since"), Until: d.date(root, "until"), WindowStart: d.instant(root, "window_start"), WindowEnd: d.instant(root, "window_end"), Scope: d.text(root, "scope"), Collection: d.text(root, "collection"), Surface: d.text(root, "surface"), Buckets: []report.Bucket{}}
	_, pricingPresent := root["pricing"]
	if pricingPresent {
		r.Pricing = d.pricing(d.object(root["pricing"]))
	}
	model := d.member(root, "model")
	if !bytes.Equal(model, []byte("null")) {
		value := d.text(root, "model")
		r.Model = &value
		if value == "" {
			d.err = errProtocol
		}
	}
	if r.SchemaVersion != 1 || r.Scope != "configured_database" || r.Collection != "best_effort" || (r.Surface != "all" && r.Surface != "anthropic" && r.Surface != "openai") || r.Timezone == "" || r.Since < "1970-01-01" || r.Until > "9999-01-01" || r.Since >= r.Until || !r.WindowStart.Before(r.WindowEnd) {
		d.err = errProtocol
	}
	if q.Surface == "" {
		q.Surface = "all"
	}
	for _, pair := range [][2]string{{q.Timezone, r.Timezone}, {q.Period, r.Period}, {q.Since, r.Since}, {q.Until, r.Until}, {q.Surface, r.Surface}} {
		if pair[0] != "" && pair[0] != pair[1] {
			d.err = errProtocol
		}
	}
	if q.Model == nil && r.Model != nil || q.Model != nil && (r.Model == nil || *q.Model != *r.Model) {
		d.err = errProtocol
	}
	switch r.Period {
	case "day", "week", "month", "year":
	default:
		d.err = errProtocol
	}
	bucketNames := map[string]bool{}
	for _, raw := range d.array(root, "buckets") {
		b := d.object(raw)
		bucket := report.Bucket{StartDate: d.date(b, "start_date"), UntilDate: d.date(b, "until_date"), RangeStart: d.instant(b, "range_start"), RangeEnd: d.instant(b, "range_end"), RangePartial: d.boolean(b, "range_partial"), InProgress: d.boolean(b, "in_progress")}
		if bucket.StartDate >= bucket.UntilDate || !bucket.RangeStart.Before(bucket.RangeEnd) || bucket.RangeStart.Before(r.WindowStart) || bucket.RangeEnd.After(r.WindowEnd) || bucketNames[bucket.StartDate] {
			d.err = errProtocol
		}
		if len(r.Buckets) > 0 {
			previous := r.Buckets[len(r.Buckets)-1]
			if !previous.RangeEnd.Equal(bucket.RangeStart) {
				d.err = errProtocol
			}
		}
		bucketNames[bucket.StartDate] = true
		r.Buckets = append(r.Buckets, bucket)
	}
	if len(r.Buckets) == 0 || len(r.Buckets) > report.MaxBuckets {
		d.err = errProtocol
	} else if !r.Buckets[0].RangeStart.Equal(r.WindowStart) || !r.Buckets[len(r.Buckets)-1].RangeEnd.Equal(r.WindowEnd) {
		d.err = errProtocol
	}
	for _, native := range []struct {
		name    string
		metrics []string
		dest    **report.Section
	}{{"anthropic", report.AnthropicMetrics(), &r.Anthropic}, {"openai", report.OpenAIMetrics(), &r.OpenAI}} {
		if r.Surface != "all" && r.Surface != native.name {
			if _, present := root[native.name]; present {
				d.err = errProtocol
			}
			continue
		}
		section := d.object(d.member(root, native.name))
		s := report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}, Total: d.total(d.object(d.member(section, "total")), true, native.metrics, pricingPresent)}
		for _, raw := range d.array(section, "rows") {
			o := d.object(raw)
			row := report.Row{BucketStart: d.date(o, "bucket_start"), ModelTotal: report.ModelTotal{Model: d.text(o, "model"), Total: d.total(o, false, native.metrics, pricingPresent)}}
			row.PricingMatch = d.pricingMatch(o, pricingPresent)
			if !bucketNames[row.BucketStart] || r.Model != nil && row.Model != *r.Model {
				d.err = errProtocol
			}
			if len(s.Rows) > 0 {
				previous := s.Rows[len(s.Rows)-1]
				if previous.BucketStart > row.BucketStart || previous.BucketStart == row.BucketStart && previous.Model >= row.Model {
					d.err = errProtocol
				}
			}
			s.Rows = append(s.Rows, row)
		}
		for _, raw := range d.array(section, "models") {
			o := d.object(raw)
			model := report.ModelTotal{Model: d.text(o, "model"), Total: d.total(o, false, native.metrics, pricingPresent)}
			model.PricingMatch = d.pricingMatch(o, pricingPresent)
			if r.Model != nil && model.Model != *r.Model {
				d.err = errProtocol
			}
			if len(s.Models) > 0 && s.Models[len(s.Models)-1].Model >= model.Model {
				d.err = errProtocol
			}
			s.Models = append(s.Models, model)
		}
		if s.Total.Turns == 0 && (len(s.Rows) != 0 || len(s.Models) != 0) || s.Total.Turns > 0 && (len(s.Rows) == 0 || len(s.Models) == 0) {
			d.err = errProtocol
		}
		*native.dest = &s
	}
	if d.err != nil {
		return report.Report{}, d.err
	}
	return r, ctx.Err()
}

type object map[string]json.RawMessage
type wireDecoder struct {
	ctx context.Context
	err error
}

func (d *wireDecoder) object(raw json.RawMessage) object {
	var o object
	if d.ctx.Err() != nil {
		d.err = d.ctx.Err()
		return o
	}
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		d.err = errProtocol
	}
	return o
}
func (d *wireDecoder) member(o object, key string) json.RawMessage {
	raw, ok := o[key]
	if !ok {
		d.err = errProtocol
	}
	return raw
}
func (d *wireDecoder) text(o object, key string) string {
	raw := d.member(o, key)
	var value string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		d.err = errProtocol
	}
	return value
}
func (d *wireDecoder) integer(o object, key string) int {
	raw := d.member(o, key)
	var value int
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
		d.err = errProtocol
	}
	return value
}
func (d *wireDecoder) boolean(o object, key string) bool {
	raw := d.member(o, key)
	var value bool
	if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
		d.err = errProtocol
	}
	_ = json.Unmarshal(raw, &value)
	return value
}
func (d *wireDecoder) count(o object, key string) int64 {
	value := d.text(o, key)
	if value == "" || len(value) > 1 && value[0] == '0' {
		d.err = errProtocol
		return 0
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			d.err = errProtocol
			return 0
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		d.err = errProtocol
	}
	return n
}
func (d *wireDecoder) instant(o object, key string) time.Time {
	value := d.text(o, key)
	t, err := time.Parse(time.RFC3339Nano, value)
	_, offset := t.Zone()
	if err != nil || offset != 0 || len(value) < len("2006-01-02T15:04:05Z") || value[len("2006-01-02T15")] != ':' || value[len("2006-01-02T15:04:05")] == ',' {
		d.err = errProtocol
	}
	return t
}
func (d *wireDecoder) date(o object, key string) string {
	value := d.text(o, key)
	t, err := time.Parse(time.DateOnly, value)
	if err != nil || t.Format(time.DateOnly) != value {
		d.err = errProtocol
	}
	return value
}
func (d *wireDecoder) array(o object, key string) []json.RawMessage {
	raw := d.member(o, key)
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		d.err = errProtocol
	}
	return values
}
func (d *wireDecoder) total(o object, section bool, names []string, pricingPresent bool) report.Total {
	total := report.Total{Turns: d.count(o, "turns"), Usage: map[string]report.Metric{}}
	if section {
		if _, present := o["pricing_match"]; present {
			d.err = errProtocol
		}
	} else if total.Turns == 0 {
		d.err = errProtocol
	}
	metrics := d.object(d.member(o, "usage"))
	for _, name := range names {
		m := d.object(d.member(metrics, name))
		metric := report.Metric{ReportedTurns: d.count(m, "reported_turns")}
		sum := d.member(m, "sum")
		if !bytes.Equal(sum, []byte("null")) {
			n := d.count(m, "sum")
			metric.Sum = &n
		}
		required := name == "input_tokens" || name == "output_tokens"
		if metric.ReportedTurns > total.Turns {
			d.err = errProtocol
		}
		if required {
			if metric.Sum == nil || metric.ReportedTurns != total.Turns || total.Turns == 0 && *metric.Sum != 0 {
				d.err = errProtocol
			}
		} else if (metric.Sum == nil) != (metric.ReportedTurns == 0) {
			d.err = errProtocol
		}
		total.Usage[name] = metric
	}
	if pricingPresent {
		total.Cost = d.cost(d.object(d.member(o, "cost")), total.Turns)
	} else if _, present := o["cost"]; present {
		d.err = errProtocol
	}
	return total
}

func (d *wireDecoder) pricing(o object) *report.PricingProvenance {
	value := &report.PricingProvenance{
		Dataset: d.text(o, "dataset"), Currency: d.text(o, "currency"), Basis: d.text(o, "basis"),
		ContextPolicy: d.text(o, "context_policy"), CacheWritePolicy: d.text(o, "cache_write_policy"),
		Version: d.text(o, "version"), Source: d.text(o, "source"),
	}
	lastSuccess := d.member(o, "last_success")
	if !bytes.Equal(lastSuccess, []byte("null")) {
		instant := d.instant(o, "last_success")
		value.LastSuccess = &instant
	}
	if value.Dataset != "models.dev/api.json" || value.Currency != "USD" || value.Basis != "original_provider" || value.ContextPolicy != "highest_tier" || value.CacheWritePolicy != "single_rate" || !validContentVersion(value.Version) || value.Source != "fallback" && value.Source != "fetched" {
		d.err = errProtocol
	}
	return value
}

func validContentVersion(value string) bool {
	if len(value) != len("sha256:")+64 || value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, digit := range value[len("sha256:"):] {
		if !('0' <= digit && digit <= '9') && !('a' <= digit && digit <= 'f') {
			return false
		}
	}
	return true
}

func (d *wireDecoder) cost(o object, turns int64) report.Cost {
	cost := report.Cost{PricedTurns: d.count(o, "priced_turns")}
	rawAmount := d.member(o, "amount")
	if !bytes.Equal(rawAmount, []byte("null")) {
		amountText := d.text(o, "amount")
		amount, err := pricing.ParseAmount(amountText)
		if err != nil {
			d.err = errProtocol
		} else {
			cost.Amount = &amount
		}
	}
	unpriced := d.object(d.member(o, "unpriced"))
	cost.Unpriced = report.UnpricedCoverage{
		UnknownModel: d.count(unpriced, "unknown_model"), AmbiguousModel: d.count(unpriced, "ambiguous_model"),
		MissingRate: d.count(unpriced, "missing_rate"), MissingUsage: d.count(unpriced, "missing_usage"),
		InconsistentUsage: d.count(unpriced, "inconsistent_usage"),
	}
	total := cost.PricedTurns
	for _, count := range []int64{cost.Unpriced.UnknownModel, cost.Unpriced.AmbiguousModel, cost.Unpriced.MissingRate, cost.Unpriced.MissingUsage, cost.Unpriced.InconsistentUsage} {
		if count > math.MaxInt64-total {
			d.err = errProtocol
			break
		}
		total += count
	}
	if total != turns || turns == 0 && (cost.Amount == nil || cost.Amount.String() != "0") || turns > 0 && cost.PricedTurns == 0 && cost.Amount != nil || cost.PricedTurns > 0 && cost.Amount == nil {
		d.err = errProtocol
	}
	return cost
}

func (d *wireDecoder) pricingMatch(o object, pricingPresent bool) report.PricingMatch {
	if !pricingPresent {
		if _, present := o["pricing_match"]; present {
			d.err = errProtocol
		}
		return report.PricingMatch{}
	}
	matchObject := d.object(d.member(o, "pricing_match"))
	match := report.PricingMatch{Status: report.PricingMatchStatus(d.text(matchObject, "status"))}
	switch match.Status {
	case report.PricingMatchMatched:
		match.Provider = d.text(matchObject, "provider")
		match.Model = d.text(matchObject, "model")
		match.Method = report.PricingMatchMethod(d.text(matchObject, "method"))
		if !validPricingIdentity(match.Provider) || !validPricingIdentity(match.Model) {
			d.err = errProtocol
		}
		switch match.Method {
		case report.PricingMatchByExact, report.PricingMatchByNormalized, report.PricingMatchByAlias, report.PricingMatchBySuffix, report.PricingMatchByDated:
		default:
			d.err = errProtocol
		}
	case report.PricingMatchUnknown, report.PricingMatchAmbiguous:
		for _, key := range []string{"provider", "model", "method"} {
			if _, present := matchObject[key]; present {
				d.err = errProtocol
			}
		}
	default:
		d.err = errProtocol
	}
	return match
}

func validPricingIdentity(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value)
}

// jsontext validates strict Unicode and unique decoded member names by default.
func unambiguousJSON(ctx context.Context, body []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(body))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := decoder.ReadToken(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errProtocol
		}
		if decoder.StackDepth() == 0 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errProtocol
	}
	return ctx.Err()
}
