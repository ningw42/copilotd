package reportcli

import "encoding/json"

const (
	windowsEvidenceSuccess     = "success"
	windowsEvidenceFailed      = "failed"
	windowsEvidenceUnavailable = "unavailable"
)

const (
	windowsMappingExactTerritory = "exact_territory"
	windowsMappingWorldDefault   = "world_default"
)

type windowsTransition struct {
	Year         uint16 `json:"year"`
	Month        uint16 `json:"month"`
	DayOfWeek    uint16 `json:"day_of_week"`
	Day          uint16 `json:"day"`
	Hour         uint16 `json:"hour"`
	Minute       uint16 `json:"minute"`
	Second       uint16 `json:"second"`
	Milliseconds uint16 `json:"milliseconds"`
}

type windowsTimezoneEvidence struct {
	dynamicStatus               string
	dynamicAPIStatus            uint32
	dynamicAPIStatusName        string
	dynamicErrorCode            uint32
	keyName                     string
	dynamicDaylightTimeDisabled bool
	bias                        int32
	standardBias                int32
	daylightBias                int32
	standardTransition          windowsTransition
	daylightTransition          windowsTransition
	territoryStatus             string
	territoryErrorCode          uint32
	territory                   string
}

func (e windowsTimezoneEvidence) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		DynamicStatus               string            `json:"dynamic_status"`
		DynamicAPIStatus            uint32            `json:"dynamic_api_status"`
		DynamicAPIStatusName        string            `json:"dynamic_api_status_name,omitempty"`
		DynamicErrorCode            uint32            `json:"dynamic_error_code,omitempty"`
		KeyName                     string            `json:"key"`
		DynamicDaylightTimeDisabled bool              `json:"dynamic_daylight_time_disabled"`
		Bias                        int32             `json:"bias"`
		StandardBias                int32             `json:"standard_bias"`
		DaylightBias                int32             `json:"daylight_bias"`
		StandardTransition          windowsTransition `json:"standard_transition"`
		DaylightTransition          windowsTransition `json:"daylight_transition"`
		TerritoryStatus             string            `json:"territory_status"`
		TerritoryErrorCode          uint32            `json:"territory_error_code,omitempty"`
		Territory                   string            `json:"territory,omitempty"`
	}{
		DynamicStatus: e.dynamicStatus, DynamicAPIStatus: e.dynamicAPIStatus, DynamicAPIStatusName: e.dynamicAPIStatusName,
		DynamicErrorCode: e.dynamicErrorCode, KeyName: e.keyName, DynamicDaylightTimeDisabled: e.dynamicDaylightTimeDisabled,
		Bias: e.bias, StandardBias: e.standardBias, DaylightBias: e.daylightBias,
		StandardTransition: e.standardTransition, DaylightTransition: e.daylightTransition,
		TerritoryStatus: e.territoryStatus, TerritoryErrorCode: e.territoryErrorCode, Territory: e.territory,
	})
}

type windowsTimezoneResolution struct {
	name       string
	mapping    string
	candidates []string
}

func resolveWindowsTimezone(
	evidence func() windowsTimezoneEvidence,
	candidates func(key, territory string) []string,
	load func(name string) error,
) (windowsTimezoneResolution, error) {
	observed := evidence()
	if observed.dynamicStatus != windowsEvidenceSuccess {
		return windowsTimezoneResolution{}, localTimezoneError("Windows timezone API failed")
	}
	if observed.keyName == "" {
		return windowsTimezoneResolution{}, localTimezoneError("Windows timezone key is empty or custom")
	}
	if observed.dynamicDaylightTimeDisabled {
		return windowsTimezoneResolution{}, localTimezoneError("Windows dynamic daylight time is disabled")
	}
	territory := observed.territory
	mapping := windowsMappingExactTerritory
	if observed.territoryStatus != windowsEvidenceSuccess || !usableWindowsTerritory(territory) {
		territory = "001"
		mapping = windowsMappingWorldDefault
	}
	resolved := candidates(observed.keyName, territory)
	if len(resolved) == 0 && mapping == windowsMappingExactTerritory {
		resolved = candidates(observed.keyName, "001")
		mapping = windowsMappingWorldDefault
	}
	if len(resolved) == 0 {
		return windowsTimezoneResolution{}, localTimezoneError("Windows timezone has no CLDR mapping")
	}
	selected := resolved[0]
	if err := load(selected); err != nil {
		return windowsTimezoneResolution{}, localTimezoneError("CLDR Windows timezone mapping is not loadable")
	}
	return windowsTimezoneResolution{name: selected, mapping: mapping, candidates: append([]string(nil), resolved...)}, nil
}

func usableWindowsTerritory(territory string) bool {
	return len(territory) == 2 && territory[0] >= 'A' && territory[0] <= 'Z' && territory[1] >= 'A' && territory[1] <= 'Z'
}
