package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
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
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/impersonation"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// capturedRecord is one emitted slog record flattened with its logger-attached
// attributes, so assertions can use structured level, component, and attrs.
type capturedRecord struct {
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

// recordSink collects every record a lifecycle emits. An optional hold blocks
// the first record with its message until released, so a test can act at an
// exact point of the lifecycle's synchronous record sequence.
type recordSink struct {
	mu      sync.Mutex
	records []capturedRecord
	hold    *recordHold
}

func (s *recordSink) snapshot() []capturedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRecord(nil), s.records...)
}

// await returns the first record with message, polling within a watchdog.
func (s *recordSink) await(t *testing.T, message string) capturedRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, record := range s.snapshot() {
			if record.message == message {
				return record
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q record; records = %+v", message, s.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

type recordHold struct {
	message     string
	reached     chan struct{}
	release     chan struct{}
	reachOnce   sync.Once
	releaseOnce sync.Once
}

func newRecordHold(t *testing.T, message string) *recordHold {
	t.Helper()
	hold := &recordHold{message: message, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(hold.Release)
	return hold
}

func (h *recordHold) await(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(5 * time.Second):
		t.Fatalf("lifecycle did not emit %q", h.message)
	}
}

func (h *recordHold) Release() { h.releaseOnce.Do(func() { close(h.release) }) }

type capturingHandler struct {
	sink  *recordSink
	attrs []slog.Attr
}

func (capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(ctx context.Context, record slog.Record) error {
	captured := capturedRecord{level: record.Level, message: record.Message, attrs: make(map[string]slog.Value)}
	for _, attr := range h.attrs {
		captured.attrs[attr.Key] = attr.Value
	}
	// Like production's context handler, keep the request-scoped correlation.
	if id, ok := logging.RequestIDFrom(ctx); ok {
		captured.attrs[logging.RequestIDKey] = slog.StringValue(id)
	}
	record.Attrs(func(attr slog.Attr) bool {
		captured.attrs[attr.Key] = attr.Value
		return true
	})
	h.sink.mu.Lock()
	h.sink.records = append(h.sink.records, captured)
	hold := h.sink.hold
	h.sink.mu.Unlock()
	if hold != nil && record.Message == hold.message {
		hold.reachOnce.Do(func() { close(hold.reached) })
		<-hold.release
	}
	return nil
}

func (h capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return capturingHandler{sink: h.sink, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

func cmdRecordsAt(records []capturedRecord, level slog.Level) []capturedRecord {
	var matched []capturedRecord
	for _, record := range records {
		if record.level == level && record.attrs[logging.ComponentKey].String() == "cmd/copilotd" {
			matched = append(matched, record)
		}
	}
	return matched
}

// recordIndex returns the index of the first record with message, or -1.
func recordIndex(records []capturedRecord, message string) int {
	for i, record := range records {
		if record.message == message {
			return i
		}
	}
	return -1
}

// offlineServeEdges refuses every edge request without dialing, so a lifecycle
// test never reaches a public origin. Tests replace the edges they exercise.
func offlineServeEdges() serveEdges {
	refuse := mainRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("offline serve edge refused %s", r.URL.Host)
	})
	client := &http.Client{Transport: refuse}
	return serveEdges{
		GitHubBaseURL: "https://github.invalid",
		GitHubClient:  client,
		Discovery: impersonation.Edge{
			VSCodeBaseURL:      "https://vscode.invalid",
			MarketplaceBaseURL: "https://marketplace.invalid",
			Client:             client,
		},
		CodexModels: catalog.ModelsEdge{BaseURL: "https://github.invalid", Client: client},
		Pricing:     pricing.NewRemote("https://models.invalid/api.json", refuse),
	}
}

// exchangeServeEdges is offlineServeEdges with the GitHub token exchange
// pointed at github.
func exchangeServeEdges(github *httptest.Server) serveEdges {
	edges := offlineServeEdges()
	edges.GitHubBaseURL, edges.GitHubClient = github.URL, github.Client()
	return edges
}

// lifecycleConfig is e2eConfig with every cached-value refresh disabled, so
// the only network edge a lifecycle test uses is the one it points at a stub.
func lifecycleConfig(oauthToken string) config.ServeConfig {
	cfg := e2eConfig(oauthToken)
	cfg.ImpersonationRefreshInterval = 0
	return cfg
}

func meteredLifecycleConfig(t *testing.T, oauthToken string) config.ServeConfig {
	t.Helper()
	cfg := lifecycleConfig(oauthToken)
	cfg.ShimUsageMeterEnabled = true
	cfg.UsageDBPath = filepath.Join(t.TempDir(), "usage", "usage.db")
	return cfg
}

// lifecycleRun is one in-process production serve lifecycle driven by a test
// through its context.
type lifecycleRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	result serveResult
}

// startServeLifecycle runs runServeLifecycle in the background. Cleanup
// cancels it and waits for its return, so finalization finishes before
// earlier-registered fixtures such as TempDir are removed.
func startServeLifecycle(t *testing.T, base *slog.Logger, input serveInput) *lifecycleRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &lifecycleRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		run.result = runServeLifecycle(ctx, base, input)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(15 * time.Second):
			t.Error("serve lifecycle did not return within the cleanup watchdog")
		}
	})
	return run
}

func (r *lifecycleRun) await(t *testing.T, within time.Duration) serveResult {
	t.Helper()
	select {
	case <-r.done:
		return r.result
	case <-time.After(within):
		t.Fatal("serve lifecycle did not return")
		return serveResult{}
	}
}

// startServedLifecycle serves input through the production lifecycle on a
// loopback listener and returns its base URL once /healthz answers. Cleanup
// requires a clean drain. It first closes the default client's idle
// connections: one dialed but never used would otherwise force the drain.
func startServedLifecycle(t *testing.T, base *slog.Logger, input serveInput) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	input.Listener = ln
	run := startServeLifecycle(t, base, input)
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		run.cancel()
		select {
		case <-run.done:
			if run.result.Outcome != serveClean || run.result.Err != nil {
				t.Errorf("serve lifecycle stop = %v (%v), want a clean drain", run.result.Outcome, run.result.Err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve lifecycle did not stop within the grace period")
		}
	})
	baseURL := "http://" + ln.Addr().String()
	run.awaitHealthy(t, baseURL)
	return baseURL
}

// awaitHealthy polls /healthz until it answers 200, failing with the
// lifecycle's result if it returned before serving.
func (r *lifecycleRun) awaitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-r.done:
			t.Fatalf("serve lifecycle returned before serving: %v (%v)", r.result.Outcome, r.result.Err)
		default:
		}
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = errors.New(resp.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s/healthz did not answer 200: %v", baseURL, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitCachedValue polls /readyz until the named cached value reports exactly
// source and version, the accepted-revision barrier for refresh fixtures.
func awaitCachedValue(t *testing.T, baseURL, name, source, version string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(5 * time.Second)
	last := "no response"
	for {
		resp, err := client.Get(baseURL + "/readyz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			last = string(body)
			var readiness struct {
				Caches map[string]struct {
					Source  string `json:"source"`
					Version string `json:"version"`
				} `json:"caches"`
			}
			if json.Unmarshal(body, &readiness) == nil {
				if got := readiness.Caches[name]; got.Source == source && got.Version == version {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cached value %s did not reach %s %s; last /readyz: %s", name, source, version, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newCodexReleaseEdge serves one synthetic Codex release over the three GitHub
// routes the Codex models cached value reads: the latest release tag, the tag's
// commit, and models.json at that commit.
func newCodexReleaseEdge(t *testing.T, tag, commit string, models []byte) *httptest.Server {
	t.Helper()
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/openai/codex/releases/latest":
			_, _ = io.WriteString(w, `{"tag_name":"`+tag+`"}`)
		case r.URL.Path == "/repos/openai/codex/commits/"+tag:
			_, _ = io.WriteString(w, commit)
		case r.URL.Path == "/repos/openai/codex/contents/codex-rs/models-manager/models.json" && r.URL.Query().Get("ref") == commit:
			_, _ = w.Write(models)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(edge.Close)
	return edge
}

func TestServedOutcomeClassifiesServerRunResults(t *testing.T) {
	genuine := errors.New("listener close failed")
	tests := []struct {
		name     string
		err      error
		want     serveOutcome
		wantExit error
	}{
		{name: "clean drain", err: nil, want: serveClean, wantExit: nil},
		{name: "forced drain", err: fmt.Errorf("graceful shutdown: %w: %w", server.ErrForcedDrain, context.DeadlineExceeded), want: serveForcedDrain, wantExit: nil},
		{name: "genuine error", err: fmt.Errorf("graceful shutdown: %w", genuine), want: serveServeFailure, wantExit: errServeFailed},
		{name: "genuine error mixed with deadline", err: fmt.Errorf("graceful shutdown: %w", errors.Join(genuine, context.DeadlineExceeded)), want: serveServeFailure, wantExit: errServeFailed},
		{name: "unmarked generic deadline", err: fmt.Errorf("serve: %w", context.DeadlineExceeded), want: serveServeFailure, wantExit: errServeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := servedOutcome(tt.err)
			if got != tt.want {
				t.Fatalf("servedOutcome(%v) = %v, want %v", tt.err, got, tt.want)
			}
			if exit := serveExitError(serveResult{Outcome: got, Err: tt.err}); exit != tt.wantExit {
				t.Errorf("serveExitError(%v) = %v, want %v", got, exit, tt.wantExit)
			}
		})
	}
}

func TestServeExitErrorIgnoresTheUsageReport(t *testing.T) {
	reports := map[string]sqlitestore.Report{
		"zero":                         {},
		"clean":                        {DriverCleanupCompleted: true},
		"losses and unconfirmed clean": {QueueFullDrops: 1, RuntimeWriteLosses: 2, LateAfterCutoffDrops: 3, FinalFlushLosses: 4},
	}
	for _, tc := range []struct {
		outcome serveOutcome
		want    error
	}{
		{outcome: serveClean, want: nil},
		{outcome: serveForcedDrain, want: nil},
		{outcome: servePreBindFailure, want: errServeFailed},
		{outcome: serveBindFailure, want: errServeFailed},
		{outcome: serveServeFailure, want: errServeFailed},
	} {
		for name, report := range reports {
			t.Run(tc.outcome.String()+"/"+name, func(t *testing.T) {
				if got := serveExitError(serveResult{Outcome: tc.outcome, Report: report}); got != tc.want {
					t.Errorf("serveExitError(%v, %+v) = %v, want %v", tc.outcome, report, got, tc.want)
				}
			})
		}
	}
}

func TestProductionServeEdgesUseProductionOriginsAndDedicatedClients(t *testing.T) {
	edges := productionServeEdges()
	if edges.GitHubBaseURL != "" {
		t.Errorf("GitHub exchange base URL = %q, want empty for the identity default", edges.GitHubBaseURL)
	}
	if edges.GitHubClient == nil || edges.GitHubClient == http.DefaultClient || edges.GitHubClient.Transport != nil || edges.GitHubClient.Timeout != 0 {
		t.Errorf("GitHub exchange client = %#v, want a dedicated plain client", edges.GitHubClient)
	}
	discovery := productionDiscoveryEdge()
	if edges.Discovery.VSCodeBaseURL != discovery.VSCodeBaseURL || edges.Discovery.MarketplaceBaseURL != discovery.MarketplaceBaseURL {
		t.Errorf("discovery edge = %+v, want production origins %+v", edges.Discovery, discovery)
	}
	if edges.CodexModels.BaseURL != productionCodexModelsBaseURL {
		t.Errorf("Codex models base URL = %q, want %q", edges.CodexModels.BaseURL, productionCodexModelsBaseURL)
	}
	clients := []*http.Client{edges.GitHubClient, edges.Discovery.Client, edges.CodexModels.Client}
	for i := range clients {
		for j := i + 1; j < len(clients); j++ {
			if clients[i] == clients[j] {
				t.Errorf("edge clients %d and %d are shared, want credential-isolated clients", i, j)
			}
		}
	}
}

func TestServeLifecycleDrainsMeteredCompletionThenFinalizes(t *testing.T) {
	const (
		requestID  = "lifecycle-completion-during-drain"
		completion = `{"id":"resp-released-during-drain","model":"reported-drain-model","status":"completed","usage":{"input_tokens":3,"output_tokens":5}}`
	)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completion)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(releaseUpstream)
	github := lifecycleExchangeStub(t, upstream.URL, make(chan http.Header, 1))

	cfg := meteredLifecycleConfig(t, "gho-lifecycle-drain")
	sink := &recordSink{}
	run := startServeLifecycle(t, slog.New(capturingHandler{sink: sink}), serveInput{Config: cfg, Edges: exchangeServeEdges(github)})
	// No listener was supplied: the lifecycle bound cfg.Addr itself.
	baseURL := "http://" + sink.await(t, "listening").attrs[logging.AddrKey].String()

	type response struct {
		status int
		body   string
		err    error
	}
	responses := make(chan response, 1)
	go func() {
		resp, body, err := performUsagePOST(context.Background(), baseURL, "/openai/v1/responses", requestID, `{"model":"requested"}`)
		if err != nil {
			responses <- response{err: err}
			return
		}
		responses <- response{status: resp.StatusCode, body: string(body)}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not reach the upstream stub")
	}

	run.cancel()
	sink.await(t, "shutting down")
	releaseUpstream()
	got := <-responses
	if got.err != nil || got.status != http.StatusOK || got.body != completion {
		t.Fatalf("drained response = %d %q (%v), want 200 with the upstream completion", got.status, got.body, got.err)
	}

	result := run.await(t, 10*time.Second)
	if result.Outcome != serveClean || result.Err != nil {
		t.Fatalf("lifecycle result = %v (%v), want clean", result.Outcome, result.Err)
	}
	if err := serveExitError(result); err != nil {
		t.Errorf("clean lifecycle exit = %v, want 0", err)
	}
	if want := (sqlitestore.Report{DriverCleanupCompleted: true}); result.Report != want {
		t.Errorf("finalization report = %+v, want %+v", result.Report, want)
	}

	db, err := sql.Open("sqlite", cfg.UsageDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if rows := queryUsageCount(t, db, "openai_turn", "request_id = ? AND response_id = 'resp-released-during-drain'", requestID); rows != 1 {
		t.Errorf("completion released during the drain persisted %d rows, want 1", rows)
	}

	records := sink.snapshot()
	shutdown, finalized := recordIndex(records, "shutting down"), recordIndex(records, "usage store finalized")
	access := -1
	for i, record := range records {
		if record.message == "access" && record.attrs[logging.RequestIDKey].String() == requestID {
			access = i
		}
	}
	if shutdown < 0 || access < shutdown || finalized < access {
		t.Errorf("record order shutting down=%d access=%d finalized=%d, want the drained access record between them", shutdown, access, finalized)
	}
	for i, record := range records {
		if record.attrs[logging.ComponentKey].String() == "internal/server" && i > finalized {
			t.Errorf("internal/server record %q followed the final aggregate; want finalization after Server.Run returned", record.message)
		}
	}
	for _, level := range []slog.Level{slog.LevelWarn, slog.LevelError} {
		if got := cmdRecordsAt(records, level); len(got) != 0 {
			t.Errorf("cmd/copilotd %v records = %+v, want none for a clean stop", level, got)
		}
	}
}

func TestServeLifecycleForcedDrainWarnsOnceAndExitsZero(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	github := lifecycleExchangeStub(t, upstream.URL, make(chan http.Header, 1))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const shutdownTimeout = 150 * time.Millisecond
	cfg := lifecycleConfig("gho-serve-outcome")
	cfg.ShutdownTimeout = shutdownTimeout
	sink := &recordSink{}
	run := startServeLifecycle(t, slog.New(capturingHandler{sink: sink}), serveInput{Config: cfg, Edges: exchangeServeEdges(github), Listener: ln})
	base := "http://" + ln.Addr().String()
	sink.await(t, "listening")

	request, err := http.NewRequest(http.MethodPost, base+"/openai/v1/responses", strings.NewReader(`{"model":"gpt","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST SSE: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if line, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || !strings.Contains(line, "response.created") {
		t.Fatalf("first SSE line = %q, %v", line, err)
	}

	run.cancel()
	result := run.await(t, 5*time.Second)
	if result.Outcome != serveForcedDrain || !errors.Is(result.Err, server.ErrForcedDrain) || !errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("lifecycle result = %v (%v), want forced drain wrapping the deadline", result.Outcome, result.Err)
	}
	if got := serveExitError(result); got != nil {
		t.Errorf("serveExitError(forced drain) = %v, want nil", got)
	}
	if result.Report != (sqlitestore.Report{}) {
		t.Errorf("disabled meter report = %+v, want zero", result.Report)
	}

	records := sink.snapshot()
	warnings := cmdRecordsAt(records, slog.LevelWarn)
	if len(warnings) != 1 {
		t.Fatalf("cmd/copilotd WARN records = %+v, want exactly one forced-drain warning", warnings)
	}
	warning := warnings[0]
	if loggedErr, ok := warning.attrs[logging.ErrorKey].Any().(error); !ok || !errors.Is(loggedErr, server.ErrForcedDrain) {
		t.Errorf("warning %s = %v, want the returned forced-drain error", logging.ErrorKey, warning.attrs[logging.ErrorKey])
	}
	if timeout, ok := warning.attrs[logging.TimeoutKey]; !ok || timeout.Kind() != slog.KindDuration || timeout.Duration() != shutdownTimeout {
		t.Errorf("warning %s = %v, want Duration %v", logging.TimeoutKey, timeout, shutdownTimeout)
	}
	if errorsLogged := cmdRecordsAt(records, slog.LevelError); len(errorsLogged) != 0 {
		t.Errorf("cmd/copilotd ERROR records = %+v, want none for a forced drain", errorsLogged)
	}
	for _, record := range records {
		if record.level == slog.LevelWarn && record.attrs[logging.ComponentKey].String() == "internal/server" {
			t.Errorf("internal/server emitted a duplicate warning: %+v", record)
		}
	}
}

// failingListener reports a fixed Accept error so Server.Run's Serve path
// fails without any shutdown drain.
type failingListener struct {
	net.Listener
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }

func TestServeLifecycleServeFailuresErrorOnceAndExitOne(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		metered bool
	}{
		{name: "genuine failure", err: errors.New("accept exploded")},
		// Wrapped so net/http does not treat it as a temporary net.Error retry.
		{name: "unrelated generic deadline", err: fmt.Errorf("accept: %w", context.DeadlineExceeded)},
		{name: "metered failure still finalizes", err: errors.New("accept exploded under the meter"), metered: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			cfg := lifecycleConfig("gho-serve-failure")
			if tt.metered {
				cfg = meteredLifecycleConfig(t, "gho-serve-failure")
			}
			sink := &recordSink{}
			run := startServeLifecycle(t, slog.New(capturingHandler{sink: sink}), serveInput{Config: cfg, Edges: offlineServeEdges(), Listener: failingListener{Listener: ln, err: tt.err}})
			result := run.await(t, 5*time.Second)
			if result.Outcome != serveServeFailure || !errors.Is(result.Err, tt.err) || errors.Is(result.Err, server.ErrForcedDrain) {
				t.Fatalf("lifecycle result = %v (%v), want a serve failure wrapping %v", result.Outcome, result.Err, tt.err)
			}
			if got := serveExitError(result); got != errServeFailed {
				t.Errorf("serveExitError = %v, want errServeFailed", got)
			}
			records := sink.snapshot()
			if got := cmdRecordsAt(records, slog.LevelError); len(got) != 1 || got[0].message != "server error" {
				t.Errorf("cmd/copilotd ERROR records = %+v, want one server error", got)
			}
			if got := cmdRecordsAt(records, slog.LevelWarn); len(got) != 0 {
				t.Errorf("cmd/copilotd WARN records = %+v, want no forced-drain warning", got)
			}
			wantReport := sqlitestore.Report{}
			if tt.metered {
				wantReport.DriverCleanupCompleted = true
				if serveErrorAt, finalizedAt := recordIndex(records, "server error"), recordIndex(records, "usage store finalized"); finalizedAt < serveErrorAt {
					t.Errorf("final aggregate at %d, server error at %d; want finalization after the outcome record", finalizedAt, serveErrorAt)
				}
			}
			if result.Report != wantReport {
				t.Errorf("finalization report = %+v, want %+v", result.Report, wantReport)
			}
		})
	}
}

func TestServeLifecycleSetupFailuresKeepTheirOutcomeAndOneDiagnostic(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })

	tests := []struct {
		name       string
		configure  func(t *testing.T, cfg *config.ServeConfig)
		want       serveOutcome
		wantErr    error
		diagnostic string
		storeOpens bool
	}{
		{
			name: "missing GitHub OAuth token",
			configure: func(t *testing.T, cfg *config.ServeConfig) {
				cfg.GithubOAuthToken = ""
				cfg.GithubOAuthTokenFile = filepath.Join(t.TempDir(), "absent-token")
			},
			want:       servePreBindFailure,
			wantErr:    identity.ErrNoOAuthToken,
			diagnostic: "cannot start: resolving the GitHub OAuth token failed",
		},
		{
			name: "usage store open failure",
			configure: func(t *testing.T, cfg *config.ServeConfig) {
				parent := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(parent, []byte("regular file"), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg.UsageDBPath = filepath.Join(parent, "usage.db")
			},
			want:       servePreBindFailure,
			diagnostic: "cannot start: opening usage database failed",
		},
		{
			name: "bind failure after the store opened",
			configure: func(_ *testing.T, cfg *config.ServeConfig) {
				cfg.Addr = held.Addr().String()
			},
			want:       serveBindFailure,
			diagnostic: "bind failed",
			storeOpens: true,
		},
	}
	for _, tc := range tests {
		for _, cancelled := range []bool{false, true} {
			name := tc.name
			if cancelled {
				// The failed step completes before cancellation can be noticed.
				name += "/cancelled while the diagnostic is held"
			}
			t.Run(name, func(t *testing.T) {
				cfg := meteredLifecycleConfig(t, "gho-setup-failure")
				tc.configure(t, &cfg)
				sink := &recordSink{}
				if cancelled {
					sink.hold = newRecordHold(t, tc.diagnostic)
				}
				run := startServeLifecycle(t, slog.New(capturingHandler{sink: sink}), serveInput{Config: cfg, Edges: offlineServeEdges()})
				if cancelled {
					sink.hold.await(t)
					run.cancel()
					sink.hold.Release()
				}
				result := run.await(t, 10*time.Second)
				if result.Outcome != tc.want || result.Err == nil || tc.wantErr != nil && !errors.Is(result.Err, tc.wantErr) {
					t.Fatalf("lifecycle result = %v (%v), want %v", result.Outcome, result.Err, tc.want)
				}
				if got := serveExitError(result); got != errServeFailed {
					t.Errorf("serveExitError = %v, want errServeFailed", got)
				}

				records := sink.snapshot()
				if got := cmdRecordsAt(records, slog.LevelError); len(got) != 1 || got[0].message != tc.diagnostic {
					t.Errorf("cmd/copilotd ERROR records = %+v, want one %q", got, tc.diagnostic)
				}
				if got := cmdRecordsAt(records, slog.LevelWarn); len(got) != 0 {
					t.Errorf("cmd/copilotd WARN records = %+v, want none", got)
				}
				if recordIndex(records, "listening") >= 0 {
					t.Error("setup failure served")
				}
				_, statErr := os.Stat(cfg.UsageDBPath)
				if tc.storeOpens {
					if want := (sqlitestore.Report{DriverCleanupCompleted: true}); result.Report != want {
						t.Errorf("finalization report = %+v, want %+v", result.Report, want)
					}
					if recordIndex(records, "usage store finalized") < recordIndex(records, tc.diagnostic) {
						t.Error("opened store was not finalized after the diagnostic")
					}
					if statErr != nil {
						t.Errorf("requested usage database not created before bind: %v", statErr)
					}
				} else {
					if result.Report != (sqlitestore.Report{}) || recordIndex(records, "usage store finalized") >= 0 {
						t.Errorf("unopened store report = %+v with records %+v, want no finalization", result.Report, records)
					}
					if statErr == nil {
						t.Error("usage database exists although the store never opened")
					}
				}
			})
		}
	}
}

func TestServeLifecycleCancelledOnEntrySetsUpNothing(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(fmt.Sprintf("supplied listener=%t", supplied), func(t *testing.T) {
			held, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = held.Close() })
			// Every setup step would fail here: no token, and an occupied address.
			cfg := meteredLifecycleConfig(t, "")
			cfg.GithubOAuthTokenFile = filepath.Join(t.TempDir(), "absent-token")
			cfg.Addr = held.Addr().String()
			input := serveInput{Config: cfg, Edges: offlineServeEdges()}
			var ln net.Listener
			if supplied {
				if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				input.Listener = ln
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			sink := &recordSink{}

			result := runServeLifecycle(ctx, slog.New(capturingHandler{sink: sink}), input)
			if result != (serveResult{Outcome: serveClean}) {
				t.Fatalf("lifecycle result = %+v, want a clean stop with a zero Report", result)
			}
			if err := serveExitError(result); err != nil {
				t.Errorf("serveExitError = %v, want 0", err)
			}
			if records := sink.snapshot(); len(records) != 0 {
				t.Errorf("records = %+v, want no setup step to run", records)
			}
			if _, err := os.Stat(filepath.Dir(cfg.UsageDBPath)); !os.IsNotExist(err) {
				t.Errorf("usage store directory exists (stat %v), want no store opened", err)
			}
			if supplied {
				if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Errorf("supplied listener Accept = %v, want net.ErrClosed", err)
				}
			}
		})
	}
}

func TestServeLifecycleCancelledAfterStoreOpenFinalizesWithoutServing(t *testing.T) {
	cfg := meteredLifecycleConfig(t, "gho-cancel-before-bind")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// The configured shim chain record follows the store open and precedes bind.
	sink := &recordSink{}
	sink.hold = newRecordHold(t, "configured shim chain")
	run := startServeLifecycle(t, slog.New(capturingHandler{sink: sink}), serveInput{Config: cfg, Edges: offlineServeEdges(), Listener: ln})
	sink.hold.await(t)
	run.cancel()
	sink.hold.Release()

	result := run.await(t, 10*time.Second)
	if result.Outcome != serveClean || result.Err != nil {
		t.Fatalf("lifecycle result = %v (%v), want a clean stop", result.Outcome, result.Err)
	}
	if err := serveExitError(result); err != nil {
		t.Errorf("serveExitError = %v, want 0", err)
	}
	if want := (sqlitestore.Report{DriverCleanupCompleted: true}); result.Report != want {
		t.Errorf("finalization report = %+v, want %+v", result.Report, want)
	}
	records := sink.snapshot()
	if recordIndex(records, "usage store finalized") < 0 {
		t.Errorf("records = %+v, want the opened store finalized", records)
	}
	if recordIndex(records, "listening") >= 0 {
		t.Error("lifecycle served after cancellation was noticed before bind")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("supplied listener Accept = %v, want net.ErrClosed", err)
	}
	if _, err := os.Stat(cfg.UsageDBPath); err != nil {
		t.Errorf("usage database was not opened before cancellation: %v", err)
	}
}

// TestFinalizeUsageStoreNeedsAFreshBudgetForAPendingObservation is the negative
// control for the lifecycle's fresh finalization budget: the same pending
// observation persists with a live budget, while an already expired budget
// loses it or leaves cleanup unconfirmed.
func TestFinalizeUsageStoreNeedsAFreshBudgetForAPendingObservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		fresh  bool
	}{
		{name: "fresh budget", budget: 2 * time.Second, fresh: true},
		{name: "expired budget", budget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage", "usage.db")
			store, err := sqlitestore.Open(path, logging.ForComponent(discardLogger(t), "internal/usage/sqlitestore"))
			if err != nil {
				t.Fatal(err)
			}
			// Contend for SQLite's write lock so no flush can persist the
			// observation before finalization.
			locker, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = locker.Close() })
			locker.SetMaxOpenConns(1)
			if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			var unlockOnce sync.Once
			unlock := func() {
				unlockOnce.Do(func() {
					if _, err := locker.Exec("ROLLBACK"); err != nil {
						t.Errorf("release usage database lock: %v", err)
					}
				})
			}
			store.Record(usage.Turn{At: time.Now(), RequestID: "pending", ResponseID: "resp-pending", Model: "reported-pending", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
			if tc.fresh {
				// Release the contention inside the budget.
				timer := time.AfterFunc(tc.budget/4, unlock)
				defer timer.Stop()
			}

			report := finalizeUsageStore(store, tc.budget)
			unlock()
			// Join native cleanup so no writer outlives the test.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store.Close(ctx)

			clean := sqlitestore.Report{DriverCleanupCompleted: true}
			if !tc.fresh {
				if report == clean {
					t.Fatalf("expired-budget report = %+v, want a final-flush loss or unconfirmed cleanup", report)
				}
				return
			}
			if report != clean {
				t.Fatalf("fresh-budget report = %+v, want the pending observation persisted and cleanup completed", report)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if rows := queryUsageCount(t, db, "openai_turn", "response_id = 'resp-pending'"); rows != 1 {
				t.Errorf("pending observation persisted %d rows, want 1", rows)
			}
		})
	}
}
