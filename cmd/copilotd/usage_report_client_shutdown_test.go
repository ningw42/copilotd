package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

// Reading signals that the real Transport owns the completed, unused dial.
// Its background read is real socket I/O, not a controlled report handler.
type reportDialReadSignal struct {
	net.Conn
	reading chan struct{}
	once    sync.Once
}

func (c *reportDialReadSignal) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}

func TestUsageReportClientTeardownClosesAnUnusedDialBeforeGracefulStop(t *testing.T) {
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), nil, nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	secondDial, releaseDial, reading := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseDial) }) }
	t.Cleanup(release)
	t.Cleanup(client.CloseIdleConnections)
	var dials atomic.Int32
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if dials.Add(1) != 2 {
			return conn, nil
		}
		// Finish opening the second real socket, but let the first connection
		// return to idle and satisfy the waiting request before delivering it.
		close(secondDial)
		select {
		case <-releaseDial:
			return &reportDialReadSignal{Conn: conn, reading: reading}, nil
		case <-ctx.Done():
			_ = conn.Close()
			return nil, ctx.Err()
		}
	}
	url := h.baseURL + reporthttp.Path + largeReportQuery
	first, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	if first.StatusCode != 200 {
		t.Fatalf("first report status=%d", first.StatusCode)
	}
	// Leave the first response unread so another request must begin a dial.
	secondDone := make(chan error, 1)
	reused := make(chan bool, 1)
	go func() {
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				select {
				case reused <- info.Reused:
				default:
				}
			},
		})
		request, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		response, err := client.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if err == nil && response.StatusCode != http.StatusOK {
				err = fmt.Errorf("second report status=%d", response.StatusCode)
			}
		}
		secondDone <- err
	}()
	select {
	case <-secondDial:
	case <-time.After(time.Second):
		t.Fatal("second request did not start a real dial")
	}
	if _, err := io.Copy(io.Discard, first.Body); err != nil {
		t.Fatal(err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first connection did not satisfy waiting report")
	}
	select {
	case wasReused := <-reused:
		if !wasReused {
			t.Fatal("fixture did not reuse the first connection while its other dial was pending")
		}
	case <-time.After(time.Second):
		t.Fatal("missing transport connection observation")
	}
	release()
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("Transport did not retain its completed unused dial")
	}
	// This is the same client-before-server teardown used by the daily command
	// test. A completed request is not proof that its Transport has no unused
	// socket awaiting a first HTTP request on the production listener.
	if err := h.stopAfterClient(client); err != nil {
		t.Fatal(err)
	}
	assertCleanUsageReport(t, h.closeStore())
}
