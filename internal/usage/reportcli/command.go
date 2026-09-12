// Package reportcli orchestrates a one-shot HTTP report and safe terminal
// presentation. It never opens SQLite or replaces server aggregates; text
// derives checked, presentation-only period subtotals.
package reportcli

import (
	"context"
	"fmt"
	"io"
	"math"
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
		text, err = render(lipgloss.NewRenderer(stdout), options.Endpoint, result.Report, options.Details)
		if err != nil {
			return err
		}
	}
	n, err := io.WriteString(stdout, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	return err
}

var (
	anthropicSurfaceColor = lipgloss.Color("#D97757")
	openAISurfaceColor    = lipgloss.Color("#3C6AC8")
)

func render(renderer *lipgloss.Renderer, endpoint string, r report.Report, details bool) (string, error) {
	var out strings.Builder
	surfaceTextColor := terminalBackgroundColor(renderer)
	fmt.Fprintf(&out, "Usage report — %s\nTimezone: %s | Range: %s to %s (exclusive) | Period: %s\nQuery time: %s\n\n", escapeASCII(endpoint), r.Timezone, r.Since, r.Until, r.Period, r.GeneratedAt.Format(time.RFC3339Nano))
	for _, native := range []struct {
		title              string
		background         lipgloss.TerminalColor
		section            *report.Section
		primary, secondary []metricColumn
	}{
		{"Anthropic", anthropicSurfaceColor, r.Anthropic,
			[]metricColumn{{"input_tokens", "Uncached input"}, {"output_tokens", "Output"}, {"cache_creation_input_tokens", "Cache create"}, {"cache_read_input_tokens", "Cache read"}},
			[]metricColumn{{"thinking_tokens", "Thinking"}, {"ephemeral_5m_input_tokens", "Cache create 5m"}, {"ephemeral_1h_input_tokens", "Cache create 1h"}}},
		{"OpenAI", openAISurfaceColor, r.OpenAI,
			[]metricColumn{{"input_tokens", "Input"}, {"output_tokens", "Output"}, {"cache_write_tokens", "Cache write"}, {"cached_tokens", "Cache read"}},
			[]metricColumn{{"reasoning_tokens", "Reasoning"}, {"total_tokens", "Reported total"}}},
	} {
		if native.section == nil {
			continue
		}
		fmt.Fprintln(&out, surfaceTitleStyle(renderer, surfaceTextColor, native.background).Render(native.title))
		if native.section.Total.Turns == 0 {
			fmt.Fprintln(&out, "No stored Turns in the selected range.")
		}
		if err := renderTables(renderer, &out, r.Period, native.section, native.primary); err != nil {
			return "", err
		}
		if details {
			fmt.Fprintln(&out, "Secondary native counts")
			if err := renderTables(renderer, &out, r.Period, native.section, native.secondary); err != nil {
				return "", err
			}
		}
		fmt.Fprintln(&out)
	}
	return out.String(), nil
}

func terminalBackgroundColor(renderer *lipgloss.Renderer) lipgloss.TerminalColor {
	background := fmt.Sprint(renderer.Output().BackgroundColor())
	if background == "" {
		return lipgloss.NoColor{}
	}
	return lipgloss.Color(background)
}

func surfaceTitleStyle(renderer *lipgloss.Renderer, foreground, background lipgloss.TerminalColor) lipgloss.Style {
	return renderer.NewStyle().
		Bold(true).
		Foreground(foreground).
		Background(background).
		Padding(0, 1)
}

type metricColumn struct{ name, label string }

// The two native projections share presentation mechanics. Period totals are
// derived only for terminal display; errors reach Run before its stdout write.
func renderTables(renderer *lipgloss.Renderer, out *strings.Builder, period string, section *report.Section, columns []metricColumn) error {
	rows, notes, err := groupedRows(section.Rows, columns)
	if err != nil || len(rows) == 0 {
		return err
	}
	renderTable(renderer, out, tableHeaders(period, columns), rows)
	renderCoverage(out, notes)
	return nil
}

func groupedRows(rows []report.Row, columns []metricColumn) ([][]string, []string, error) {
	renderedGroups := make([][]string, 0, 2*len(rows))
	var notes []string
	for start := 0; start < len(rows); {
		bucket := rows[start].BucketStart
		end := start + 1
		for end < len(rows) && rows[end].BucketStart == bucket {
			end++
		}

		total, err := periodTotal(rows[start:end], columns)
		if err != nil {
			return nil, nil, err
		}
		renderedTotal, totalCoverage := renderTotal("Total", total, columns)
		renderedGroups = append(renderedGroups, append([]string{bucket}, renderedTotal...))
		for _, note := range totalCoverage {
			notes = append(notes, bucket+" / Total — "+note)
		}

		cells := make([][]string, 3+len(columns))
		for index := start; index < end; index++ {
			model := escapeModel(rows[index].Model)
			cells[0] = append(cells[0], "")
			rendered, coverage := renderTotal(model, rows[index].Total, columns)
			for column, value := range rendered {
				cells[column+1] = append(cells[column+1], value)
			}
			for _, note := range coverage {
				notes = append(notes, bucket+" / "+model+" — "+note)
			}
		}
		groupRow := make([]string, len(cells))
		for column := range cells {
			groupRow[column] = strings.Join(cells[column], "\n")
		}
		renderedGroups = append(renderedGroups, groupRow)
		start = end
	}
	return renderedGroups, notes, nil
}

// periodTotal derives a terminal-only subtotal from validated model rows.
// Independently valid rows may be mutually inconsistent; checked int64 addition
// rejects their overflow rather than wrapping, saturating, or changing JSON.
func periodTotal(rows []report.Row, columns []metricColumn) (report.Total, error) {
	total := report.Total{Usage: make(map[string]report.Metric, len(columns))}
	for _, row := range rows {
		if err := addPeriodCount(&total.Turns, row.Turns, "Turns"); err != nil {
			return report.Total{}, err
		}
		for _, column := range columns {
			source := row.Usage[column.name]
			metric := total.Usage[column.name]
			if err := addPeriodCount(&metric.ReportedTurns, source.ReportedTurns, column.label+" coverage"); err != nil {
				return report.Total{}, err
			}
			if source.Sum != nil {
				if metric.Sum == nil {
					metric.Sum = new(int64)
				}
				if err := addPeriodCount(metric.Sum, *source.Sum, column.label+" sum"); err != nil {
					return report.Total{}, err
				}
			}
			total.Usage[column.name] = metric
		}
	}
	return total, nil
}

func addPeriodCount(total *int64, value int64, field string) error {
	if value > math.MaxInt64-*total {
		return periodSubtotalOverflow(field)
	}
	*total += value
	return nil
}

func periodSubtotalOverflow(field string) error {
	return fmt.Errorf("terminal text period subtotal exceeds int64 while accumulating %s; narrow the date range or model/Surface selection", field)
}

func escapeASCII(value string) string {
	quoted := strconv.QuoteToASCII(value)
	return quoted[1 : len(quoted)-1]
}

func escapeModel(model string) string {
	start := 0
	for start < len(model) && model[start] == ' ' {
		start++
	}
	end := len(model)
	for end > start && model[end-1] == ' ' {
		end--
	}

	var escaped strings.Builder
	escaped.Grow(len(model))
	for range start {
		escaped.WriteString(`\x20`)
	}
	quoted := strconv.QuoteToASCII(model[start:end])
	escaped.WriteString(quoted[1 : len(quoted)-1])
	for range len(model) - end {
		escaped.WriteString(`\x20`)
	}
	return escaped.String()
}

func tableHeaders(period string, columns []metricColumn) []string {
	heading := strings.ToUpper(period[:1]) + period[1:]
	headers := []string{heading, "Model(s)", "Turns"}
	for _, column := range columns {
		headers = append(headers, column.label)
	}
	return headers
}

func renderTable(renderer *lipgloss.Renderer, out *strings.Builder, headers []string, rows [][]string) {
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderRow(true).
		Headers(headers...).
		Rows(rows...).
		Wrap(true).
		StyleFunc(func(_ int, column int) lipgloss.Style {
			style := renderer.NewStyle().Padding(0, 1)
			if column >= 2 {
				style = style.Align(lipgloss.Right)
			}
			return style
		})
	fmt.Fprintln(out, t.Render())
}

func renderTotal(model string, total report.Total, columns []metricColumn) ([]string, []string) {
	row := []string{model, count(total.Turns)}
	var coverage []string
	for _, column := range columns {
		metric := total.Usage[column.name]
		value := "—"
		if metric.Sum != nil {
			value = count(*metric.Sum)
			if metric.ReportedTurns < total.Turns {
				value += "*"
			}
		}
		row = append(row, value)
		if metric.ReportedTurns > 0 && metric.ReportedTurns < total.Turns {
			coverage = append(coverage, fmt.Sprintf("%s: %s/%s stored Turns", strings.ToLower(column.label), count(metric.ReportedTurns), count(total.Turns)))
		}
	}
	return row, coverage
}

func renderCoverage(out *strings.Builder, notes []string) {
	for _, note := range notes {
		fmt.Fprintf(out, "  %s\n", note)
	}
}

func count(n int64) string {
	return commaCount(strconv.FormatInt(n, 10))
}

func commaCount(value string) string {
	for i := len(value) - 3; i > 0; i -= 3 {
		value = value[:i] + "," + value[i:]
	}
	return value
}
