package sqlitestore_test

import (
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
)

func TestStoreRoundTripsNullableRequestedModelInBothTables(t *testing.T) {
	path, store := openStore(t, io.Discard)
	empty, exact := "", "  GPT.Alias\t雪\n  "
	for _, requested := range []*string{nil, &empty, &exact} {
		for _, native := range []usage.Usage{
			usage.OpenAIUsage{InputTokens: 12, OutputTokens: 6},
			usage.AnthropicUsage{InputTokens: 12, OutputTokens: 6},
		} {
			store.Record(usage.Turn{
				At: time.UnixMilli(1), RequestID: "reused", ResponseID: "reused",
				Model: "reported", RequestedModel: requested, Transport: usage.TransportBuffered, Usage: native,
			})
		}
	}
	if report := closeStore(t, store); report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	db := openExternal(t, path)
	for _, table := range []string{"anthropic_turn", "openai_turn"} {
		rows, err := db.Query("SELECT model, requested_model FROM " + table + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for _, want := range []sql.NullString{{}, {String: "", Valid: true}, {String: "  GPT.Alias\t雪\n  ", Valid: true}} {
			if !rows.Next() {
				t.Fatalf("%s: missing row: %v", table, rows.Err())
			}
			var model string
			var requested sql.NullString
			if err := rows.Scan(&model, &requested); err != nil {
				t.Fatal(err)
			}
			if model != "reported" || requested != want {
				t.Errorf("%s model/requested = %q/%+v, want reported/%+v", table, model, requested, want)
			}
		}
		if rows.Next() || rows.Err() != nil {
			t.Fatalf("%s: unexpected extra row or error: %v", table, rows.Err())
		}
	}
}
