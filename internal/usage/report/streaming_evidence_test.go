package report_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestQueryIndexedStreamingNativeEvidence(t *testing.T) {
	path := stored(t)
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000)
		INSERT INTO anthropic_turn(at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens)
		SELECT 1788220800000,'','',0,'native','sse',12,9 FROM n;
		WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000)
		INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens)
		SELECT 1788220800000,'','',0,'native','sse',8012,9 FROM n`); err != nil {
		t.Fatal(err)
	}
	q := selection()
	q.Surface = "all"
	started := time.Now()
	got, err := newReporter(t, path).Query(context.Background(), q)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		section *report.Section
		input   int64
	}{{got.Anthropic, 600000}, {got.OpenAI, 400600000}} {
		if want.section == nil || len(want.section.Rows) != 1 || len(want.section.Models) != 1 {
			t.Fatalf("native section: %+v", want.section)
		}
		for _, total := range []report.Total{want.section.Rows[0].Total, want.section.Models[0].Total, want.section.Total} {
			if total.Turns != 50000 || *total.Usage["input_tokens"].Sum != want.input || *total.Usage["output_tokens"].Sum != 450000 || total.Usage["input_tokens"].ReportedTurns != 50000 {
				t.Fatalf("native streamed totals: %+v", total)
			}
		}
	}
	// Observation, not a portable wall-clock performance guarantee. The fixed
	// HTTP work budget is exercised separately; slower native/race runs still
	// provide a useful measurement of the unchanged timestamp-indexed reader.
	t.Logf("100000 committed Turns, 2 native groups, no summaries/caches: Query=%s; Anthropic input=600000, OpenAI input=400600000, each output=450000", elapsed)
}
