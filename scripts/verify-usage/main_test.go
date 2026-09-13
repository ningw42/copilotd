package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fakeWindowsTimezoneHost struct {
	current        windowsHostTimezoneState
	events         []string
	stateOverrides map[string]windowsHostTimezoneState
	timezoneErrors map[string]error
	homeErrors     map[string]error
}

func (h *fakeWindowsTimezoneHost) state(label string) (windowsHostTimezoneState, error) {
	h.events = append(h.events, "state:"+label)
	if state, ok := h.stateOverrides[label]; ok {
		return state, nil
	}
	return h.current, nil
}

func (h *fakeWindowsTimezoneHost) setTimezone(label, timezone string) error {
	h.events = append(h.events, "timezone:"+label+":"+timezone)
	if err := h.timezoneErrors[label]; err != nil {
		return err
	}
	h.current.Timezone = timezone
	return nil
}

func (h *fakeWindowsTimezoneHost) setHomeGeoID(label string, geoID int) error {
	h.events = append(h.events, fmt.Sprintf("home:%s:%d", label, geoID))
	if err := h.homeErrors[label]; err != nil {
		return err
	}
	h.current.HomeGeoID = geoID
	return nil
}

func TestControlledWindowsTimezoneRestoresAfterTestFailure(t *testing.T) {
	host := &fakeWindowsTimezoneHost{current: windowsHostTimezoneState{Timezone: "Pacific Standard Time_dstoff", HomeGeoID: 39}}
	testFailure := errors.New("controlled executable failed")
	test := controlledWindowsTimezoneCases()[0]
	err := runControlledWindowsTimezoneCases(host, []controlledWindowsTimezoneCase{test}, func(controlledWindowsTimezoneCase) error {
		host.events = append(host.events, "test")
		return testFailure
	})
	if !errors.Is(err, testFailure) {
		t.Fatalf("controlled failure was lost: %v", err)
	}
	if host.current != (windowsHostTimezoneState{Timezone: "Pacific Standard Time_dstoff", HomeGeoID: 39}) {
		t.Fatalf("host was not restored: %+v", host.current)
	}
	wantEvents := "state:snapshot;timezone:setup-central_us_exact:Central Standard Time;home:setup-central_us_exact:244;state:setup-check-central_us_exact;test;timezone:restore:Pacific Standard Time_dstoff;home:restore:39;state:restore-check"
	if got := strings.Join(host.events, ";"); got != wantEvents {
		t.Fatalf("events = %s, want %s", got, wantEvents)
	}
}

func TestControlledWindowsTimezoneSetupMismatchFailsBeforeTestAndRestores(t *testing.T) {
	original := windowsHostTimezoneState{Timezone: "Tokyo Standard Time", HomeGeoID: 122}
	host := &fakeWindowsTimezoneHost{
		current: original,
		stateOverrides: map[string]windowsHostTimezoneState{
			"setup-check-central_us_exact": {Timezone: "Central Standard Time", HomeGeoID: 39},
		},
	}
	ran := false
	err := runControlledWindowsTimezoneCases(host, controlledWindowsTimezoneCases(), func(controlledWindowsTimezoneCase) error {
		ran = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "setup") || ran {
		t.Fatalf("setup mismatch did not fail closed: ran=%v err=%v", ran, err)
	}
	if host.current != original {
		t.Fatalf("host was not restored: %+v", host.current)
	}
}

func TestControlledWindowsTimezoneRestoreFailureIsReportedAfterAllRestoreSteps(t *testing.T) {
	original := windowsHostTimezoneState{Timezone: "Tokyo Standard Time", HomeGeoID: 122}
	restoreFailure := errors.New("tzutil restore failed")
	host := &fakeWindowsTimezoneHost{
		current:        original,
		timezoneErrors: map[string]error{"restore": restoreFailure},
	}
	test := controlledWindowsTimezoneCases()[1]
	err := runControlledWindowsTimezoneCases(host, []controlledWindowsTimezoneCase{test}, func(controlledWindowsTimezoneCase) error { return nil })
	if !errors.Is(err, restoreFailure) || !strings.Contains(strings.Join(host.events, ";"), "home:restore:122;state:restore-check") {
		t.Fatalf("restore did not fail closed after all steps: events=%v err=%v", host.events, err)
	}
}

func TestControlledWindowsTimezoneRunsEveryApprovedCaseAndRestoresOnce(t *testing.T) {
	original := windowsHostTimezoneState{Timezone: "Tokyo Standard Time", HomeGeoID: 122}
	host := &fakeWindowsTimezoneHost{current: original}
	var ran []string
	err := runControlledWindowsTimezoneCases(host, controlledWindowsTimezoneCases(), func(test controlledWindowsTimezoneCase) error {
		if host.current != (windowsHostTimezoneState{Timezone: test.windowsKey, HomeGeoID: 244}) {
			t.Fatalf("%s ran under %+v", test.name, host.current)
		}
		ran = append(ran, test.name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if host.current != original {
		t.Fatalf("host was not restored: %+v", host.current)
	}
	if got, want := strings.Join(ran, ","), "central_us_exact,china_us_world,nepal_us_world,central_dst_disabled"; got != want {
		t.Fatalf("ran %s, want %s", got, want)
	}
	var snapshotCount, restoreCount int
	for _, event := range host.events {
		if event == "state:snapshot" {
			snapshotCount++
		}
		if strings.HasPrefix(event, "timezone:restore:") {
			restoreCount++
		}
	}
	if snapshotCount != 1 || restoreCount != 1 {
		t.Fatalf("snapshot=%d restore=%d events=%v", snapshotCount, restoreCount, host.events)
	}
}

func TestWindowsTimezoneVerificationInventory(t *testing.T) {
	required := fmt.Sprint(mandatoryTests("windows"))
	if !strings.Contains(required, "internal/usage/reportcli:TestWindowsNativeTimezoneAdapterUsesRealAPIs") ||
		!strings.Contains(required, "internal/usage/reportcli:TestWindowsNativeTimezoneTransitionEvidenceUsesRealAPIs") ||
		!strings.Contains(required, "cmd/copilotd:TestUsageExecutableAcceptance/system_timezone") {
		t.Fatalf("Windows mandatory inventory = %s", required)
	}
	cases := controlledWindowsTimezoneCases()
	want := "central_us_exact=Central Standard Time/US/America/Chicago/;china_us_world=China Standard Time/US/Asia/Shanghai/;nepal_us_world=Nepal Standard Time/US/Asia/Katmandu/;central_dst_disabled=Central Standard Time_dstoff/US//dynamic daylight time is disabled"
	var got []string
	for _, test := range cases {
		got = append(got, test.name+"="+test.windowsKey+"/"+test.territory+"/"+test.wantZone+"/"+test.wantError)
	}
	if strings.Join(got, ";") != want {
		t.Fatalf("controlled Windows timezone cases = %s, want %s", strings.Join(got, ";"), want)
	}
}

func TestMandatoryReportCLITestInventoryNamesExistingTests(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "usage", "reportcli", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no reportcli test files found")
	}

	available := make(map[string]struct{})
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil {
				available[function.Name.Name] = struct{}{}
			}
		}
	}

	const prefix = "internal/usage/reportcli:"
	for _, required := range mandatoryTests(runtime.GOOS) {
		name, found := strings.CutPrefix(required, prefix)
		if !found || strings.Contains(name, "/") {
			continue
		}
		if _, found := available[name]; !found {
			t.Errorf("mandatory test %s does not name an existing reportcli test", required)
		}
	}
}
