package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
)

func TestServeOutcomeMapsOnlyCleanAndForcedDrainToSuccess(t *testing.T) {
	genuine := errors.New("listener close failed")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "clean drain", err: nil, want: nil},
		{name: "forced drain", err: fmt.Errorf("graceful shutdown: %w: %w", server.ErrForcedDrain, context.DeadlineExceeded), want: nil},
		{name: "genuine error", err: fmt.Errorf("graceful shutdown: %w", genuine), want: errServeFailed},
		{name: "genuine error mixed with deadline", err: fmt.Errorf("graceful shutdown: %w", errors.Join(genuine, context.DeadlineExceeded)), want: errServeFailed},
		{name: "unmarked generic deadline", err: fmt.Errorf("serve: %w", context.DeadlineExceeded), want: errServeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveOutcome(tt.err); got != tt.want {
				t.Fatalf("serveOutcome(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// capturedRecord is one emitted slog record flattened with its logger-attached
// attributes, so assertions can use structured level, component, and attrs.
type capturedRecord struct {
	level   slog.Level
	message string
	attrs   map[string]slog.Value
}

type recordSink struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (s *recordSink) snapshot() []capturedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRecord(nil), s.records...)
}

type capturingHandler struct {
	sink  *recordSink
	attrs []slog.Attr
}

func (capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, record slog.Record) error {
	captured := capturedRecord{level: record.Level, message: record.Message, attrs: make(map[string]slog.Value)}
	for _, attr := range h.attrs {
		captured.attrs[attr.Key] = attr.Value
	}
	record.Attrs(func(attr slog.Attr) bool {
		captured.attrs[attr.Key] = attr.Value
		return true
	})
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	h.sink.records = append(h.sink.records, captured)
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

// startCapturedBoundServe runs the production runBoundServe against upstream
// with a capturing logger and returns its raw result channel.
func startCapturedBoundServe(t *testing.T, upstreamURL string, shutdownTimeout time.Duration, ln net.Listener) (context.CancelFunc, <-chan error, *recordSink) {
	t.Helper()
	sink := &recordSink{}
	logger := slog.New(capturingHandler{sink: sink})
	cfg := e2eConfig("gho-serve-outcome")
	cfg.ImpersonationRefreshInterval = 0
	cfg.ShutdownTimeout = shutdownTimeout
	var exchangeAuth, exchangeUA string
	github := newGitHubExchangeStub(t, "copilot-serve-outcome", upstreamURL, &exchangeAuth, &exchangeUA)
	cacheRegistry := cache.NewRegistry()
	mgr, imp, err := buildServeProvider(cfg, logger, github.URL, github.Client(), productionDiscoveryEdge(), cacheRegistry)
	if err != nil {
		t.Fatalf("buildServeProvider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- runBoundServe(ctx, cfg, logger, mgr, imp, nil, nil, cacheRegistry, configuredShimRegistry(cfg, nil), ln, nil)
	}()
	return cancel, done, sink
}

func awaitServeResult(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("runBoundServe did not return")
		return nil
	}
}

func TestRunBoundServeLogsForcedDrainOnceAtWarnAndReturnsSentinel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	const shutdownTimeout = 150 * time.Millisecond
	cancel, done, sink := startCapturedBoundServe(t, upstream.URL, shutdownTimeout, ln)
	base := "http://" + ln.Addr().String()
	assertHTTPStatusEventually(t, base+"/healthz", http.StatusOK)

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

	cancel()
	serveErr := awaitServeResult(t, done, 5*time.Second)
	if !errors.Is(serveErr, server.ErrForcedDrain) || !errors.Is(serveErr, context.DeadlineExceeded) {
		t.Fatalf("runBoundServe = %v, want raw ErrForcedDrain result wrapping deadline", serveErr)
	}
	if got := serveOutcome(serveErr); got != nil {
		t.Errorf("serveOutcome(forced drain) = %v, want nil", got)
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

func TestRunBoundServeLogsGenuineServeFailuresAtError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "genuine failure", err: errors.New("accept exploded")},
		// Wrapped so net/http does not treat it as a temporary net.Error retry.
		{name: "unrelated generic deadline", err: fmt.Errorf("accept: %w", context.DeadlineExceeded)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			_, done, sink := startCapturedBoundServe(t, "http://upstream.invalid", time.Second, failingListener{Listener: ln, err: tt.err})
			serveErr := awaitServeResult(t, done, 5*time.Second)
			if !errors.Is(serveErr, tt.err) || errors.Is(serveErr, server.ErrForcedDrain) {
				t.Fatalf("runBoundServe = %v, want ordinary error wrapping %v", serveErr, tt.err)
			}
			if got := serveOutcome(serveErr); got != errServeFailed {
				t.Errorf("serveOutcome = %v, want errServeFailed", got)
			}
			records := sink.snapshot()
			if got := cmdRecordsAt(records, slog.LevelError); len(got) != 1 {
				t.Errorf("cmd/copilotd ERROR records = %+v, want one server error", got)
			}
			if got := cmdRecordsAt(records, slog.LevelWarn); len(got) != 0 {
				t.Errorf("cmd/copilotd WARN records = %+v, want no forced-drain warning", got)
			}
		})
	}
}
