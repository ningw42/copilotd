package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestUsageReportDeadlinesDoNotLeakIntoReusedInferenceConnections(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if string(body) == `{"stream":true}` {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
			_ = http.NewResponseController(w).Flush()
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", concurrentOpenAICompletion)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, concurrentOpenAICompletion)
	}))
	t.Cleanup(upstream.Close)
	h := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) { cfg.StreamIdleTimeout = 15 * time.Second }, nil)
	t.Cleanup(release)
	// Raw TCP prevents an automatic client reconnect/retry from hiding leaked
	// per-report deadlines. Both connections first finish an ordinary report,
	// including its complete ignored GET body, then carry authenticated inference.
	var conns []net.Conn
	var readers []*bufio.Reader
	for range 2 {
		conn, err := net.Dial("tcp", strings.TrimPrefix(h.baseURL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(12 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n\r\nx", reporthttp.Path, largeReportQuery); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != 200 || response.Close {
			t.Fatalf("report must remain reusable: status=%d close=%t error=%v", response.StatusCode, response.Close, err)
		}
		conns, readers = append(conns, conn), append(readers, reader)
	}
	started := time.Now()
	// One subsequent inference upload waits past the prior report's read bound.
	if _, err := fmt.Fprintf(conns[0], "POST /openai/v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\nContent-Type: application/json\r\n\r\n", testAPIKey); err != nil {
		t.Fatal(err)
	}
	// The other subsequent inference response streams beyond its prior write bound.
	payload := `{"stream":true}`
	if _, err := fmt.Fprintf(conns[1], "POST /openai/v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s", testAPIKey, len(payload), payload); err != nil {
		t.Fatal(err)
	}
	stream, err := http.ReadResponse(readers[1], &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE status=%d headers=%v", stream.StatusCode, stream.Header)
	}
	defer stream.Body.Close()
	time.Sleep(time.Until(started.Add(5500 * time.Millisecond)))
	if _, err := io.WriteString(conns[0], "{}"); err != nil {
		t.Fatal(err)
	}
	buffered, err := http.ReadResponse(readers[0], &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("reused inference read inherited report deadline: %v", err)
	}
	body, err := io.ReadAll(buffered.Body)
	_ = buffered.Body.Close()
	if err != nil || buffered.StatusCode != 200 || string(body) != concurrentOpenAICompletion {
		t.Fatalf("delayed inference upload: status=%d body=%s error=%v", buffered.StatusCode, body, err)
	}
	release()
	body, err = io.ReadAll(stream.Body)
	if err != nil || !strings.Contains(string(body), concurrentOpenAICompletion) {
		t.Fatalf("reused SSE inherited report deadline: body=%s error=%v", body, err)
	}
	t.Logf("same HTTP/1 connections accepted delayed inference upload and completed SSE after %s", time.Since(started))
}
