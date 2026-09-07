package report

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TZif fixtures are external operator data, not a fake calendar collaborator.
// Run Query in a fresh process because time.LoadLocation caches ZONEINFO once.
func TestQueryOperatorTimezoneData(t *testing.T) {
	if os.Getenv("COPILOTD_TEST_CALENDAR_TZIF") == "1" {
		path := filepath.Join(t.TempDir(), "absent", "usage.db")
		for _, tc := range []struct {
			zone, since, until string
			code               Code
		}{
			{"Test/Reversal", "2024-01-01", "2024-01-04", InvalidQuery},
			{"Test/Excessive", "2024-01-01", "2024-01-02", TooLarge},
			{"Test/Unrepresentable", "9998-12-31", "9999-01-01", InvalidQuery},
		} {
			got, err := New(path).Query(context.Background(), Query{Timezone: tc.zone, Since: tc.since, Until: tc.until})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != tc.code || got.OpenAI != nil {
				t.Fatalf("%s: %+v %v", tc.zone, got, err)
			}
			if tc.zone == "Test/Reversal" && !strings.Contains(err.Error(), "non-monotonic") {
				t.Fatalf("must reject the actual reversed edges: %v", err)
			}
		}
		ctx := &calendarCancelContext{Context: context.Background(), remaining: 30}
		_, err := New(path).Query(ctx, Query{Timezone: "Test/Excessive", Since: "2024-01-01", Until: "2024-01-02"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("calendar cancellation: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("invalid calendar touched DB: %v", err)
		}
		r := calendarReader(t, "2024-01-01T12:00:00Z")
		got, err := r.Query(context.Background(), Query{Timezone: "Test/RangeReversal", Period: "year", Since: "2024-01-02", Until: "2024-01-03"})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != InvalidQuery || got.OpenAI != nil {
			t.Fatalf("nominal period edge after window start must not fabricate coverage: %+v %v", got, err)
		}
		for _, period := range []string{"week", "month", "year"} {
			r := calendarReader(t, "2023-12-31T12:00:00Z")
			got, err := r.Query(context.Background(), Query{Timezone: "Test/Calendar.v1-+_", Period: period, Since: "2023-12-31", Until: "2024-01-03"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Buckets) != 2 || got.Buckets[1].StartDate != "2024-01-01" || got.Buckets[1].RangeStart.Format(time.RFC3339) != "2024-01-01T00:00:00Z" || got.Buckets[1].RangeEnd.Format(time.RFC3339) != "2024-01-02T00:00:00Z" || !got.Buckets[1].RangePartial {
				t.Fatalf("skipped larger-period nominal edge %s: %+v", period, got.Buckets)
			}
		}
		return
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Test"), 0700); err != nil {
		t.Fatal(err)
	}
	unix := func(s string) int32 {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return int32(at.Unix())
	}
	writeTZif(t, filepath.Join(root, "Test", "Unrepresentable"), nil, nil, []int32{-2147483648})
	writeTZif(t, filepath.Join(root, "Test", "Reversal"), []int32{unix("2024-01-01T00:00:00Z"), unix("2024-01-01T01:00:00Z")}, []byte{1, 0}, []int32{0, 172800})
	writeTZif(t, filepath.Join(root, "Test", "RangeReversal"), []int32{unix("2023-12-31T23:00:00Z"), unix("2024-01-01T00:00:00Z")}, []byte{1, 0}, []int32{0, 90000})
	writeTZif(t, filepath.Join(root, "Test", "Calendar.v1-+_"), []int32{unix("2024-01-01T00:00:00Z")}, []byte{1}, []int32{0, 86400})
	transitions, types := make([]int32, 20000), make([]byte, 20000)
	for i := range transitions {
		transitions[i] = unix("2023-01-01T00:00:00Z") + int32(i)
		types[i] = byte(i % 2)
	}
	writeTZif(t, filepath.Join(root, "Test", "Excessive"), transitions, types, []int32{0, 1})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestQueryOperatorTimezoneData$", "-test.v")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ZONEINFO=") && !strings.HasPrefix(entry, "COPILOTD_TEST_CALENDAR_TZIF=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "ZONEINFO="+root, "COPILOTD_TEST_CALENDAR_TZIF=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("operator TZif Query: %v\n%s", err, output)
	}
}

type calendarCancelContext struct {
	context.Context
	remaining int
}

func (c *calendarCancelContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func writeTZif(t *testing.T, path string, transitions []int32, types []byte, offsets []int32) {
	t.Helper()
	var data bytes.Buffer
	data.WriteString("TZif")
	data.Write(make([]byte, 16))
	for _, n := range []int32{0, 0, 0, int32(len(transitions)), int32(len(offsets)), 2} {
		_ = binary.Write(&data, binary.BigEndian, n)
	}
	for _, at := range transitions {
		_ = binary.Write(&data, binary.BigEndian, at)
	}
	data.Write(types)
	for _, offset := range offsets {
		_ = binary.Write(&data, binary.BigEndian, offset)
		data.Write([]byte{0, 0})
	}
	data.Write([]byte{'X', 0})
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
