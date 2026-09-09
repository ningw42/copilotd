// Package reportcli orchestrates a one-shot HTTP report and safe terminal
// presentation. It never opens SQLite or recomputes report aggregates.
package reportcli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
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
	} else if _, err := report.LoadTimezone(*zone); err != nil {
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
		renderTables(&out, native.section, native.primary)
		if details {
			fmt.Fprintln(&out, "Secondary native counts")
			renderTables(&out, native.section, native.secondary)
		}
		fmt.Fprintln(&out)
	}
	fmt.Fprintln(&out, "\n"+caveat)
	return out.String()
}

type metricColumn struct{ name, label string }

// The two native projections share presentation mechanics, never aggregation.
// All writes here target the in-memory builder; Run owns fallible stdout writes.
func renderTables(out *strings.Builder, section *report.Section, columns []metricColumn) {
	rows, notes := hierarchicalRows(section, columns)
	if len(rows) == 0 {
		return
	}
	renderTable(out, tableHeaders(columns), rows)
	renderCoverage(out, notes)
}

func hierarchicalRows(section *report.Section, columns []metricColumn) ([][]string, []string) {
	byPeriod := make(map[string][]report.Row, len(section.Periods))
	for _, row := range section.Rows {
		byPeriod[row.BucketStart] = append(byPeriod[row.BucketStart], row)
	}

	rows := make([][]string, 0, len(section.Rows)+len(section.Periods))
	var notes []string
	if section.Periods == nil {
		for _, group := range orderedRows(section.Rows) {
			for i, model := range group.models {
				period := ""
				if i == 0 {
					period = group.bucket
				}
				rendered, coverage := renderTotal(period, modelLabel(i, len(group.models), model.Model), model.Total, columns)
				rows = append(rows, rendered)
				notes = append(notes, coverage...)
			}
		}
		return rows, notes
	}

	for _, period := range section.Periods {
		rendered, coverage := renderTotal(period.BucketStart, "All", period.Total, columns)
		rows = append(rows, rendered)
		notes = append(notes, coverage...)
		models := byPeriod[period.BucketStart]
		for i, model := range models {
			rendered, coverage = renderTotal("", modelLabel(i, len(models), model.Model), model.Total, columns)
			rows = append(rows, rendered)
			notes = append(notes, coverage...)
		}
	}
	return rows, notes
}

type periodRows struct {
	bucket string
	models []report.Row
}

func orderedRows(rows []report.Row) []periodRows {
	var grouped []periodRows
	for _, row := range rows {
		if len(grouped) == 0 || grouped[len(grouped)-1].bucket != row.BucketStart {
			grouped = append(grouped, periodRows{bucket: row.BucketStart})
		}
		grouped[len(grouped)-1].models = append(grouped[len(grouped)-1].models, row)
	}
	return grouped
}

func modelLabel(index, total int, model string) string {
	branch := "├─ "
	if index == total-1 {
		branch = "└─ "
	}
	return branch + strconv.QuoteToASCII(model)
}

func tableHeaders(columns []metricColumn) []string {
	headers := []string{"Period", "Model", "Turns"}
	for _, column := range columns {
		headers = append(headers, column.label)
	}
	return headers
}

func renderTable(out *strings.Builder, headers []string, rows [][]string) {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		Headers(headers...).
		Rows(rows...).
		Wrap(false).
		StyleFunc(func(_ int, column int) lipgloss.Style {
			style := lipgloss.NewStyle().Padding(0, 1)
			if column >= 2 {
				style = style.Align(lipgloss.Right)
			}
			return style
		})
	fmt.Fprintln(out, t.Render())
}

func renderTotal(label, model string, total report.Total, columns []metricColumn) ([]string, []string) {
	row := []string{label, model, count(total.Turns)}
	for _, column := range columns {
		row = append(row, metric(total, column.name))
	}
	var coverage []string
	for _, column := range columns {
		m := total.Usage[column.name]
		if m.ReportedTurns > 0 && m.ReportedTurns < total.Turns {
			coverage = append(coverage, fmt.Sprintf("%s: %s/%s stored Turns", strings.ToLower(column.label), count(m.ReportedTurns), count(total.Turns)))
		}
	}
	return row, coverage
}

func renderCoverage(out *strings.Builder, notes []string) {
	for _, note := range notes {
		fmt.Fprintf(out, "  %s\n", note)
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
