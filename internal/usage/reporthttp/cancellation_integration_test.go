package reporthttp_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestHandlerCanceledTCPRequestReturnsSafeFailure(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			entered := make(chan struct{})
			observed := make(chan error, 1)
			handler := reporthttp.Handler(func(ctx context.Context, _ report.Query) (report.Report, error) {
				close(entered)
				<-ctx.Done()
				observed <- ctx.Err()
				return report.Report{}, &report.Error{Code: report.Unavailable, Message: "Usage data is unavailable on this daemon."}
			})
			server := httptest.NewServer(handler)
			defer server.Close()

			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "%s %s?timezone=UTC HTTP/1.1\r\nHost: localhost\r\n\r\n", method, reporthttp.Path); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("Query was not entered")
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}

			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case cause := <-observed:
				if cause != context.Canceled {
					t.Fatalf("Query cancellation = %v, want context.Canceled", cause)
				}
			case <-time.After(time.Second):
				t.Fatal("Query did not report cancellation")
			}

			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%q", response.StatusCode, body)
			}
			if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("safe failure headers = %v", response.Header)
			}
			if method == http.MethodHead {
				if len(body) != 0 {
					t.Fatalf("HEAD body = %q", body)
				}
				return
			}
			const want = `{"schema_version":1,"error":{"code":"usage_unavailable","message":"Usage data is unavailable on this daemon."}}`
			if string(body) != want {
				t.Fatalf("GET body = %q, want %q", body, want)
			}
		})
	}
}
