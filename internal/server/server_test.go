package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
)

const testAPIKey = "test-api-key"

func testConfig() config.ServeConfig {
	return config.ServeConfig{
		Addr:            "127.0.0.1:0",
		LogLevel:        "info",
		LogFormat:       "text",
		ShutdownTimeout: 2 * time.Second,
		APIKey:          testAPIKey,
		OutboundTimeout: 5 * time.Second,
		MaxRequestBytes: 1 << 20,
	}
}

// readyStub returns a ready Static provider carrying baseURL (empty for the
// health/middleware tests that never reach the forwarder).
func readyStub(baseURL string) *identity.Static {
	return identity.NewStatic(identity.Credential{
		BaseURL: baseURL,
		Token:   "copilot-token",
		Headers: http.Header{"Copilot-Integration-Id": {"vscode-chat"}},
	}, true)
}

// testHandler builds the assembled handler with a ready stub provider and a
// forwarder, for the health/correlation/access-log tests that never exercise a
// provider route.
func testHandler(t *testing.T, logger *slog.Logger) http.Handler {
	t.Helper()
	prov := readyStub("")
	fwd := forward.New(prov, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
	return newHandler(testAPIKey, prov, fwd, logger, NewStreamOutcomeCounter())
}

// bufferLogger returns a logger writing to an in-memory buffer at the given
// level, with request-id injection intact (via logging.NewWithWriter).
func bufferLogger(t *testing.T, level string) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.NewWithWriter(&buf, config.ServeConfig{LogLevel: level, LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	return logger, &buf
}

func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	logger, err := logging.NewWithWriter(io.Discard, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	return logger
}

func TestHealthGET(t *testing.T) {
	h := testHandler(t, discardLogger(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := rec.Body.String(); body != `{"status":"ok"}` {
		t.Errorf("body = %q, want {\"status\":\"ok\"}", body)
	}
	// Liveness only: must not leak build version onto the unauthenticated route.
	if strings.Contains(rec.Body.String(), "dev") || strings.Contains(rec.Body.String(), "version") {
		t.Errorf("healthz body must not expose version: %q", rec.Body.String())
	}
}

func TestHealthHEAD(t *testing.T) {
	h := testHandler(t, discardLogger(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must not write a body, got %q", rec.Body.String())
	}
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	h := testHandler(t, discardLogger(t))

	t.Run("generated when absent", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		id := rec.Header().Get("X-Request-Id")
		if !logging.ValidRequestID(id) {
			t.Errorf("generated X-Request-Id %q is not well-formed", id)
		}
	})

	t.Run("well-formed inbound honored", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", "client-abc.123")
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("X-Request-Id"); got != "client-abc.123" {
			t.Errorf("X-Request-Id = %q, want the inbound value honored", got)
		}
	})

	t.Run("malformed inbound regenerated, never rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", "bad id with spaces")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("a malformed request-id must not fail the request; status = %d", rec.Code)
		}
		got := rec.Header().Get("X-Request-Id")
		if got == "bad id with spaces" {
			t.Errorf("malformed request-id should have been regenerated")
		}
		if !logging.ValidRequestID(got) {
			t.Errorf("regenerated X-Request-Id %q is not well-formed", got)
		}
	})
}

func TestAccessLogHealthzAtDebug(t *testing.T) {
	t.Run("emitted once at debug with route template and fields", func(t *testing.T) {
		logger, buf := bufferLogger(t, "debug")
		h := testHandler(t, logger)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", "rid-access")
		h.ServeHTTP(rec, req)

		out := buf.String()
		if n := strings.Count(out, "msg=access"); n != 1 {
			t.Fatalf("want exactly one access line, got %d:\n%s", n, out)
		}
		for _, want := range []string{
			"level=DEBUG",
			"method=GET",
			`route="GET /healthz"`,
			"status=200",
			"bytes=",
			"duration=",
			"request_id=rid-access",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("access line missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("silent at info so health polling does not flood logs", func(t *testing.T) {
		logger, buf := bufferLogger(t, "info")
		h := testHandler(t, logger)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if strings.Contains(buf.String(), "msg=access") {
			t.Errorf("/healthz should log at debug, not info:\n%s", buf.String())
		}
	})
}

func TestAccessLogUnmatchedRoute(t *testing.T) {
	logger, buf := bufferLogger(t, "info")
	h := testHandler(t, logger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	out := buf.String()
	if n := strings.Count(out, "msg=access"); n != 1 {
		t.Fatalf("want exactly one access line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "route=unmatched") {
		t.Errorf("unmatched route should be labeled 'unmatched':\n%s", out)
	}
	if !strings.Contains(out, "status=404") {
		t.Errorf("access line missing status=404:\n%s", out)
	}
	for _, streamOnly := range []string{"outcome=", "frames="} {
		if strings.Contains(out, streamOnly) {
			t.Errorf("non-stream Phase 1 access line unexpectedly contains %q:\n%s", streamOnly, out)
		}
	}
}

func TestAccessLogEnrichesStreamSummary(t *testing.T) {
	logger, buf := bufferLogger(t, "info")
	metrics := NewStreamOutcomeCounter()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forward.StoreStreamResult(r.Context(), forward.StreamResult{
			Surface:   "anthropic",
			Outcome:   sse.OutcomeClean,
			Frames:    2,
			Fallbacks: 1,
		})
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "stream bytes")
	})
	h := accessLog(logger, metrics, inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil))

	out := buf.String()
	if n := strings.Count(out, "msg=access"); n != 1 {
		t.Fatalf("want exactly one access line, got %d:\n%s", n, out)
	}
	for _, want := range []string{
		"level=INFO",
		"status=202",
		"bytes=12",
		"duration=",
		"outcome=clean",
		"frames=2",
		"fallbacks=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stream access line missing %q:\n%s", want, out)
		}
	}
}

func TestAccessLogObservesEveryStreamOutcomeBySurface(t *testing.T) {
	metrics := NewStreamOutcomeCounter()
	outcomes := []sse.Outcome{
		sse.OutcomeClean,
		sse.OutcomeSynthesized,
		sse.OutcomeStall,
		sse.OutcomeClientCancel,
		sse.OutcomeUpstreamError,
		sse.OutcomeShimError,
	}
	for _, surface := range []string{"anthropic", "openai"} {
		for _, outcome := range outcomes {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forward.StoreStreamResult(r.Context(), forward.StreamResult{
					Surface: surface,
					Outcome: outcome,
				})
			})
			h := accessLog(discardLogger(t), metrics, inner)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/stream", nil))
		}
	}

	for _, surface := range []string{"anthropic", "openai"} {
		for _, outcome := range outcomes {
			if got := metrics.Count(surface, outcome); got != 1 {
				t.Errorf("metric count for surface=%q outcome=%q = %d, want 1", surface, outcome, got)
			}
		}
	}
}

func TestStreamOutcomeCounterIsBoundedAndConcurrent(t *testing.T) {
	metrics := NewStreamOutcomeCounter()
	metrics.ObserveStreamOutcome("provider-github", sse.OutcomeClean)
	metrics.ObserveStreamOutcome("anthropic", sse.Outcome("user-derived"))

	const workers = 64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			metrics.ObserveStreamOutcome("openai", sse.OutcomeStall)
			_ = metrics.Count("openai", sse.OutcomeStall)
		}()
	}
	wg.Wait()

	if got := metrics.Count("openai", sse.OutcomeStall); got != workers {
		t.Errorf("concurrent metric count = %d, want %d", got, workers)
	}
	if got := metrics.Count("anthropic", sse.OutcomeClean); got != 0 {
		t.Errorf("unknown surface changed canonical count to %d, want 0", got)
	}
	if got := metrics.Count("openai", sse.OutcomeClean); got != 0 {
		t.Errorf("unknown surface changed canonical count to %d, want 0", got)
	}
}

func TestAccessLogUsesOutcomeSeverity(t *testing.T) {
	tests := []struct {
		outcome sse.Outcome
		level   string
	}{
		{outcome: sse.OutcomeClean, level: "INFO"},
		{outcome: sse.OutcomeClientCancel, level: "INFO"},
		{outcome: sse.OutcomeSynthesized, level: "WARN"},
		{outcome: sse.OutcomeStall, level: "WARN"},
		{outcome: sse.OutcomeUpstreamError, level: "WARN"},
		{outcome: sse.OutcomeShimError, level: "WARN"},
	}
	for _, tt := range tests {
		t.Run(string(tt.outcome), func(t *testing.T) {
			logger, buf := bufferLogger(t, "info")
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forward.StoreStreamResult(r.Context(), forward.StreamResult{
					Surface: "anthropic",
					Outcome: tt.outcome,
				})
			})
			h := accessLog(logger, NewStreamOutcomeCounter(), inner)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/stream", nil))

			out := buf.String()
			if !strings.Contains(out, "level="+tt.level) {
				t.Errorf("access level for outcome %q = unexpected:\n%s", tt.outcome, out)
			}
			if !strings.Contains(out, "outcome="+string(tt.outcome)) {
				t.Errorf("access line missing exact outcome %q:\n%s", tt.outcome, out)
			}
		})
	}
}

func TestAccessLogDoesNotLogStreamBodiesOrSecrets(t *testing.T) {
	logger, buf := bufferLogger(t, "info")
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forward.StoreStreamResult(r.Context(), forward.StreamResult{
			Surface: "anthropic",
			Outcome: sse.OutcomeShimError,
			Frames:  1,
		})
		_, _ = io.WriteString(w, "private-frame-body")
	})
	h := accessLog(logger, NewStreamOutcomeCounter(), inner)
	req := httptest.NewRequest(http.MethodPost, "/stream", strings.NewReader("private-request-body"))
	req.Header.Set("Authorization", "Bearer private-api-key")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, forbidden := range []string{"private-frame-body", "private-request-body", "private-api-key"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("access line leaked %q:\n%s", forbidden, out)
		}
	}
}

// A panicking handler must yield a generic 500 with no stack leak, the panic
// must be logged with its request-id, and the access line must record the 500 —
// which together prove the order request-id -> access-log -> recover.
func TestPanicRecoveryAndMiddlewareOrder(t *testing.T) {
	logger, buf := bufferLogger(t, "info")
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom secret internals")
	})
	h := requestID(accessLog(logger, NewStreamOutcomeCounter(), recoverMW(logger, panicky)))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/explode", nil)
	req.Header.Set("X-Request-Id", "rid-panic")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); body != "internal server error" {
		t.Errorf("body = %q, want generic message", body)
	}
	// No stack trace / panic detail leaked to the client.
	if strings.Contains(rec.Body.String(), "boom") || strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("response leaked internals: %q", rec.Body.String())
	}
	// Outermost RequestID still echoed the id even though the handler panicked.
	if got := rec.Header().Get("X-Request-Id"); got != "rid-panic" {
		t.Errorf("X-Request-Id = %q, want rid-panic echoed on panic", got)
	}

	out := buf.String()
	if !strings.Contains(out, "request_id=rid-panic") {
		t.Errorf("panic log missing request_id:\n%s", out)
	}
	// Recover is innermost, so AccessLog records the resulting 500.
	if !strings.Contains(out, "status=500") {
		t.Errorf("access log should record the recovered 500:\n%s", out)
	}
}

func TestLifecycleSmoke(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(testConfig(), discardLogger(t), readyStub(""), forward.New(readyStub(""), forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil), NewStreamOutcomeCounter())

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx, ln) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	resp, err := getWithRetry(t, url)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run returned %v, want clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within the grace period after cancel")
	}
}

func getWithRetry(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	var lastErr error
	for range 50 {
		resp, err := http.Get(url) //nolint:noctx // test helper
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return nil, lastErr
}
