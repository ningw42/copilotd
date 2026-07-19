// Package server assembles copilotd's HTTP surface: the router, the health
// endpoint, the correlation/resilience middleware chain, and the graceful
// lifecycle. main injects a bound net.Listener so the server can be driven
// end to end against an ephemeral port in tests.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
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
// outbound Copilot credential (and readiness), fwd drives the provider routes,
// and streamOutcomes receives the bounded stream terminal-outcome metric. The
// listener is supplied later to Run, so main owns bind and the server owns
// serve/shutdown.
func New(cfg config.ServeConfig, logger *slog.Logger, provider identity.Provider, fwd *forward.Forwarder, wsProxy *wsforward.Proxy, streamOutcomes StreamOutcomeObserver) *Server {
	var ws websocketDrainer
	if wsProxy != nil {
		ws = wsProxy
	}
	httpServer := &http.Server{
		Handler:           newHandler(cfg.APIKey, provider, fwd, logger, streamOutcomes, cfg.Codex, wsProxy),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		// Bridge the server's internal errors into the structured logger so
		// all output shares one format and destination.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return &Server{
		cfg:    cfg,
		logger: logger,
		ws:     ws,
		http:   httpServer,
	}
}

// Run serves on ln until ctx is cancelled, then shuts down gracefully within
// the configured timeout, falling back to a hard close if that overruns. A
// clean shutdown returns nil; http.ErrServerClosed is not treated as an error.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		s.logger.InfoContext(ctx, "listening", slog.String("addr", ln.Addr().String()))
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
	s.logger.Info("shutting down", slog.Duration("timeout", s.cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if s.ws != nil {
		s.ws.StartDrain()
	}
	httpErr := s.http.Shutdown(shutdownCtx)
	var wsErr error
	if s.ws != nil {
		wsErr = s.ws.Shutdown(shutdownCtx)
	}
	if err := errors.Join(httpErr, wsErr); err != nil {
		// Graceful shutdown overran; force the remaining connections closed.
		_ = s.http.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}
