package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestReportBypassesReadinessAndOnlyMatchesExactLocalPath(t *testing.T) {
	provider := panickingProvider{panicOnReady: true}
	logger := discardLogger(t)
	reports := reporthttp.Handler(func(context.Context, report.Query) (report.Report, error) {
		return report.Report{}, &report.Error{Code: report.Unavailable, Message: "Usage data is unavailable on this daemon."}
	})
	handler := newHandler("key", provider, newTestReadyObservers(), logger, NewStreamOutcomeCounter(), []Mount{MountReport(reports)})
	for _, tc := range []struct {
		path   string
		status int
	}{{"/usage/v1/report?timezone=UTC", 503}, {"/usage/v1/report/", 404}, {"/usage/v1/report/more", 404}} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", tc.path, nil))
		if recorder.Code != tc.status {
			t.Errorf("%s: %d", tc.path, recorder.Code)
		}
	}
}

func TestForcedServerCloseCancelsActiveReportWork(t *testing.T) {
	provider := readyStub("")
	logger := discardLogger(t)
	entered := make(chan struct{})
	finished := make(chan struct{})
	reports := reporthttp.Handler(func(ctx context.Context, _ report.Query) (report.Report, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return report.Report{}, ctx.Err()
	})
	// With only the report mounted, this server has no WebSocket drain: the
	// forced drain below is HTTP-only.
	srv := New(testAPIKey, 20*time.Millisecond, logger, newTestDependencyErrorLog(), provider, newTestReadyObservers(), NewStreamOutcomeCounter(), MountReport(reports))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + listener.Addr().String() + "/usage/v1/report?timezone=UTC")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("report never entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrForcedDrain) {
			t.Errorf("Run = %v, want ErrForcedDrain", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server shutdown exceeded bound")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("native Close did not cancel report work")
	}
	<-requestDone
}
