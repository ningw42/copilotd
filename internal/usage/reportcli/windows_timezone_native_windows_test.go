//go:build windows

package reportcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli/windowszonesdata"
)

var (
	windowsTimezoneAPI                                     = windows.NewLazySystemDLL("api-ms-win-core-timezone-l1-1-0.dll")
	windowsGetDynamicTimeZoneInformationEffectiveYearsProc = windowsTimezoneAPI.NewProc("GetDynamicTimeZoneInformationEffectiveYears")
	windowsGetTimeZoneInformationForYearProc               = windowsTimezoneAPI.NewProc("GetTimeZoneInformationForYear")
)

type windowsTimeZoneInformation struct {
	Bias         int32
	StandardName [32]uint16
	StandardDate windowsSystemTime
	StandardBias int32
	DaylightName [32]uint16
	DaylightDate windowsSystemTime
	DaylightBias int32
}

type windowsNativeRuleEvidence struct {
	EffectiveStatus     uint32                      `json:"effective_years_status"`
	EffectiveStatusName string                      `json:"effective_years_status_name"`
	FirstYear           uint32                      `json:"first_year"`
	LastYear            uint32                      `json:"last_year"`
	Years               []windowsAnnualRuleEvidence `json:"representative_years"`
}

type windowsAnnualRuleEvidence struct {
	Year               uint16                  `json:"year"`
	Status             string                  `json:"status"`
	ErrorCode          uint32                  `json:"error_code,omitempty"`
	Bias               int32                   `json:"bias"`
	StandardBias       int32                   `json:"standard_bias"`
	DaylightBias       int32                   `json:"daylight_bias"`
	StandardTransition windowsTransition       `json:"standard_transition"`
	DaylightTransition windowsTransition       `json:"daylight_transition"`
	Comparisons        []windowsRuleComparison `json:"comparisons,omitempty"`
	Matches            bool                    `json:"matches"`
}

type windowsRuleComparison struct {
	Boundary       string `json:"boundary"`
	AtUTC          string `json:"at_utc"`
	ExpectedBefore int    `json:"expected_before_seconds"`
	ActualBefore   int    `json:"actual_before_seconds"`
	ExpectedAfter  int    `json:"expected_after_seconds"`
	ActualAfter    int    `json:"actual_after_seconds"`
	Matches        bool   `json:"matches"`
}

func readNativeWindowsRuleEvidence(selected string) (windowsNativeRuleEvidence, error) {
	var evidence windowsNativeRuleEvidence
	location, err := report.LoadTimezone(selected)
	if err != nil {
		return evidence, fmt.Errorf("load selected timezone for native rule comparison: %w", err)
	}
	if unsafe.Sizeof(windowsTimeZoneInformation{}) != 172 {
		return evidence, fmt.Errorf("unexpected TIME_ZONE_INFORMATION size %d", unsafe.Sizeof(windowsTimeZoneInformation{}))
	}
	var dynamic windowsDynamicTimeZoneInformation
	if err := windowsGetDynamicTimeZoneInfoProc.Find(); err != nil {
		return evidence, errors.New("GetDynamicTimeZoneInformation unavailable")
	}
	status, _, callErr := windowsGetDynamicTimeZoneInfoProc.Call(uintptr(unsafe.Pointer(&dynamic)))
	if status == windowsTimeZoneIDInvalid {
		return evidence, fmt.Errorf("GetDynamicTimeZoneInformation failed with code %d", windowsErrorCode(callErr))
	}
	if err := windowsGetDynamicTimeZoneInformationEffectiveYearsProc.Find(); err != nil {
		return evidence, errors.New("GetDynamicTimeZoneInformationEffectiveYears unavailable")
	}
	result, _, _ := windowsGetDynamicTimeZoneInformationEffectiveYearsProc.Call(
		uintptr(unsafe.Pointer(&dynamic)),
		uintptr(unsafe.Pointer(&evidence.FirstYear)),
		uintptr(unsafe.Pointer(&evidence.LastYear)),
	)
	evidence.EffectiveStatus = uint32(result)
	evidence.EffectiveStatusName = string(windowsEvidenceSuccess)
	// A stock fixed-offset key may have no Dynamic DST registry subkey. The API
	// reports ERROR_FILE_NOT_FOUND and no range; GetTimeZoneInformationForYear
	// still provides the representative annual rules to compare.
	if result == uintptr(syscall.ERROR_FILE_NOT_FOUND) {
		evidence.EffectiveStatusName = "no_dynamic_range"
	} else if result != 0 {
		evidence.EffectiveStatusName = string(windowsEvidenceFailed)
		return evidence, fmt.Errorf("GetDynamicTimeZoneInformationEffectiveYears failed with status %d", result)
	}
	if err := windowsGetTimeZoneInformationForYearProc.Find(); err != nil {
		return evidence, errors.New("GetTimeZoneInformationForYear unavailable")
	}
	for _, year := range representativeWindowsRuleYears(evidence.FirstYear, evidence.LastYear) {
		annual := windowsAnnualRuleEvidence{Year: year, Status: string(windowsEvidenceFailed)}
		var information windowsTimeZoneInformation
		ok, _, yearErr := windowsGetTimeZoneInformationForYearProc.Call(
			uintptr(year),
			uintptr(unsafe.Pointer(&dynamic)),
			uintptr(unsafe.Pointer(&information)),
		)
		if ok == 0 {
			annual.ErrorCode = windowsErrorCode(yearErr)
			evidence.Years = append(evidence.Years, annual)
			return evidence, fmt.Errorf("GetTimeZoneInformationForYear failed for %d with code %d", year, annual.ErrorCode)
		}
		annual.Status = string(windowsEvidenceSuccess)
		annual.Bias = information.Bias
		annual.StandardBias = information.StandardBias
		annual.DaylightBias = information.DaylightBias
		annual.StandardTransition = windowsTransitionFromSystemTime(information.StandardDate)
		annual.DaylightTransition = windowsTransitionFromSystemTime(information.DaylightDate)
		annual.Comparisons, err = compareWindowsAnnualRule(location, int(year), information)
		if err != nil {
			evidence.Years = append(evidence.Years, annual)
			return evidence, fmt.Errorf("compare Windows/IANA rules for %d: %w", year, err)
		}
		annual.Matches = true
		for _, comparison := range annual.Comparisons {
			annual.Matches = annual.Matches && comparison.Matches
		}
		evidence.Years = append(evidence.Years, annual)
	}
	if len(evidence.Years) == 0 {
		return evidence, errors.New("effective Windows rule range has no representative year")
	}
	return evidence, nil
}

func representativeWindowsRuleYears(first, last uint32) []uint16 {
	current := time.Now().Year()
	if first == 0 && last == 0 {
		first, last = uint32(current-1), uint32(current+1)
	}
	var years []uint16
	seen := map[uint16]bool{}
	for _, candidate := range []int{current - 1, current, current + 1} {
		if candidate < 1 || candidate > int(^uint16(0)) || uint32(candidate) < first || uint32(candidate) > last {
			continue
		}
		year := uint16(candidate)
		if !seen[year] {
			years = append(years, year)
			seen[year] = true
		}
	}
	if len(years) == 0 {
		for _, candidate := range []uint32{first, last} {
			if candidate == 0 || candidate > uint32(^uint16(0)) || seen[uint16(candidate)] {
				continue
			}
			years = append(years, uint16(candidate))
			seen[uint16(candidate)] = true
		}
	}
	return years
}

func compareWindowsAnnualRule(location *time.Location, year int, information windowsTimeZoneInformation) ([]windowsRuleComparison, error) {
	standardOffset := -int(information.Bias+information.StandardBias) * 60
	daylightOffset := -int(information.Bias+information.DaylightBias) * 60
	standard := windowsTransitionFromSystemTime(information.StandardDate)
	daylight := windowsTransitionFromSystemTime(information.DaylightDate)
	if standard.Month == 0 && daylight.Month == 0 {
		return []windowsRuleComparison{
			compareWindowsOffsets(location, "fixed-january", time.Date(year, time.January, 15, 12, 0, 0, 0, time.UTC), standardOffset, standardOffset),
			compareWindowsOffsets(location, "fixed-july", time.Date(year, time.July, 15, 12, 0, 0, 0, time.UTC), standardOffset, standardOffset),
		}, nil
	}
	if standard.Month == 0 || daylight.Month == 0 {
		return nil, errors.New("only one Windows daylight transition is defined")
	}
	daylightAt, err := windowsTransitionUTC(year, daylight, standardOffset)
	if err != nil {
		return nil, fmt.Errorf("daylight transition: %w", err)
	}
	standardAt, err := windowsTransitionUTC(year, standard, daylightOffset)
	if err != nil {
		return nil, fmt.Errorf("standard transition: %w", err)
	}
	return []windowsRuleComparison{
		compareWindowsOffsets(location, "daylight", daylightAt, standardOffset, daylightOffset),
		compareWindowsOffsets(location, "standard", standardAt, daylightOffset, standardOffset),
	}, nil
}

func compareWindowsOffsets(location *time.Location, boundary string, at time.Time, before, after int) windowsRuleComparison {
	_, actualBefore := at.Add(-time.Second).In(location).Zone()
	_, actualAfter := at.In(location).Zone()
	return windowsRuleComparison{
		Boundary: boundary, AtUTC: at.Format(time.RFC3339),
		ExpectedBefore: before, ActualBefore: actualBefore, ExpectedAfter: after, ActualAfter: actualAfter,
		Matches: actualBefore == before && actualAfter == after,
	}
}

func windowsTransitionUTC(year int, transition windowsTransition, priorOffset int) (time.Time, error) {
	if transition.Month < 1 || transition.Month > 12 || transition.Hour > 23 || transition.Minute > 59 || transition.Second > 59 || transition.Milliseconds > 999 {
		return time.Time{}, fmt.Errorf("invalid transition fields: %+v", transition)
	}
	transitionYear := year
	day := int(transition.Day)
	if transition.Year != 0 {
		transitionYear = int(transition.Year)
	} else {
		if transition.Day < 1 || transition.Day > 5 || transition.DayOfWeek > 6 {
			return time.Time{}, fmt.Errorf("invalid relative transition fields: %+v", transition)
		}
		month := time.Month(transition.Month)
		if transition.Day == 5 {
			last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
			delta := (int(last.Weekday()) - int(transition.DayOfWeek) + 7) % 7
			day = last.Day() - delta
		} else {
			first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
			delta := (int(transition.DayOfWeek) - int(first.Weekday()) + 7) % 7
			day = 1 + delta + 7*(int(transition.Day)-1)
		}
	}
	localFields := time.Date(
		transitionYear, time.Month(transition.Month), day,
		int(transition.Hour), int(transition.Minute), int(transition.Second), int(transition.Milliseconds)*int(time.Millisecond), time.UTC,
	)
	if localFields.Year() != transitionYear || uint16(localFields.Month()) != transition.Month || localFields.Day() != day {
		return time.Time{}, fmt.Errorf("transition date overflow: %+v", transition)
	}
	return localFields.Add(-time.Duration(priorOffset) * time.Second), nil
}

func utf16Array128(value string) [128]uint16 {
	encoded, err := windows.UTF16FromString(value)
	if err != nil {
		panic(err)
	}
	var result [128]uint16
	copy(result[:], encoded)
	return result
}

func TestWindowsAnnualRuleComparisonUsesPreTransitionBiases(t *testing.T) {
	location, err := report.LoadTimezone("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	information := windowsTimeZoneInformation{
		Bias:         360,
		StandardDate: windowsSystemTime{Month: 11, DayOfWeek: 0, Day: 1, Hour: 2},
		DaylightDate: windowsSystemTime{Month: 3, DayOfWeek: 0, Day: 2, Hour: 2},
		DaylightBias: -60,
	}
	comparisons, err := compareWindowsAnnualRule(location, 2026, information)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparisons) != 2 || comparisons[0].AtUTC != "2026-03-08T08:00:00Z" || comparisons[1].AtUTC != "2026-11-01T07:00:00Z" || !comparisons[0].Matches || !comparisons[1].Matches {
		t.Fatalf("Central 2026 comparisons = %+v", comparisons)
	}
}

func TestWindowsNativeTimezoneTransitionEvidenceUsesRealAPIs(t *testing.T) {
	evidence := readNativeWindowsTimezoneEvidence()
	resolution, err := resolveWindowsTimezone(
		func() windowsTimezoneEvidence { return evidence },
		windowszonesdata.Candidates,
		func(name string) error { _, err := report.LoadTimezone(name); return err },
	)
	if err != nil {
		t.Fatalf("resolve current Windows timezone: %v", err)
	}
	rules, ruleErr := readNativeWindowsRuleEvidence(resolution.name)
	record := struct {
		OSBuild      uint32                    `json:"os_build"`
		Architecture string                    `json:"process_architecture"`
		Selected     string                    `json:"selected_name"`
		Rules        windowsNativeRuleEvidence `json:"rule_evidence"`
	}{
		OSBuild: windows.RtlGetVersion().BuildNumber, Architecture: runtime.GOARCH,
		Selected: resolution.name, Rules: rules,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 16*1024 {
		t.Fatalf("native transition evidence exceeds bound: %d", len(encoded))
	}
	t.Logf("windows_timezone_transition_evidence=%s", encoded)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	if len(rules.Years) == 0 {
		t.Fatal("no representative Windows annual rules were compared")
	}
	for _, year := range rules.Years {
		if !year.Matches {
			t.Errorf("Windows/IANA transition mismatch for %d: %+v", year.Year, year)
		}
	}
}

func TestWindowsNativeTimezoneAdapterUsesRealAPIs(t *testing.T) {
	if unsafe.Sizeof(windowsSystemTime{}) != 16 || unsafe.Sizeof(windowsDynamicTimeZoneInformation{}) != 432 {
		t.Fatalf("native layouts: SYSTEMTIME=%d DYNAMIC_TIME_ZONE_INFORMATION=%d", unsafe.Sizeof(windowsSystemTime{}), unsafe.Sizeof(windowsDynamicTimeZoneInformation{}))
	}
	var layout windowsDynamicTimeZoneInformation
	if unsafe.Offsetof(layout.StandardName) != 4 || unsafe.Offsetof(layout.StandardDate) != 68 || unsafe.Offsetof(layout.StandardBias) != 84 ||
		unsafe.Offsetof(layout.DaylightName) != 88 || unsafe.Offsetof(layout.DaylightDate) != 152 || unsafe.Offsetof(layout.DaylightBias) != 168 ||
		unsafe.Offsetof(layout.TimeZoneKeyName) != 172 || unsafe.Offsetof(layout.DynamicDaylightTimeDisabled) != 428 {
		t.Fatalf("unexpected native field offsets: %+v", layout)
	}

	// Win32 documents last-error only for TIME_ZONE_ID_INVALID. A successful
	// status must not be rejected because syscall retained an older error.
	stale := windowsDynamicTimeZoneInformation{TimeZoneKeyName: utf16Array128("Central Standard Time")}
	staleEvidence := classifyWindowsDynamicTimezone(&stale, windowsTimeZoneIDDaylight, syscall.Errno(5))
	if staleEvidence.dynamicStatus != windowsEvidenceSuccess || staleEvidence.keyName != "Central Standard Time" || staleEvidence.dynamicErrorCode != 0 {
		t.Fatalf("successful status consumed stale last-error: %+v", staleEvidence)
	}
	territory := windowsTimezoneEvidence{}
	encodedTerritory, err := windows.UTF16FromString("US")
	if err != nil {
		t.Fatal(err)
	}
	classifyWindowsTerritory(&territory, encodedTerritory, 1, syscall.Errno(5))
	if territory.territoryStatus != windowsEvidenceSuccess || territory.territory != "US" || territory.territoryErrorCode != 0 {
		t.Fatalf("successful territory result consumed stale last-error: %+v", territory)
	}

	evidence := readNativeWindowsTimezoneEvidence()
	result, resolveErr := resolveWindowsTimezone(
		func() windowsTimezoneEvidence { return evidence },
		windowszonesdata.Candidates,
		func(name string) error { _, err := report.LoadTimezone(name); return err },
	)
	version := windows.RtlGetVersion()
	record := struct {
		OSBuild      uint32                  `json:"os_build"`
		Architecture string                  `json:"process_architecture"`
		Evidence     windowsTimezoneEvidence `json:"native_evidence"`
		CLDRRelease  string                  `json:"cldr_release"`
		CLDRSource   string                  `json:"cldr_source_sha256"`
		Candidates   []string                `json:"ordered_candidates,omitempty"`
		Selected     string                  `json:"selected_name,omitempty"`
		Mapping      string                  `json:"selection_reason,omitempty"`
		Loader       string                  `json:"shared_loader"`
	}{
		OSBuild: version.BuildNumber, Architecture: runtime.GOARCH, Evidence: evidence,
		CLDRRelease: windowszonesdata.CLDRRelease, CLDRSource: windowszonesdata.SourceSHA256,
		Candidates: result.candidates, Selected: result.name, Mapping: string(result.mapping), Loader: "failed",
	}
	if resolveErr == nil {
		record.Loader = "success"
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 4096 {
		t.Fatalf("native evidence exceeds bound: %d", len(encoded))
	}
	t.Logf("windows_timezone_native_evidence=%s", encoded)
	if evidence.dynamicStatus != windowsEvidenceSuccess || evidence.keyName == "" {
		t.Fatalf("real GetDynamicTimeZoneInformation evidence is unusable: %+v", evidence)
	}
	if resolveErr != nil || result.name == "" {
		t.Fatalf("real native evidence did not resolve and load: result=%+v error=%v", result, resolveErr)
	}
	if want := os.Getenv("COPILOTD_TEST_WINDOWS_TERRITORY"); want != "" && (evidence.territoryStatus != windowsEvidenceSuccess || evidence.territory != want) {
		t.Fatalf("real GetUserDefaultGeoName evidence status=%q territory=%q, want success/%q", evidence.territoryStatus, evidence.territory, want)
	}
	if want := os.Getenv("COPILOTD_TEST_WINDOWS_MAPPING_SOURCE"); want != "" && string(result.mapping) != want {
		t.Fatalf("real native mapping source=%q, want %q", result.mapping, want)
	}
	if want := os.Getenv("COPILOTD_TEST_SYSTEM_ZONE"); want != "" && result.name != want {
		t.Fatalf("real native selected name=%q, want %q", result.name, want)
	}
}
