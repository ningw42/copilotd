package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

// The logger is an observable completion boundary, not a report implementation
// hook. Snapshotting is synchronized because inference and access logs overlap.
type usageReportLogs struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newUsageReportLogs() *usageReportLogs { return &usageReportLogs{changed: make(chan struct{}, 1)} }
func (l *usageReportLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
	return n, err
}
func (l *usageReportLogs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }
func (l *usageReportLogs) await(t *testing.T, fragments ...string) string {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		lines := phase4LogLinesContaining(l.String(), fragments...)
		if len(lines) != 0 {
			return lines[0]
		}
		select {
		case <-l.changed:
		case <-deadline.C:
			t.Fatalf("missing log %v", fragments)
		}
	}
}

func seedLargeUsageReport(t *testing.T, h *usageMeterServeHarness) {
	t.Helper()
	writer := openReportWriter(t, h.cfg.UsageDBPath)
	// A legal 512 KiB model occurs in one row and one model total. JSON control
	// escaping makes a little over 6 MiB: below 8 MiB, beyond this fixture's
	// explicitly bounded TCP buffers (not arbitrary platform defaults).
	if _, err := writer.Exec(`INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'synthetic-private-row','',0,?,'buffered',1,2)`, strings.Repeat("\x01", 524288)); err != nil {
		t.Fatal(err)
	}
}

const largeReportQuery = "?timezone=UTC&since=2026-09-01&until=2026-09-02&surface=openai"

// Configure the accepted socket before the production server can write. Only
// these slow-client scenarios use this listener; all writes remain real TCP.
func usageBackpressureListener(t *testing.T) *usageSlowTCPListener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return &usageSlowTCPListener{Listener: listener, t: t, accepted: make(map[string]*net.TCPConn)}
}

type usageSlowTCPListener struct {
	net.Listener
	t        *testing.T
	mu       sync.Mutex
	accepted map[string]*net.TCPConn
}

func (l *usageSlowTCPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tcp := conn.(*net.TCPConn)
	if err := tcp.SetWriteBuffer(usageBlockedSendBuffer); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := usageLogSocketBuffers(l.t, fmt.Sprintf("blocked server requested SO_SNDBUF=%d", usageBlockedSendBuffer), tcp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	l.mu.Lock()
	l.accepted[tcp.RemoteAddr().String()] = tcp
	l.mu.Unlock()
	return conn, nil
}

// Call only after observing listener closure at graceful drain. Match both
// endpoints: the last accepted connection could instead be a status probe or
// inference client. Forced-close tests never release the blocked server socket.
func (l *usageSlowTCPListener) resume(client *net.TCPConn) {
	l.t.Helper()
	l.mu.Lock()
	server := l.accepted[client.LocalAddr().String()]
	l.mu.Unlock()
	if server == nil || server.LocalAddr().String() != client.RemoteAddr().String() {
		l.t.Fatalf("no accepted TCP socket matches slow client %s -> %s", client.LocalAddr(), client.RemoteAddr())
	}
	if err := server.SetWriteBuffer(4 << 20); err != nil {
		l.t.Fatal(err)
	}
	if err := client.SetReadBuffer(4 << 20); err != nil {
		l.t.Fatal(err)
	}
	if err := usageLogSocketBuffers(l.t, "graceful release server requested SO_SNDBUF=4194304", server); err != nil {
		l.t.Fatal(err)
	}
	if err := usageLogSocketBuffers(l.t, "graceful release client requested SO_RCVBUF=4194304", client); err != nil {
		l.t.Fatal(err)
	}
}

func usageSlowPeerControl(t *testing.T) func(string, string, syscall.RawConn) error {
	return func(_, address string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) { optionErr = usageSetReceiveBuffer(fd) }); err != nil {
			return err
		}
		if optionErr != nil {
			return optionErr
		}
		// The receive window must be set before connect, not after negotiation.
		// Keep 65536 bytes to allow scaling when the graceful peer resumes.
		return usageLogRawBuffers(t, "pre-connect client requested SO_RCVBUF=65536 remote="+address, raw)
	}
}

func usageLogSocketBuffers(t *testing.T, phase string, conn *net.TCPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	return usageLogRawBuffers(t, fmt.Sprintf("%s local=%s remote=%s", phase, conn.LocalAddr(), conn.RemoteAddr()), raw)
}

func usageLogRawBuffers(t *testing.T, phase string, raw syscall.RawConn) error {
	var receive, send int
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		receive, optionErr = usageGetSocketBuffer(fd, syscall.SO_RCVBUF)
		if optionErr == nil {
			send, optionErr = usageGetSocketBuffer(fd, syscall.SO_SNDBUF)
		}
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return optionErr
	}
	t.Logf("TCP fixture %s: SO_RCVBUF=%d SO_SNDBUF=%d (OS readback, not a hard quota)", phase, receive, send)
	return nil
}

type slowUsageResponse struct {
	conn     *net.TCPConn
	response *http.Response
	started  time.Time
}

func startSlowUsageResponse(t *testing.T, h *usageMeterServeHarness, id string) slowUsageResponse {
	t.Helper()
	dialer := net.Dialer{Control: usageSlowPeerControl(t)}
	connection, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(h.baseURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn := connection.(*net.TCPConn)
	t.Cleanup(func() { _ = conn.Close() })
	if err := usageLogSocketBuffers(t, "connected slow client", conn); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(12 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: localhost\r\nX-Request-Id: %s\r\nConnection: close\r\n\r\n", reporthttp.Path, largeReportQuery, id); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("large report status=%d", response.StatusCode)
	}
	// Headers establish materialization, not blocking. The owning test must
	// still prove slot occupancy and deadline/close behavior while not reading.
	t.Logf("TCP fixture request_id=%s headers received; body bytes prefetched=%d", id, reader.Buffered())
	return slowUsageResponse{conn: conn, response: response, started: started}
}

func requestReportStatus(t *testing.T, h *usageMeterServeHarness, method, query string, status int, code string) {
	t.Helper()
	request, _ := http.NewRequest(method, h.baseURL+reporthttp.Path+query, nil)
	request.Header.Set("X-Request-Id", "report-precedence")
	request.Header.Set("Authorization", "Bearer synthetic-do-not-log")
	start := time.Now()
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != status || method != "HEAD" && !strings.Contains(string(body), `"code":"`+code+`"`) {
		t.Fatalf("%s %s: status=%d read=%v body_bytes=%d prefix=%.512s", method, query, response.StatusCode, err, len(body), body)
	}
	if time.Since(start) > time.Second {
		t.Fatal("report admission/validation queued instead of returning promptly")
	}
	if code == "usage_unavailable" && string(body) != `{"schema_version":1,"error":{"code":"usage_unavailable","message":"Usage data is unavailable on this daemon."}}` {
		t.Fatalf("storage error was not generic and path-free: %s", body)
	}
	if response.Header.Get("X-Request-Id") != "report-precedence" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("report headers: %v", response.Header)
	}
	if status == 429 && response.Header.Get("Retry-After") != "1" {
		t.Fatal("missing Retry-After")
	}
	if status == 405 && response.Header.Get("Allow") != "GET, HEAD" {
		t.Fatal("missing Allow")
	}
}

func assertTruncatedUsageResponseClosed(t *testing.T, slow slowUsageResponse) {
	t.Helper()
	if err := slow.conn.SetReadBuffer(4 << 20); err != nil {
		t.Fatal(err)
	}
	if err := slow.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, slow.response.Body)
	if err == nil || n >= 6<<20 {
		t.Fatalf("server close should truncate blocked report: bytes=%d error=%v", n, err)
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("report connection remained open after server close: %v", err)
	}
	t.Logf("TCP fixture server-closed response: bytes=%d error=%v", n, err)
	_ = slow.conn.Close()
}

func TestUsageGracefulDrainFinishesReportAndInferenceBeforeWriterCutoff(t *testing.T) {
	finish := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finish) }) }
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", concurrentOpenAICompletion)
	}))
	t.Cleanup(upstream.Close)
	logs := newUsageReportLogs()
	listener := usageBackpressureListener(t)
	h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, logs), nil, nil, listener)
	seedLargeUsageReport(t, h)
	req, _ := http.NewRequest("POST", h.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	first := startSlowUsageResponse(t, h, "graceful-aborted-report")
	second := startSlowUsageResponse(t, h, "graceful-complete-report")
	requestReportStatus(t, h, "GET", largeReportQuery, 429, "report_busy")
	// Disconnect a genuinely blocked response and observe completion/slot release
	// before asking for drain; no client timeout stands in for server cleanup.
	_ = first.conn.SetLinger(0)
	_ = first.conn.Close()
	logs.await(t, "msg=access", "request_id=graceful-aborted-report")
	requestReportStatus(t, h, "GET", "?timezone=UTC&since=invalid", 400, "invalid_query")
	h.cancel()
	logs.await(t, `msg="shutting down"`)
	// Shutdown closes its listener before waiting for active handlers. Observe
	// that state rather than treating the pre-drain log as proof of admission.
	listenerDeadline := time.Now().Add(time.Second)
	for {
		probe, err := net.DialTimeout("tcp", strings.TrimPrefix(h.baseURL, "http://"), 20*time.Millisecond)
		if err != nil {
			break
		}
		_ = probe.Close()
		if time.Now().After(listenerDeadline) {
			t.Fatal("drain did not close the listener")
		}
	}
	releaseStarted := time.Now()
	listener.resume(second.conn)
	body, err := io.ReadAll(second.response.Body)
	_ = second.conn.Close()
	if err != nil || len(body) <= 6<<20 || len(body) > 8<<20 {
		t.Fatalf("graceful report size=%d error=%v", len(body), err)
	}
	t.Logf("graceful report received %d bytes in %s after reader release", len(body), time.Since(releaseStarted))
	logs.await(t, "msg=access", "request_id=graceful-complete-report")
	// The server is still draining this stream. Its completion must be admitted,
	// unlike a late producer after Run returns (covered by the forced-log test).
	release()
	streamBody, err := io.ReadAll(stream.Body)
	if err != nil || !strings.Contains(string(streamBody), concurrentOpenAICompletion) {
		t.Fatalf("draining inference: %s %v", streamBody, err)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	assertCleanUsageReport(t, h.closeStore())
	q := reportSelectionNow()
	model := "live-openai"
	q.Model = &model
	persisted, err := report.New(h.cfg.UsageDBPath).Query(context.Background(), q)
	if err != nil || persisted.OpenAI.Total.Turns != 1 || *persisted.OpenAI.Total.Usage["input_tokens"].Sum != 8012 {
		t.Fatalf("drain completion was not admitted and finalized: %+v %v", persisted, err)
	}
}

func TestUsageForcedDrainCancelsRealSQLiteReadAndInference(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	logs := newUsageReportLogs()
	h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, logs), func(cfg *config.ServeConfig) { cfg.ShutdownTimeout = 20 * time.Millisecond }, nil)
	writer := openReportWriter(t, h.cfg.UsageDBPath)
	if _, err := writer.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<250000)
		INSERT INTO openai_turn(at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens)
		SELECT 1788220800000,'','',0,'scan-in-flight','buffered',1,2 FROM n`); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", h.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	reportDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest("GET", h.baseURL+reporthttp.Path+largeReportQuery, nil)
		request.Header.Set("X-Request-Id", "forced-real-sqlite-read")
		response, err := (&http.Client{Timeout: 4 * time.Second}).Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		reportDone <- err
	}()
	// An appended commit that PASSIVE cannot checkpoint establishes a real
	// older SQLite read snapshot, not merely entry into a controlled QueryFunc.
	deadline := time.Now().Add(2 * time.Second)
	var pinnedPages int
	for {
		if _, err := writer.Exec(`INSERT INTO anthropic_turn(at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(0,'','',0,'outside-selection','buffered',0,0)`); err != nil {
			t.Fatal(err)
		}
		var busy, pages, checkpointed int
		if err := writer.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &pages, &checkpointed); err != nil {
			t.Fatal(err)
		}
		if pages > checkpointed {
			pinnedPages = pages - checkpointed
			break
		}
		select {
		case err := <-reportDone:
			t.Fatalf("report completed before native read overlap was established: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("never observed a real pinned read snapshot")
		}
	}
	started := time.Now()
	if err := h.stop(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced read drain: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 15*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("forced shutdown waited on report work: %s", elapsed)
	}
	select {
	case err := <-reportDone:
		if err == nil {
			t.Fatal("forced read unexpectedly produced complete report")
		}
	case <-time.After(time.Second):
		t.Fatal("report connection not canceled")
	}
	logs.await(t, "msg=access", "request_id=forced-real-sqlite-read")
	var busy, pages, checkpointed int
	if err := writer.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil || busy != 0 || pages != 0 || checkpointed != 0 {
		t.Fatalf("canceled read cleanup: %d/%d/%d %v", busy, pages, checkpointed, err)
	}
	if _, err := io.ReadAll(stream.Body); err == nil {
		t.Fatal("forced inference connection remained complete")
	}
	assertCleanUsageReport(t, h.closeStore())
	t.Logf("250000-row native read pinned %d WAL pages; forced report/inference close returned in %s, then checkpoint=0/0/0", pinnedPages, elapsed)
}

func TestUsageSlowTCPReportsHoldSlotsReleaseSQLiteAndDoNotDeadlineSSE(t *testing.T) {
	finish := make(chan struct{})
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	defer release()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", concurrentOpenAICompletion)
	}))
	t.Cleanup(upstream.Close)
	logs := newUsageReportLogs()
	h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, logs), func(cfg *config.ServeConfig) { cfg.StreamIdleTimeout = 15 * time.Second }, nil, usageBackpressureListener(t))
	seedLargeUsageReport(t, h)
	req, _ := http.NewRequest("POST", h.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	streamStarted := time.Now()
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "event: response.created\n" {
		t.Fatalf("SSE prelude %q %v", first, err)
	}
	first := startSlowUsageResponse(t, h, "slow-report-one")
	second := startSlowUsageResponse(t, h, "slow-report-two")
	requestReportStatus(t, h, "GET", largeReportQuery, 429, "report_busy")
	for _, query := range []string{"?timezone=UTC&timezone=UTC", "?timezone=%xx", "?timezone=UTC&model=", "?timezone=UTC&unknown=x"} {
		requestReportStatus(t, h, "GET", query, 400, "invalid_query")
	}
	for _, query := range []string{"?timezone=UTC&since=2026-02-30", "?timezone=UTC&model=%ff", "?timezone=NoSuch/Zone", "?timezone=UTC&period=invalid"} {
		requestReportStatus(t, h, "GET", query, 429, "report_busy")
	}
	requestReportStatus(t, h, "POST", "?timezone=%xx", 405, "method_not_allowed")
	writer := openReportWriter(t, h.cfg.UsageDBPath)
	if _, err := writer.Exec(`INSERT INTO anthropic_turn(at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens) VALUES(1788220800000,'','',0,'committed-while-output-blocked','buffered',12,9)`); err != nil {
		t.Fatal(err)
	}
	var busy, pages, checkpointed int
	if err := writer.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &pages, &checkpointed); err != nil || busy != 0 || pages != 0 || checkpointed != 0 {
		t.Fatalf("report kept a read transaction during network output: %d %d %d %v", busy, pages, checkpointed, err)
	}
	// Confirm both slots remain occupied after the independent commit/checkpoint.
	requestReportStatus(t, h, "GET", largeReportQuery, 429, "report_busy")
	for _, id := range []string{"slow-report-one", "slow-report-two"} {
		line := logs.await(t, "msg=access", "request_id="+id)
		for _, required := range []string{"component=internal/server", "inbound=/usage/v1/report", "status=200", "level=INFO"} {
			if !strings.Contains(line, required) {
				t.Fatalf("access metadata: %s", line)
			}
		}
	}
	elapsed := time.Since(first.started)
	if elapsed < 4500*time.Millisecond || elapsed > 7500*time.Millisecond || time.Since(second.started) < 4500*time.Millisecond {
		t.Fatalf("real blocked writes did not obey route-local 5s limit: %s", elapsed)
	}
	assertTruncatedUsageResponseClosed(t, first)
	assertTruncatedUsageResponseClosed(t, second)
	for _, query := range []string{"?timezone=UTC&since=2026-02-30", "?timezone=UTC&model=%ff", "?timezone=NoSuch/Zone", "?timezone=UTC&period=invalid"} {
		requestReportStatus(t, h, "GET", query, 400, "invalid_query")
	}
	release()
	rest, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(rest), concurrentOpenAICompletion) || time.Since(streamStarted) < 5*time.Second {
		t.Fatalf("SSE did not survive report deadline: duration=%s error=%v body=%s", time.Since(streamStarted), err, rest)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	assertCleanUsageReport(t, h.closeStore())
	for _, forbidden := range []string{h.cfg.UsageDBPath, "synthetic-do-not-log", "synthetic-private-row", "since=", "timezone=", "model=", "SELECT", "input_tokens", "\\u0001"} {
		for _, line := range phase4LogLinesContaining(logs.String(), "msg=access", "inbound=/usage/v1/report") {
			if strings.Contains(line, forbidden) || strings.Contains(line, "surface=") {
				t.Fatalf("report access leaked %q: %s", forbidden, line)
			}
		}
	}
	t.Logf("two >6 MiB materialized TCP reports blocked for %s; independent TRUNCATE checkpoint=0/0/0; SSE completed at %s", elapsed, time.Since(streamStarted))
}
