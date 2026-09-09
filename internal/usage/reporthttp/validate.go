package reporthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

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
		s := report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}, Total: d.total(d.object(d.member(section, "total")), true, native.metrics)}
		periodBuckets := map[string]bool{}
		if _, present := section["periods"]; present {
			s.Periods = []report.PeriodTotal{}
			for _, raw := range d.array(section, "periods") {
				o := d.object(raw)
				period := report.PeriodTotal{BucketStart: d.date(o, "bucket_start"), Total: d.total(o, false, native.metrics)}
				if !bucketNames[period.BucketStart] || periodBuckets[period.BucketStart] || len(s.Periods) > 0 && s.Periods[len(s.Periods)-1].BucketStart >= period.BucketStart {
					d.err = errProtocol
				}
				periodBuckets[period.BucketStart] = true
				s.Periods = append(s.Periods, period)
			}
		}
		rowBuckets := map[string]bool{}
		for _, raw := range d.array(section, "rows") {
			o := d.object(raw)
			row := report.Row{BucketStart: d.date(o, "bucket_start"), ModelTotal: report.ModelTotal{Model: d.text(o, "model"), Total: d.total(o, false, native.metrics)}}
			if !bucketNames[row.BucketStart] || r.Model != nil && row.Model != *r.Model {
				d.err = errProtocol
			}
			if len(s.Rows) > 0 {
				previous := s.Rows[len(s.Rows)-1]
				if previous.BucketStart > row.BucketStart || previous.BucketStart == row.BucketStart && previous.Model >= row.Model {
					d.err = errProtocol
				}
			}
			rowBuckets[row.BucketStart] = true
			s.Rows = append(s.Rows, row)
		}
		for _, raw := range d.array(section, "models") {
			o := d.object(raw)
			model := report.ModelTotal{Model: d.text(o, "model"), Total: d.total(o, false, native.metrics)}
			if r.Model != nil && model.Model != *r.Model {
				d.err = errProtocol
			}
			if len(s.Models) > 0 && s.Models[len(s.Models)-1].Model >= model.Model {
				d.err = errProtocol
			}
			s.Models = append(s.Models, model)
		}
		if s.Periods != nil {
			if len(periodBuckets) != len(rowBuckets) {
				d.err = errProtocol
			}
			for bucket := range rowBuckets {
				if !periodBuckets[bucket] {
					d.err = errProtocol
				}
			}
		}
		hasData := len(s.Rows) != 0 || len(s.Models) != 0 || len(s.Periods) != 0
		missingData := len(s.Rows) == 0 || len(s.Models) == 0 || s.Periods != nil && len(s.Periods) == 0
		if s.Total.Turns == 0 && hasData || s.Total.Turns > 0 && missingData {
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
func (d *wireDecoder) total(o object, section bool, names []string) report.Total {
	total := report.Total{Turns: d.count(o, "turns"), Usage: map[string]report.Metric{}}
	if !section && total.Turns == 0 {
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
	return total
}

// encoding/json repairs invalid UTF-8 and lone surrogate escapes. Reject those
// before tokenization, and reject duplicate decoded member names at every depth.
func unambiguousJSON(ctx context.Context, body []byte) error {
	if !utf8.Valid(body) || !json.Valid(body) {
		return errProtocol
	}
	for i := 0; i < len(body); i++ {
		if i%4096 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if body[i] != '"' {
			continue
		}
		for i++; i < len(body) && body[i] != '"'; i++ {
			if body[i] != '\\' {
				continue
			}
			i++
			if body[i] != 'u' {
				continue
			}
			n, _ := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
			i += 4
			if n >= 0xdc00 && n <= 0xdfff {
				return errProtocol
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
					return errProtocol
				}
				low, e := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
				if e != nil || low < 0xdc00 || low > 0xdfff {
					return errProtocol
				}
				i += 6
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := uniqueValue(ctx, decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errProtocol
	}
	return nil
}
func uniqueValue(ctx context.Context, d *json.Decoder) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	token, err := d.Token()
	if err != nil {
		return errProtocol
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		names := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errProtocol
			}
			name, ok := key.(string)
			if !ok || names[name] {
				return errProtocol
			}
			names[name] = true
			if err := uniqueValue(ctx, d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueValue(ctx, d); err != nil {
				return err
			}
		}
	default:
		return errProtocol
	}
	if _, err := d.Token(); err != nil {
		return errProtocol
	}
	return nil
}
