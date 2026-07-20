// Package wsforward forwards OpenAI Responses WebSocket messages opaquely
// between a client and GitHub Copilot.
package wsforward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

const requestIDHeader = "X-Request-Id"

// Proxy owns the upstream dial and both message pumps for Responses WebSocket
// sessions.
type Proxy struct {
	provider        identity.Provider
	dialClient      *http.Client
	dialTimeout     time.Duration
	writeTimeout    time.Duration
	maxMessageBytes int64
	logger          *slog.Logger
	metrics         WsMetrics

	baseCtx     context.Context
	cancel      context.CancelFunc
	drainCtx    context.Context
	cancelDrain context.CancelFunc
	wg          sync.WaitGroup
	draining    atomic.Bool

	sessionsMu sync.Mutex
	sessions   map[*activeSession]struct{}
}

type activeSession struct {
	client   *websocket.Conn
	upstream *websocket.Conn
}

func (s *activeSession) forceClose() {
	// CloseNow is an explicit backstop, but do not make an already-expired
	// shutdown caller wait on the library's internal close serialization.
	go func() { _ = s.client.CloseNow() }()
	go func() { _ = s.upstream.CloseNow() }()
}

// New returns a WebSocket Proxy with an independently cancellable session
// context. dialClient must not impose a total client timeout.
func New(provider identity.Provider, dialClient *http.Client, dialTimeout, writeTimeout time.Duration, maxMessageBytes int64, logger *slog.Logger, metrics WsMetrics) *Proxy {
	baseCtx, cancel := context.WithCancel(context.Background())
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	return &Proxy{
		provider:        provider,
		dialClient:      dialClient,
		dialTimeout:     dialTimeout,
		writeTimeout:    writeTimeout,
		maxMessageBytes: maxMessageBytes,
		logger:          logger,
		metrics:         metrics,
		baseCtx:         baseCtx,
		cancel:          cancel,
		drainCtx:        drainCtx,
		cancelDrain:     cancelDrain,
		sessions:        make(map[*activeSession]struct{}),
	}
}

// Handler returns the WebSocket forwarding handler for one endpoint contract.
func (p *Proxy) Handler(ep endpoint.WSForward) http.HandlerFunc {
	surface := ep.Surface()
	upstream := ep.Upstream()
	return func(w http.ResponseWriter, r *http.Request) {
		handshakeStart := time.Now()
		p.wg.Add(1)
		defer p.wg.Done()
		phaseCtx, cancelPhase := context.WithCancel(r.Context())
		stopForceCancel := context.AfterFunc(p.baseCtx, cancelPhase)
		defer stopForceCancel()
		defer cancelPhase()
		requestID, _ := logging.RequestIDFrom(r.Context())
		if p.draining.Load() {
			apierror.Write(w, surface, apierror.NotReady, "the server is shutting down")
			p.metrics.observeAccept(AcceptRejected)
			return
		}

		if !isWebSocketUpgrade(r) {
			apierror.Write(w, surface, apierror.NotAWebSocketUpgrade, "request is not a WebSocket upgrade")
			p.metrics.observeAccept(AcceptRejected)
			return
		}

		cred, err := p.provider.Current(phaseCtx)
		if err != nil {
			apierror.Write(w, surface, apierror.NotReady, "no upstream credential available")
			p.metrics.observeAccept(AcceptRejected)
			return
		}

		upstreamURL, err := websocketURL(cred.BaseURL, upstream, r.URL.RawQuery, r.URL.ForceQuery)
		if err != nil {
			apierror.Write(w, surface, apierror.BadGateway, "could not build the upstream WebSocket URL")
			p.metrics.observeAccept(AcceptDialFailed)
			return
		}
		dialCtx, cancelDial := context.WithTimeout(phaseCtx, p.dialTimeout)
		upstream, response, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
			HTTPClient:      p.dialClient,
			HTTPHeader:      upstreamHeaders(cred, r.Context()),
			CompressionMode: websocket.CompressionDisabled,
		})
		cancelDial()
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(dialCtx.Err(), context.DeadlineExceeded) {
				apierror.Write(w, surface, apierror.GatewayTimeout, "the upstream WebSocket handshake timed out")
				p.metrics.observeAccept(AcceptDialFailed)
				return
			}
			apierror.Write(w, surface, apierror.BadGateway, "could not reach the upstream WebSocket")
			p.metrics.observeAccept(AcceptDialFailed)
			return
		}
		p.logUpstreamRequestID(r.Context(), response.Header)
		defer func() { _ = upstream.CloseNow() }()

		client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			p.metrics.observeAccept(AcceptRejected)
			return
		}
		defer func() { _ = client.CloseNow() }()
		sessionStart := time.Now()
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		p.logger.LogAttrs(r.Context(), slog.LevelInfo, "websocket established",
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", http.StatusSwitchingProtocols),
			slog.Int64("bytes", 0),
			slog.Bool("ws", true),
			slog.Duration("duration", time.Since(handshakeStart)),
		)
		p.metrics.observeAccept(AcceptEstablished)

		session := &activeSession{client: client, upstream: upstream}
		p.trackSession(session)
		defer p.untrackSession(session)

		result := runSession(p.drainCtx, p.baseCtx, client, upstream, p.writeTimeout, p.maxMessageBytes)
		p.logSession(requestID, time.Since(sessionStart), result)
		p.metrics.observeSessionTerminal(result.terminal)
	}
}

func (p *Proxy) logSession(requestID string, duration time.Duration, result sessionResult) {
	level := slog.LevelInfo
	if result.terminal == SessionError {
		level = slog.LevelWarn
	}
	attrs := make([]slog.Attr, 0, 9)
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	attrs = append(attrs,
		slog.Int64("msgs_c2u", result.messagesClientToUpstream),
		slog.Int64("msgs_u2c", result.messagesUpstreamToClient),
		slog.Int64("bytes_c2u", result.bytesClientToUpstream),
		slog.Int64("bytes_u2c", result.bytesUpstreamToClient),
		slog.Int("close_code", int(result.closeCode)),
		slog.String("terminal_reason", string(result.terminal)),
		slog.Duration("duration", duration),
	)
	p.logger.LogAttrs(context.Background(), level, "websocket session", attrs...)
}

func (p *Proxy) trackSession(session *activeSession) {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	p.sessions[session] = struct{}{}
}

func (p *Proxy) untrackSession(session *activeSession) {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	delete(p.sessions, session)
}

func (p *Proxy) forceCloseSessions() {
	p.sessionsMu.Lock()
	sessions := make([]*activeSession, 0, len(p.sessions))
	for session := range p.sessions {
		sessions = append(sessions, session)
	}
	p.sessionsMu.Unlock()

	for _, session := range sessions {
		session.forceClose()
	}
}

func websocketURL(baseURL string, upstream endpoint.Route, rawQuery string, forceQuery bool) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse upstream base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported upstream URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + string(upstream)
	u.RawPath = ""
	u.RawQuery = rawQuery
	u.ForceQuery = forceQuery
	return u.String(), nil
}

func upstreamHeaders(cred identity.Credential, ctx context.Context) http.Header {
	header := make(http.Header, len(cred.Headers)+2)
	for name, values := range cred.Headers {
		if isHandshakeHeader(name) {
			continue
		}
		for _, value := range values {
			header.Add(name, value)
		}
	}
	header.Set("Authorization", "Bearer "+cred.Token)
	if requestID, ok := logging.RequestIDFrom(ctx); ok {
		header.Set(requestIDHeader, requestID)
	}
	return header
}

func isHandshakeHeader(name string) bool {
	return strings.EqualFold(name, "Connection") ||
		strings.EqualFold(name, "Upgrade") ||
		strings.HasPrefix(strings.ToLower(name), "sec-websocket-")
}

// StartDrain makes subsequent upgrade attempts fail before any upstream work.
// It is separate from Shutdown so the HTTP server can refuse upgrades before
// it begins draining non-hijacked requests.
func (p *Proxy) StartDrain() {
	p.draining.Store(true)
}

// Shutdown starts draining, asks live sessions to close with 1001, and waits
// for every registered handler until ctx expires. Established survivors are
// force-closed when the caller's deadline wins.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.StartDrain()
	p.cancelDrain()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.cancel()
		return nil
	case <-ctx.Done():
		// Cancelling the base context is the single force authority for every
		// post-upgrade pump and every pre-upgrade phase context.
		p.cancel()
		p.forceCloseSessions()
		return ctx.Err()
	}
}
