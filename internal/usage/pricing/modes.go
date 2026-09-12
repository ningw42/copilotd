package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxServiceTierLookupBytes is the longest supported reported service-tier
// alias. Report readers use it to avoid materializing evidence that cannot
// affect pricing.
const MaxServiceTierLookupBytes = len("priority")

func parseOpenAIFast(ctx context.Context, model map[string]json.RawMessage) (*rateSchedule, error) {
	rawExperimental, present := model["experimental"]
	if !present {
		return nil, nil
	}
	experimental, err := requiredObject(rawExperimental, "experimental")
	if err != nil {
		return nil, err
	}
	rawModes, present := experimental["modes"]
	if !present {
		return nil, nil
	}
	modes, err := requiredObject(rawModes, "experimental.modes")
	if err != nil {
		return nil, err
	}

	var fast *rateSchedule
	for name, rawMode := range modes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateIdentity(name); err != nil {
			return nil, errors.New("mode name must be non-empty valid UTF-8 of at most 1024 bytes")
		}
		mode, err := requiredObject(rawMode, "mode entry")
		if err != nil {
			return nil, fmt.Errorf("mode %q: %w", name, err)
		}
		wireTier, wirePresent, err := modeWireTier(mode)
		if err != nil {
			return nil, fmt.Errorf("mode %q: %w", name, err)
		}

		nameIsFast := isFastAlias(name)
		wireIsFast := wirePresent && isFastAlias(wireTier)
		if nameIsFast && wirePresent && !wireIsFast {
			return nil, fmt.Errorf("mode %q contradicts its Fast identity", name)
		}
		if wireIsFast && isReservedNonFastMode(name) {
			return nil, fmt.Errorf("mode %q contradicts its reserved identity", name)
		}
		candidate := nameIsFast || wireIsFast

		var rates Rates
		if rawCost, present := mode["cost"]; present {
			cost, err := requiredObject(rawCost, "cost")
			if err != nil {
				return nil, fmt.Errorf("mode %q: %w", name, err)
			}
			if candidate {
				if _, present := cost["tiers"]; present {
					return nil, fmt.Errorf("mode %q: Fast cost tiers are unsupported", name)
				}
				if _, present := cost["context_over_200k"]; present {
					return nil, fmt.Errorf("mode %q: Fast cost context_over_200k is unsupported", name)
				}
			}
			rates, err = parseCostRow(cost)
			if err != nil {
				return nil, fmt.Errorf("mode %q cost: %w", name, err)
			}
		}
		if !candidate {
			continue
		}
		if fast != nil {
			return nil, errors.New("multiple OpenAI Fast mode declarations")
		}
		fast = &rateSchedule{base: rates}
	}
	return fast, ctx.Err()
}

func requiredObject(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s is not an object", name)
	}
	return object, nil
}

func modeWireTier(mode map[string]json.RawMessage) (string, bool, error) {
	rawProvider, present := mode["provider"]
	if !present {
		return "", false, nil
	}
	provider, err := requiredObject(rawProvider, "provider")
	if err != nil {
		return "", false, err
	}
	rawBody, present := provider["body"]
	if !present {
		return "", false, nil
	}
	body, err := requiredObject(rawBody, "provider.body")
	if err != nil {
		return "", false, err
	}
	rawTier, present := body["service_tier"]
	if !present {
		return "", false, nil
	}
	var tier string
	if err := json.Unmarshal(rawTier, &tier); err != nil || tier == "" || len(tier) > 1024 {
		return "", false, errors.New("provider.body.service_tier must be a non-empty valid UTF-8 string of at most 1024 bytes")
	}
	return tier, true, nil
}

func isFastAlias(value string) bool {
	return equalASCIIFold(value, "fast") || equalASCIIFold(value, "priority")
}

func isReservedNonFastMode(value string) bool {
	for _, reserved := range [...]string{"default", "auto", "flex", "scale", "ultrafast"} {
		if equalASCIIFold(value, reserved) {
			return true
		}
	}
	return false
}

func equalASCIIFold(value, target string) bool {
	if len(value) != len(target) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		if character != target[index] {
			return false
		}
	}
	return true
}

func useFastRates(reportedTier *string) bool {
	return reportedTier != nil && len(*reportedTier) <= MaxServiceTierLookupBytes && isFastAlias(*reportedTier)
}
