package reporthttp_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

// A scheduling pause at the public ResponseController boundary widens the
// interval between installing the first native read deadline and starting work.
// The actual TCP deadline and net/http background read remain untouched.
type pausedReadDeadlineWriter struct {
	http.ResponseWriter
	paused bool
}

func (w *pausedReadDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *pausedReadDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(w.ResponseWriter).SetReadDeadline(deadline)
	if !w.paused {
		w.paused = true
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func TestHandlerBodylessWorkTimeoutSurvivesDeadlineScheduling(t *testing.T) {
	type observation struct {
		err     error
		elapsed time.Duration
	}
	observed := make(chan observation, 1)
	handler := reporthttp.Handler(func(ctx context.Context, _ report.Query) (report.Report, error) {
		started := time.Now()
		<-ctx.Done()
		observed <- observation{ctx.Err(), time.Since(started)}
		return report.Report{}, &report.Error{Code: report.Unavailable, Message: "driver interrupted"}
	})
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(&pausedReadDeadlineWriter{ResponseWriter: w}, r)
	}))
	t.Cleanup(edge.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(edge.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// No body, pipelined data, client cancellation or automatic reconnect/retry.
	if _, err := fmt.Fprintf(conn, "GET %s?timezone=UTC HTTP/1.1\r\nHost: localhost\r\n\r\n", reporthttp.Path); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var got observation
	select {
	case got = <-observed:
	case <-time.After(time.Second):
		t.Fatal("response returned without the query observation")
	}
	t.Logf("bodyless work: status=%d context=%v elapsed=%s body=%s", response.StatusCode, got.err, got.elapsed, body)
	if response.StatusCode != 504 || !strings.Contains(string(body), `"code":"report_timeout"`) || !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatal("server read deadline preempted the work deadline or hid its timeout classification")
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("timeout response headers: %v", response.Header)
	}
}
