package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

type panicOnOpenAICompletion struct{}

func (panicOnOpenAICompletion) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	if frame.Type == "response.completed" {
		panic("outer shim failed after usage observation")
	}
	return []sse.Frame{frame}
}

type panicOnAnthropicStop struct{}

func (panicOnAnthropicStop) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	if frame.Type == "message_stop" {
		panic("outer shim failed after usage observation")
	}
	return []sse.Frame{frame}
}

type usageMeterServeHarness struct {
	cfg     config.ServeConfig
	baseURL string
	store   *sqlitestore.Store
	cancel  context.CancelFunc
	done    <-chan error

	stopOnce    sync.Once
	stopErr     error
	closeOnce   sync.Once
	closeReport sqlitestore.Report
}

func startUsageMeterServeHarness(t *testing.T, upstreamURL string, base *slog.Logger, configure func(*config.ServeConfig), decorate func(shim.Registry) shim.Registry) *usageMeterServeHarness {
	t.Helper()
	cfg := e2eConfig("gho-usage-meter-serve-harness")
	cfg.ImpersonationRefreshInterval = 0
	cfg.WebSocketHandshakeTimeout = 5 * time.Second
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = filepath.Join(t.TempDir(), "usage", "usage.db")
	if configure != nil {
		configure(&cfg)
	}
	store, err := sqlitestore.Open(cfg.UsageDBPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}
	harness := &usageMeterServeHarness{cfg: cfg, store: store}
	t.Cleanup(func() { _ = harness.closeStore() })

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-serve-harness", upstreamURL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	if decorate != nil {
		registry = decorate(registry)
	}
	if registry[len(registry)-1].Name != "usage-meter" {
		t.Fatalf("last registration = %q, want usage-meter innermost", registry[len(registry)-1].Name)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	harness.baseURL = "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	harness.cancel = cancel
	t.Cleanup(cancel)
	done := make(chan error, 1)
	harness.done = done
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln, store)
	}()
	t.Cleanup(func() { _ = harness.stop() })
	assertHTTPStatusEventually(t, harness.baseURL+"/healthz", http.StatusOK)
	return harness
}

func (h *usageMeterServeHarness) stop() error {
	h.stopOnce.Do(func() {
		if h.cancel == nil || h.done == nil {
			return
		}
		h.cancel()
		select {
		case h.stopErr = <-h.done:
		case <-time.After(5 * time.Second):
			h.stopErr = errors.New("runBoundServe did not stop within five seconds")
		}
	})
	return h.stopErr
}

func (h *usageMeterServeHarness) closeStore() sqlitestore.Report {
	h.closeOnce.Do(func() {
		if h.store == nil {
			return
		}
		h.store.StopAdmission()
		ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ShutdownTimeout)
		defer cancel()
		h.closeReport = h.store.Close(ctx)
	})
	return h.closeReport
}

func dialUsageMeterWebSocket(t *testing.T, baseURL, requestID string) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(baseURL, "http")+"/openai/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + testAPIKey},
			"X-Request-Id":  {requestID},
		},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial WebSocket transport: %v", err)
	}
	return conn
}

func TestRunServeUsageStoreFailurePrecedesBindAndDisabledServeCreatesNothing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	t.Run("requested unsafe store fails before bind", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "shared")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(parent, "usage.db")
		logPath := filepath.Join(root, "serve.log")
		code := run([]string{
			"serve", "--apikey", testAPIKey, "--github-oauth-token", "gho-local",
			"--addr", held.Addr().String(), "--shim-usage-meter-enabled=true",
			"--usage-db-path", dbPath, "--log-file", logPath,
			"--impersonation-refresh-interval", "0",
		}, noEnv(), io.Discard, io.Discard)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		logs, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logs), "opening usage database failed") || strings.Contains(string(logs), "bind failed") {
			t.Errorf("startup logs do not prove store failure before bind:\n%s", logs)
		}
		if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
			t.Errorf("unsafe store path exists: %v", err)
		}
	})

	t.Run("disabled store has no filesystem effects", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "absent", "usage.db")
		logPath := filepath.Join(root, "serve.log")
		code := run([]string{
			"serve", "--apikey", testAPIKey, "--github-oauth-token", "gho-local",
			"--addr", held.Addr().String(), "--usage-db-path", dbPath,
			"--log-file", logPath, "--impersonation-refresh-interval", "0",
		}, noEnv(), io.Discard, io.Discard)
		if code != 1 {
			t.Fatalf("exit code = %d, want occupied-bind failure", code)
		}
		logs, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(logs), "bind failed") || strings.Contains(string(logs), "usage store finalized") {
			t.Errorf("disabled startup logs = %s", logs)
		}
		if _, err := os.Stat(filepath.Dir(dbPath)); !os.IsNotExist(err) {
			t.Errorf("disabled meter created filesystem artifacts: %v", err)
		}
	})
}

func TestRunServeFinalizesOpenedUsageStoreOnBindFailure(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	root := t.TempDir()
	dbPath := filepath.Join(root, "private", "usage.db")
	logPath := filepath.Join(root, "serve.log")
	code := run([]string{
		"serve", "--apikey", testAPIKey, "--github-oauth-token", "gho-local",
		"--addr", held.Addr().String(), "--shim-usage-meter-enabled=true",
		"--usage-db-path", dbPath, "--log-file", logPath,
		"--impersonation-refresh-interval", "0",
	}, noEnv(), io.Discard, io.Discard)
	if code != 1 {
		t.Fatalf("exit code = %d, want bind failure", code)
	}
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bind failed", "usage store finalized", "driver_cleanup_completed=true"} {
		if !strings.Contains(string(logs), want) {
			t.Errorf("bind-failure logs missing %q:\n%s", want, logs)
		}
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("requested usage database not created before bind: %v", err)
	}
}

func TestRunBoundServeMetersBufferedOpenAIResponseWithoutChangingPayload(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "openai-responses-buffered.recorded.json"))
	if err != nil {
		t.Fatalf("read recorded response fixture: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(upstream.Close)
	harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-model"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "meter-tracer-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read forwarded response: %v", err)
	}
	if string(body) != string(fixture) {
		t.Fatalf("forwarded body changed:\n got: %q\nwant: %q", body, fixture)
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var (
		atMS, inputTokens, cachedTokens, cacheWriteTokens int64
		outputTokens, reasoningTokens, totalTokens        int64
		requestID, responseID, model, transport           string
		turnIndex                                         int
	)
	err = db.QueryRow(`SELECT at_ms, request_id, response_id, turn_index, model, transport,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens, total_tokens
		FROM openai_turn`).Scan(
		&atMS, &requestID, &responseID, &turnIndex, &model, &transport,
		&inputTokens, &cachedTokens, &cacheWriteTokens, &outputTokens, &reasoningTokens, &totalTokens,
	)
	if err != nil {
		t.Fatalf("query buffered OpenAI usage: %v", err)
	}
	if atMS <= 0 || requestID != "meter-tracer-request" || responseID != "resp_redacted_recorded_buffered" ||
		turnIndex != 0 || model != "gpt-5.6-sol" || transport != "buffered" || inputTokens != 12 ||
		cachedTokens != 0 || cacheWriteTokens != 0 || outputTokens != 6 || reasoningTokens != 0 || totalTokens != 18 {
		t.Errorf("persisted row = at_ms:%d request:%q response:%q turn:%d model:%q transport:%q usage:[%d %d %d %d %d %d]",
			atMS, requestID, responseID, turnIndex, model, transport, inputTokens, cachedTokens, cacheWriteTokens, outputTokens, reasoningTokens, totalTokens)
	}
}

func TestRunBoundServeMetersOpenAISSECompletionWithoutChangingFrames(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "openai-responses-sse.recorded.sse"))
	if err != nil {
		t.Fatalf("read recorded SSE fixture: %v", err)
	}
	frameEnd := bytes.Index(fixture, []byte("\n\n"))
	if frameEnd < 0 {
		t.Fatal("recorded SSE fixture has no frame separator")
	}
	fixture = fixture[:frameEnd+2]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(upstream.Close)
	harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-model"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "meter-sse-tracer-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward SSE response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read forwarded SSE response: %v", err)
	}
	if !bytes.Equal(body, fixture) {
		t.Fatalf("forwarded SSE frames changed:\n got: %q\nwant: %q", body, fixture)
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var (
		atMS, inputTokens, cachedTokens, cacheWriteTokens int64
		outputTokens, reasoningTokens, totalTokens        int64
		requestID, responseID, model, transport           string
		turnIndex                                         int
	)
	err = db.QueryRow(`SELECT at_ms, request_id, response_id, turn_index, model, transport,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens, total_tokens
		FROM openai_turn`).Scan(
		&atMS, &requestID, &responseID, &turnIndex, &model, &transport,
		&inputTokens, &cachedTokens, &cacheWriteTokens, &outputTokens, &reasoningTokens, &totalTokens,
	)
	if err != nil {
		t.Fatalf("query OpenAI SSE usage: %v", err)
	}
	if atMS <= 0 || requestID != "meter-sse-tracer-request" || responseID != "resp_redacted_recorded_sse" ||
		turnIndex != 0 || model != "gpt-5.6-sol" || transport != "sse" || inputTokens != 12 ||
		cachedTokens != 0 || cacheWriteTokens != 0 || outputTokens != 20 || reasoningTokens != 12 || totalTokens != 32 {
		t.Errorf("persisted row = at_ms:%d request:%q response:%q turn:%d model:%q transport:%q usage:[%d %d %d %d %d %d]",
			atMS, requestID, responseID, turnIndex, model, transport, inputTokens, cachedTokens, cacheWriteTokens, outputTokens, reasoningTokens, totalTokens)
	}
}

func TestRunBoundServeMetersOpenAIWebSocketCompletionsWithoutChangingMessages(t *testing.T) {
	recorded, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "openai-responses-websocket.recorded.jsonl"))
	if err != nil {
		t.Fatalf("read recorded WebSocket fixture: %v", err)
	}
	messages := []struct {
		kind websocket.MessageType
		data []byte
	}{
		{kind: websocket.MessageText, data: []byte(`{"type":"response.failed","response":{"id":"failed","model":"not-recorded","status":"failed","usage":{"input_tokens":99,"output_tokens":99}}}`)},
		{kind: websocket.MessageBinary, data: []byte(`{"type":"response.completed","response":{"id":"incomplete","model":"not-recorded","status":"incomplete","usage":{"input_tokens":98,"output_tokens":98}}}`)},
		{kind: websocket.MessageText, data: []byte(`{"type":"response.completed","response":{"id":"resp-ws-a","model":"reported-model-a","status":"completed","usage":{"input_tokens":3,"output_tokens":5}}}`)},
		{kind: websocket.MessageBinary, data: []byte(`{"type":"response.completed","response":`)},
		{kind: websocket.MessageText, data: []byte(`{"type":"error","error":{"message":"session continues"}}`)},
		{kind: websocket.MessageText, data: bytes.TrimSuffix(recorded, []byte("\n"))},
		{kind: websocket.MessageBinary, data: []byte(`{"type":"response.incomplete","response":{"id":"incomplete-terminal","model":"not-recorded","status":"incomplete","usage":{"input_tokens":97,"output_tokens":97}}}`)},
		{kind: websocket.MessageBinary, data: []byte(`{"type":"response.completed","response":{"id":"resp-ws-b","model":"reported-model-b","status":"completed","usage":{"input_tokens":7,"output_tokens":11}}}`)},
	}
	bufferedPayload := []byte(`{"id":"resp-shared-http","model":"reported-http-model","status":"completed","usage":{"input_tokens":2,"output_tokens":3}}`)
	clientMessage := []byte(`{"type":"response.create","model":"requested-model"}`)
	upstreamReceived := make(chan struct {
		kind websocket.MessageType
		data []byte
	}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(bufferedPayload)
			return
		}
		conn, acceptErr := websocket.Accept(w, r, nil)
		if acceptErr != nil {
			t.Errorf("accept upstream WebSocket: %v", acceptErr)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		kind, data, readErr := conn.Read(context.Background())
		if readErr != nil {
			t.Errorf("read upstream client Message: %v", readErr)
			return
		}
		upstreamReceived <- struct {
			kind websocket.MessageType
			data []byte
		}{kind: kind, data: data}
		for _, message := range messages {
			if writeErr := conn.Write(context.Background(), message.kind, message.data); writeErr != nil {
				t.Errorf("write upstream server Message: %v", writeErr)
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "completed")
	}))
	t.Cleanup(upstream.Close)
	harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
	baseURL := harness.baseURL

	httpRequest, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-http-model"}`))
	if err != nil {
		t.Fatalf("build shared-sink HTTP request: %v", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+testAPIKey)
	httpRequest.Header.Set("X-Request-Id", "meter-shared-http-request")
	httpResponse, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatalf("forward shared-sink HTTP response: %v", err)
	}
	httpBody, err := io.ReadAll(httpResponse.Body)
	_ = httpResponse.Body.Close()
	if err != nil {
		t.Fatalf("read shared-sink HTTP response: %v", err)
	}
	if !bytes.Equal(httpBody, bufferedPayload) {
		t.Fatalf("shared-sink HTTP body = %q, want exact %q", httpBody, bufferedPayload)
	}

	conn, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(baseURL, "http")+"/openai/v1/responses", &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + testAPIKey},
			"X-Request-Id":  {"meter-websocket-tracer-request"},
		},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial WebSocket transport: %v", err)
	}
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wsCancel()
	if err := conn.Write(wsCtx, websocket.MessageText, clientMessage); err != nil {
		t.Fatalf("write client Message: %v", err)
	}
	select {
	case got := <-upstreamReceived:
		if got.kind != websocket.MessageText || !bytes.Equal(got.data, clientMessage) {
			t.Errorf("upstream client Message = (%v, %q), want exact text %q", got.kind, got.data, clientMessage)
		}
	case <-wsCtx.Done():
		t.Fatal("client Message did not reach fake Copilot WebSocket")
	}
	for i, want := range messages {
		kind, data, readErr := conn.Read(wsCtx)
		if readErr != nil {
			t.Fatalf("read server Message %d: %v", i, readErr)
		}
		if kind != want.kind || !bytes.Equal(data, want.data) {
			t.Errorf("server Message %d = (%v, %q), want exact (%v, %q)", i, kind, data, want.kind, want.data)
		}
	}
	if _, _, readErr := conn.Read(wsCtx); websocket.CloseStatus(readErr) != websocket.StatusNormalClosure {
		t.Errorf("WebSocket close = %v, want normal completion", readErr)
	}
	_ = conn.CloseNow()

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var httpRequestID, httpResponseID, httpModel, httpTransport string
	var httpTurnIndex int
	if err := db.QueryRow(`SELECT request_id, response_id, turn_index, model, transport FROM openai_turn WHERE transport = 'buffered'`).Scan(
		&httpRequestID, &httpResponseID, &httpTurnIndex, &httpModel, &httpTransport,
	); err != nil {
		t.Fatalf("query shared-sink HTTP usage: %v", err)
	}
	if httpRequestID != "meter-shared-http-request" || httpResponseID != "resp-shared-http" || httpTurnIndex != 0 ||
		httpModel != "reported-http-model" || httpTransport != "buffered" {
		t.Errorf("shared-sink HTTP row = request:%q response:%q turn:%d model:%q transport:%q",
			httpRequestID, httpResponseID, httpTurnIndex, httpModel, httpTransport)
	}

	rows, err := db.Query(`SELECT at_ms, request_id, response_id, turn_index, model, transport,
		input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens, total_tokens
		FROM openai_turn WHERE transport = 'websocket' ORDER BY id`)
	if err != nil {
		t.Fatalf("query OpenAI WebSocket usage: %v", err)
	}
	defer rows.Close()
	wantRows := []struct {
		responseID     string
		model          string
		input          int64
		output         int64
		optionals      [4]int64
		optionalsValid bool
	}{
		{responseID: "resp-ws-a", model: "reported-model-a", input: 3, output: 5},
		{
			responseID: "resp_redacted_recorded_websocket", model: "gpt-5.6-sol", input: 12, output: 20,
			optionals: [4]int64{0, 0, 12, 32}, optionalsValid: true,
		},
		{responseID: "resp-ws-b", model: "reported-model-b", input: 7, output: 11},
	}
	for i, want := range wantRows {
		if !rows.Next() {
			t.Fatalf("persisted WebSocket rows ended at %d, want %d", i, len(wantRows))
		}
		var atMS, input, output int64
		var requestID, responseID, model, transport string
		var turnIndex int
		var optionals [4]sql.NullInt64
		if err := rows.Scan(&atMS, &requestID, &responseID, &turnIndex, &model, &transport,
			&input, &optionals[0], &optionals[1], &output, &optionals[2], &optionals[3]); err != nil {
			t.Fatalf("scan OpenAI WebSocket row %d: %v", i, err)
		}
		if atMS <= 0 || requestID != "meter-websocket-tracer-request" || responseID != want.responseID ||
			turnIndex != i || model != want.model || transport != "websocket" || input != want.input || output != want.output {
			t.Errorf("persisted WebSocket row %d = at_ms:%d request:%q response:%q turn:%d model:%q transport:%q usage:[%d %d]",
				i, atMS, requestID, responseID, turnIndex, model, transport, input, output)
		}
		for field, got := range optionals {
			if got.Valid != want.optionalsValid || got.Valid && got.Int64 != want.optionals[field] {
				t.Errorf("persisted WebSocket row %d optional %d = %+v, want valid=%t value=%d",
					i, field, got, want.optionalsValid, want.optionals[field])
			}
		}
	}
	if rows.Next() {
		t.Fatal("persisted more OpenAI WebSocket rows than expected")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate OpenAI WebSocket rows: %v", err)
	}
}

func TestRunBoundServeRetainsOpenAIWebSocketUsageWhenSessionLaterFails(t *testing.T) {
	completion := []byte(`{"type":"response.completed","response":{"id":"resp-before-session-error","model":"reported-before-session-error","status":"completed","usage":{"input_tokens":5,"output_tokens":8}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, completion); err != nil {
			t.Errorf("write completion before session failure: %v", err)
			return
		}
		_ = conn.CloseNow()
	}))
	t.Cleanup(upstream.Close)

	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	conn := dialUsageMeterWebSocket(t, harness.baseURL, "websocket-later-session-error")
	wsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(wsCtx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write client Message: %v", err)
	}
	kind, data, err := conn.Read(wsCtx)
	if err != nil || kind != websocket.MessageText || !bytes.Equal(data, completion) {
		t.Fatalf("completion before session failure = kind:%v data:%q err:%v", kind, data, err)
	}
	if _, _, err := conn.Read(wsCtx); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("session close = %v, want 1011 after upstream failure", err)
	}
	_ = conn.CloseNow()

	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	report := harness.closeStore()
	if report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("usage shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requestID, responseID, model, transport string
	var inputTokens, outputTokens int64
	if err := db.QueryRow(`SELECT request_id, response_id, model, transport, input_tokens, output_tokens FROM openai_turn`).Scan(
		&requestID, &responseID, &model, &transport, &inputTokens, &outputTokens,
	); err != nil {
		t.Fatalf("query retained WebSocket usage: %v", err)
	}
	if requestID != "websocket-later-session-error" || responseID != "resp-before-session-error" ||
		model != "reported-before-session-error" || transport != "websocket" || inputTokens != 5 || outputTokens != 8 {
		t.Errorf("retained WebSocket row = request:%q response:%q model:%q transport:%q usage:[%d %d]",
			requestID, responseID, model, transport, inputTokens, outputTokens)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=websocket-later-session-error")
	if len(accessLines) != 1 {
		t.Fatalf("terminal access records = %d, want one after handler completion:\n%s", len(accessLines), logText)
	}
	for _, want := range []string{"terminal_reason=error", "close_code=1011", "msgs_u2c=1", "hook_overruns=0"} {
		if !strings.Contains(accessLines[0], want) {
			t.Errorf("session-error access record missing %q: %s", want, accessLines[0])
		}
	}
}

type heldServerMessageShim struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newHeldServerMessageShim() *heldServerMessageShim {
	return &heldServerMessageShim{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *heldServerMessageShim) TransformServerMessage(_ context.Context, _ *shim.Message) bool {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	return true
}

func (s *heldServerMessageShim) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func withHeldServerMessageShim(held *heldServerMessageShim) func(shim.Registry) shim.Registry {
	return func(registry shim.Registry) shim.Registry {
		return append(shim.Registry{{
			Name:    "hold-after-usage-observation",
			Enabled: true,
			Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
				return surface == endpoint.OpenAI && route == endpoint.RouteOpenAIResponses
			},
			New: func(context.Context, endpoint.Surface, endpoint.Route) any { return held },
		}}, registry...)
	}
}

func TestRunBoundServeRetainsOpenAIWebSocketUsageObservedBeforeDownstreamWriteFailure(t *testing.T) {
	completion := []byte(`{"type":"response.completed","response":{"id":"resp-before-write-failure","model":"reported-before-write-failure","status":"completed","usage":{"input_tokens":13,"output_tokens":21}}}`)
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		if err := conn.Write(context.Background(), websocket.MessageText, completion); err != nil {
			t.Errorf("write completion before downstream failure: %v", err)
			return
		}
		_, _, _ = conn.Read(context.Background())
		close(upstreamClosed)
	}))
	t.Cleanup(upstream.Close)

	held := newHeldServerMessageShim()
	t.Cleanup(held.Release)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, withHeldServerMessageShim(held))
	conn := dialUsageMeterWebSocket(t, harness.baseURL, "websocket-downstream-write-failure")
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write client Message: %v", err)
	}
	select {
	case <-held.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("outer Shim did not hold the Message after the Usage meter observed it")
	}
	_ = conn.CloseNow()
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not observe the downstream close before the held Message was released")
	}
	held.Release()

	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after downstream failure: %v", err)
	}
	report := harness.closeStore()
	if report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("usage shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE request_id = 'websocket-downstream-write-failure' AND response_id = 'resp-before-write-failure' AND transport = 'websocket'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("retained rows = %d, want completion observed before failed downstream write", count)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=websocket-downstream-write-failure")
	if len(accessLines) != 1 {
		t.Fatalf("terminal access records = %d, want sole after-handler summary:\n%s", len(accessLines), logText)
	}
	if !strings.Contains(accessLines[0], "msgs_u2c=0") || !strings.Contains(accessLines[0], "bytes_u2c=0") {
		t.Errorf("downstream-failure access record counted an unwritten completion: %s", accessLines[0])
	}
}

func TestRunBoundServeOpenAIWebSocketStaysResponsiveWhileRealStoreIsFullAndFailing(t *testing.T) {
	const submissions = 1153
	completion := []byte(`{"type":"response.completed","response":{"id":"resp-duplicate-under-lock","model":"reported-under-lock","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
	recovery := []byte(`{"type":"response.completed","response":{"id":"resp-after-lock","model":"reported-after-lock","status":"completed","usage":{"input_tokens":3,"output_tokens":5}}}`)
	sendRecovery := make(chan struct{})
	var sendRecoveryOnce sync.Once
	releaseRecovery := func() { sendRecoveryOnce.Do(func() { close(sendRecovery) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		for range submissions {
			if err := conn.Write(context.Background(), websocket.MessageText, completion); err != nil {
				return
			}
		}
		<-sendRecovery
		if err := conn.Write(context.Background(), websocket.MessageBinary, recovery); err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(releaseRecovery)

	base := discardLogger(t)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	locker, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatalf("open external SQLite locker: %v", err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock usage database: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.Exec("ROLLBACK")
		}
	}()

	conn := dialUsageMeterWebSocket(t, harness.baseURL, "websocket-while-store-locked")
	readCtx, readCancel := context.WithTimeout(context.Background(), 4*time.Second)
	if err := conn.Write(readCtx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		readCancel()
		t.Fatalf("write client Message: %v", err)
	}
	started := time.Now()
	for index := range submissions {
		kind, data, err := conn.Read(readCtx)
		if err != nil {
			readCancel()
			t.Fatalf("read locked-store Message %d: %v", index, err)
		}
		if kind != websocket.MessageText || !bytes.Equal(data, completion) {
			readCancel()
			t.Fatalf("locked-store Message %d changed: kind=%v data=%q", index, kind, data)
		}
	}
	elapsed := time.Since(started)
	readCancel()
	if elapsed >= 4*time.Second {
		t.Fatalf("WebSocket pump took %s while SQLite writer was blocked, want prompt nonblocking forwarding", elapsed)
	}

	// Keep the real external write lock beyond the store's native runtime budget:
	// the in-flight batch fails while the WebSocket session itself remains live.
	time.Sleep(5500 * time.Millisecond)
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatalf("release usage database lock: %v", err)
	}
	locked = false
	time.Sleep(100 * time.Millisecond)
	releaseRecovery()
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 2*time.Second)
	kind, data, err := conn.Read(recoveryCtx)
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(data, recovery) {
		recoveryCancel()
		t.Fatalf("recovery Message = kind:%v data:%q err:%v", kind, data, err)
	}
	if _, _, err := conn.Read(recoveryCtx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("recovered WebSocket close = %v, want normal", err)
	}
	recoveryCancel()
	_ = conn.CloseNow()

	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	report := harness.closeStore()
	if report.QueueFullDrops == 0 || report.RuntimeWriteLosses == 0 || report.LateAfterCutoffDrops != 0 ||
		report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("locked/failing-store shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows, distinctOrdinals, recoveryRows int
	var maxOrdinal int
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT turn_index), max(turn_index),
		count(*) FILTER (WHERE response_id = 'resp-after-lock') FROM openai_turn WHERE transport = 'websocket'`).Scan(
		&rows, &distinctOrdinals, &maxOrdinal, &recoveryRows,
	); err != nil {
		t.Fatalf("query locked-store WebSocket usage: %v", err)
	}
	attempts := submissions + 1
	if rows+int(report.QueueFullDrops)+int(report.RuntimeWriteLosses) != attempts {
		t.Errorf("persisted %d + queue drops %d + runtime losses %d = %d, want %d submission attempts",
			rows, report.QueueFullDrops, report.RuntimeWriteLosses,
			rows+int(report.QueueFullDrops)+int(report.RuntimeWriteLosses), attempts)
	}
	if distinctOrdinals != rows || recoveryRows != 1 || maxOrdinal != submissions {
		t.Errorf("persisted ordinals/recovery = distinct:%d rows:%d max:%d recovery:%d", distinctOrdinals, rows, maxOrdinal, recoveryRows)
	}
}

func TestRunBoundServeForcedWebSocketDrainAndFreshUsageFinalizationAreBounded(t *testing.T) {
	completion := []byte(`{"type":"response.completed","response":{"id":"resp-before-forced-drain","model":"reported-before-forced-drain","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		_ = conn.Write(context.Background(), websocket.MessageText, completion)
	}))
	t.Cleanup(upstream.Close)

	held := newHeldServerMessageShim()
	t.Cleanup(held.Release)
	base := discardLogger(t)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, func(cfg *config.ServeConfig) {
		cfg.ShutdownTimeout = 75 * time.Millisecond
	}, withHeldServerMessageShim(held))
	locker, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.Exec("ROLLBACK")
		}
	}()

	conn := dialUsageMeterWebSocket(t, harness.baseURL, "websocket-forced-drain")
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("outer Shim did not hold the Message after usage observation")
	}

	drainStarted := time.Now()
	serveErr := harness.stop()
	drainElapsed := time.Since(drainStarted)
	if !errors.Is(serveErr, context.DeadlineExceeded) {
		t.Fatalf("forced drain error = %v, want deadline exceeded", serveErr)
	}
	if drainElapsed < 50*time.Millisecond || drainElapsed > 500*time.Millisecond {
		t.Errorf("forced drain elapsed = %s, want one bounded shutdown interval", drainElapsed)
	}

	harness.store.StopAdmission()
	harness.store.Record(usage.Turn{})
	finalizeStarted := time.Now()
	report := harness.closeStore()
	finalizeElapsed := time.Since(finalizeStarted)
	if finalizeElapsed < 50*time.Millisecond || finalizeElapsed > 500*time.Millisecond {
		t.Errorf("fresh usage finalization elapsed = %s, want an independent bounded interval", finalizeElapsed)
	}
	if report.LateAfterCutoffDrops != 1 || report.FinalFlushLosses != 1 || report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 {
		t.Fatalf("forced-drain usage report = %+v, want one late call and one contended observed completion", report)
	}

	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	held.Release()
}

type blockingServerErrorLogState struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (s *blockingServerErrorLogState) Release() {
	s.releaseOnce.Do(func() { close(s.release) })
}

type blockingServerErrorLogHandler struct {
	inner slog.Handler
	state *blockingServerErrorLogState
}

func (h blockingServerErrorLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h blockingServerErrorLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "server error" {
		h.state.enteredOnce.Do(func() { close(h.state.entered) })
		<-h.state.release
	}
	return h.inner.Handle(ctx, record)
}

func (h blockingServerErrorLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return blockingServerErrorLogHandler{inner: h.inner.WithAttrs(attrs), state: h.state}
}

func (h blockingServerErrorLogHandler) WithGroup(name string) slog.Handler {
	return blockingServerErrorLogHandler{inner: h.inner.WithGroup(name), state: h.state}
}

func TestRunBoundServeStopsUsageAdmissionBeforeReportingForcedDrainError(t *testing.T) {
	completion := []byte(`{"type":"response.completed","response":{"id":"resp-before-forced-drain-log","model":"reported-before-forced-drain-log","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream WebSocket: %v", err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if _, _, err := conn.Read(context.Background()); err != nil {
			return
		}
		_ = conn.Write(context.Background(), websocket.MessageText, completion)
	}))
	t.Cleanup(upstream.Close)

	held := newHeldServerMessageShim()
	t.Cleanup(held.Release)
	logState := &blockingServerErrorLogState{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(logState.Release)
	base := slog.New(blockingServerErrorLogHandler{
		inner: slog.NewTextHandler(io.Discard, nil),
		state: logState,
	})
	harness := startUsageMeterServeHarness(t, upstream.URL, base, func(cfg *config.ServeConfig) {
		cfg.ShutdownTimeout = 75 * time.Millisecond
	}, withHeldServerMessageShim(held))
	conn := dialUsageMeterWebSocket(t, harness.baseURL, "forced-drain-error-log-order")
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("outer Shim did not hold the observed completion")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- harness.stop() }()
	select {
	case <-logState.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("forced drain did not reach synchronous server-error logging")
	}

	// Model a producer that was already in flight when the forced drain returned.
	// The production serve lifecycle, not the harness, must already have cut off
	// admission before entering the synchronous logger.
	harness.store.Record(usage.Turn{
		At:         time.UnixMilli(1_750_000_000_000),
		RequestID:  "forced-drain-error-log-order",
		ResponseID: "must-be-late-after-cutoff",
		Model:      "reported-too-late",
		Transport:  usage.TransportWebSocket,
		Usage:      usage.OpenAIUsage{InputTokens: 3, OutputTokens: 5},
	})
	logState.Release()
	if err := <-stopDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("forced drain error = %v, want deadline exceeded", err)
	}
	held.Release()

	report := harness.closeStore()
	wantReport := sqlitestore.Report{LateAfterCutoffDrops: 1, DriverCleanupCompleted: true}
	if report != wantReport {
		t.Fatalf("usage shutdown report = %+v, want one producer rejected before server-error logging and otherwise clean %+v", report, wantReport)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var observedRows, lateRows int
	if err := db.QueryRow(`SELECT
		count(*) FILTER (WHERE response_id = 'resp-before-forced-drain-log'),
		count(*) FILTER (WHERE response_id = 'must-be-late-after-cutoff')
		FROM openai_turn`).Scan(&observedRows, &lateRows); err != nil {
		t.Fatal(err)
	}
	if observedRows != 1 {
		t.Errorf("completion observed before forced drain persisted %d rows, want one", observedRows)
	}
	if lateRows != 0 {
		t.Errorf("late producer persisted %d rows while server-error logging was blocked, want none", lateRows)
	}
}

func TestRunBoundServeDeliversCleanOpenAINoncompletionTerminalsWithoutUsageRows(t *testing.T) {
	terminals := []string{
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"failed\",\"model\":\"reported\",\"status\":\"failed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n",
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"incomplete\",\"model\":\"reported\",\"status\":\"incomplete\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream terminal\"}}\n\n",
	}
	var responseIndex atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(responseIndex.Add(1) - 1)
		if index >= len(terminals) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, terminals[index])
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)

	for i, want := range terminals {
		req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatalf("build request %d: %v", i, err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("X-Request-Id", fmt.Sprintf("noncompletion-%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forward terminal %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read terminal %d: %v", i, err)
		}
		if string(body) != want {
			t.Errorf("terminal %d body = %q, want byte-identical %q", i, body, want)
		}
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&rows); err != nil {
		t.Fatalf("query OpenAI usage row count: %v", err)
	}
	if rows != 0 {
		t.Errorf("OpenAI usage rows = %d, want none for clean noncompletion terminals", rows)
	}
	logText := logs.String()
	for i := range terminals {
		requestID := fmt.Sprintf("noncompletion-%d", i)
		accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id="+requestID, "outcome=clean")
		if len(accessLines) != 1 {
			t.Errorf("clean terminal access records for %q = %d, want one:\n%s", requestID, len(accessLines), logText)
		}
	}
	if accessLines := phase4LogLinesContaining(logText, "msg=access"); len(accessLines) != len(terminals) {
		t.Errorf("total terminal access records = %d, want %d:\n%s", len(accessLines), len(terminals), logText)
	}
}

func TestRunBoundServeDisconnectBeforeOpenAICompletionProducesNoUsageRow(t *testing.T) {
	const firstFrame = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, firstFrame)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "disconnect-before-completion")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward response: %v", err)
	}
	got := make([]byte, len(firstFrame))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if string(got) != firstFrame {
		t.Fatalf("first frame = %q, want %q", got, firstFrame)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("disconnect downstream: %v", err)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream disconnect did not cancel the upstream stream")
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&rows); err != nil {
		t.Fatalf("query OpenAI usage row count: %v", err)
	}
	if rows != 0 {
		t.Errorf("OpenAI usage rows = %d, want none before completion was observed", rows)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=disconnect-before-completion", "outcome=client_cancel")
	if len(accessLines) != 1 {
		t.Errorf("disconnect terminal access records = %d, want one:\n%s", len(accessLines), logText)
	}
}

func TestRunBoundServeRetainsOpenAIUsageObservedBeforeOuterShimPanic(t *testing.T) {
	const completion = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-before-panic\",\"model\":\"reported-before-panic\",\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":8}}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completion)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, func(registry shim.Registry) shim.Registry {
		return append(shim.Registry{{
			Name:    "outer-panic-after-usage",
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return panicOnOpenAICompletion{}
			},
		}}, registry...)
	})

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "completion-before-outer-panic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if bytes.Equal(body, []byte(completion)) || bytes.Contains(body, []byte("resp-before-panic")) || !bytes.Contains(body, []byte("shim failed")) {
		t.Errorf("post-panic body = %q, want only the existing native shim-failure terminal", body)
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var requestID, responseID, model, transport string
	var inputTokens, outputTokens int64
	if err := db.QueryRow(`SELECT request_id, response_id, model, transport, input_tokens, output_tokens FROM openai_turn`).Scan(
		&requestID, &responseID, &model, &transport, &inputTokens, &outputTokens,
	); err != nil {
		t.Fatalf("query retained OpenAI usage: %v", err)
	}
	if requestID != "completion-before-outer-panic" || responseID != "resp-before-panic" || model != "reported-before-panic" ||
		transport != "sse" || inputTokens != 5 || outputTokens != 8 {
		t.Errorf("retained row = request:%q response:%q model:%q transport:%q input:%d output:%d",
			requestID, responseID, model, transport, inputTokens, outputTokens)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=completion-before-outer-panic", "outcome=shim_error")
	if len(accessLines) != 1 {
		t.Errorf("outer-panic terminal access records = %d, want one:\n%s", len(accessLines), logText)
	}
}

func TestRunBoundServeOpenAISSEStaysResponsiveWhileSQLiteLockFillsQueue(t *testing.T) {
	const (
		submissions = 1153
		completion  = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-duplicate-under-lock\",\"model\":\"reported-under-lock\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	)
	stream := strings.Repeat(completion, submissions)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	locker, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatalf("open external SQLite locker: %v", err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock usage database: %v", err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_, _ = locker.Exec("ROLLBACK")
		}
	})

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "sse-while-store-locked")
	client := &http.Client{Timeout: 3 * time.Second}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward SSE while SQLite writer waits: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("read SSE while SQLite writer waits: %v", err)
	}
	if !bytes.Equal(body, []byte(stream)) {
		t.Fatalf("locked-store SSE bytes changed: got %d bytes, want %d", len(body), len(stream))
	}
	if elapsed >= 2*time.Second {
		t.Errorf("SSE hook took %s while SQLite writer was locked, want prompt nonblocking response", elapsed)
	}

	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatalf("release usage database lock: %v", err)
	}
	locked = false
	db, report := externalUsageDB(t, harness)
	if report.QueueFullDrops == 0 {
		t.Fatalf("locked writer queue_full_drops = 0, want bounded queue pressure from %d submissions", submissions)
	}
	if report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("locked-store shutdown report = %+v", report)
	}

	var rows, distinctOrdinals int
	if err := db.QueryRow("SELECT count(*), count(DISTINCT turn_index) FROM openai_turn").Scan(&rows, &distinctOrdinals); err != nil {
		t.Fatalf("query locked-store OpenAI usage: %v", err)
	}
	if rows+int(report.QueueFullDrops) != submissions {
		t.Errorf("persisted rows %d + queue drops %d = %d, want all %d submission attempts accounted", rows, report.QueueFullDrops, rows+int(report.QueueFullDrops), submissions)
	}
	if distinctOrdinals != rows {
		t.Errorf("distinct persisted ordinals = %d, want one per %d duplicate observations", distinctOrdinals, rows)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=sse-while-store-locked", "outcome=clean")
	if len(accessLines) != 1 {
		t.Errorf("locked-store terminal access records = %d, want one:\n%s", len(accessLines), logText)
	}
	if !strings.Contains(logText, "msg=\"usage store finalized\"") || !strings.Contains(logText, "queue_full_drops=") || !strings.Contains(logText, "driver_cleanup_completed=true") {
		t.Errorf("locked-store final loss report missing:\n%s", logText)
	}
}

func TestRunBoundServeAnthropicMalformedErrorAndPrematureStreamsProduceNoUsageRows(t *testing.T) {
	const start = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-noncompletion\",\"model\":\"reported\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
	responses := []string{
		start + "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":\"bad\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		start + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream terminal\"}}\n\n",
		start + "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n",
	}
	var responseIndex atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(responseIndex.Add(1) - 1)
		if index >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responses[index])
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	for index, upstreamBody := range responses {
		req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		requestID := fmt.Sprintf("anthropic-noncompletion-%d", index)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("X-Request-Id", requestID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forward stream %d: %v", index, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read stream %d: %v", index, err)
		}
		if index < 2 && !bytes.Equal(body, []byte(upstreamBody)) {
			t.Errorf("terminal stream %d changed: got %q want %q", index, body, upstreamBody)
		}
		if index == 2 && (!bytes.HasPrefix(body, []byte(upstreamBody)) ||
			!bytes.Contains(body[len(upstreamBody):], []byte("event: error")) ||
			!bytes.Contains(body[len(upstreamBody):], []byte("copilotd:"))) {
			t.Errorf("premature stream body = %q, want upstream prefix plus synthesized Anthropic error", body)
		}
	}
	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	if report := harness.closeStore(); report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM anthropic_turn").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("Anthropic usage rows = %d, want none for malformed, error, or premature streams", rows)
	}
	logText := logs.String()
	for index := range responses {
		requestID := fmt.Sprintf("anthropic-noncompletion-%d", index)
		if lines := phase4LogLinesContaining(logText, "msg=access", "request_id="+requestID); len(lines) != 1 {
			t.Errorf("terminal access records for %q = %d, want one after handler return:\n%s", requestID, len(lines), logText)
		}
	}
	if lines := phase4LogLinesContaining(logText, "msg=access"); len(lines) != len(responses) {
		t.Errorf("total terminal access records = %d, want %d:\n%s", len(lines), len(responses), logText)
	}
}

func TestRunBoundServeDisconnectBeforeAnthropicStopProducesNoUsageRow(t *testing.T) {
	const partial = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-interrupted\",\"model\":\"reported\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, partial)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "disconnect-before-anthropic-stop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward response: %v", err)
	}
	got := make([]byte, len(partial))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read partial stream: %v", err)
	}
	if !bytes.Equal(got, []byte(partial)) {
		t.Fatalf("partial stream = %q, want %q", got, partial)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("disconnect downstream: %v", err)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream disconnect did not cancel the upstream Anthropic stream")
	}
	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	if report := harness.closeStore(); report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM anthropic_turn").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("Anthropic usage rows = %d, want none before message_stop observation", rows)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=disconnect-before-anthropic-stop", "outcome=client_cancel")
	if len(accessLines) != 1 {
		t.Errorf("disconnect terminal access records = %d, want one after handler return:\n%s", len(accessLines), logText)
	}
}

func TestRunBoundServeRetainsAnthropicSSEUsageObservedBeforeOuterShimPanic(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-before-panic\",\"model\":\"reported-before-panic\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, func(registry shim.Registry) shim.Registry {
		return append(shim.Registry{{
			Name:    "outer-panic-after-anthropic-usage",
			Enabled: true,
			Scope: func(surface endpoint.Surface, route endpoint.Route) bool {
				return surface == endpoint.Anthropic && route == endpoint.RouteAnthropicMessages
			},
			New: func(context.Context, endpoint.Surface, endpoint.Route) any { return panicOnAnthropicStop{} },
		}}, registry...)
	})
	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "anthropic-completion-before-outer-panic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if bytes.Contains(body, []byte("event: message_stop")) || !bytes.Contains(body, []byte("shim failed")) {
		t.Errorf("post-panic body = %q, want pre-stop frames followed by existing shim-failure terminal", body)
	}
	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	if report := harness.closeStore(); report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var requestID, messageID, model, transport string
	var inputTokens, outputTokens int64
	if err := db.QueryRow(`SELECT request_id, message_id, model, transport, input_tokens, output_tokens FROM anthropic_turn`).Scan(
		&requestID, &messageID, &model, &transport, &inputTokens, &outputTokens,
	); err != nil {
		t.Fatalf("query retained Anthropic usage: %v", err)
	}
	if requestID != "anthropic-completion-before-outer-panic" || messageID != "msg-before-panic" ||
		model != "reported-before-panic" || transport != "sse" || inputTokens != 5 || outputTokens != 8 {
		t.Errorf("retained row = request:%q message:%q model:%q transport:%q input:%d output:%d",
			requestID, messageID, model, transport, inputTokens, outputTokens)
	}
	logText := logs.String()
	accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id=anthropic-completion-before-outer-panic", "outcome=shim_error")
	if len(accessLines) != 1 {
		t.Errorf("outer-panic terminal access records = %d, want one after handler return:\n%s", len(accessLines), logText)
	}
}

func TestRunBoundServeAnthropicSSEStaysResponsiveThroughFullFailingStoreAndRecovers(t *testing.T) {
	const (
		submissions = 1153
		workers     = 32
		completion  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-duplicate-under-lock\",\"model\":\"reported-under-lock\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		recovery = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-after-lock\",\"model\":\"reported-after-lock\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	)
	var responseIndex atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if responseIndex.Add(1) <= submissions {
			_, _ = io.WriteString(w, completion)
			return
		}
		_, _ = io.WriteString(w, recovery)
	}))
	t.Cleanup(upstream.Close)

	base := discardLogger(t)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	locker, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatalf("open external SQLite locker: %v", err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock usage database: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = locker.Exec("ROLLBACK")
		}
	}()

	client := &http.Client{Transport: &http.Transport{MaxIdleConns: workers, MaxIdleConnsPerHost: workers}}
	t.Cleanup(client.CloseIdleConnections)
	jobs := make(chan int)
	errs := make(chan error, submissions)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				requestCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
				if err == nil {
					req.Header.Set("Authorization", "Bearer "+testAPIKey)
					req.Header.Set("X-Request-Id", fmt.Sprintf("anthropic-sse-lock-%d", index))
					var resp *http.Response
					resp, err = client.Do(req)
					if err == nil {
						var body []byte
						body, err = io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						if err == nil && !bytes.Equal(body, []byte(completion)) {
							err = fmt.Errorf("request %d body changed: got %q want %q", index, body, completion)
						}
					}
				}
				cancel()
				if err != nil {
					errs <- fmt.Errorf("request %d: %w", index, err)
				}
			}
		}()
	}
	started := time.Now()
	for index := range submissions {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed >= 4*time.Second {
		t.Fatalf("%d Anthropic SSE requests took %s while SQLite writer was blocked", submissions, elapsed)
	}

	// Keep the real external lock beyond the runtime busy budget. An admitted
	// batch must fail, while the request wave above already completed unchanged.
	if remaining := 5500*time.Millisecond - elapsed; remaining > 0 {
		time.Sleep(remaining)
	}
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatalf("release usage database lock: %v", err)
	}
	locked = false
	time.Sleep(100 * time.Millisecond)

	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "anthropic-sse-after-store-recovery")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward recovery request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !bytes.Equal(body, []byte(recovery)) {
		t.Fatalf("recovery SSE = %q err:%v, want exact %q", body, err, recovery)
	}

	if err := harness.stop(); err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	report := harness.closeStore()
	if report.QueueFullDrops == 0 || report.RuntimeWriteLosses == 0 || report.LateAfterCutoffDrops != 0 ||
		report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("locked/failing-store shutdown report = %+v", report)
	}
	db, err := sql.Open("sqlite", harness.cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows, nonzeroOrdinals, recoveryRows int
	if err := db.QueryRow(`SELECT count(*), count(*) FILTER (WHERE turn_index != 0),
		count(*) FILTER (WHERE message_id = 'msg-after-lock') FROM anthropic_turn WHERE transport = 'sse'`).Scan(
		&rows, &nonzeroOrdinals, &recoveryRows,
	); err != nil {
		t.Fatalf("query locked-store Anthropic SSE usage: %v", err)
	}
	attempts := submissions + 1
	if rows+int(report.QueueFullDrops)+int(report.RuntimeWriteLosses) != attempts {
		t.Errorf("persisted %d + queue drops %d + runtime losses %d = %d, want %d submission attempts",
			rows, report.QueueFullDrops, report.RuntimeWriteLosses,
			rows+int(report.QueueFullDrops)+int(report.RuntimeWriteLosses), attempts)
	}
	if nonzeroOrdinals != 0 || recoveryRows != 1 {
		t.Errorf("per-request ordinals/recovery = nonzero:%d recovery:%d", nonzeroOrdinals, recoveryRows)
	}
}

func TestRunBoundServeMetersAnthropicSSECompletionWithoutChangingFrames(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "anthropic-messages-sse-cumulative.synthetic.sse"))
	if err != nil {
		t.Fatalf("read generated SSE fixture: %v", err)
	}
	if suffix := bytes.Index(fixture, []byte("\n\n: end of synthetic contract stream")); suffix >= 0 {
		fixture = fixture[:suffix+2]
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(upstream.Close)

	base := discardLogger(t)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)
	req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"model":"requested-model","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("X-Request-Id", "anthropic-sse-tracer-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("forward Anthropic SSE response: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read Anthropic SSE response: %v", err)
	}
	if !bytes.Equal(body, fixture) {
		t.Fatalf("forwarded Anthropic SSE frames changed:\n got: %q\nwant: %q", body, fixture)
	}

	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	var (
		atMS, inputTokens, outputTokens                              int64
		cacheCreation, cacheRead, ephemeral5m, ephemeral1h, thinking int64
		requestID, messageID, model, transport                       string
		turnIndex                                                    int
	)
	err = db.QueryRow(`SELECT at_ms, request_id, message_id, turn_index, model, transport,
		input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		ephemeral_5m_input_tokens, ephemeral_1h_input_tokens, thinking_tokens
		FROM anthropic_turn`).Scan(
		&atMS, &requestID, &messageID, &turnIndex, &model, &transport,
		&inputTokens, &outputTokens, &cacheCreation, &cacheRead, &ephemeral5m, &ephemeral1h, &thinking,
	)
	if err != nil {
		t.Fatalf("query Anthropic SSE usage: %v", err)
	}
	if atMS <= 0 || requestID != "anthropic-sse-tracer-request" ||
		messageID != "msg_redacted_synthetic_cumulative" || turnIndex != 0 ||
		model != "claude-synthetic" || transport != "sse" || inputTokens != 12 || outputTokens != 9 ||
		cacheCreation != 2000 || cacheRead != 6000 || ephemeral5m != 750 || ephemeral1h != 1250 || thinking != 4 {
		t.Errorf("persisted Anthropic SSE row = at_ms:%d request:%q message:%q turn:%d model:%q transport:%q usage:[%d %d %d %d %d %d %d]",
			atMS, requestID, messageID, turnIndex, model, transport, inputTokens, outputTokens,
			cacheCreation, cacheRead, ephemeral5m, ephemeral1h, thinking)
	}
}

func TestRunBoundServeMetersBufferedAnthropicMessageWithoutChangingPayload(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "anthropic-messages-buffered.synthetic.json"))
	if err != nil {
		t.Fatalf("read generated response fixture: %v", err)
	}
	responses := [][]byte{
		fixture,
		[]byte(`{"id":"msg_redacted_synthetic_buffered","type":"message","model":"claude-null","stop_reason":"max_tokens","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":null,"cache_creation":null,"output_tokens_details":{"thinking_tokens":null}}}`),
		[]byte(`{"id":"msg_redacted_synthetic_buffered","type":"message","model":"claude-zero","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":0}}}`),
	}
	var responseIndex atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(responseIndex.Add(1) - 1)
		if index >= len(responses) {
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responses[index])
	}))
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	harness := startUsageMeterServeHarness(t, upstream.URL, base, nil, nil)

	for i, wantBody := range responses {
		req, err := http.NewRequest(http.MethodPost, harness.baseURL+"/anthropic/v1/messages", strings.NewReader(`{"model":"requested-model"}`))
		if err != nil {
			t.Fatalf("build request %d: %v", i, err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("X-Request-Id", fmt.Sprintf("anthropic-meter-request-%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("forward response %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read forwarded response %d: %v", i, err)
		}
		if string(body) != string(wantBody) {
			t.Fatalf("forwarded body %d changed:\n got: %q\nwant: %q", i, body, wantBody)
		}
	}
	db, report := externalUsageDB(t, harness)
	assertCleanUsageReport(t, report)
	logText := logs.String()
	for i := range responses {
		requestID := fmt.Sprintf("anthropic-meter-request-%d", i)
		accessLines := phase4LogLinesContaining(logText, "msg=access", "request_id="+requestID)
		if len(accessLines) != 1 {
			t.Errorf("terminal access records for %q = %d, want one:\n%s", requestID, len(accessLines), logText)
		}
	}
	if accessLines := phase4LogLinesContaining(logText, "msg=access"); len(accessLines) != len(responses) {
		t.Errorf("total terminal access records = %d, want %d:\n%s", len(accessLines), len(responses), logText)
	}

	rows, err := db.Query(`SELECT at_ms, request_id, message_id, turn_index, model, transport,
		input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		ephemeral_5m_input_tokens, ephemeral_1h_input_tokens, thinking_tokens
		FROM anthropic_turn ORDER BY id`)
	if err != nil {
		t.Fatalf("query buffered Anthropic usage: %v", err)
	}
	defer rows.Close()

	wantRows := []struct {
		model          string
		input, output  int64
		optionalValid  bool
		optionalValues [5]int64
	}{
		{model: "claude-synthetic", input: 12, output: 9, optionalValid: true, optionalValues: [5]int64{2000, 6000, 750, 1250, 4}},
		{model: "claude-null", input: 0, output: 0},
		{model: "claude-zero", input: 1, output: 2, optionalValid: true},
	}
	for i, want := range wantRows {
		if !rows.Next() {
			t.Fatalf("persisted Anthropic rows ended at %d, want %d", i, len(wantRows))
		}
		var (
			atMS, inputTokens, outputTokens int64
			requestID, messageID            string
			model, transport                string
			turnIndex                       int
			optionals                       [5]sql.NullInt64
		)
		if err := rows.Scan(&atMS, &requestID, &messageID, &turnIndex, &model, &transport,
			&inputTokens, &outputTokens, &optionals[0], &optionals[1], &optionals[2], &optionals[3], &optionals[4]); err != nil {
			t.Fatalf("scan buffered Anthropic row %d: %v", i, err)
		}
		if atMS <= 0 || requestID != fmt.Sprintf("anthropic-meter-request-%d", i) ||
			messageID != "msg_redacted_synthetic_buffered" || turnIndex != 0 || model != want.model ||
			transport != "buffered" || inputTokens != want.input || outputTokens != want.output {
			t.Errorf("persisted row %d envelope/core = at_ms:%d request:%q message:%q turn:%d model:%q transport:%q usage:[%d %d]",
				i, atMS, requestID, messageID, turnIndex, model, transport, inputTokens, outputTokens)
		}
		for field, got := range optionals {
			if got.Valid != want.optionalValid || got.Valid && got.Int64 != want.optionalValues[field] {
				t.Errorf("persisted row %d optional %d = %+v, want valid=%t value=%d", i, field, got, want.optionalValid, want.optionalValues[field])
			}
		}
	}
	if rows.Next() {
		t.Fatal("persisted more Anthropic rows than expected")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate buffered Anthropic rows: %v", err)
	}
}
