package reportcli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli/windowszonesdata"
)

func TestWindowsTimezoneEvidenceUsesTaggedFieldsAndOmitsOptionalZeros(t *testing.T) {
	evidence := windowsTimezoneEvidence{
		DynamicStatus:    windowsEvidenceSuccess,
		DynamicAPIStatus: 1,
		KeyName:          "Central Standard Time",
		TerritoryStatus:  windowsEvidenceUnavailable,
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"dynamic_status", "dynamic_api_status", "key", "dynamic_daylight_time_disabled",
		"bias", "standard_bias", "daylight_bias", "standard_transition", "daylight_transition", "territory_status",
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("required native evidence field %q omitted from %s", name, encoded)
		}
	}
	for _, name := range []string{"dynamic_api_status_name", "dynamic_error_code", "territory_error_code", "territory"} {
		if _, ok := fields[name]; ok {
			t.Errorf("optional zero native evidence field %q present in %s", name, encoded)
		}
	}
	if len(fields) != 10 {
		t.Fatalf("native evidence fields = %v", fields)
	}
}

func TestWindowsTimezoneResolverRejectsUnusableDynamicEvidenceBeforeDependencies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence windowsTimezoneEvidence
		reason   string
	}{
		{name: "API failure", evidence: windowsTimezoneEvidence{DynamicStatus: windowsEvidenceFailed, KeyName: "Central Standard Time", TerritoryStatus: windowsEvidenceSuccess, Territory: "US"}, reason: "Windows dynamic timezone API failed"},
		{name: "API unavailable", evidence: windowsTimezoneEvidence{DynamicStatus: windowsEvidenceUnavailable, KeyName: "Central Standard Time", TerritoryStatus: windowsEvidenceSuccess, Territory: "US"}, reason: "Windows dynamic timezone API failed"},
		{name: "empty key", evidence: windowsTimezoneEvidence{DynamicStatus: windowsEvidenceSuccess}, reason: "Windows timezone key is empty or custom"},
		{name: "dynamic DST disabled", evidence: windowsTimezoneEvidence{DynamicStatus: windowsEvidenceSuccess, KeyName: "Central Standard Time", DynamicDaylightTimeDisabled: true}, reason: "Windows dynamic daylight time is disabled"},
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
				return windowsTimezoneEvidence{DynamicStatus: windowsEvidenceSuccess, KeyName: "Custom operator zone", TerritoryStatus: windowsEvidenceSuccess, Territory: "US"}
			},
			func(string, string) []string { return nil },
			func(string) error { t.Fatal("loader reached"); return nil },
		)
		if err == nil || result.name != "" || !strings.Contains(err.Error(), "no CLDR mapping") || strings.Contains(err.Error(), "Custom operator zone") {
			t.Fatalf("resolution=%+v error=%v", result, err)
		}
	})
	for _, tc := range []struct {
		name      string
		candidate string
	}{
		{name: "syntactically valid but unavailable", candidate: "Unavailable/First"},
		{name: "local pseudo-name", candidate: "Local"},
		{name: "empty name", candidate: ""},
		{name: "path-like name", candidate: "../Etc/UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups, loaded []string
			result, err := resolveWindowsTimezone(
				func() windowsTimezoneEvidence {
					return windowsTimezoneEvidence{DynamicStatus: windowsEvidenceSuccess, KeyName: "Central Standard Time", TerritoryStatus: windowsEvidenceSuccess, Territory: "US"}
				},
				func(key, territory string) []string {
					lookups = append(lookups, key+"/"+territory)
					if territory == "US" {
						return []string{tc.candidate, "America/Chicago"}
					}
					return []string{"America/Chicago"}
				},
				func(name string) error {
					loaded = append(loaded, name)
					_, err := report.LoadTimezone(name)
					return err
				},
			)
			if err == nil || result.name != "" || !strings.Contains(err.Error(), "not loadable") || !strings.Contains(err.Error(), "--timezone Area/City") || len(err.Error()) > 300 ||
				fmt.Sprint(lookups) != "[Central Standard Time/US]" || len(loaded) != 1 || loaded[0] != tc.candidate {
				t.Fatalf("resolution=%+v lookups=%v loaded=%q error=%v", result, lookups, loaded, err)
			}
		})
	}
}

func TestWindowsTimezoneResolverPrefersFirstExactTerritoryCandidate(t *testing.T) {
	var lookups []string
	result, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence {
			return windowsTimezoneEvidence{
				DynamicStatus:   windowsEvidenceSuccess,
				KeyName:         "Central Standard Time",
				TerritoryStatus: windowsEvidenceSuccess,
				Territory:       "US",
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
				DynamicStatus:   windowsEvidenceSuccess,
				KeyName:         "Nepal Standard Time",
				TerritoryStatus: windowsEvidenceSuccess,
				Territory:       "US",
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
					return windowsTimezoneEvidence{DynamicStatus: windowsEvidenceSuccess, KeyName: "China Standard Time", TerritoryStatus: windowsEvidenceSuccess, Territory: territory}
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
	for _, status := range []windowsEvidenceStatus{windowsEvidenceUnavailable, windowsEvidenceFailed} {
		t.Run(string(status), func(t *testing.T) {
			var lookups []string
			result, err := resolveWindowsTimezone(
				func() windowsTimezoneEvidence {
					return windowsTimezoneEvidence{
						DynamicStatus:   windowsEvidenceSuccess,
						KeyName:         "China Standard Time",
						TerritoryStatus: status,
						Territory:       "US",
					}
				},
				func(key, territory string) []string {
					lookups = append(lookups, key+"/"+territory)
					return windowszonesdata.Candidates(key, territory)
				},
				func(name string) error {
					_, err := report.LoadTimezone(name)
					return err
				},
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
		})
	}
}

func TestWindowsTimezoneResolverTreatsCLDRZZAsUnavailableTerritory(t *testing.T) {
	var lookups []string
	result, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence {
			return windowsTimezoneEvidence{
				DynamicStatus:   windowsEvidenceSuccess,
				KeyName:         "Hawaiian Standard Time",
				TerritoryStatus: windowsEvidenceSuccess,
				Territory:       "ZZ",
			}
		},
		func(key, territory string) []string {
			lookups = append(lookups, key+"/"+territory)
			return windowszonesdata.Candidates(key, territory)
		},
		func(name string) error {
			_, err := report.LoadTimezone(name)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.name != "Pacific/Honolulu" || result.mapping != windowsMappingWorldDefault || fmt.Sprint(result.candidates) != "[Pacific/Honolulu]" {
		t.Fatalf("resolution = %+v", result)
	}
	if fmt.Sprint(lookups) != "[Hawaiian Standard Time/001]" {
		t.Fatalf("lookups = %v", lookups)
	}
}
