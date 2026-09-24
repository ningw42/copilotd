// Package server assembles copilotd's HTTP surface: the router, the health
// endpoint, the correlation/resilience middleware chain, and the graceful
// lifecycle. main injects a bound net.Listener so the server can be driven
// end to end against an ephemeral port in tests.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/wsforward"
)

// Inbound HTTP timeouts (client <-> copilotd), distinct from the Phase-1
// outbound client. All four are named constants so the two deliberate zeros
// read as intentional, not forgotten.
const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second

	// Deliberately unbounded (0): a blunt global cap fights large LLM uploads
	// (long histories, base64 images) and long SSE responses. Real per-request
	// bounding is introduced in later phases.
	readTimeout  = 0 * time.Second
	writeTimeout = 0 * time.Second
)

// ErrForcedDrain marks a Run result whose only failure is that the configured
// shutdown grace period expired before every HTTP request and WebSocket
// session drained, so the survivors were force-closed. It always wraps the
// underlying drain causes, including context.DeadlineExceeded.
var ErrForcedDrain = errors.New("shutdown grace period expired; remaining connections force-closed")

// Server owns the configured http.Server and drives its lifecycle.
type Server struct {
	cfg    config.ServeConfig
	logger *slog.Logger
	http   httpLifecycle
	ws     websocketDrainer
}

type httpLifecycle interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

type websocketDrainer interface {
	StartDrain()
	Shutdown(context.Context) error
}

// New builds the server from cfg and logger. The identity Provider supplies the
// outbound Copilot credential and local readiness, observers supply non-secret
// readiness details, fwd drives forwarding endpoints, source supplies bounded
// Catalog bytes, and streamOutcomes receives the bounded stream terminal-outcome
// metric. reportHandler is the explicit local Usage reporting handler (including
// its disabled variant), independent of inference dependencies. The listener is
// supplied later to Run, so main owns bind and the
// server owns serve/shutdown.
// Invariant: catalog settings cross the render seam only through catalogs, never through cfg.
func New(cfg config.ServeConfig, logger, catalogLogger *slog.Logger, dependencyErrorLog *log.Logger, provider identity.Provider, observers ReadyObservers, fwd *forward.Forwarder, source catalog.Source, wsProxy *wsforward.Proxy, streamOutcomes StreamOutcomeObserver, catalogs catalog.RenderDescriptors, reportHandler http.Handler) *Server {
	httpServer := &http.Server{
		Handler:           newHandler(cfg.APIKey, provider, observers, fwd, source, logger, catalogLogger, streamOutcomes, catalogs, wsProxy, reportHandler),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		// Bridge the server's dependency-owned errors into the shared format and
		// destination without falsely attributing them to internal/server.
		ErrorLog: dependencyErrorLog,
	}
	return &Server{
		cfg:    cfg,
		logger: logger,
		ws:     wsProxy,
		http:   httpServer,
	}
}

// Run serves on ln until ctx is cancelled, then drains within the configured
// shutdown timeout. Cancellation closes WebSocket admission and drains HTTP
// requests and WebSocket sessions concurrently under one shared grace
// deadline; if either drain fails, the remaining connections are hard-closed.
//
// A clean drain returns nil; http.ErrServerClosed is not treated as an error.
// When the grace period expired and every drain failure was that deadline,
// Run returns an error matching ErrForcedDrain (and context.DeadlineExceeded)
// so callers can distinguish a forced drain from a clean one. Any genuine
// drain or serve failure, including one combined with a timeout, is returned
// as an ordinary error without ErrForcedDrain.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		s.logger.InfoContext(ctx, "listening", slog.String(logging.AddrKey, ln.Addr().String()))
		serveErr <- s.http.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		return s.shutdown()
	}
}

func (s *Server) shutdown() error {
	s.logger.Info("shutting down", slog.Duration(logging.TimeoutKey, s.cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	// Close WebSocket admission first so late upgrades are refused, then drain
	// both transports at once: neither may consume the other's grace period.
	s.ws.StartDrain()
	var (
		wg             sync.WaitGroup
		httpErr, wsErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		httpErr = s.http.Shutdown(shutdownCtx)
	}()
	go func() {
		defer wg.Done()
		wsErr = s.ws.Shutdown(shutdownCtx)
	}()
	wg.Wait()
	if httpErr == nil && wsErr == nil {
		return nil
	}
	// Graceful drain failed; force the remaining connections closed.
	_ = s.http.Close()
	joined := errors.Join(httpErr, wsErr)
	// Classify before the deferred cancel runs: only an actually expired grace
	// deadline with deadline-only drain failures is a forced drain.
	if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) && deadlineOnly(httpErr) && deadlineOnly(wsErr) {
		return fmt.Errorf("graceful shutdown: %w: %w", ErrForcedDrain, joined)
	}
	return fmt.Errorf("graceful shutdown: %w", joined)
}

// deadlineOnly reports whether one drain result is either success or the
// shutdown deadline, classified per drain before the results are joined.
func deadlineOnly(err error) bool {
	return err == nil || errors.Is(err, context.DeadlineExceeded)
}
