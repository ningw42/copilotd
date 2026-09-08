package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func startReportBodyServer(t *testing.T, query reporthttp.QueryFunc) string {
	t.Helper()
	logger := discardLogger(t)
	provider := readyStub("")
	fwd := newTestForwarder(provider, forward.NewClient(time.Second), time.Second, time.Second, time.Second, time.Second, 1<<20, 1<<20, nil)
	srv := New(testConfig(), logger, logger, newTestDependencyErrorLog(), provider, newTestReadyObservers(), fwd, newTestCatalogSource(provider), newTestWSProxy(provider), NewStreamOutcomeCounter(), catalog.RenderDescriptors{}, reporthttp.Handler(query))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String()
}

func TestUsageIncompleteRequestBodiesReleaseReportSlotsWithinWriteBudget(t *testing.T) {
	for _, framing := range []struct{ name, headers string }{
		{"content_length", "Content-Length: 1\r\n\r\n"},
		{"chunked", "Transfer-Encoding: chunked\r\n\r\n1\r\n"},
	} {
		t.Run(framing.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "usage.db")
			store, err := sqlitestore.Open(path, logging.ForComponent(discardLogger(t), "internal/usage/sqlitestore"))
			if err != nil {
				t.Fatal(err)
			}
			// Legal, small enough not to exhaust TCP buffers, but larger than
			// net/http's response buffer: Write must flush while holding its slot.
			store.Record(usage.Turn{At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Model: strings.Repeat("m", 4096), Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			closed := store.Close(ctx)
			cancel()
			if !closed.DriverCleanupCompleted || closed.FinalFlushLosses != 0 {
				t.Fatalf("fixture writer: %+v", closed)
			}
			reader := report.New(path)
			completed := make(chan struct{}, 2)
			addr := startReportBodyServer(t, func(ctx context.Context, q report.Query) (report.Report, error) {
				result, err := reader.Query(ctx, q)
				if q.Model != nil {
					// Observe the approved QueryFunc boundary, not internal SQL or
					// handler state. Do not race a status probe for admission before
					// both incomplete requests have finished real materialization.
					completed <- struct{}{}
				}
				return result, err
			})
			transport := http.DefaultTransport.(*http.Transport).Clone()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			t.Cleanup(client.CloseIdleConnections)
			target := reporthttp.Path + "?timezone=UTC&since=2026-09-01&until=2026-09-02&surface=openai"
			response, err := client.Get("http://" + addr + target)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != 200 || len(body) < 8192 || len(body) > 16384 {
				t.Fatalf("materializable report: status=%d bytes=%d error=%v", response.StatusCode, len(body), err)
			}
			started := time.Now()
			type result struct {
				body []byte
				err  error
			}
			done := make(chan result, 2)
			for range 2 {
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				// Cleanup, not the behavioral test, closes attackers even on RED.
				t.Cleanup(func() { _ = conn.Close() })
				if err := conn.SetDeadline(started.Add(7500 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintf(conn, "GET %s&model=%s HTTP/1.1\r\nHost: localhost\r\n%s", target, strings.Repeat("m", 4096), framing.headers); err != nil {
					t.Fatal(err)
				}
				go func() {
					body, err := io.ReadAll(conn)
					done <- result{body, err}
				}()
			}
			for range 2 {
				select {
				case <-completed:
				case <-time.After(time.Second):
					t.Fatal("real query did not materialize before network waiting")
				}
			}
			status := func() int {
				response, err := client.Head("http://" + addr + target)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				return response.StatusCode
			}
			if got := status(); got != 429 {
				t.Fatalf("materialized reports did not hold both slots during network waiting: %d", got)
			}
			for range 2 {
				result := <-done
				if result.err != nil {
					t.Fatalf("withheld request body outlived 5s write budget plus tolerance: %v; later report status=%d", result.err, status())
				}
				if len(result.body) != 0 {
					t.Fatalf("failed implicit drain emitted a response or appended error: %q", result.body)
				}
			}
			elapsed := time.Since(started)
			if got := status(); elapsed < 4500*time.Millisecond || elapsed > 7500*time.Millisecond || got != 200 {
				t.Fatalf("report slots not released at route budget: elapsed=%s status=%d", elapsed, got)
			}
			t.Logf("two real queries materialized; withheld request bodies returned without client close in %s; later report=200", elapsed)
		})
	}
}

func TestReportIncompleteBodiesBoundFinalFlushAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, method, query string
		status              int
		invoked             bool
	}{
		{"small_success", "GET", "timezone=UTC", 200, true},
		{"head", "HEAD", "timezone=UTC", 200, true},
		{"method", "POST", "timezone=%xx", 405, false},
		{"disabled", "GET", "timezone=%xx", 503, false},
		{"syntax", "GET", "timezone=%xx", 400, false},
		{"semantic", "GET", "timezone=invalid", 400, true},
		{"panic", "GET", "timezone=UTC", 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger := discardLogger(t)
			path := filepath.Join(t.TempDir(), "private", "usage.db")
			store, err := sqlitestore.Open(path, logging.ForComponent(logger, "internal/usage/sqlitestore"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			closed := store.Close(ctx)
			cancel()
			if !closed.DriverCleanupCompleted {
				t.Fatal("fixture writer did not close")
			}
			reader := report.New(path)
			completed := make(chan struct{}, 2)
			var query reporthttp.QueryFunc = func(ctx context.Context, q report.Query) (report.Report, error) {
				defer func() { completed <- struct{}{} }()
				if tc.name == "panic" {
					panic("synthetic report failure")
				}
				return reader.Query(ctx, q)
			}
			if tc.name == "disabled" {
				query = nil
			}
			addr := startReportBodyServer(t, query)
			// Establish the usual response contract separately from the incomplete
			// body's network failure. No alternate handler/middleware is mounted.
			transport := http.DefaultTransport.(*http.Transport).Clone()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			t.Cleanup(client.CloseIdleConnections)
			target := reporthttp.Path + "?" + tc.query + "&since=2026-09-01&until=2026-09-02"
			request, _ := http.NewRequest(tc.method, "http://"+addr+target, nil)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != tc.status {
				t.Fatalf("control status=%d want=%d error=%v", response.StatusCode, tc.status, err)
			}
			if tc.invoked {
				<-completed
			}
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			started := time.Now()
			if err := conn.SetDeadline(started.Add(7500 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nTransfer-Encoding: chunked\r\n\r\n1\r\n", tc.method, target); err != nil {
				t.Fatal(err)
			}
			if tc.invoked {
				select {
				case <-completed:
				case <-time.After(time.Second):
					t.Fatal("query did not finish before blocked final flush")
				}
			}
			body, err := io.ReadAll(conn)
			elapsed := time.Since(started)
			if err != nil || len(body) != 0 || elapsed < 4500*time.Millisecond || elapsed > 7500*time.Millisecond {
				t.Fatalf("post-handler flush/body cleanup: elapsed=%s error=%v response=%q", elapsed, err, body)
			}
			t.Logf("query/control status=%d; incomplete chunked body returned in %s without client close", tc.status, elapsed)
		})
	}
}
