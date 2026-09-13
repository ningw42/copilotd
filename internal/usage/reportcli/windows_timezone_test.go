package reportcli

import (
	"fmt"
	"strings"
	"testing"
)

func TestWindowsTimezoneResolverRejectsUnusableDynamicEvidenceBeforeDependencies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence windowsTimezoneEvidence
		reason   string
	}{
		{name: "API failure", evidence: windowsTimezoneEvidence{dynamicStatus: "failed"}, reason: "Windows timezone API failed"},
		{name: "empty key", evidence: windowsTimezoneEvidence{dynamicStatus: windowsEvidenceSuccess}, reason: "Windows timezone key is empty or custom"},
		{name: "dynamic DST disabled", evidence: windowsTimezoneEvidence{dynamicStatus: windowsEvidenceSuccess, keyName: "Central Standard Time", dynamicDaylightTimeDisabled: true}, reason: "Windows dynamic daylight time is disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolveWindowsTimezone(
				func() windowsTimezoneEvidence { return tc.evidence },
				func(string, string) []string { t.Fatal("candidate lookup reached"); return nil },
				func(string) error { t.Fatal("loader reached"); return nil },
			)
			if err == nil || result.name != "" || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), "--timezone Area/City") || len(err.Error()) > 300 {
				t.Fatalf("resolution=%+v error=%v", result, err)
			}
		})
	}
}

func TestWindowsTimezoneResolverRejectsUnmappedOrUnloadableSelection(t *testing.T) {
	t.Run("custom or unmapped key without 001", func(t *testing.T) {
		result, err := resolveWindowsTimezone(
			func() windowsTimezoneEvidence {
				return windowsTimezoneEvidence{dynamicStatus: windowsEvidenceSuccess, keyName: "Custom operator zone", territoryStatus: windowsEvidenceSuccess, territory: "US"}
			},
			func(string, string) []string { return nil },
			func(string) error { t.Fatal("loader reached"); return nil },
		)
		if err == nil || result.name != "" || !strings.Contains(err.Error(), "no CLDR mapping") || strings.Contains(err.Error(), "Custom operator zone") {
			t.Fatalf("resolution=%+v error=%v", result, err)
		}
	})
	t.Run("first ordered name is unloadable", func(t *testing.T) {
		var loaded []string
		result, err := resolveWindowsTimezone(
			func() windowsTimezoneEvidence {
				return windowsTimezoneEvidence{dynamicStatus: windowsEvidenceSuccess, keyName: "Central Standard Time", territoryStatus: windowsEvidenceSuccess, territory: "US"}
			},
			func(_ string, territory string) []string {
				if territory == "US" {
					return []string{"Unavailable/First", "America/Chicago"}
				}
				return []string{"America/Chicago"}
			},
			func(name string) error { loaded = append(loaded, name); return fmt.Errorf("private loader detail") },
		)
		if err == nil || result.name != "" || !strings.Contains(err.Error(), "not loadable") || strings.Contains(err.Error(), "private loader detail") || fmt.Sprint(loaded) != "[Unavailable/First]" {
			t.Fatalf("resolution=%+v loaded=%v error=%v", result, loaded, err)
		}
	})
}

func TestWindowsTimezoneResolverPrefersFirstExactTerritoryCandidate(t *testing.T) {
	var lookups []string
	result, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence {
			return windowsTimezoneEvidence{
				dynamicStatus:   windowsEvidenceSuccess,
				keyName:         "Central Standard Time",
				territoryStatus: windowsEvidenceSuccess,
				territory:       "US",
			}
		},
		func(key, territory string) []string {
			lookups = append(lookups, key+"/"+territory)
			if key == "Central Standard Time" && territory == "US" {
				return []string{"America/Chicago", "America/Indiana/Knox"}
			}
			return nil
		},
		func(name string) error {
			if name != "America/Chicago" {
				return fmt.Errorf("unexpected selected name %q", name)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.name != "America/Chicago" || result.mapping != windowsMappingExactTerritory {
		t.Fatalf("resolution = %+v", result)
	}
	if fmt.Sprint(result.candidates) != "[America/Chicago America/Indiana/Knox]" {
		t.Fatalf("ordered candidates = %v", result.candidates)
	}
	if fmt.Sprint(lookups) != "[Central Standard Time/US]" {
		t.Fatalf("lookups = %v", lookups)
	}
}

func TestWindowsTimezoneResolverFallsBackWhenExactTerritoryIsUnmapped(t *testing.T) {
	var lookups []string
	result, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence {
			return windowsTimezoneEvidence{
				dynamicStatus:   windowsEvidenceSuccess,
				keyName:         "Nepal Standard Time",
				territoryStatus: windowsEvidenceSuccess,
				territory:       "US",
			}
		},
		func(key, territory string) []string {
			lookups = append(lookups, key+"/"+territory)
			if key == "Nepal Standard Time" && territory == "001" {
				return []string{"Asia/Katmandu"}
			}
			return nil
		},
		func(name string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.name != "Asia/Katmandu" || result.mapping != windowsMappingWorldDefault {
		t.Fatalf("resolution = %+v", result)
	}
	if fmt.Sprint(lookups) != "[Nepal Standard Time/US Nepal Standard Time/001]" {
		t.Fatalf("lookups = %v", lookups)
	}
}

func TestWindowsTimezoneResolverTreatsMalformedTerritoryAsUnavailable(t *testing.T) {
	for _, territory := range []string{"", "us", "USA", "001", "U1", "\u00dcS"} {
		t.Run(fmt.Sprintf("%q", territory), func(t *testing.T) {
			var lookups []string
			result, err := resolveWindowsTimezone(
				func() windowsTimezoneEvidence {
					return windowsTimezoneEvidence{dynamicStatus: windowsEvidenceSuccess, keyName: "China Standard Time", territoryStatus: windowsEvidenceSuccess, territory: territory}
				},
				func(key, gotTerritory string) []string {
					lookups = append(lookups, key+"/"+gotTerritory)
					if gotTerritory == "001" {
						return []string{"Asia/Shanghai"}
					}
					return nil
				},
				func(string) error { return nil },
			)
			if err != nil || result.name != "Asia/Shanghai" || result.mapping != windowsMappingWorldDefault || fmt.Sprint(lookups) != "[China Standard Time/001]" {
				t.Fatalf("resolution=%+v lookups=%v error=%v", result, lookups, err)
			}
		})
	}
}

func TestWindowsTimezoneResolverUsesWorldDefaultWithoutUsableTerritory(t *testing.T) {
	var lookups []string
	result, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence {
			return windowsTimezoneEvidence{
				dynamicStatus:   windowsEvidenceSuccess,
				keyName:         "China Standard Time",
				territoryStatus: "unavailable",
			}
		},
		func(key, territory string) []string {
			lookups = append(lookups, key+"/"+territory)
			if key == "China Standard Time" && territory == "001" {
				return []string{"Asia/Shanghai"}
			}
			return nil
		},
		func(name string) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.name != "Asia/Shanghai" || result.mapping != windowsMappingWorldDefault {
		t.Fatalf("resolution = %+v", result)
	}
	if fmt.Sprint(lookups) != "[China Standard Time/001]" {
		t.Fatalf("lookups = %v", lookups)
	}
}
