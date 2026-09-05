package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

type bufferedUsageSurfaceCase struct {
	name       string
	path       string
	table      string
	completion string
}

var bufferedUsageSurfaceCases = []bufferedUsageSurfaceCase{
	{
		name:       "OpenAI Responses",
		path:       "/openai/v1/responses",
		table:      "openai_turn",
		completion: `{"id":"resp-composed-buffered","model":"reported-model","status":"completed","usage":{"input_tokens":12,"output_tokens":6}}`,
	},
	{
		name:       "Anthropic Messages",
		path:       "/anthropic/v1/messages",
		table:      "anthropic_turn",
		completion: `{"id":"msg-composed-buffered","type":"message","model":"reported-model","stop_reason":"future-reason","usage":{"input_tokens":12,"output_tokens":6}}`,
	},
}

func performUsagePOST(ctx context.Context, baseURL, target, requestID, body string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+target, strings.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", requestID)
	// Keep unsupported upstream encodings opaque to this test client too.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return resp, nil, err
	}
	responseBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, responseBody, nil
}

func doUsagePOST(t *testing.T, ctx context.Context, baseURL, target, requestID, body string) (*http.Response, []byte) {
	t.Helper()
	resp, responseBody, err := performUsagePOST(ctx, baseURL, target, requestID, body)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	return resp, responseBody
}

func externalUsageDB(t *testing.T, harness *usageMeterServeHarness) (*sql.DB, sqlitestore.Report) {
	t.Helper()
	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	report := harness.closeStore()
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, report
}

func assertCleanUsageReport(t *testing.T, report sqlitestore.Report) {
	t.Helper()
	if report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}
}

func queryUsageCount(t *testing.T, db *sql.DB, table, predicate string, args ...any) int {
	t.Helper()
	if table != "openai_turn" && table != "anthropic_turn" {
		t.Fatalf("unsupported usage table %q", table)
	}
	query := "SELECT count(*) FROM " + table
	if predicate != "" {
		query += " WHERE " + predicate
	}
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("query %s row count: %v", table, err)
	}
	return count
}

func TestRunBoundServeUsageEligibilityDependsOnPayloadNotStatusOrContentType(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name, func(t *testing.T) {
			responses := map[string]struct {
				status      int
				contentType string
				body        string
			}{
				"success":        {status: http.StatusOK, contentType: "application/json", body: surface.completion},
				"valid-on-error": {status: http.StatusTeapot, contentType: "text/plain", body: surface.completion},
				"error-object":   {status: http.StatusBadRequest, contentType: "application/json", body: `{"error":{"message":"upstream rejected the request"}}`},
				"non-json":       {status: http.StatusBadGateway, contentType: "text/plain", body: "upstream gateway failed"},
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response, ok := responses[r.URL.Query().Get("case")]
				if !ok {
					http.Error(w, "unknown case", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", response.contentType)
				w.WriteHeader(response.status)
				_, _ = io.WriteString(w, response.body)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)

			for name, want := range responses {
				resp, body := doUsagePOST(t, context.Background(), harness.baseURL, surface.path+"?case="+name, "payload-eligibility-"+name, `{}`)
				if resp.StatusCode != want.status || string(body) != want.body {
					t.Errorf("%s response = %d %q, want unchanged %d %q", name, resp.StatusCode, body, want.status, want.body)
				}
			}

			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, ""); got != 2 {
				t.Errorf("persisted rows = %d, want only the two qualifying payloads", got)
			}
			for _, requestID := range []string{"payload-eligibility-success", "payload-eligibility-valid-on-error"} {
				if got := queryUsageCount(t, db, surface.table, "request_id = ?", requestID); got != 1 {
					t.Errorf("rows for %q = %d, want one", requestID, got)
				}
			}
		})
	}
}

func TestRunBoundServeUsageMeterIdentityEncodingPredicateAndOpaqueBypass(t *testing.T) {
	encodingCases := []struct {
		name       string
		values     []string
		wantRecord bool
	}{
		{name: "absent", wantRecord: true},
		{name: "single-trimmed-case", values: []string{"  IdEnTiTy\t"}, wantRecord: true},
		{name: "explicit-empty", values: []string{""}},
		{name: "repeated", values: []string{"identity", "identity"}},
		{name: "list", values: []string{"identity, gzip"}},
		{name: "gzip", values: []string{"gzip"}},
	}
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				index, err := strconv.Atoi(r.URL.Query().Get("case"))
				if err != nil || index < 0 || index >= len(encodingCases) {
					http.Error(w, "unknown case", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				for _, value := range encodingCases[index].values {
					w.Header().Add("Content-Encoding", value)
				}
				_, _ = io.WriteString(w, surface.completion)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)

			for index, test := range encodingCases {
				resp, body := doUsagePOST(t, context.Background(), harness.baseURL, surface.path+"?case="+strconv.Itoa(index), "identity-"+test.name, `{}`)
				if resp.StatusCode != http.StatusOK || string(body) != surface.completion {
					t.Errorf("%s response = %d %q, want unchanged 200 %q", test.name, resp.StatusCode, body, surface.completion)
				}
			}

			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, ""); got != 2 {
				t.Errorf("persisted rows = %d, want absent and one identity encoding only", got)
			}
			for _, test := range encodingCases {
				want := 0
				if test.wantRecord {
					want = 1
				}
				if got := queryUsageCount(t, db, surface.table, "request_id = ?", "identity-"+test.name); got != want {
					t.Errorf("%s rows = %d, want %d", test.name, got, want)
				}
			}
		})
	}
}

func TestRunBoundServeUsageMeterAloneActivatesBoundedRead(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		for _, enabled := range []bool{false, true} {
			name := "meter-off"
			if enabled {
				name = "meter-on"
			}
			t.Run(surface.name+"/"+name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					w.(http.Flusher).Flush() // force honest chunked upstream framing
					_, _ = io.WriteString(w, surface.completion)
				}))
				t.Cleanup(upstream.Close)
				harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) {
					cfg.ShimUsageMeterEnabled = enabled
					cfg.MaxBufferedResponseBytes = 8
				}, nil)

				resp, body := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "over-cap-"+name, `{}`)
				if !enabled {
					if resp.StatusCode != http.StatusAccepted || string(body) != surface.completion {
						t.Errorf("meter off response = %d %q, want upstream passthrough", resp.StatusCode, body)
					}
				} else if resp.StatusCode != http.StatusBadGateway || !bytes.Contains(body, []byte("exceeds the maximum allowed size")) {
					t.Errorf("meter on response = %d %q, want precommit 502", resp.StatusCode, body)
				}

				db, report := externalUsageDB(t, harness)
				assertCleanUsageReport(t, report)
				if got := queryUsageCount(t, db, surface.table, ""); got != 0 {
					t.Errorf("over-cap rows = %d, want none", got)
				}
			})
		}
	}
}

func TestRunBoundServeUsageMeterClassifiesBufferedReadFailures(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name+"/read-failure", func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack upstream response: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n%s", surface.completion[:8])
				_ = rw.Flush()
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
			resp, _ := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "buffered-read-failure", `{}`)
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("read failure status = %d, want 502", resp.StatusCode)
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, ""); got != 0 {
				t.Errorf("read-failure rows = %d, want none", got)
			}
		})

		t.Run(surface.name+"/timeout", func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) {
				cfg.OutboundTimeout = 50 * time.Millisecond
			}, nil)
			resp, _ := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "buffered-read-timeout", `{}`)
			if resp.StatusCode != http.StatusGatewayTimeout {
				t.Errorf("read timeout status = %d, want 504", resp.StatusCode)
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, ""); got != 0 {
				t.Errorf("timeout rows = %d, want none", got)
			}
		})

		t.Run(surface.name+"/client-cancellation", func(t *testing.T) {
			upstreamStarted := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(upstreamStarted)
				<-r.Context().Done()
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
			requestCtx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, harness.baseURL+surface.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("X-Request-Id", "buffered-client-cancel")
			result := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				result <- err
			}()
			select {
			case <-upstreamStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("buffered upstream response did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("client cancellation error = %v, want context canceled without a response", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("client cancellation did not return promptly")
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, ""); got != 0 {
				t.Errorf("client-cancellation rows = %d, want none", got)
			}
		})
	}
}

func TestRunBoundServeUsageMeterDelaysCommitAndRecomputesChunkedLength(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name, func(t *testing.T) {
			firstChunkWritten := make(chan struct{})
			releaseRemainder := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRemainder) }) }
			t.Cleanup(release)
			midpoint := len(surface.completion) / 2
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, surface.completion[:midpoint])
				w.(http.Flusher).Flush()
				close(firstChunkWritten)
				<-releaseRemainder
				_, _ = io.WriteString(w, surface.completion[midpoint:])
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)

			type responseResult struct {
				resp *http.Response
				body []byte
				err  error
			}
			result := make(chan responseResult, 1)
			go func() {
				resp, body, err := performUsagePOST(context.Background(), harness.baseURL, surface.path, "delayed-buffered-commit", `{}`)
				result <- responseResult{resp: resp, body: body, err: err}
			}()
			select {
			case <-firstChunkWritten:
			case <-time.After(2 * time.Second):
				t.Fatal("fake upstream did not flush its first chunk")
			}
			select {
			case <-result:
				t.Fatal("downstream response committed before the metered body was read in full")
			case <-time.After(75 * time.Millisecond):
			}
			release()
			got := <-result
			if got.err != nil {
				t.Fatalf("read delayed buffered response: %v", got.err)
			}
			if got.resp.StatusCode != http.StatusCreated || !bytes.Equal(got.body, []byte(surface.completion)) {
				t.Errorf("downstream response = %d %q, want unchanged chunked upstream body", got.resp.StatusCode, got.body)
			}
			if got.resp.ContentLength != int64(len(surface.completion)) || got.resp.Header.Get("Content-Length") != strconv.Itoa(len(surface.completion)) || len(got.resp.TransferEncoding) != 0 {
				t.Errorf("downstream length = parsed:%d header:%q transfer:%v, want recomputed %d", got.resp.ContentLength, got.resp.Header.Get("Content-Length"), got.resp.TransferEncoding, len(surface.completion))
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, "request_id = ?", "delayed-buffered-commit"); got != 1 {
				t.Errorf("persisted rows = %d, want one", got)
			}
		})
	}
}

type rejectingComposedBufferedShim struct{}

func (rejectingComposedBufferedShim) TransformBuffered(context.Context, *shim.Body) error {
	return errors.New("outer composed rejection")
}

type heldComposedBufferedShim struct {
	entered     chan struct{}
	release     chan struct{}
	returned    chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	returnOnce  sync.Once
}

func newHeldComposedBufferedShim() *heldComposedBufferedShim {
	return &heldComposedBufferedShim{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
}

func (s *heldComposedBufferedShim) TransformBuffered(context.Context, *shim.Body) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	s.returnOnce.Do(func() { close(s.returned) })
	return nil
}

func (s *heldComposedBufferedShim) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func composedOuterBufferedRegistration(name string, transformer shim.BufferedTransformer) func(shim.Registry) shim.Registry {
	return func(registry shim.Registry) shim.Registry {
		return append(shim.Registry{{
			Name:    name,
			Enabled: true,
			Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
				return (surface == endpoint.OpenAI && route == endpoint.RouteOpenAIResponses) ||
					(surface == endpoint.Anthropic && route == endpoint.RouteAnthropicMessages)
			},
			New: func(context.Context, endpoint.Surface, endpoint.Route) any { return transformer },
		}}, registry...)
	}
}

func TestRunBoundServeRetainsBufferedUsageAfterDownstreamOrOuterFailure(t *testing.T) {
	for _, surface := range bufferedUsageSurfaceCases {
		t.Run(surface.name+"/outer-rejection", func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, surface.completion)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil,
				composedOuterBufferedRegistration("outer-buffered-rejection", rejectingComposedBufferedShim{}))
			resp, _ := doUsagePOST(t, context.Background(), harness.baseURL, surface.path, "outer-buffered-rejection", `{}`)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Errorf("outer rejection status = %d, want 500", resp.StatusCode)
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, "request_id = ?", "outer-buffered-rejection"); got != 1 {
				t.Errorf("retained rows = %d, want completion observed before outer rejection", got)
			}
		})

		t.Run(surface.name+"/downstream-disconnect", func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, surface.completion)
			}))
			t.Cleanup(upstream.Close)
			held := newHeldComposedBufferedShim()
			t.Cleanup(held.Release)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil,
				composedOuterBufferedRegistration("hold-buffered-after-usage", held))

			conn, err := net.Dial("tcp", strings.TrimPrefix(harness.baseURL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			request := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: usage.test\r\nAuthorization: Bearer %s\r\nX-Request-Id: buffered-downstream-disconnect\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}", surface.path, testAPIKey)
			if _, err := io.WriteString(conn, request); err != nil {
				_ = conn.Close()
				t.Fatal(err)
			}
			select {
			case <-held.entered:
			case <-time.After(2 * time.Second):
				_ = conn.Close()
				t.Fatal("outer buffered Shim did not hold after usage observation")
			}
			_ = conn.Close()
			held.Release()
			select {
			case <-held.returned:
			case <-time.After(2 * time.Second):
				t.Fatal("held buffered Shim did not return")
			}

			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, surface.table, "request_id = ?", "buffered-downstream-disconnect"); got != 1 {
				t.Errorf("retained rows = %d, want completion observed before downstream write failure", got)
			}
		})
	}
}

func TestRunBoundServeSelectsUsageTransportFromEndpointAndUpstreamContentType(t *testing.T) {
	const (
		completedEvent = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-selected-sse\",\"model\":\"reported-sse\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":7}}}\n\n"
		bufferedBody   = `{"id":"resp-selected-buffered","model":"reported-buffered","status":"completed","usage":{"input_tokens":8,"output_tokens":9}}`
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("transport") == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, completedEvent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bufferedBody)
	}))
	t.Cleanup(upstream.Close)
	harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
	resp, body := doUsagePOST(t, context.Background(), harness.baseURL, "/openai/v1/responses?transport=sse", "selected-sse", `{"stream":false}`)
	if resp.StatusCode != http.StatusOK || string(body) != completedEvent {
		t.Errorf("SSE-selected response = %d %q, want exact event stream", resp.StatusCode, body)
	}
	resp, body = doUsagePOST(t, context.Background(), harness.baseURL, "/openai/v1/responses?transport=buffered", "selected-buffered", `{"stream":true}`)
	if resp.StatusCode != http.StatusOK || string(body) != bufferedBody {
		t.Errorf("buffered-selected response = %d %q, want exact JSON", resp.StatusCode, body)
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	for _, want := range []struct {
		requestID string
		transport string
	}{
		{requestID: "selected-sse", transport: "sse"},
		{requestID: "selected-buffered", transport: "buffered"},
	} {
		var transport string
		if err := db.QueryRow("SELECT transport FROM openai_turn WHERE request_id = ?", want.requestID).Scan(&transport); err != nil {
			t.Fatalf("query %s transport: %v", want.requestID, err)
		}
		if transport != want.transport {
			t.Errorf("%s transport = %q, want %q", want.requestID, transport, want.transport)
		}
	}
}

func TestRunBoundServeRejectsUnsupportedSSEEncodingBeforeUsageHooks(t *testing.T) {
	streams := []struct {
		name  string
		path  string
		table string
		body  string
	}{
		{
			name: "OpenAI Responses", path: "/openai/v1/responses", table: "openai_turn",
			body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"encoded-openai\",\"model\":\"reported\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n",
		},
		{
			name: "Anthropic Messages", path: "/anthropic/v1/messages", table: "anthropic_turn",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"encoded-anthropic\",\"model\":\"reported\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, stream := range streams {
		t.Run(stream.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = io.WriteString(w, stream.body)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
			resp, body := doUsagePOST(t, context.Background(), harness.baseURL, stream.path, "encoded-sse", `{"stream":true}`)
			if resp.StatusCode != http.StatusBadGateway || !bytes.Contains(body, []byte("unsupported Content-Encoding")) {
				t.Errorf("encoded SSE response = %d %q, want pre-hook 502", resp.StatusCode, body)
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, stream.table, ""); got != 0 {
				t.Errorf("encoded SSE rows = %d, want none", got)
			}
		})
	}
}
