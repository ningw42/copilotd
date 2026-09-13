//go:build windows

package reportcli

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsTimeZoneIDUnknown  = 0
	windowsTimeZoneIDStandard = 1
	windowsTimeZoneIDDaylight = 2
	windowsTimeZoneIDInvalid  = ^uintptr(0) & 0xffffffff
	windowsGeoNameLength      = 85
)

type windowsSystemTime struct {
	Year         uint16
	Month        uint16
	DayOfWeek    uint16
	Day          uint16
	Hour         uint16
	Minute       uint16
	Second       uint16
	Milliseconds uint16
}

// windowsDynamicTimeZoneInformation is the Win32
// DYNAMIC_TIME_ZONE_INFORMATION layout. Windows uses 4-byte structure
// alignment on both amd64 and arm64; the explicit tail padding pins sizeof 432.
type windowsDynamicTimeZoneInformation struct {
	Bias                        int32
	StandardName                [32]uint16
	StandardDate                windowsSystemTime
	StandardBias                int32
	DaylightName                [32]uint16
	DaylightDate                windowsSystemTime
	DaylightBias                int32
	TimeZoneKeyName             [128]uint16
	DynamicDaylightTimeDisabled uint8
	_                           [3]byte
}

var (
	windowsKernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	windowsGetDynamicTimeZoneInfoProc = windowsKernel32.NewProc("GetDynamicTimeZoneInformation")
	windowsGetUserDefaultGeoNameProc  = windowsKernel32.NewProc("GetUserDefaultGeoName")
)

func configureNativeWindowsTimezoneSystem(system *localTimezoneSystem) {
	system.windowsEvidence = readNativeWindowsTimezoneEvidence
}

func readNativeWindowsTimezoneEvidence() windowsTimezoneEvidence {
	var information windowsDynamicTimeZoneInformation
	if err := windowsGetDynamicTimeZoneInfoProc.Find(); err != nil {
		return windowsTimezoneEvidence{dynamicStatus: windowsEvidenceUnavailable, dynamicErrorCode: windowsErrorCode(err)}
	}
	status, _, callErr := windowsGetDynamicTimeZoneInfoProc.Call(uintptr(unsafe.Pointer(&information)))
	evidence := classifyWindowsDynamicTimezone(&information, status, callErr)
	if evidence.dynamicStatus != windowsEvidenceSuccess {
		return evidence
	}

	if err := windowsGetUserDefaultGeoNameProc.Find(); err != nil {
		evidence.territoryStatus = windowsEvidenceUnavailable
		evidence.territoryErrorCode = windowsErrorCode(err)
		return evidence
	}
	var territory [windowsGeoNameLength]uint16
	result, _, geoErr := windowsGetUserDefaultGeoNameProc.Call(
		uintptr(unsafe.Pointer(&territory[0])),
		uintptr(len(territory)),
	)
	classifyWindowsTerritory(&evidence, territory[:], result, geoErr)
	return evidence
}

func classifyWindowsDynamicTimezone(information *windowsDynamicTimeZoneInformation, status uintptr, callErr error) windowsTimezoneEvidence {
	if status == windowsTimeZoneIDInvalid {
		return windowsTimezoneEvidence{
			dynamicStatus:    windowsEvidenceFailed,
			dynamicAPIStatus: uint32(status),
			dynamicErrorCode: windowsErrorCode(callErr),
		}
	}
	statusName := "unknown"
	switch status {
	case windowsTimeZoneIDStandard:
		statusName = "standard"
	case windowsTimeZoneIDDaylight:
		statusName = "daylight"
	}
	// Win32 defines last-error only for TIME_ZONE_ID_INVALID. In particular,
	// never consume Proc.Call's stale last-error after a successful status.
	return windowsTimezoneEvidence{
		dynamicStatus:               windowsEvidenceSuccess,
		dynamicAPIStatus:            uint32(status),
		dynamicAPIStatusName:        statusName,
		keyName:                     windows.UTF16ToString(information.TimeZoneKeyName[:]),
		dynamicDaylightTimeDisabled: information.DynamicDaylightTimeDisabled != 0,
		bias:                        information.Bias,
		standardBias:                information.StandardBias,
		daylightBias:                information.DaylightBias,
		standardTransition:          windowsTransitionFromSystemTime(information.StandardDate),
		daylightTransition:          windowsTransitionFromSystemTime(information.DaylightDate),
	}
}

func classifyWindowsTerritory(evidence *windowsTimezoneEvidence, buffer []uint16, result uintptr, callErr error) {
	if result == 0 {
		evidence.territoryStatus = windowsEvidenceFailed
		evidence.territoryErrorCode = windowsErrorCode(callErr)
		return
	}
	// Like the dynamic-timezone API, successful Win32 BOOL results do not make
	// a retained last-error meaningful.
	evidence.territoryStatus = windowsEvidenceSuccess
	evidence.territory = windows.UTF16ToString(buffer)
}

func windowsTransitionFromSystemTime(value windowsSystemTime) windowsTransition {
	return windowsTransition{
		Year: value.Year, Month: value.Month, DayOfWeek: value.DayOfWeek, Day: value.Day,
		Hour: value.Hour, Minute: value.Minute, Second: value.Second, Milliseconds: value.Milliseconds,
	}
}

func windowsErrorCode(err error) uint32 {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	return 0
}
