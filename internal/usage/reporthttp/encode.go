package reporthttp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
)

const MaxBodyBytes = 8 << 20

// Encode only bounded fragments, never the complete report in encoding/json's
// unbounded internal buffer. Each model-bearing fragment guards its Reported
// and Pricing model identities before marshal.
func encodeReport(ctx context.Context, result report.Report) ([]byte, error) {
	out := boundedJSON{ctx: ctx}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePricingProvenance(result.Pricing); err != nil {
		return nil, err
	}
	// The effective filter is also an identity-bearing fragment. Check before
	// Marshal, including when a direct Query provider bypasses raw HTTP limits.
	if result.Model != nil && len(*result.Model) > report.MaxModelBytes {
		return nil, &report.Error{Code: report.TooLarge, Message: "Model exceeds the report size limit."}
	}
	header, err := json.Marshal(struct {
		*report.Report
		Buckets   *int         `json:"buckets,omitempty"`
		Pricing   *wirePricing `json:"pricing,omitempty"`
		Anthropic *int         `json:"anthropic,omitempty"`
		OpenAI    *int         `json:"openai,omitempty"`
	}{Report: &result, Pricing: pricingForWire(result.Pricing)})
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
			model := modelTotalForWire(row.ModelTotal, result.Pricing != nil)
			out.model(model, wireRow{
				BucketStart:    row.BucketStart,
				wireModelTotal: model,
			})
		}
		out.append([]byte(`],"models":[`))
		for i, model := range native.section.Models {
			if i > 0 {
				out.append([]byte(","))
			}
			wire := modelTotalForWire(model, result.Pricing != nil)
			out.model(wire, wire)
		}
		out.append([]byte(`],"total":`))
		out.value(totalForWire(native.section.Total, result.Pricing != nil))
		out.append([]byte("}"))
	}
	out.append([]byte("}"))
	return out.body, out.err
}

type wirePricing struct {
	Dataset          string     `json:"dataset"`
	Currency         string     `json:"currency"`
	Basis            string     `json:"basis"`
	ContextPolicy    string     `json:"context_policy"`
	CacheWritePolicy string     `json:"cache_write_policy"`
	Version          string     `json:"version"`
	Source           string     `json:"source"`
	LastSuccess      *time.Time `json:"last_success"`
}

type wireUnpricedCoverage struct {
	UnknownModel      int64 `json:"unknown_model,string"`
	AmbiguousModel    int64 `json:"ambiguous_model,string"`
	MissingRate       int64 `json:"missing_rate,string"`
	MissingUsage      int64 `json:"missing_usage,string"`
	InconsistentUsage int64 `json:"inconsistent_usage,string"`
}

type wireCost struct {
	Amount      *string              `json:"amount"`
	PricedTurns int64                `json:"priced_turns,string"`
	Unpriced    wireUnpricedCoverage `json:"unpriced"`
}

type wirePricingMatch struct {
	Status   report.PricingMatchStatus `json:"status"`
	Provider string                    `json:"provider,omitempty"`
	Model    string                    `json:"model,omitempty"`
	Method   report.PricingMatchMethod `json:"method,omitempty"`
}

type wireTotal struct {
	Turns int64                    `json:"turns,string"`
	Usage map[string]report.Metric `json:"usage"`
	Cost  *wireCost                `json:"cost,omitempty"`
}

type wireModelTotal struct {
	Model        string            `json:"model"`
	PricingMatch *wirePricingMatch `json:"pricing_match,omitempty"`
	wireTotal
}

type wireRow struct {
	BucketStart string `json:"bucket_start"`
	wireModelTotal
}

var errInvalidPricingProvenance = errors.New("invalid pricing report provenance")

// Keep provenance strings bounded before marshaling the header. Cost and match
// semantics are established by Reporter.Query, not revalidated by the encoder.
func validatePricingProvenance(provenance *report.PricingProvenance) error {
	if provenance == nil {
		return nil
	}
	if provenance.Dataset != "models.dev/api.json" || provenance.Currency != "USD" || provenance.Basis != "original_provider" || provenance.ContextPolicy != "highest_tier" || provenance.CacheWritePolicy != "single_rate" || !validContentVersion(provenance.Version) || provenance.Source != "fallback" && provenance.Source != "fetched" {
		return errInvalidPricingProvenance
	}
	if provenance.LastSuccess != nil {
		_, offset := provenance.LastSuccess.Zone()
		if provenance.LastSuccess.IsZero() || offset != 0 || provenance.LastSuccess.Year() < 0 || provenance.LastSuccess.Year() > 9999 {
			return errInvalidPricingProvenance
		}
	}
	return nil
}

func pricingForWire(value *report.PricingProvenance) *wirePricing {
	if value == nil {
		return nil
	}
	return &wirePricing{
		Dataset: value.Dataset, Currency: value.Currency, Basis: value.Basis,
		ContextPolicy: value.ContextPolicy, CacheWritePolicy: value.CacheWritePolicy,
		Version: value.Version, Source: value.Source, LastSuccess: value.LastSuccess,
	}
}

func totalForWire(value report.Total, pricingPresent bool) wireTotal {
	wire := wireTotal{Turns: value.Turns, Usage: value.Usage}
	if pricingPresent {
		var amount *string
		if value.Cost.Amount != nil {
			text := value.Cost.Amount.String()
			amount = &text
		}
		wire.Cost = &wireCost{
			Amount: amount, PricedTurns: value.Cost.PricedTurns,
			Unpriced: wireUnpricedCoverage{
				UnknownModel: value.Cost.Unpriced.UnknownModel, AmbiguousModel: value.Cost.Unpriced.AmbiguousModel,
				MissingRate: value.Cost.Unpriced.MissingRate, MissingUsage: value.Cost.Unpriced.MissingUsage,
				InconsistentUsage: value.Cost.Unpriced.InconsistentUsage,
			},
		}
	}
	return wire
}

func modelTotalForWire(value report.ModelTotal, pricingPresent bool) wireModelTotal {
	wire := wireModelTotal{Model: value.Model, wireTotal: totalForWire(value.Total, pricingPresent)}
	if pricingPresent {
		wire.PricingMatch = &wirePricingMatch{
			Status: value.PricingMatch.Status, Provider: value.PricingMatch.Provider,
			Model: value.PricingMatch.Model, Method: value.PricingMatch.Method,
		}
	}
	return wire
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
func (b *boundedJSON) model(model wireModelTotal, value any) {
	if b.err != nil {
		return
	}
	if len(model.Model) > report.MaxModelBytes {
		b.err = &report.Error{Code: report.TooLarge, Message: "Model exceeds the report size limit."}
		return
	}
	if match := model.PricingMatch; match != nil && (len(match.Provider) > 1024 || len(match.Model) > 1024) {
		b.err = &report.Error{Code: report.TooLarge, Message: "Pricing identity exceeds the report size limit."}
		return
	}
	b.value(value)
}
