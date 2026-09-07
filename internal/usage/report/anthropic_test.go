package report_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func anthropicTurn(date, model string, counts usage.AnthropicUsage) usage.Turn {
	t := turn(date, model, usage.OpenAIUsage{})
	t.Usage = counts
	return t
}

func TestQueryCombinedHistoryPreservesAttributionAndCoverage(t *testing.T) {
	first := anthropicTurn("2026-09-01T00:00:00Z", "Claude", usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9, CacheCreationInputTokens: ptr(2000), CacheReadInputTokens: ptr(6000), Ephemeral5mInputTokens: ptr(750), Ephemeral1hInputTokens: ptr(1250), ThinkingTokens: ptr(4)})
	second := anthropicTurn("2026-09-01T12:00:00Z", "Claude", usage.AnthropicUsage{InputTokens: 10, OutputTokens: 3})
	requested1, requested2 := "alias-one", "alias-two"
	first.RequestedModel, second.RequestedModel = &requested1, &requested2
	first.ResponseID, second.ResponseID = "repeated", "repeated"
	second.Transport = usage.TransportSSE
	path := stored(t, first, second, anthropicTurn("2026-09-02T00:00:00Z", "claude", usage.AnthropicUsage{CacheCreationInputTokens: ptr(0), CacheReadInputTokens: ptr(0)}))
	// A later writer/process run contributes to the same database, not a
	// reporter-owned cache or current-writer attribution scope.
	writer, err := sqlitestore.Open(path, logging.ForComponent(slog.New(slog.NewTextHandler(io.Discard, nil)), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	writer.Record(turn("2026-09-01T00:00:00Z", "Claude", usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9}))
	if result := writer.Close(context.Background()); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("second writer: %+v", result)
	}
	for _, surface := range []string{"", "all"} {
		q := selection()
		q.Surface = surface
		got, err := report.New(path).Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if got.Surface != "all" || got.Anthropic == nil || got.OpenAI == nil {
			t.Fatalf("combined selection: %+v", got)
		}
		a, o := got.Anthropic, got.OpenAI
		want := report.Total{Turns: 3, Usage: map[string]report.Metric{
			"input_tokens": {Sum: ptr(22), ReportedTurns: 3}, "output_tokens": {Sum: ptr(12), ReportedTurns: 3},
			"cache_creation_input_tokens": {Sum: ptr(2000), ReportedTurns: 2}, "cache_read_input_tokens": {Sum: ptr(6000), ReportedTurns: 2},
			"ephemeral_5m_input_tokens": {Sum: ptr(750), ReportedTurns: 1}, "ephemeral_1h_input_tokens": {Sum: ptr(1250), ReportedTurns: 1}, "thinking_tokens": {Sum: ptr(4), ReportedTurns: 1},
		}}
		if !reflect.DeepEqual(a.Total, want) || len(a.Rows) != 2 || len(a.Models) != 2 || a.Models[0].Model != "Claude" || a.Models[1].Model != "claude" || a.Rows[1].BucketStart != "2026-09-02" {
			t.Fatalf("attribution/total: %+v", a)
		}
		if a.Rows[0].Turns != 2 || a.Rows[0].Usage["cache_read_input_tokens"].ReportedTurns != 1 || *a.Rows[0].Usage["cache_read_input_tokens"].Sum != 6000 || !reflect.DeepEqual(a.Rows[0].Total, a.Models[0].Total) {
			t.Fatalf("mixed coverage: %+v", a.Rows[0])
		}
		if a.Rows[1].Usage["cache_read_input_tokens"].ReportedTurns != 1 || *a.Rows[1].Usage["cache_read_input_tokens"].Sum != 0 || a.Rows[1].Usage["thinking_tokens"].Sum != nil || a.Rows[1].Usage["thinking_tokens"].ReportedTurns != 0 {
			t.Fatalf("zero vs NULL: %+v", a.Rows[1])
		}
		if o.Total.Turns != 1 || *o.Total.Usage["input_tokens"].Sum != 8012 || *o.Total.Usage["output_tokens"].Sum != 9 || o.Models[0].Model != "Claude" {
			t.Fatalf("native Surface identity: %+v", o)
		}
	}
}

func TestQueryDistinguishesSelectedEmptyAndUnselectedNativeSections(t *testing.T) {
	path := stored(t, turn("2026-09-01T00:00:00Z", "only-openai", usage.OpenAIUsage{InputTokens: 7, OutputTokens: 2}))
	for _, surface := range []string{"anthropic", "openai", "all", ""} {
		q := selection()
		q.Surface = surface
		got, err := report.New(path).Query(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if surface == "openai" {
			if got.Anthropic != nil {
				t.Fatal("unselected Anthropic section invented")
			}
		} else {
			a := got.Anthropic
			if a == nil || a.Rows == nil || a.Models == nil || len(a.Rows) != 0 || len(a.Models) != 0 || a.Total.Turns != 0 || len(a.Total.Usage) != 7 {
				t.Fatalf("empty section: %+v", a)
			}
			for name, m := range a.Total.Usage {
				if m.ReportedTurns != 0 {
					t.Fatalf("empty coverage: %s %+v", name, m)
				}
				if name == "input_tokens" || name == "output_tokens" {
					if m.Sum == nil || *m.Sum != 0 {
						t.Fatalf("empty required sum: %s %+v", name, m)
					}
				} else if m.Sum != nil {
					t.Fatalf("invented optional zero: %s %+v", name, m)
				}
			}
		}
		if surface == "anthropic" {
			if got.OpenAI != nil {
				t.Fatal("unselected OpenAI section invented")
			}
		} else if got.OpenAI == nil || got.OpenAI.Total.Turns != 1 {
			t.Fatalf("selected OpenAI history: %+v", got.OpenAI)
		}
	}
}

func TestQueryFailsAtomicallyForEitherSelectedNativeSection(t *testing.T) {
	for _, tc := range []struct {
		name, table, change string
		code                report.Code
	}{
		{"Anthropic negative optional", "anthropic_turn", "thinking_tokens=-1", report.Unavailable},
		{"Anthropic invalid UTF8", "anthropic_turn", "model=CAST(x'ff' AS TEXT)", report.Unavailable},
		{"Anthropic oversized model", "anthropic_turn", "model=printf('%.*c',1048577,'x')", report.TooLarge},
		{"Anthropic input overflow", "anthropic_turn", "input_tokens=9223372036854775807", report.Overflow},
		{"Anthropic output overflow", "anthropic_turn", "output_tokens=9223372036854775807", report.Overflow},
		{"Anthropic cache creation overflow", "anthropic_turn", "cache_creation_input_tokens=9223372036854775807", report.Overflow},
		{"Anthropic cache read overflow", "anthropic_turn", "cache_read_input_tokens=9223372036854775807", report.Overflow},
		{"Anthropic five-minute overflow", "anthropic_turn", "ephemeral_5m_input_tokens=9223372036854775807", report.Overflow},
		{"Anthropic one-hour overflow", "anthropic_turn", "ephemeral_1h_input_tokens=9223372036854775807", report.Overflow},
		{"Anthropic thinking overflow", "anthropic_turn", "thinking_tokens=9223372036854775807", report.Overflow},
		{"OpenAI negative", "openai_turn", "input_tokens=-1", report.Unavailable},
		{"OpenAI overflow", "openai_turn", "input_tokens=9223372036854775807", report.Overflow},
		{"OpenAI oversized model", "openai_turn", "model=printf('%.*c',1048577,'x')", report.TooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := anthropicTurn("2026-09-01T00:00:00Z", "a", usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9})
			o := turn("2026-09-01T00:00:00Z", "o", usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9})
			path := stored(t, a, o, a, o)
			db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("UPDATE " + tc.table + " SET " + tc.change); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			q := selection()
			q.Surface = "all"
			got, err := report.New(path).Query(context.Background(), q)
			var failure *report.Error
			if !errors.As(err, &failure) || failure.Code != tc.code || !reflect.DeepEqual(got, report.Report{}) {
				t.Fatalf("whole report must fail: %+v %v", got, err)
			}
			if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "UPDATE") {
				t.Fatal("private error escaped")
			}
			q.Surface = "anthropic"
			if tc.table == "anthropic_turn" {
				q.Surface = "openai"
			}
			if _, err := report.New(path).Query(context.Background(), q); err != nil {
				t.Fatalf("unselected bad rows must not be examined: %v", err)
			}
		})
	}
}

func TestQuerySharesRetainedModelIdentityAcrossNativeSections(t *testing.T) {
	model := strings.Repeat("m", 600000)
	path := stored(t,
		anthropicTurn("2026-09-01T00:00:00Z", model, usage.AnthropicUsage{InputTokens: 12}),
		anthropicTurn("2026-09-02T00:00:00Z", model, usage.AnthropicUsage{InputTokens: 10}),
		turn("2026-09-01T00:00:00Z", model, usage.OpenAIUsage{InputTokens: 8012}))
	q := selection()
	q.Surface = "all"
	got, err := report.New(path).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Anthropic.Total.Turns != 2 || *got.Anthropic.Total.Usage["input_tokens"].Sum != 22 || len(got.Anthropic.Rows) != 2 || got.OpenAI.Total.Turns != 1 || *got.OpenAI.Total.Usage["input_tokens"].Sum != 8012 || got.Anthropic.Models[0].Model != model || got.OpenAI.Models[0].Model != model {
		t.Fatal("shared exact model identity did not retain independent native groups")
	}
}

func TestQueryDailyAnthropicNativeCounts(t *testing.T) {
	path := stored(t, anthropicTurn("2026-09-01T00:00:00Z", "Claude", usage.AnthropicUsage{
		InputTokens: 12, OutputTokens: 9, CacheCreationInputTokens: ptr(2000), CacheReadInputTokens: ptr(6000),
		Ephemeral5mInputTokens: ptr(750), Ephemeral1hInputTokens: ptr(1250), ThinkingTokens: ptr(4),
	}))
	q := selection()
	q.Surface = "anthropic"
	got, err := report.New(path).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	want := report.Total{Turns: 1, Usage: map[string]report.Metric{
		"input_tokens": {Sum: ptr(12), ReportedTurns: 1}, "output_tokens": {Sum: ptr(9), ReportedTurns: 1},
		"cache_creation_input_tokens": {Sum: ptr(2000), ReportedTurns: 1}, "cache_read_input_tokens": {Sum: ptr(6000), ReportedTurns: 1},
		"ephemeral_5m_input_tokens": {Sum: ptr(750), ReportedTurns: 1}, "ephemeral_1h_input_tokens": {Sum: ptr(1250), ReportedTurns: 1},
		"thinking_tokens": {Sum: ptr(4), ReportedTurns: 1},
	}}
	s := got.Anthropic
	if got.Surface != "anthropic" || got.OpenAI != nil || s == nil || len(s.Rows) != 1 || len(s.Models) != 1 {
		t.Fatalf("selected section: %+v", got)
	}
	if s.Rows[0].BucketStart != "2026-09-01" || s.Rows[0].Model != "Claude" || s.Models[0].Model != "Claude" ||
		!reflect.DeepEqual(s.Total, want) || !reflect.DeepEqual(s.Rows[0].Total, want) || !reflect.DeepEqual(s.Models[0].Total, want) {
		t.Fatalf("native counts must stay independent, not normalized: %+v", s)
	}
}
