package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
)

func TestStoreRoundTripsNullableOpenAIServiceTierExactly(t *testing.T) {
	path, store := openStore(t, io.Discard)
	values := []*string{
		nil,
		textPtr(""),
		textPtr("default"),
		textPtr("future-tier"),
		textPtr("  \t\n  "),
		textPtr("control\x00byte"),
		textPtr("雪🚀�"),
	}
	for index, serviceTier := range values {
		requested := fmt.Sprintf("requested-%d", index)
		store.Record(usage.Turn{
			At: time.UnixMilli(int64(index + 1)), RequestID: "request", ResponseID: fmt.Sprintf("response-%d", index),
			Model: fmt.Sprintf("reported-%d", index), RequestedModel: &requested, OpenAIServiceTier: serviceTier,
			Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2},
		})
	}
	if report := closeStore(t, store); report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatal(report)
	}

	db := openExternal(t, path)
	rows, err := db.Query(`SELECT model, requested_model, service_tier FROM openai_turn ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for index, want := range values {
		if !rows.Next() {
			t.Fatalf("missing row %d: %v", index, rows.Err())
		}
		var model, requested string
		var serviceTier sql.NullString
		if err := rows.Scan(&model, &requested, &serviceTier); err != nil {
			t.Fatal(err)
		}
		wantTier := sql.NullString{}
		if want != nil {
			wantTier = sql.NullString{String: *want, Valid: true}
		}
		if model != fmt.Sprintf("reported-%d", index) || requested != fmt.Sprintf("requested-%d", index) || serviceTier != wantTier {
			t.Errorf("row %d = model:%q requested:%q service_tier:%+v, want exact independent values and %+v", index, model, requested, serviceTier, wantTier)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected extra row or scan error: %v", rows.Err())
	}
}

func TestStoreIgnoresOpenAIServiceTierOnAnthropicTurns(t *testing.T) {
	path, store := openStore(t, io.Discard)
	serviceTier := "unsupported-metadata"
	store.Record(usage.Turn{
		At: time.UnixMilli(1), ResponseID: "message", Model: "reported", OpenAIServiceTier: &serviceTier,
		Transport: usage.TransportBuffered, Usage: usage.AnthropicUsage{InputTokens: 1, OutputTokens: 2},
	})
	if report := closeStore(t, store); report.FinalFlushLosses != 0 || report.RuntimeWriteLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	db := openExternal(t, path)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM anthropic_turn WHERE message_id='message'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("Anthropic row count = %d, %v; want 1", count, err)
	}
	var tierColumns int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_xinfo('anthropic_turn') WHERE name='service_tier'`).Scan(&tierColumns); err != nil || tierColumns != 0 {
		t.Fatalf("Anthropic service_tier column count = %d, %v; want 0", tierColumns, err)
	}
}

func textPtr(value string) *string { return &value }
