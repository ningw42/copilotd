//go:build linux

package report_test

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestQueryCapturesPricingBeforeCalendarFilesystemWork(t *testing.T) {
	const helperEnvironment = "COPILOTD_QUERY_ENTRY_TIMEZONE_HELPER"
	if os.Getenv(helperEnvironment) != "1" {
		// time.LoadLocation reads ZONEINFO once per process. A helper process keeps
		// this test repeatable and independent of other calendar tests without a
		// production reset/progress hook.
		command := exec.Command(os.Args[0], "-test.run=^TestQueryCapturesPricingBeforeCalendarFilesystemWork$", "-test.count=1", "-test.timeout=20s")
		command.Env = append(os.Environ(), helperEnvironment+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("query-entry timezone helper: %v\n%s", err, output)
		}
		return
	}

	// Linux-only public-boundary fixture: a FIFO delays time.LoadLocation without
	// adding a production hook. A unique operator-provided ZONEINFO directory
	// prevents interaction with installed or embedded timezone data.
	zoneRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(zoneRoot, "Review"), 0o700); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(zoneRoot, "Review", "Calendar")
	if err := syscall.Mkfifo(zonePath, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZONEINFO", zoneRoot)

	before := pricingSource(t, `{"mixed":{"id":"mixed","cost":{"input":1,"output":1}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:before-calendar"})
	during := pricingSource(t, `{"mixed":{"id":"mixed","cost":{"input":10,"output":10}}}`, pricing.SnapshotStatus{Source: "fetched", Version: "sha256:during-calendar"})
	source := &mutablePricingSource{current: before}
	zero := int64(0)
	path := stored(t, turn("2026-09-01T12:00:00Z", "mixed", usage.OpenAIUsage{
		InputTokens: 10, OutputTokens: 2, CachedTokens: &zero, CacheWriteTokens: &zero,
	}))
	reader := report.New(path, source)
	q := selection()
	q.Timezone = "Review/Calendar"

	type outcome struct {
		report report.Report
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		got, err := reader.Query(context.Background(), q)
		done <- outcome{report: got, err: err}
	}()

	// Opening the writer pairs only after Query has entered timezone loading.
	writer, err := os.OpenFile(zonePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	callsBeforeCalendarCompleted := source.callCount()
	source.set(during)
	if _, err := writer.Write(minimalUTCTZif()); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	captured := <-done
	if captured.err != nil {
		t.Fatal(captured.err)
	}
	if callsBeforeCalendarCompleted != 1 || captured.report.Pricing.Version != "sha256:before-calendar" || captured.report.OpenAI.Total.Cost.Amount == nil || captured.report.OpenAI.Total.Cost.Amount.String() != "0.000012" {
		t.Fatalf("entry capture calls=%d report=%+v", callsBeforeCalendarCompleted, captured.report)
	}

	next, err := reader.Query(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if source.callCount() != 2 || next.Pricing.Version != "sha256:during-calendar" || next.OpenAI.Total.Cost.Amount == nil || next.OpenAI.Total.Cost.Amount.String() != "0.00012" {
		t.Fatalf("next Query calls=%d report=%+v", source.callCount(), next)
	}
	if captured.report.Pricing.Version != "sha256:before-calendar" || captured.report.OpenAI.Total.Cost.Amount.String() != "0.000012" {
		t.Fatal("later source revision mutated the entry-captured report")
	}
}

func minimalUTCTZif() []byte {
	data := make([]byte, 54)
	copy(data, "TZif")
	binary.BigEndian.PutUint32(data[36:40], 1)
	binary.BigEndian.PutUint32(data[40:44], 4)
	copy(data[50:], "UTC\x00")
	return data
}
