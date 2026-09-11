package reporthttp_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

const nonUTCRefreshHelper = "COPILOTD_REPORTHTTP_NONUTC_REFRESH_HELPER"

func TestHandlerClientNormalizesCachedPricingSuccessToUTC(t *testing.T) {
	if os.Getenv(nonUTCRefreshHelper) == "1" {
		testHandlerClientNormalizesCachedPricingSuccessToUTC(t)
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestHandlerClientNormalizesCachedPricingSuccessToUTC$", "-test.v")
	command.Env = append(os.Environ(), nonUTCRefreshHelper+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("non-UTC helper process failed: %v\n%s", err, output)
	}
}

func testHandlerClientNormalizesCachedPricingSuccessToUTC(t *testing.T) {
	previousLocal := time.Local
	time.Local = time.FixedZone("report-daemon", 9*60*60)
	defer func() { time.Local = previousLocal }()

	const artifact = `{"openai":{"id":"openai","models":{}},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`
	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, artifact)
	}))
	defer prices.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(
		pricing.CacheConfig{RefreshInterval: time.Hour},
		pricing.NewRemote(prices.URL, prices.Client().Transport),
		registry,
		logger,
	)
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(logger, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	closed := store.Close(context.Background())
	if !closed.DriverCleanupCompleted || closed.FinalFlushLosses != 0 {
		t.Fatalf("fixture close = %+v", closed)
	}

	reader := report.New(path, source)
	server := httptest.NewServer(reporthttp.Handler(reader.Query))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := report.Query{
		Period: "day", Timezone: "UTC", Surface: "all",
		Since: "2026-09-01", Until: "2026-09-02",
	}

	cold, err := client.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("cold fallback report: %v", err)
	}
	if cold.Report.Pricing == nil || cold.Report.Pricing.Source != "fallback" || cold.Report.Pricing.LastSuccess != nil {
		t.Fatalf("cold fallback provenance = %+v", cold.Report.Pricing)
	}

	registry.Prime(context.Background())
	_, capturedStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if capturedStatus.Source != "fetched" || capturedStatus.LastSuccess == nil {
		t.Fatalf("primed source status = %+v", capturedStatus)
	}
	capturedSuccess := *capturedStatus.LastSuccess
	_, capturedOffset := capturedSuccess.Zone()
	if capturedOffset != 9*60*60 {
		t.Fatalf("captured successful-fetch offset = %d, want +09:00", capturedOffset)
	}

	fetched, err := client.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("fetched report: %v", err)
	}
	if fetched.Report.Pricing == nil || fetched.Report.Pricing.Source != "fetched" || fetched.Report.Pricing.LastSuccess == nil {
		t.Fatalf("fetched provenance = %+v", fetched.Report.Pricing)
	}
	wireSuccess := *fetched.Report.Pricing.LastSuccess
	_, wireOffset := wireSuccess.Zone()
	if wireOffset != 0 || !wireSuccess.Equal(capturedSuccess) {
		t.Fatalf("wire successful-fetch time = %s (offset %d), want UTC instant equal to %s", wireSuccess.Format(time.RFC3339Nano), wireOffset, capturedSuccess.Format(time.RFC3339Nano))
	}

	_, retainedStatus, err := source.Current(context.Background(), pricing.ProjectionLimit{MaxIdentityBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if retainedStatus.LastSuccess == nil || !retainedStatus.LastSuccess.Equal(capturedSuccess) {
		t.Fatalf("source successful-fetch instant changed: before=%s after=%+v", capturedSuccess.Format(time.RFC3339Nano), retainedStatus.LastSuccess)
	}
	_, retainedOffset := retainedStatus.LastSuccess.Zone()
	if retainedOffset != capturedOffset {
		t.Fatalf("source successful-fetch representation was mutated: offset=%d, want %d", retainedOffset, capturedOffset)
	}
}
