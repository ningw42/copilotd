package reporthttp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
	"unicode/utf8"

	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
)

const MaxBodyBytes = 8 << 20

// Encode only bounded fragments, never the complete report in encoding/json's
// unbounded internal buffer. Each model-bearing fragment contains one guarded
// identity; repeated identities cannot cause a whole-report allocation.
func encodeReport(ctx context.Context, result report.Report) ([]byte, error) {
	out := boundedJSON{ctx: ctx}
	if err := validatePricingExtension(ctx, result); err != nil {
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
			out.model(row.Model, wireRow{
				BucketStart:    row.BucketStart,
				wireModelTotal: modelTotalForWire(row.ModelTotal, result.Pricing != nil),
			})
		}
		out.append([]byte(`],"models":[`))
		for i, model := range native.section.Models {
			if i > 0 {
				out.append([]byte(","))
			}
			out.model(model.Model, modelTotalForWire(model, result.Pricing != nil))
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

var errInvalidPricingExtension = errors.New("invalid pricing report extension")

func validatePricingExtension(ctx context.Context, result report.Report) error {
	present := result.Pricing != nil
	if present {
		provenance := result.Pricing
		if provenance.Dataset != "models.dev/api.json" || provenance.Currency != "USD" || provenance.Basis != "original_provider" || provenance.ContextPolicy != "highest_tier" || provenance.CacheWritePolicy != "single_rate" || !validContentVersion(provenance.Version) || provenance.Source != "fallback" && provenance.Source != "fetched" {
			return errInvalidPricingExtension
		}
		if provenance.LastSuccess != nil {
			_, offset := provenance.LastSuccess.Zone()
			if provenance.LastSuccess.IsZero() || offset != 0 || provenance.LastSuccess.Year() < 0 || provenance.LastSuccess.Year() > 9999 {
				return errInvalidPricingExtension
			}
		}
	}
	for _, section := range []*report.Section{result.Anthropic, result.OpenAI} {
		if section == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, row := range section.Rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validateCostExtension(row.Total, present); err != nil {
				return err
			}
			if err := validatePricingMatchExtension(row.PricingMatch, present); err != nil {
				return err
			}
		}
		for _, model := range section.Models {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validateCostExtension(model.Total, present); err != nil {
				return err
			}
			if err := validatePricingMatchExtension(model.PricingMatch, present); err != nil {
				return err
			}
		}
		if err := validateCostExtension(section.Total, present); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func validateCostExtension(total report.Total, present bool) error {
	cost := total.Cost
	hasCost := cost.Amount != nil || cost.PricedTurns != 0 || cost.Unpriced != (report.UnpricedCoverage{})
	if !present {
		if hasCost {
			return errInvalidPricingExtension
		}
		return nil
	}
	if total.Turns < 0 || cost.PricedTurns < 0 || cost.Unpriced.UnknownModel < 0 || cost.Unpriced.AmbiguousModel < 0 || cost.Unpriced.MissingRate < 0 || cost.Unpriced.MissingUsage < 0 || cost.Unpriced.InconsistentUsage < 0 {
		return errInvalidPricingExtension
	}
	covered := cost.PricedTurns
	for _, count := range []int64{cost.Unpriced.UnknownModel, cost.Unpriced.AmbiguousModel, cost.Unpriced.MissingRate, cost.Unpriced.MissingUsage, cost.Unpriced.InconsistentUsage} {
		if count > math.MaxInt64-covered {
			return errInvalidPricingExtension
		}
		covered += count
	}
	if covered != total.Turns {
		return errInvalidPricingExtension
	}
	if cost.Amount != nil {
		amount := cost.Amount.String()
		parsed, err := pricing.ParseAmount(amount)
		if err != nil || parsed.String() != amount {
			return errInvalidPricingExtension
		}
	}
	if total.Turns == 0 {
		if cost.Amount == nil || cost.Amount.String() != "0" {
			return errInvalidPricingExtension
		}
	} else if cost.PricedTurns == 0 {
		if cost.Amount != nil {
			return errInvalidPricingExtension
		}
	} else if cost.Amount == nil {
		return errInvalidPricingExtension
	}
	return nil
}

func validatePricingMatchExtension(match report.PricingMatch, present bool) error {
	hasMatch := match.Status != "" || match.Provider != "" || match.Model != "" || match.Method != ""
	if !present {
		if hasMatch {
			return errInvalidPricingExtension
		}
		return nil
	}
	switch match.Status {
	case report.PricingMatchUnknown, report.PricingMatchAmbiguous:
		if match.Provider != "" || match.Model != "" || match.Method != "" {
			return errInvalidPricingExtension
		}
	case report.PricingMatchMatched:
		for _, identity := range []string{match.Provider, match.Model} {
			if len(identity) > 1024 {
				return &report.Error{Code: report.TooLarge, Message: "Pricing identity exceeds the report size limit."}
			}
			if identity == "" || !utf8.ValidString(identity) {
				return errInvalidPricingExtension
			}
		}
		switch match.Method {
		case report.PricingMatchByExact, report.PricingMatchByNormalized, report.PricingMatchByAlias, report.PricingMatchBySuffix, report.PricingMatchByDated:
		default:
			return errInvalidPricingExtension
		}
	default:
		return errInvalidPricingExtension
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
func (b *boundedJSON) model(model string, value any) {
	if len(model) > report.MaxModelBytes {
		b.err = &report.Error{Code: report.TooLarge, Message: "Model exceeds the report size limit."}
		return
	}
	b.value(value)
}
