package reportcli

type windowsEvidenceStatus string

const (
	windowsEvidenceSuccess     windowsEvidenceStatus = "success"
	windowsEvidenceFailed      windowsEvidenceStatus = "failed"
	windowsEvidenceUnavailable windowsEvidenceStatus = "unavailable"
)

type windowsMappingSource string

const (
	windowsMappingExactTerritory windowsMappingSource = "exact_territory"
	windowsMappingWorldDefault   windowsMappingSource = "world_default"
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
	DynamicStatus               windowsEvidenceStatus `json:"dynamic_status"`
	DynamicAPIStatus            uint32                `json:"dynamic_api_status"`
	DynamicAPIStatusName        string                `json:"dynamic_api_status_name,omitempty"`
	DynamicErrorCode            uint32                `json:"dynamic_error_code,omitempty"`
	KeyName                     string                `json:"key"`
	DynamicDaylightTimeDisabled bool                  `json:"dynamic_daylight_time_disabled"`
	Bias                        int32                 `json:"bias"`
	StandardBias                int32                 `json:"standard_bias"`
	DaylightBias                int32                 `json:"daylight_bias"`
	StandardTransition          windowsTransition     `json:"standard_transition"`
	DaylightTransition          windowsTransition     `json:"daylight_transition"`
	TerritoryStatus             windowsEvidenceStatus `json:"territory_status"`
	TerritoryErrorCode          uint32                `json:"territory_error_code,omitempty"`
	Territory                   string                `json:"territory,omitempty"`
}

type windowsTimezoneResolution struct {
	name       string
	mapping    windowsMappingSource
	candidates []string
}

func resolveWindowsTimezone(
	evidence func() windowsTimezoneEvidence,
	candidates func(key, territory string) []string,
	load func(name string) error,
) (windowsTimezoneResolution, error) {
	observed := evidence()
	if observed.DynamicStatus != windowsEvidenceSuccess {
		return windowsTimezoneResolution{}, localTimezoneError("Windows dynamic timezone API failed")
	}
	if observed.KeyName == "" {
		return windowsTimezoneResolution{}, localTimezoneError("Windows timezone key is empty or custom")
	}
	if observed.DynamicDaylightTimeDisabled {
		return windowsTimezoneResolution{}, localTimezoneError("Windows dynamic daylight time is disabled")
	}
	territory := observed.Territory
	mapping := windowsMappingExactTerritory
	if observed.TerritoryStatus != windowsEvidenceSuccess || !usableWindowsTerritory(territory) {
		territory = "001"
		mapping = windowsMappingWorldDefault
	}
	resolved := candidates(observed.KeyName, territory)
	if len(resolved) == 0 && mapping == windowsMappingExactTerritory {
		resolved = candidates(observed.KeyName, "001")
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
	return territory != "ZZ" && len(territory) == 2 && territory[0] >= 'A' && territory[0] <= 'Z' && territory[1] >= 'A' && territory[1] <= 'Z'
}
