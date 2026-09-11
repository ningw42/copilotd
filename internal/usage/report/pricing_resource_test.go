package report_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestQueryRejectsOversizedPricingProjectionWithoutWalkingEverySelectedKey(t *testing.T) {
	path := stored(t)
	small := cachedPricingSource(t, selectedPricingArtifact(8, 1_024))
	largeArtifact := selectedPricingArtifact(2_000, 1_024)
	if len(largeArtifact) > 8<<20 {
		t.Fatalf("large artifact is %d bytes, exceeds remote acceptance cap", len(largeArtifact))
	}
	large := cachedPricingSource(t, largeArtifact)

	queryAllocations := func(source pricing.Source) float64 {
		t.Helper()
		reader := report.NewReadLimitsForTest(path, source, report.MaxRows, report.MaxGroups, 0)
		var got report.Report
		var err error
		allocations := testing.AllocsPerRun(3, func() {
			got, err = reader.Query(context.Background(), selection())
		})
		var failure *report.Error
		if !errors.As(err, &failure) || failure.Code != report.TooLarge || !reflect.DeepEqual(got, report.Report{}) {
			t.Fatalf("zero-byte real Query = %+v, %v; want report_too_large", got, err)
		}
		return allocations
	}

	smallAllocations := queryAllocations(small)
	largeAllocations := queryAllocations(large)
	t.Logf("zero-byte real Query allocation counts: 8 selected models=%.0f, 2,000 selected models=%.0f", smallAllocations, largeAllocations)
	// This characterizes allocation counts, not peak-live memory. Calendar and
	// public-error construction add a constant baseline; selected-key work must
	// not grow with the remainder of an already accepted raw artifact.
	if largeAllocations > smallAllocations+300 {
		t.Fatalf("real Query allocations grew with all selected keys: small=%.0f large=%.0f", smallAllocations, largeAllocations)
	}
}

func cachedPricingSource(t *testing.T, artifact []byte) *pricing.CachedSource {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(artifact)
	}))
	t.Cleanup(server.Close)
	registry := cache.NewRegistry()
	source := pricing.NewCachedSource(
		pricing.CacheConfig{RefreshInterval: time.Hour},
		pricing.NewRemote(server.URL, server.Client().Transport),
		registry,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	registry.Prime(context.Background())
	status := registry.Observe()
	if len(status) != 1 || status[0].Source != "fetched" {
		t.Fatalf("pricing artifact was not accepted: %#v", status)
	}
	return source
}

func selectedPricingArtifact(count, identityBytes int) []byte {
	var models strings.Builder
	models.WriteByte('{')
	for index := range count {
		if index != 0 {
			models.WriteByte(',')
		}
		prefix := strconv.Itoa(index) + "-"
		model := prefix + strings.Repeat("x", identityBytes-len(prefix))
		models.WriteString(strconv.Quote(model))
		models.WriteString(`:{"id":`)
		models.WriteString(strconv.Quote(model))
		models.WriteByte('}')
	}
	models.WriteByte('}')
	return []byte(`{"openai":{"id":"openai","models":` + models.String() + `},"anthropic":{"id":"anthropic","models":{}},"google":{"id":"google","models":{}},"xai":{"id":"xai","models":{}}}`)
}
