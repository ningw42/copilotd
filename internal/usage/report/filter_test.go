package report_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestQueryRejectsOversizedModelFilterBeforeSQL(t *testing.T) {
	// Unlike raw HTTP, direct Query callers are not under the 16 KiB query cap.
	// Never compare a too-large matching identity before applying its size cap.
	model := strings.Repeat("x", report.MaxModelBytes+1)
	q := selection()
	q.Model = &model
	r, err := report.New(filepath.Join(t.TempDir(), "absent.db")).Query(context.Background(), q)
	var failure *report.Error
	if !errors.As(err, &failure) || failure.Code != report.TooLarge || r.OpenAI != nil {
		t.Fatalf("oversized filter before missing DB: %+v %v", r, err)
	}
}

// Paired with driver_characterization_test.go: Query exposes only the selected
// identity and exact native totals; an unfiltered scan rejects, never omits it.
func TestQueryExactFilterExcludesOversizedUnrelatedIdentity(t *testing.T) {
	path := stored(t, turn("2026-09-01T12:00:00Z", "模型", usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9}))
	db, err := sql.Open("sqlite", sqlitestore.LiteralFileURL(path).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'','',0,printf('%.*c',1048577,'x'),'buffered',1,1)`); err != nil {
		t.Fatal(err)
	}
	q := selection()
	model := "模型"
	q.Model = &model
	r, err := report.New(path).Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model == nil || *r.Model != model || len(r.OpenAI.Rows) != 1 || r.OpenAI.Rows[0].Model != model || len(r.OpenAI.Models) != 1 || r.OpenAI.Models[0].Model != model || r.OpenAI.Total.Turns != 1 || *r.OpenAI.Total.Usage["input_tokens"].Sum != 8012 {
		t.Fatalf("filtered: %+v", r)
	}
	q.Model = nil
	r, err = report.New(path).Query(context.Background(), q)
	var failure *report.Error
	if !errors.As(err, &failure) || failure.Code != report.TooLarge || r.OpenAI != nil {
		t.Fatalf("unfiltered: %+v %v", r, err)
	}
}

func TestQueryFiltersExactReportedModelAcrossNativeSurfaces(t *testing.T) {
	var turns []usage.Turn
	for _, model := range []string{"Model", "model", " model ", "é", "e\u0301", "模型", " ", "model\x00suffix", "' OR 1=1 --"} {
		requested := "requested-only"
		o := turn("2026-09-01T12:00:00Z", model, usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9, ReasoningTokens: ptr(4), TotalTokens: ptr(8021)})
		o.RequestedModel = &requested
		a := o
		a.Usage = usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9, CacheCreationInputTokens: ptr(2000), CacheReadInputTokens: ptr(6000)}
		turns = append(turns, a, o)
	}
	path := stored(t, turns...)
	for _, surface := range []string{"anthropic", "openai", "all"} {
		for _, model := range []string{"Model", "model", " model ", "é", "e\u0301", "模型", " ", "model\x00suffix", "' OR 1=1 --", "requested-only", "unknown", "MODEL"} {
			t.Run(surface+"/"+model, func(t *testing.T) {
				q := selection()
				q.Surface, q.Model = surface, &model
				r, err := report.New(path).Query(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				if r.Model == nil || *r.Model != model || r.Surface != surface {
					t.Fatalf("effective filter: %+v", r)
				}
				for _, native := range []struct {
					name    string
					section *report.Section
					input   int64
				}{{"anthropic", r.Anthropic, 12}, {"openai", r.OpenAI, 8012}} {
					if surface != "all" && surface != native.name {
						if native.section != nil {
							t.Fatal("unselected section present")
						}
						continue
					}
					s := native.section
					if s == nil {
						t.Fatal("selected section absent")
					}
					if model == "requested-only" || model == "unknown" || model == "MODEL" {
						if s.Rows == nil || s.Models == nil || len(s.Rows) != 0 || len(s.Models) != 0 || s.Total.Turns != 0 || *s.Total.Usage["input_tokens"].Sum != 0 {
							t.Fatalf("selected empty: %+v", s)
						}
						continue
					}
					if len(s.Rows) != 1 || len(s.Models) != 1 || s.Rows[0].Model != model || s.Models[0].Model != model {
						t.Fatalf("exact identity: %+v", s)
					}
					for _, total := range []report.Total{s.Rows[0].Total, s.Models[0].Total, s.Total} {
						if total.Turns != 1 || *total.Usage["input_tokens"].Sum != native.input || *total.Usage["output_tokens"].Sum != 9 {
							t.Fatalf("native total: %+v", total)
						}
					}
				}
			})
		}
	}
}
