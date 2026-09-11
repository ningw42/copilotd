// Package pricing projects immutable original-provider rates from models.dev.
package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

var selectedProviders = [...]string{"openai", "anthropic", "google", "xai"}

// Identity names one model in its original-provider namespace.
type Identity struct {
	Provider string
	Model    string
}

// Rates is the selected standard USD-per-million-token rate vector. Each nil
// rate is absent; a non-nil zero remains an explicitly reported zero.
type Rates struct {
	Input      *Rate
	Output     *Rate
	CacheRead  *Rate
	CacheWrite *Rate
}

// Snapshot is an immutable projection of one complete accepted artifact.
type Snapshot struct {
	identities []Identity
	rates      map[Identity]Rates
}

// ParseSnapshot validates and projects one complete models.dev artifact.
func ParseSnapshot(ctx context.Context, raw []byte) (*Snapshot, error) {
	if err := unambiguousJSON(ctx, raw); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		return nil, errors.New("decode models.dev root object")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snapshot := &Snapshot{rates: make(map[Identity]Rates)}
	for _, providerID := range selectedProviders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rawProvider, present := root[providerID]
		if !present {
			return nil, fmt.Errorf("models.dev provider %q is missing", providerID)
		}
		var provider map[string]json.RawMessage
		if err := json.Unmarshal(rawProvider, &provider); err != nil || provider == nil {
			return nil, fmt.Errorf("decode models.dev provider %q", providerID)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := requireMatchingID(provider, providerID); err != nil {
			return nil, fmt.Errorf("provider %q: %w", providerID, err)
		}
		var models map[string]json.RawMessage
		if err := json.Unmarshal(provider["models"], &models); err != nil || models == nil {
			return nil, fmt.Errorf("provider %q models are missing or invalid", providerID)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for modelID, rawModel := range models {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var model map[string]json.RawMessage
			if err := json.Unmarshal(rawModel, &model); err != nil || model == nil {
				return nil, fmt.Errorf("decode model %q/%q", providerID, modelID)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := requireMatchingID(model, modelID); err != nil {
				return nil, fmt.Errorf("model %q/%q: %w", providerID, modelID, err)
			}
			identity := Identity{Provider: providerID, Model: modelID}
			snapshot.identities = append(snapshot.identities, identity)
			rawCost, priced := model["cost"]
			if !priced {
				continue
			}
			rates, err := parseCost(ctx, rawCost)
			if err != nil {
				return nil, fmt.Errorf("model %q/%q cost: %w", providerID, modelID, err)
			}
			snapshot.rates[identity] = rates
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(snapshot.identities, func(a, b Identity) int {
		if order := bytes.Compare([]byte(a.Provider), []byte(b.Provider)); order != 0 {
			return order
		}
		return bytes.Compare([]byte(a.Model), []byte(b.Model))
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func requireMatchingID(object map[string]json.RawMessage, want string) error {
	if want == "" || len(want) > 1024 || !utf8.ValidString(want) {
		return errors.New("keyed identity must be non-empty valid UTF-8 of at most 1024 bytes")
	}
	raw, present := object["id"]
	if !present {
		return errors.New("id is missing")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		return errors.New("id is not a string")
	}
	if got != want {
		return fmt.Errorf("id %q does not match keyed identity", got)
	}
	return nil
}

func unambiguousJSON(ctx context.Context, raw []byte) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := decoder.ReadToken(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("invalid models.dev JSON: %w", err)
		}
		if decoder.StackDepth() == 0 {
			break
		}
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("invalid models.dev trailing JSON data")
	}
	return ctx.Err()
}

func parseCost(ctx context.Context, raw json.RawMessage) (Rates, error) {
	if err := ctx.Err(); err != nil {
		return Rates{}, err
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(raw, &base); err != nil || base == nil {
		return Rates{}, errors.New("cost is not an object")
	}
	if err := ctx.Err(); err != nil {
		return Rates{}, err
	}
	baseRates, err := parseCostRow(base)
	if err != nil {
		return Rates{}, err
	}

	var legacyRates *Rates
	if legacy, present := base["context_over_200k"]; present {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(legacy, &row); err != nil || row == nil {
			return Rates{}, errors.New("context_over_200k is not an object")
		}
		if err := ctx.Err(); err != nil {
			return Rates{}, err
		}
		parsed, err := parseCostRow(row)
		if err != nil {
			return Rates{}, fmt.Errorf("context_over_200k: %w", err)
		}
		legacyRates = &parsed
	}

	if rawTiers, present := base["tiers"]; present {
		var tiers []json.RawMessage
		if err := json.Unmarshal(rawTiers, &tiers); err != nil || tiers == nil {
			return Rates{}, errors.New("tiers is not an array")
		}
		if err := ctx.Err(); err != nil {
			return Rates{}, err
		}
		var selected *Rates
		var highest uint64
		seen := make(map[uint64]struct{}, len(tiers))
		for index, rawTier := range tiers {
			if err := ctx.Err(); err != nil {
				return Rates{}, err
			}
			var tier map[string]json.RawMessage
			if err := json.Unmarshal(rawTier, &tier); err != nil || tier == nil {
				return Rates{}, fmt.Errorf("tiers[%d] is not an object", index)
			}
			parsed, err := parseCostRow(tier)
			if err != nil {
				return Rates{}, fmt.Errorf("tiers[%d]: %w", index, err)
			}
			threshold, err := contextThreshold(tier)
			if err != nil {
				return Rates{}, fmt.Errorf("tiers[%d]: %w", index, err)
			}
			if _, duplicate := seen[threshold]; duplicate {
				return Rates{}, fmt.Errorf("tiers[%d] duplicates context threshold %d", index, threshold)
			}
			seen[threshold] = struct{}{}
			if selected == nil || threshold > highest {
				selected, highest = &parsed, threshold
			}
		}
		if selected != nil {
			if err := ctx.Err(); err != nil {
				return Rates{}, err
			}
			return *selected, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return Rates{}, err
	}
	if legacyRates != nil {
		return *legacyRates, nil
	}
	return baseRates, nil
}

func contextThreshold(row map[string]json.RawMessage) (uint64, error) {
	rawTier, present := row["tier"]
	if !present {
		return 0, errors.New("tier descriptor is missing")
	}
	var tier map[string]json.RawMessage
	if err := json.Unmarshal(rawTier, &tier); err != nil || tier == nil {
		return 0, errors.New("tier descriptor is not an object")
	}
	var kind string
	if err := json.Unmarshal(tier["type"], &kind); err != nil || kind != "context" {
		return 0, errors.New("tier type is not context")
	}
	rawSize, present := tier["size"]
	if !present {
		return 0, errors.New("tier size is missing")
	}
	threshold, err := ParseRate(string(rawSize))
	if err != nil || threshold.scale != 0 || threshold.coefficient == nil || !threshold.coefficient.IsUint64() {
		return 0, errors.New("tier size is not a bounded nonnegative integer")
	}
	return threshold.coefficient.Uint64(), nil
}

func parseCostRow(object map[string]json.RawMessage) (Rates, error) {
	parsed := make(map[string]*Rate, 7)
	for _, name := range []string{"input", "output", "reasoning", "cache_read", "cache_write", "input_audio", "output_audio"} {
		rate, err := optionalRate(object, name)
		if err != nil {
			return Rates{}, err
		}
		parsed[name] = rate
	}
	return Rates{
		Input:      parsed["input"],
		Output:     parsed["output"],
		CacheRead:  parsed["cache_read"],
		CacheWrite: parsed["cache_write"],
	}, nil
}

func optionalRate(object map[string]json.RawMessage, name string) (*Rate, error) {
	raw, present := object[name]
	if !present {
		return nil, nil
	}
	rate, err := ParseRate(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s rate is invalid", name)
	}
	return &rate, nil
}

// Identities returns a detached, stable-order copy of all projected model
// identities, including models with no selected prices.
func (s *Snapshot) Identities() []Identity {
	if s == nil {
		return nil
	}
	return append([]Identity(nil), s.identities...)
}

// Rates returns a detached selected rate vector for an identity. Nil fields
// preserve absent prices. False means the identity has no cost object or is
// absent from this snapshot.
func (s *Snapshot) Rates(identity Identity) (Rates, bool) {
	if s == nil {
		return Rates{}, false
	}
	rates, ok := s.rates[identity]
	if !ok {
		return Rates{}, false
	}
	rates.Input = detachedRate(rates.Input)
	rates.Output = detachedRate(rates.Output)
	rates.CacheRead = detachedRate(rates.CacheRead)
	rates.CacheWrite = detachedRate(rates.CacheWrite)
	return rates, true
}

func detachedRate(rate *Rate) *Rate {
	if rate == nil {
		return nil
	}
	detached := *rate // Rate's shared big integer is immutable.
	return &detached
}
