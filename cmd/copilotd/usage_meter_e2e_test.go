package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

type panicOnOpenAICompletion struct{}

func (panicOnOpenAICompletion) TransformEvent(_ context.Context, frame sse.Frame) []sse.Frame {
	if frame.Type == "response.completed" {
		panic("outer shim failed after usage observation")
	}
	return []sse.Frame{frame}
}

func TestRunServeUsageStoreFailurePrecedesBindAndDisabledServeCreatesNothing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

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
	defer held.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	base := discardLogger(t)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-model"}`))
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-sse")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	base := discardLogger(t)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-sse", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested-model"}`))
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-noncompletion")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-noncompletion", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	for i, want := range terminals {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-disconnect")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-disconnect", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-outer-panic")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-outer-panic", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	registry = append(shim.Registry{{
		Name:    "outer-panic-after-usage",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return panicOnOpenAICompletion{}
		},
	}}, registry...)
	if registry[len(registry)-1].Name != "usage-meter" {
		t.Fatalf("last registration = %q, want usage-meter innermost", registry[len(registry)-1].Name)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
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

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-locked-store")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}
	locker, err := sql.Open("sqlite", dbPath)
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

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-locked-store", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
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
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops == 0 {
		t.Fatalf("locked writer queue_full_drops = 0, want bounded queue pressure from %d submissions", submissions)
	}
	if report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("locked-store shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
	defer upstream.Close()

	dbPath := filepath.Join(t.TempDir(), "usage", "usage.db")
	cfg := e2eConfig("gho-usage-meter-anthropic")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = dbPath
	var logs bytes.Buffer
	base := newPhase4Logger(t, &logs)
	store, err := sqlitestore.Open(dbPath, logging.ForComponent(base, "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatalf("open usage store: %v", err)
	}

	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-usage-meter-anthropic", upstream.URL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, base, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("build serve provider: %v", err)
	}
	registry := configuredShimRegistry(cfg, store)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, base, mgr, imp, nil, cacheRegistry, registry, ln)
	}()
	baseURL := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, baseURL+"/healthz", http.StatusOK)

	for i, wantBody := range responses {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/anthropic/v1/messages", strings.NewReader(`{"model":"requested-model"}`))
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
	for i := range responses {
		requestID := fmt.Sprintf("anthropic-meter-request-%d", i)
		accessLines := phase4LogLinesContaining(logs.String(), "msg=access", "request_id="+requestID)
		if len(accessLines) != 1 {
			t.Errorf("terminal access records for %q = %d, want one:\n%s", requestID, len(accessLines), logs.String())
		}
	}
	if accessLines := phase4LogLinesContaining(logs.String(), "msg=access"); len(accessLines) != len(responses) {
		t.Errorf("total terminal access records = %d, want %d:\n%s", len(accessLines), len(responses), logs.String())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runBoundServe after cancellation: %v", err)
	}
	store.StopAdmission()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	report := store.Close(closeCtx)
	closeCancel()
	if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("clean usage shutdown report = %+v", report)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open usage database for external query: %v", err)
	}
	defer db.Close()
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
