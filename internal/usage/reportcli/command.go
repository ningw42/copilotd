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
	"unicode/utf8"

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

	localSystem *localTimezoneSystem
}

func Run(ctx context.Context, client *reporthttp.Client, options Options, stdout io.Writer) error {
	zone := options.Timezone
	if zone == nil {
		system := options.localSystem
		if system == nil {
			system = processTimezoneSystem()
		}
		name, err := system.discover()
		if err != nil {
			return err
		}
		zone = &name
	}
	if _, err := report.LoadTimezone(*zone); err != nil {
		return err
	}
	if model := options.Query.Model; model != nil && (*model == "" || !utf8.ValidString(*model)) {
		return fmt.Errorf("supply a non-empty valid UTF-8 Reported model")
	}
	if options.Timeout <= 0 {
		return fmt.Errorf("report timeout must be positive")
	}
	q := options.Query
	q.Timezone = *zone
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	result, err := client.Query(ctx, q)
	if err != nil {
		return err
	}
	var text string
	if options.JSON {
		text = string(result.JSON) + "\n"
	} else {
		text = render(options.Endpoint, result.Report, options.Details)
	}
	n, err := io.WriteString(stdout, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

const caveat = "Persisted successful Turns observed by the Usage meter; best-effort and potentially incomplete. Optional-count coverage refers only to stored Turns."

func render(endpoint string, r report.Report, details bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Usage report — %s\nTimezone: %s | Range: %s to %s (exclusive) | Period: %s\nQuery time: %s\n\n", strconv.QuoteToASCII(endpoint), strconv.QuoteToASCII(r.Timezone), r.Since, r.Until, r.Period, r.GeneratedAt.Format(time.RFC3339Nano))
	labels := map[string]string{}
	for _, bucket := range r.Buckets {
		label := bucket.StartDate
		if bucket.RangePartial {
			label += " [clipped]"
		}
		if bucket.InProgress {
			label += " [in progress]"
		}
		labels[bucket.StartDate] = label
	}
	for _, native := range []struct {
		title              string
		section            *report.Section
		primary, secondary []metricColumn
	}{
		{"Anthropic", r.Anthropic,
			[]metricColumn{{"input_tokens", "Uncached input"}, {"output_tokens", "Output"}, {"cache_creation_input_tokens", "Cache create"}, {"cache_read_input_tokens", "Cache read"}},
			[]metricColumn{{"thinking_tokens", "Thinking"}, {"ephemeral_5m_input_tokens", "Cache create 5m"}, {"ephemeral_1h_input_tokens", "Cache create 1h"}}},
		{"OpenAI", r.OpenAI,
			[]metricColumn{{"input_tokens", "Input"}, {"output_tokens", "Output"}, {"cache_write_tokens", "Cache write"}, {"cached_tokens", "Cache read"}},
			[]metricColumn{{"reasoning_tokens", "Reasoning"}, {"total_tokens", "Reported total"}}},
	} {
		if native.section == nil {
			continue
		}
		fmt.Fprintln(&out, native.title)
		if native.section.Total.Turns == 0 {
			fmt.Fprintln(&out, "No stored Turns in the selected range.")
		}
		renderTables(&out, labels, native.section, native.primary, "Model totals")
		if details {
			fmt.Fprintln(&out, "Secondary native counts")
			renderTables(&out, labels, native.section, native.secondary, "Secondary model totals")
		}
		fmt.Fprintln(&out)
	}
	fmt.Fprintln(&out, "\n"+caveat)
	return out.String()
}

type metricColumn struct{ name, label string }

// The two native projections share presentation mechanics, never aggregation.
// All writes here target the in-memory builder; Run owns fallible stdout writes.
func renderTables(out *strings.Builder, labels map[string]string, section *report.Section, columns []metricColumn, totalsHeading string) {
	if len(section.Rows) > 0 {
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprint(table, "Period\tModel\tTurns")
		for _, column := range columns {
			fmt.Fprintf(table, "\t%s", column.label)
		}
		fmt.Fprintln(table)
		for _, row := range section.Rows {
			renderTotal(table, labels[row.BucketStart], strconv.QuoteToASCII(row.Model), row.Total, columns)
		}
		_ = table.Flush()
	}
	fmt.Fprintln(out, totalsHeading)
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, model := range section.Models {
		renderTotal(table, "Range", strconv.QuoteToASCII(model.Model), model.Total, columns)
	}
	renderTotal(table, "Section total", "", section.Total, columns)
	_ = table.Flush()
}

func renderTotal(out io.Writer, label, model string, total report.Total, columns []metricColumn) {
	fmt.Fprintf(out, "%s\t%s\t%s", label, model, count(total.Turns))
	for _, column := range columns {
		fmt.Fprintf(out, "\t%s", metric(total, column.name))
	}
	fmt.Fprintln(out)
	for _, column := range columns {
		m := total.Usage[column.name]
		if m.ReportedTurns > 0 && m.ReportedTurns < total.Turns {
			fmt.Fprintf(out, "  %s: %s/%s stored Turns\n", strings.ToLower(column.label), count(m.ReportedTurns), count(total.Turns))
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
