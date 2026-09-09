package reporthttp

import (
	"context"
	"encoding/json"

	"github.com/ningw42/copilotd/internal/usage/report"
)

const MaxBodyBytes = 8 << 20

// Encode only bounded fragments, never the complete report in encoding/json's
// unbounded internal buffer. Each model-bearing fragment contains one guarded
// identity; repeated identities cannot cause a whole-report allocation.
func encodeReport(ctx context.Context, result report.Report) ([]byte, error) {
	out := boundedJSON{ctx: ctx}
	// The effective filter is also an identity-bearing fragment. Check before
	// Marshal, including when a direct Query provider bypasses raw HTTP limits.
	if result.Model != nil && len(*result.Model) > report.MaxModelBytes {
		return nil, &report.Error{Code: report.TooLarge, Message: "Model exceeds the report size limit."}
	}
	header, err := json.Marshal(struct {
		*report.Report
		Buckets   *int `json:"buckets,omitempty"`
		Anthropic *int `json:"anthropic,omitempty"`
		OpenAI    *int `json:"openai,omitempty"`
	}{Report: &result})
	if err != nil {
		return nil, err
	}
	out.append(header[:len(header)-1])
	out.append([]byte(`,"buckets":[`))
	for i, bucket := range result.Buckets {
		if i > 0 {
			out.append([]byte(","))
		}
		out.value(bucket)
	}
	out.append([]byte("]"))
	for _, native := range []struct {
		name    string
		section *report.Section
	}{{"anthropic", result.Anthropic}, {"openai", result.OpenAI}} {
		if native.section == nil {
			continue
		}
		out.append([]byte(`,"` + native.name + `":{"rows":[`))
		for i, row := range native.section.Rows {
			if i > 0 {
				out.append([]byte(","))
			}
			out.model(row.Model, row)
		}
		out.append([]byte(`],"models":[`))
		for i, model := range native.section.Models {
			if i > 0 {
				out.append([]byte(","))
			}
			out.model(model.Model, model)
		}
		out.append([]byte(`],"total":`))
		out.value(native.section.Total)
		out.append([]byte("}"))
	}
	out.append([]byte("}"))
	return out.body, out.err
}

type boundedJSON struct {
	ctx  context.Context
	body []byte
	err  error
}

func (b *boundedJSON) append(fragment []byte) {
	if b.err != nil {
		return
	}
	if b.err = b.ctx.Err(); b.err != nil {
		return
	}
	if len(fragment) > MaxBodyBytes-len(b.body) {
		b.err = &report.Error{Code: report.TooLarge, Message: "Encoded report exceeds 8 MiB; narrow the range or model/Surface selection."}
		return
	}
	b.body = append(b.body, fragment...)
}
func (b *boundedJSON) value(value any) {
	if b.err != nil {
		return
	}
	if b.err = b.ctx.Err(); b.err != nil {
		return
	}
	fragment, err := json.Marshal(value)
	if err != nil {
		b.err = err
		return
	}
	b.append(fragment)
}
func (b *boundedJSON) model(model string, value any) {
	if len(model) > report.MaxModelBytes {
		b.err = &report.Error{Code: report.TooLarge, Message: "Model exceeds the report size limit."}
		return
	}
	b.value(value)
}
