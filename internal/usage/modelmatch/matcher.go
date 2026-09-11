package modelmatch

import (
	"context"
	"time"
)

var recognizedProviders = map[string]struct{}{
	"anthropic": {},
	"google":    {},
	"openai":    {},
	"xai":       {},
}

// Identity names one model in its original-provider namespace.
type Identity struct {
	Provider string
	Model    string
}

// Status describes whether model resolution selected one identity.
type Status string

const (
	StatusMatched   Status = "matched"
	StatusUnknown   Status = "unknown"
	StatusAmbiguous Status = "ambiguous"
)

// Method identifies the strongest matching policy that selected an identity.
type Method string

const (
	MethodExact      Method = "exact"
	MethodNormalized Method = "normalized"
	MethodSuffix     Method = "suffix"
	MethodDated      Method = "dated"
)

// Resolution is a matched identity and method, or an explicit unresolved status.
type Resolution struct {
	Status   Status
	Identity Identity
	Method   Method
}

// Matcher is an immutable index of original-provider candidate identities.
type Matcher struct {
	exact           map[string]selection
	normalized      map[string]selection
	datedNormalized map[string]selection
}

type selection struct {
	identity  Identity
	ambiguous bool
}

// New builds an immutable matcher over the supplied candidate identities.
func New(ctx context.Context, candidates []Identity) (*Matcher, error) {
	matcher := &Matcher{
		exact:           make(map[string]selection),
		normalized:      make(map[string]selection),
		datedNormalized: make(map[string]selection),
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, recognized := recognizedProviders[candidate.Provider]; !recognized {
			continue
		}
		addSelection(matcher.exact, lookupKey("", candidate.Model), candidate)
		addSelection(matcher.exact, lookupKey(candidate.Provider, candidate.Model), candidate)
		normalized := normalize(candidate.Model)
		addSelection(matcher.normalized, lookupKey("", normalized), candidate)
		addSelection(matcher.normalized, lookupKey(candidate.Provider, normalized), candidate)
		if stem, dated := stripDate(candidate.Model); dated {
			normalizedStem := normalize(stem)
			addSelection(matcher.datedNormalized, lookupKey("", normalizedStem), candidate)
			addSelection(matcher.datedNormalized, lookupKey(candidate.Provider, normalizedStem), candidate)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return matcher, nil
}

// Resolve matches a Reported model without modifying its spelling.
func (m *Matcher) Resolve(reportedModel string) Resolution {
	provider, model := reportedScope(reportedModel)
	if selected, found := m.exact[lookupKey(provider, model)]; found {
		return selected.resolution(MethodExact)
	}
	if selected, found := m.normalized[lookupKey(provider, normalize(model))]; found {
		return selected.resolution(MethodNormalized)
	}
	if stem, stripped := stripFast(model); stripped {
		if selected, found := m.exact[lookupKey(provider, stem)]; found {
			return selected.resolution(MethodSuffix)
		}
		if selected, found := m.normalized[lookupKey(provider, normalize(stem))]; found {
			return selected.resolution(MethodSuffix)
		}
		if base, dated := stripDate(stem); dated {
			if selected, found := m.exact[lookupKey(provider, base)]; found {
				return selected.resolution(MethodSuffix)
			}
			if selected, found := m.normalized[lookupKey(provider, normalize(base))]; found {
				return selected.resolution(MethodSuffix)
			}
		}
	}
	if stem, stripped := stripDate(model); stripped {
		if selected, found := m.exact[lookupKey(provider, stem)]; found {
			return selected.resolution(MethodSuffix)
		}
		if selected, found := m.normalized[lookupKey(provider, normalize(stem))]; found {
			return selected.resolution(MethodSuffix)
		}
		if base, fast := stripFast(stem); fast {
			if selected, found := m.exact[lookupKey(provider, base)]; found {
				return selected.resolution(MethodSuffix)
			}
			if selected, found := m.normalized[lookupKey(provider, normalize(base))]; found {
				return selected.resolution(MethodSuffix)
			}
		}
	}
	if !hasDateSuffix(model) {
		selected, found := m.datedNormalized[lookupKey(provider, normalize(model))]
		if stem, stripped := stripFast(model); stripped {
			if alternative, exists := m.datedNormalized[lookupKey(provider, normalize(stem))]; exists {
				if found {
					selected = mergeSelections(selected, alternative)
				} else {
					selected, found = alternative, true
				}
			}
		}
		if found {
			return selected.resolution(MethodDated)
		}
	}
	return Resolution{Status: StatusUnknown}
}

func addSelection(index map[string]selection, key string, identity Identity) {
	selected, found := index[key]
	if !found {
		index[key] = selection{identity: identity}
		return
	}
	if selected.identity != identity {
		selected.ambiguous = true
		index[key] = selected
	}
}

func mergeSelections(left, right selection) selection {
	if left.ambiguous || right.ambiguous || left.identity != right.identity {
		left.ambiguous = true
	}
	return left
}

func (s selection) resolution(method Method) Resolution {
	if s.ambiguous {
		return Resolution{Status: StatusAmbiguous}
	}
	return Resolution{Status: StatusMatched, Identity: s.identity, Method: method}
}

func lookupKey(provider, model string) string {
	return provider + "\x00" + model
}

func normalize(model string) string {
	folded := []byte(model)
	for index, value := range folded {
		if value >= 'A' && value <= 'Z' {
			folded[index] = value + ('a' - 'A')
		}
	}
	if len(folded) >= len("claude-") && string(folded[:len("claude-")]) == "claude-" {
		for index := 1; index+1 < len(folded); index++ {
			if folded[index] == '.' && isASCIIDigit(folded[index-1]) && isASCIIDigit(folded[index+1]) {
				folded[index] = '-'
			}
		}
	}
	return string(folded)
}

func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func hasDateSuffix(model string) bool {
	if _, dated := stripDate(model); dated {
		return true
	}
	if stem, fast := stripFast(model); fast {
		_, dated := stripDate(stem)
		return dated
	}
	return false
}

func stripDate(model string) (string, bool) {
	for _, form := range []struct {
		length int
		layout string
	}{
		{length: len("YYYYMMDD"), layout: "20060102"},
		{length: len("YYYY-MM-DD"), layout: "2006-01-02"},
	} {
		if len(model) <= form.length || model[len(model)-form.length-1] != '-' {
			continue
		}
		date := model[len(model)-form.length:]
		parsed, err := time.Parse(form.layout, date)
		if err == nil && parsed.Year() != 0 {
			return model[:len(model)-form.length-1], true
		}
	}
	return model, false
}

func stripFast(model string) (string, bool) {
	const suffix = "-fast"
	if len(model) < len(suffix) {
		return model, false
	}
	start := len(model) - len(suffix)
	for index := range len(suffix) {
		got := model[start+index]
		if got >= 'A' && got <= 'Z' {
			got += 'a' - 'A'
		}
		if got != suffix[index] {
			return model, false
		}
	}
	return model[:start], true
}

func reportedScope(reported string) (string, string) {
	for provider := range recognizedProviders {
		prefix := provider + "/"
		if len(reported) >= len(prefix) && reported[:len(prefix)] == prefix {
			return provider, reported[len(prefix):]
		}
	}
	return "", reported
}
