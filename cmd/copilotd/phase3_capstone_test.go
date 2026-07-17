package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
)

const (
	phase3RequestBody  = "{\n  \"model\": \"claude-3-5-sonnet\",\n  \"stream\": false,\n  \"messages\": [{\"role\":\"user\",\"content\":\"preserve me\"}]\n}"
	phase3BufferedBody = "{\n  \"id\": \"msg_phase3\",\n  \"content\": [{\"text\":\"unchanged\"}]\n}\n"
	phase3FirstFrame   = ": vendor heartbeat\r\nid: 7\r\nevent: vendor.unknown\r\ndata: {\"opaque\":true}\r\ndata: second-line\r\n\r\n"
	phase3Terminal     = "event: message_stop\ndata: {\"type\":\"message_stop\",\"opaque\":\"terminal\"}\n\n"
)

func startPhase3CapstoneServer(t *testing.T, cfg config.ServeConfig, upstreamURL string, registry shim.Registry) string {
	t.Helper()
	base, _ := startPhase3CapstoneServerWithObservers(
		t, cfg, upstreamURL, registry, discardLogger(t), server.NewStreamOutcomeCounter(),
	)
	return base
}

func startPhase3CapstoneServerWithObservers(
	t *testing.T,
	cfg config.ServeConfig,
	upstreamURL string,
	registry shim.Registry,
	logger *slog.Logger,
	outcomes *server.StreamOutcomeCounter,
) (string, *forward.Forwarder) {
	t.Helper()
	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstreamURL,
		Token:   "stub-copilot-token",
		Headers: http.Header{
			"Copilot-Integration-Id": {"vscode-chat"},
			"Editor-Version":         {"vscode/1.104.1"},
		},
	}, true)
	previousDefault := slog.Default()
	slog.SetDefault(logger)
	forwarder := forward.New(
		provider,
		forward.NewClient(cfg.ResponseHeaderTimeout),
		cfg.OutboundTimeout,
		cfg.WriteTimeout,
		cfg.StreamIdleTimeout,
		cfg.StreamKeepaliveInterval,
		cfg.MaxRequestBytes,
		cfg.MaxBufferedResponseBytes,
		registry,
	)
	slog.SetDefault(previousDefault)
	return startTestServer(t, server.New(cfg, logger, provider, forwarder, outcomes)), forwarder
}

type phase3BufferedTranscript struct {
	requestBody  string
	status       int
	contentType  string
	proofHeader  string
	responseBody string
}

func runPhase3Buffered(t *testing.T, registry shim.Registry) phase3BufferedTranscript {
	t.Helper()
	requestBodies := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies <- string(body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Upstream-Proof", "phase-3-buffered")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, phase3BufferedBody)
	}))
	t.Cleanup(upstream.Close)

	cfg := e2eConfig("unused-static-provider-token")
	base := startPhase3CapstoneServer(t, cfg, upstream.URL, registry)
	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages?beta=verbatim", http.NoBody)
	if err != nil {
		t.Fatalf("build buffered request: %v", err)
	}
	req.Body = io.NopCloser(strings.NewReader(phase3RequestBody))
	req.ContentLength = int64(len(phase3RequestBody))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "phase3-buffered-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("buffered request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read buffered response: %v", err)
	}

	return phase3BufferedTranscript{
		requestBody:  <-requestBodies,
		status:       resp.StatusCode,
		contentType:  resp.Header.Get("Content-Type"),
		proofHeader:  resp.Header.Get("X-Upstream-Proof"),
		responseBody: string(body),
	}
}

type phase3StreamTranscript struct {
	requestBody  string
	status       int
	contentType  string
	firstRead    string
	responseBody string
}

func runPhase3Stream(t *testing.T, registry shim.Registry) phase3StreamTranscript {
	t.Helper()
	requestBodies := make(chan string, 1)
	releaseTerminal := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseTerminal) }) }
	t.Cleanup(release)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies <- string(body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, phase3FirstFrame)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-releaseTerminal:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, phase3Terminal)
		_ = http.NewResponseController(w).Flush()
	}))
	t.Cleanup(upstream.Close)

	cfg := e2eConfig("unused-static-provider-token")
	base := startPhase3CapstoneServer(t, cfg, upstream.URL, registry)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(`{"stream":true,"opaque":"request-bytes"}`))
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "phase3-stream-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request before terminal release: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, len(phase3FirstFrame))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first frame before terminal release: %v", err)
	}
	release()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed response: %v", err)
	}

	return phase3StreamTranscript{
		requestBody:  <-requestBodies,
		status:       resp.StatusCode,
		contentType:  resp.Header.Get("Content-Type"),
		firstRead:    string(first),
		responseBody: string(first) + string(rest),
	}
}

func TestPhase3EnabledNopMatchesEmptyChainBufferedEndToEnd(t *testing.T) {
	emptyCfg := e2eConfig("unused")
	emptyCfg.ShimNopEnabled = false
	enabledCfg := emptyCfg
	enabledCfg.ShimNopEnabled = true

	empty := runPhase3Buffered(t, configuredShimRegistry(emptyCfg))
	enabledNop := runPhase3Buffered(t, configuredShimRegistry(enabledCfg))
	if enabledNop != empty {
		t.Fatalf("enabled canonical NopShim transcript = %#v, want empty-chain transcript %#v", enabledNop, empty)
	}
	want := phase3BufferedTranscript{
		requestBody:  phase3RequestBody,
		status:       http.StatusCreated,
		contentType:  "application/json; charset=utf-8",
		proofHeader:  "phase-3-buffered",
		responseBody: phase3BufferedBody,
	}
	if enabledNop != want {
		t.Errorf("buffered passthrough transcript = %#v, want exact bytes %#v", enabledNop, want)
	}
}

func TestPhase3EnabledNopMatchesEmptyChainStreamingEndToEnd(t *testing.T) {
	emptyCfg := e2eConfig("unused")
	emptyCfg.ShimNopEnabled = false
	enabledCfg := emptyCfg
	enabledCfg.ShimNopEnabled = true

	empty := runPhase3Stream(t, configuredShimRegistry(emptyCfg))
	enabledNop := runPhase3Stream(t, configuredShimRegistry(enabledCfg))
	if enabledNop != empty {
		t.Fatalf("enabled canonical NopShim transcript = %#v, want empty-chain transcript %#v", enabledNop, empty)
	}
	want := phase3StreamTranscript{
		requestBody:  `{"stream":true,"opaque":"request-bytes"}`,
		status:       http.StatusOK,
		contentType:  "text/event-stream; charset=utf-8",
		firstRead:    phase3FirstFrame,
		responseBody: phase3FirstFrame + phase3Terminal,
	}
	if enabledNop != want {
		t.Errorf("stream passthrough transcript = %#v, want exact frame and flush fidelity %#v", enabledNop, want)
	}
}

type phase3HookCalls struct {
	constructed atomic.Int64
	request     atomic.Int64
	prelude     atomic.Int64
	buffered    atomic.Int64
	event       atomic.Int64
	finalize    atomic.Int64
}

type phase3IdentityShim struct{ calls *phase3HookCalls }

var (
	_ shim.RequestTransformer  = (*phase3IdentityShim)(nil)
	_ shim.PreludeTransformer  = (*phase3IdentityShim)(nil)
	_ shim.BufferedTransformer = (*phase3IdentityShim)(nil)
	_ shim.EventTransformer    = (*phase3IdentityShim)(nil)
	_ shim.StreamFinalizer     = (*phase3IdentityShim)(nil)
)

func (s *phase3IdentityShim) TransformRequest(context.Context, *shim.Request) error {
	s.calls.request.Add(1)
	return nil
}

func (s *phase3IdentityShim) TransformPrelude(context.Context, *shim.Prelude) error {
	s.calls.prelude.Add(1)
	return nil
}

func (s *phase3IdentityShim) TransformBuffered(context.Context, *shim.Body) error {
	s.calls.buffered.Add(1)
	return nil
}

func (s *phase3IdentityShim) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	s.calls.event.Add(1)
	return []sse.Frame{frame}, nil
}

func (s *phase3IdentityShim) Finalize(context.Context) ([]sse.Frame, error) {
	s.calls.finalize.Add(1)
	return nil, nil
}

func phase3IdentityRegistry(calls *phase3HookCalls) shim.Registry {
	return shim.Registry{{
		Name:    "phase3-identity-double",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			calls.constructed.Add(1)
			return &phase3IdentityShim{calls: calls}
		},
	}}
}

func TestPhase3IdentityDoublePreservesBufferedRequestAndResponseEndToEnd(t *testing.T) {
	calls := &phase3HookCalls{}
	got := runPhase3Buffered(t, phase3IdentityRegistry(calls))
	want := phase3BufferedTranscript{
		requestBody:  phase3RequestBody,
		status:       http.StatusCreated,
		contentType:  "application/json; charset=utf-8",
		proofHeader:  "phase-3-buffered",
		responseBody: phase3BufferedBody,
	}
	if got != want {
		t.Errorf("identity-shim buffered transcript = %#v, want byte-exact %#v", got, want)
	}
	if constructed, request, prelude, buffered, event, finalize :=
		calls.constructed.Load(), calls.request.Load(), calls.prelude.Load(), calls.buffered.Load(), calls.event.Load(), calls.finalize.Load(); constructed != 1 || request != 1 || prelude != 1 || buffered != 1 || event != 0 || finalize != 0 {
		t.Errorf("buffered hook dispatch = constructed:%d request:%d prelude:%d buffered:%d event:%d finalize:%d, want 1,1,1,1,0,0",
			constructed, request, prelude, buffered, event, finalize)
	}
}

func TestPhase3IdentityDoublePreservesStreamFramesAndFlushEndToEnd(t *testing.T) {
	calls := &phase3HookCalls{}
	got := runPhase3Stream(t, phase3IdentityRegistry(calls))
	want := phase3StreamTranscript{
		requestBody:  `{"stream":true,"opaque":"request-bytes"}`,
		status:       http.StatusOK,
		contentType:  "text/event-stream; charset=utf-8",
		firstRead:    phase3FirstFrame,
		responseBody: phase3FirstFrame + phase3Terminal,
	}
	if got != want {
		t.Errorf("identity-shim stream transcript = %#v, want exact frames and incremental first flush %#v", got, want)
	}
	if constructed, request, prelude, buffered, event, finalize :=
		calls.constructed.Load(), calls.request.Load(), calls.prelude.Load(), calls.buffered.Load(), calls.event.Load(), calls.finalize.Load(); constructed != 1 || request != 1 || prelude != 1 || buffered != 0 || event != 2 || finalize != 0 {
		t.Errorf("stream hook dispatch = constructed:%d request:%d prelude:%d buffered:%d event:%d finalize:%d, want 1,1,1,0,2,0",
			constructed, request, prelude, buffered, event, finalize)
	}
}

type phase3FailingEventShim struct{ err error }

var _ shim.EventTransformer = (*phase3FailingEventShim)(nil)

func (s *phase3FailingEventShim) TransformEvent(context.Context, sse.Frame) ([]sse.Frame, error) {
	return nil, s.err
}

func TestPhase3PostCommitShimFailureIsWarnedCountedAndRedactedEndToEnd(t *testing.T) {
	const (
		requestSecret = "phase3-private-request-body"
		frameSecret   = "phase3-private-frame-content"
		errorSecret   = "phase3-private-shim-error"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"secret\":\""+frameSecret+"\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	registry := shim.Registry{{
		Name:    "phase3-failing-event",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return &phase3FailingEventShim{err: errors.New(errorSecret)}
		},
	}}
	var logOutput bytes.Buffer
	logger, err := logging.NewWithWriter(&logOutput, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build capstone logger: %v", err)
	}
	outcomes := server.NewStreamOutcomeCounter()
	cfg := e2eConfig("unused-static-provider-token")
	base, _ := startPhase3CapstoneServerWithObservers(t, cfg, upstream.URL, registry, logger, outcomes)

	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(requestSecret))
	if err != nil {
		t.Fatalf("build post-commit failure request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "phase3-shim-error")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post-commit failure request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read post-commit failure response: %v", err)
	}

	const wantBody = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"copilotd: shim failed\"}}\n\n"
	if resp.StatusCode != http.StatusOK || string(body) != wantBody {
		t.Errorf("post-commit response = status %d body %q, want committed 200 and native shim terminal %q", resp.StatusCode, body, wantBody)
	}
	if got := outcomes.Count("anthropic", sse.OutcomeShimError); got != 1 {
		t.Errorf("shim_error outcome count = %d, want 1", got)
	}
	logs := logOutput.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "msg=access") || !strings.Contains(logs, "outcome=shim_error") {
		t.Errorf("post-commit access log missing warn shim_error metadata:\n%s", logs)
	}
	for _, secret := range []string{requestSecret, frameSecret, errorSecret, testAPIKey, "stub-copilot-token"} {
		if strings.Contains(logs, secret) {
			t.Errorf("post-commit logs leaked %q:\n%s", secret, logs)
		}
	}
}

type phase3PostTerminalFailingShim struct{ err error }

var _ shim.EventTransformer = (*phase3PostTerminalFailingShim)(nil)

func (s *phase3PostTerminalFailingShim) TransformEvent(_ context.Context, frame sse.Frame) ([]sse.Frame, error) {
	if frame.Type == "message_stop" {
		return []sse.Frame{frame}, nil
	}
	return nil, s.err
}

func TestPhase3SuppressedPostTerminalShimFailureWarnsCountsAndRedactsEndToEnd(t *testing.T) {
	const (
		requestSecret  = "phase3-private-suppressed-request"
		trailingSecret = "phase3-private-trailing-frame"
		errorSecret    = "phase3-private-suppressed-error"
		terminal       = "event: message_stop\ndata: {\"type\":\"message_stop\",\"opaque\":\"safe-wire-value\"}\n\n"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, terminal+"event: vendor.trailing\ndata: "+trailingSecret+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	registry := shim.Registry{{
		Name:    "phase3-post-terminal-failure",
		Enabled: true,
		New: func(context.Context, apierror.Surface, shim.Route) any {
			return &phase3PostTerminalFailingShim{err: errors.New(errorSecret)}
		},
	}}
	var logOutput bytes.Buffer
	logger, err := logging.NewWithWriter(&logOutput, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build capstone logger: %v", err)
	}
	outcomes := server.NewStreamOutcomeCounter()
	cfg := e2eConfig("unused-static-provider-token")
	base, forwarder := startPhase3CapstoneServerWithObservers(t, cfg, upstream.URL, registry, logger, outcomes)

	req, err := http.NewRequest(http.MethodPost, base+"/anthropic/v1/messages", strings.NewReader(requestSecret))
	if err != nil {
		t.Fatalf("build suppressed failure request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "phase3-suppressed-shim-error")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("suppressed failure request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read suppressed failure response: %v", err)
	}

	if resp.StatusCode != http.StatusOK || string(body) != terminal {
		t.Errorf("suppressed failure response = status %d body %q, want the single upstream terminal %q", resp.StatusCode, body, terminal)
	}
	if got := outcomes.Count("anthropic", sse.OutcomeClean); got != 1 {
		t.Errorf("clean outcome count after suppression = %d, want 1", got)
	}
	if got := forwarder.SuppressedShimErrorCount(); got != 1 {
		t.Errorf("dedicated suppressed-shim-error count = %d, want 1", got)
	}
	logs := logOutput.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "suppressed post-terminal shim error") || !strings.Contains(logs, "stage=transform") {
		t.Errorf("suppression warning missing metadata:\n%s", logs)
	}
	if !strings.Contains(logs, "msg=access") || !strings.Contains(logs, "outcome=clean") {
		t.Errorf("suppressed failure access line missing clean wire outcome:\n%s", logs)
	}
	for _, secret := range []string{requestSecret, trailingSecret, errorSecret, testAPIKey, "stub-copilot-token"} {
		if strings.Contains(logs, secret) {
			t.Errorf("suppression logs leaked %q:\n%s", secret, logs)
		}
	}
}
