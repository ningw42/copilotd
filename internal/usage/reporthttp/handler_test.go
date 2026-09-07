package reporthttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

type controlledWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	entered  chan struct{}
	release  <-chan struct{}
}

func (w *controlledWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *controlledWriter) Write(body []byte) (int, error) {
	if w.entered != nil {
		w.entered <- struct{}{}
		<-w.release
	}
	return w.ResponseRecorder.Write(body)
}

func TestHandlerInvalidModelUTF8IsAdmittedSemanticsBeforeSQL(t *testing.T) {
	reader := report.New(filepath.Join(t.TempDir(), "missing", "usage.db"))
	var calls atomic.Int32
	entered, release := make(chan struct{}, 2), make(chan struct{})
	handler := reporthttp.Handler(func(ctx context.Context, q report.Query) (report.Report, error) {
		if calls.Add(1) <= 2 {
			entered <- struct{}{}
			<-release
		}
		return reader.Query(ctx, q)
	})
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
		}()
	}
	<-entered
	<-entered
	for _, tc := range []struct {
		query  string
		status int
	}{{"timezone=UTC&model=%ff", 429}, {"timezone=UTC&model=%xx", 400}, {"timezone=UTC&model=", 400}, {"timezone=UTC&model=a&model=b", 400}} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?"+tc.query, nil))
		if rr.Code != tc.status {
			t.Errorf("%s status=%d", tc.query, rr.Code)
		}
	}
	close(release)
	workers.Wait()
	for _, query := range []string{"timezone=UTC&model=%ff", "timezone=UTC&model=a%ed%a0%80"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?"+query, nil))
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), "invalid_query") {
			t.Fatalf("semantic model validation before missing DB: %d %s", rr.Code, rr.Body.String())
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("syntax/admission performed query work: %d", calls.Load())
	}
}

func TestHandlerPanicRecoveryCanWriteUnderRouteDeadline(t *testing.T) {
	writer := &controlledWriter{ResponseRecorder: httptest.NewRecorder()}
	handler := reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { panic("unexpected report failure") })
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected generic recovery to receive panic")
			}
		}()
		handler.ServeHTTP(writer, httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
	}()
	if writer.deadline.IsZero() {
		t.Error("panic recovery response has no route-local write bound")
	}
}

func TestHandlerEncodingBudgetIsSharedBetweenNativeSections(t *testing.T) {
	model := strings.Repeat("\x01", 1<<20)
	section := &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{{Model: model}}}
	handler := reporthttp.Handler(func(_ context.Context, q report.Query) (report.Report, error) {
		r := report.Report{}
		if q.Surface != "openai" {
			r.Anthropic = section
		}
		if q.Surface != "anthropic" {
			r.OpenAI = section
		}
		return r, nil
	})
	for _, surface := range []string{"anthropic", "openai", "all"} {
		for _, method := range []string{"GET", "HEAD"} {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(method, reporthttp.Path+"?timezone=UTC&surface="+surface, nil))
			wantStatus := 200
			if surface == "all" {
				wantStatus = 422
			}
			if rr.Code != wantStatus {
				t.Fatalf("%s %s status=%d body bytes=%d", method, surface, rr.Code, rr.Body.Len())
			}
			if method == "HEAD" {
				if rr.Body.Len() != 0 {
					t.Fatal("HEAD body")
				}
			} else if surface == "all" {
				if !strings.Contains(rr.Body.String(), "report_too_large") || strings.Contains(rr.Body.String(), `"models"`) {
					t.Fatal("partial success escaped shared body budget")
				}
			} else if rr.Body.Len() < 6<<20 || rr.Body.Len() > 8<<20 {
				t.Fatalf("single native section size=%d", rr.Body.Len())
			}
		}
	}
}

func TestHandlerBoundsEffectiveModelMetadataBeforeEncoding(t *testing.T) {
	// A direct Query provider is not constrained by the HTTP raw-query cap.
	// Metadata is a model-bearing fragment too, even with no selected rows.
	model := strings.Repeat("x", report.MaxModelBytes+1)
	handler := reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return report.Report{Model: &model}, nil })
	for _, method := range []string{"GET", "HEAD"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(method, reporthttp.Path+"?timezone=UTC", nil))
		if rr.Code != 422 {
			t.Fatalf("%s metadata size status=%d body bytes=%d", method, rr.Code, rr.Body.Len())
		}
		if method == "GET" && (!strings.Contains(rr.Body.String(), "report_too_large") || strings.Contains(rr.Body.String(), `"model"`)) {
			t.Fatal("partial metadata escaped")
		}
	}
}

func TestHandlerBoundsEncodingAndEveryWrite(t *testing.T) {
	model := strings.Repeat("\x01", 1<<20)
	handler := reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) {
		return report.Report{OpenAI: &report.Section{Models: []report.ModelTotal{{Model: model}, {Model: model}}}}, nil
	})
	rr := &controlledWriter{ResponseRecorder: httptest.NewRecorder()}
	handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), "report_too_large") {
		t.Fatalf("encoding limit status=%d len=%d", rr.Code, rr.Body.Len())
	}
	if rr.deadline.IsZero() || time.Until(rr.deadline) > 5*time.Second {
		t.Error("missing bounded write deadline")
	}
	rr = &controlledWriter{ResponseRecorder: httptest.NewRecorder()}
	reporthttp.Handler(nil).ServeHTTP(rr, httptest.NewRequest("POST", reporthttp.Path, nil))
	if rr.deadline.IsZero() {
		t.Error("early errors have no write deadline")
	}
}

func TestHandlerRetainsAdmissionWhileResponseWriteBlocks(t *testing.T) {
	handler := reporthttp.Handler(nil)
	handler = reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) { return report.Report{}, nil })
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := &controlledWriter{ResponseRecorder: httptest.NewRecorder(), entered: entered, release: release}
			handler.ServeHTTP(w, httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
		}()
	}
	<-entered
	<-entered
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
	close(release)
	workers.Wait()
	if rr.Code != 429 {
		t.Fatalf("slow-write admission status=%d", rr.Code)
	}
}

func TestHandlerAdmissionPrecedesSemanticsAndHoldsThroughWrites(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	handler := reporthttp.Handler(func(ctx context.Context, q report.Query) (report.Report, error) {
		if calls.Add(1) <= 2 {
			entered <- struct{}{}
			<-release
		}
		return report.Report{}, &report.Error{Code: report.InvalidQuery, Message: "invalid date"}
	})
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC", nil))
		}()
	}
	<-entered
	<-entered
	for _, tc := range []struct {
		query  string
		status int
	}{{"timezone=UTC&since=invalid", 429}, {"timezone=UTC&timezone=UTC", 400}} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?"+tc.query, nil))
		if rr.Code != tc.status {
			t.Errorf("%s status=%d want %d", tc.query, rr.Code, tc.status)
		}
		if tc.status == 429 && rr.Header().Get("Retry-After") != "1" {
			t.Error("missing retry hint")
		}
	}
	close(release)
	workers.Wait()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?timezone=UTC&since=invalid", nil))
	if rr.Code != 400 {
		t.Fatalf("released slot: %d", rr.Code)
	}
}

func TestHandlerWorkTimeoutWinsOverQueryError(t *testing.T) {
	handler := reporthttp.Handler(func(ctx context.Context, _ report.Query) (report.Report, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("missing fixed work deadline")
		}
		<-ctx.Done()
		return report.Report{}, &report.Error{Code: report.Unavailable, Message: "driver interrupted"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, reporthttp.Path+"?timezone=UTC", nil).WithContext(ctx))
	if rr.Code != 504 || !strings.Contains(rr.Body.String(), "report_timeout") {
		t.Fatalf("timeout response: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerLocalContractAndErrorPrecedence(t *testing.T) {
	disabled := reporthttp.Handler(nil)
	for _, tc := range []struct {
		method, query string
		status        int
		code          string
	}{
		{"GET", "%xx", 503, "usage_meter_disabled"}, {"HEAD", "", 503, "usage_meter_disabled"}, {"POST", "%xx", 405, "method_not_allowed"},
	} {
		rr := httptest.NewRecorder()
		disabled.ServeHTTP(rr, httptest.NewRequest(tc.method, reporthttp.Path+"?"+tc.query, nil))
		if rr.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.query, rr.Code)
		}
		if tc.method == "HEAD" {
			if rr.Body.Len() != 0 {
				t.Fatal("HEAD body")
			}
		} else if !strings.Contains(rr.Body.String(), tc.code) {
			t.Fatal(rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Content-Type") != "application/json" || rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("headers: %v", rr.Header())
		}
		if tc.status == 405 && rr.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("missing Allow")
		}
	}
	handler := reporthttp.Handler(func(_ context.Context, q report.Query) (report.Report, error) {
		return report.Report{}, &report.Error{Code: report.InvalidQuery, Message: "invalid date"}
	})
	for _, query := range []string{"", "timezone=UTC&timezone=UTC", "timezone=UTC&bogus=x", "timezone=UTC&since=", "timezone=%xx", strings.Repeat("x", 16385)} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", reporthttp.Path+"?"+query, nil))
		if rr.Code != 400 {
			t.Errorf("query %q: %d", query, rr.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
}
