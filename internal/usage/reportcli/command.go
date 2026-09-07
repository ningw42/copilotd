// Package reportcli orchestrates a one-shot HTTP report and safe terminal
// presentation. It never opens SQLite or recomputes report aggregates.
package reportcli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

type Options struct {
	Endpoint string
	Query    report.Query
	Timezone *string
	Details  bool
	JSON     bool
	Timeout  time.Duration
}

func Run(ctx context.Context, client *reporthttp.Client, options Options, stdout io.Writer) error {
	if options.Timezone == nil || *options.Timezone != "UTC" {
		return fmt.Errorf("this release requires explicit --timezone UTC; terminal-local discovery and named zones are not yet supported")
	}
	if options.Details || options.JSON || options.Query.Model != nil {
		return fmt.Errorf("--details, --json, and --model are not yet supported")
	}
	if options.Timeout <= 0 {
		return fmt.Errorf("report timeout must be positive")
	}
	q := options.Query
	q.Timezone = *options.Timezone
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	result, err := client.Query(ctx, q)
	if err != nil {
		return err
	}
	text := render(options.Endpoint, result.Report)
	n, err := io.WriteString(stdout, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

const caveat = "Persisted successful Turns observed by the Usage meter; best-effort and potentially incomplete. Optional-count coverage refers only to stored Turns."

func render(endpoint string, r report.Report) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Usage report — %s\nTimezone: %s | Range: %s to %s (exclusive) | Period: %s\nQuery time: %s\n\n", strconv.QuoteToASCII(endpoint), strconv.QuoteToASCII(r.Timezone), r.Since, r.Until, r.Period, r.GeneratedAt.Format(time.RFC3339Nano))
	for _, native := range []struct {
		title, heading string
		section        *report.Section
		cache          [2]cacheColumn
	}{
		{"Anthropic", "Period\tModel\tTurns\tUncached input\tOutput\tCache create\tCache read", r.Anthropic, [2]cacheColumn{{"cache_creation_input_tokens", "cache create"}, {"cache_read_input_tokens", "cache read"}}},
		{"OpenAI", "Period\tModel\tTurns\tInput\tOutput\tCache write\tCache read", r.OpenAI, [2]cacheColumn{{"cache_write_tokens", "cache write"}, {"cached_tokens", "cache read"}}},
	} {
		if native.section == nil {
			continue
		}
		fmt.Fprintln(&out, native.title)
		if native.section.Total.Turns == 0 {
			fmt.Fprintln(&out, "No stored Turns in the selected range.")
		} else {
			table := tabwriter.NewWriter(&out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(table, native.heading)
			buckets := map[string]report.Bucket{}
			for _, bucket := range r.Buckets {
				buckets[bucket.StartDate] = bucket
			}
			for _, row := range native.section.Rows {
				label := row.BucketStart
				bucket := buckets[label]
				if bucket.RangePartial {
					label += " [clipped]"
				}
				if bucket.InProgress {
					label += " [in progress]"
				}
				renderTotal(table, label, strconv.QuoteToASCII(row.Model), row.Total, native.cache)
			}
			_ = table.Flush()
		}
		fmt.Fprintln(&out, "Model totals")
		table := tabwriter.NewWriter(&out, 0, 4, 2, ' ', 0)
		for _, model := range native.section.Models {
			renderTotal(table, "Range", strconv.QuoteToASCII(model.Model), model.Total, native.cache)
		}
		renderTotal(table, "Section total", "", native.section.Total, native.cache)
		_ = table.Flush()
		fmt.Fprintln(&out)
	}
	fmt.Fprintln(&out, "\n"+caveat)
	return out.String()
}

type cacheColumn struct{ name, label string }

func renderTotal(out io.Writer, label, model string, total report.Total, cache [2]cacheColumn) {
	fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", label, model, count(total.Turns), metric(total, "input_tokens"), metric(total, "output_tokens"), metric(total, cache[0].name), metric(total, cache[1].name))
	for _, field := range cache {
		m := total.Usage[field.name]
		if m.ReportedTurns > 0 && m.ReportedTurns < total.Turns {
			fmt.Fprintf(out, "  %s: %s/%s stored Turns\n", field.label, count(m.ReportedTurns), count(total.Turns))
		}
	}
}
func metric(total report.Total, name string) string {
	m := total.Usage[name]
	if m.Sum == nil {
		return "—"
	}
	value := count(*m.Sum)
	if m.ReportedTurns < total.Turns {
		value += "*"
	}
	return value
}
func count(n int64) string {
	value := strconv.FormatInt(n, 10)
	for i := len(value) - 3; i > 0; i -= 3 {
		value = value[:i] + "," + value[i:]
	}
	return value
}
