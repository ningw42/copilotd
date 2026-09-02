// Package wsforward forwards OpenAI Responses WebSocket messages opaquely
// between a client and GitHub Copilot.
package wsforward

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/upstream"
)

// Proxy owns the upstream dial and both message pumps for Responses WebSocket
// sessions.
type Proxy struct {
	caller                   *upstream.Caller
	dialClient               *http.Client
	dialTimeout              time.Duration
	writeTimeout             time.Duration
	maxMessageBytes          int64
	logger                   *slog.Logger
	shimLogger               *slog.Logger
	shimHookOverrunThreshold time.Duration
	shimMonitorOptions       []shim.MonitorOption
	shimMonitor              *shim.Monitor
	metrics                  WsMetrics
	registry                 shim.Registry

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
	client            *websocket.Conn
	upstream          *websocket.Conn
	clientTransport   net.Conn
	upstreamTransport net.Conn
}

func (s *activeSession) forceClose() {
	forceCloseConnection(s.client, s.clientTransport)
	forceCloseConnection(s.upstream, s.upstreamTransport)
}

func forceCloseConnection(conn *websocket.Conn, transport net.Conn) {
	// coder/websocket serializes CloseNow behind an in-progress graceful Close,
	// so only the upgraded transport can interrupt a stuck close handshake.
	if transport != nil {
		go func() { _ = transport.Close() }()
		return
	}
	go func() { _ = conn.CloseNow() }()
}

func rawNetworkConn(conn net.Conn) net.Conn {
	// tls.Conn.Close may spend five seconds writing close_notify. Its bottom
	// NetConn is the force boundary; repeated unwrapping also covers TLS through
	// a TLS proxy.
	for {
		wrapper, ok := conn.(interface{ NetConn() net.Conn })
		if !ok {
			return conn
		}
		raw := wrapper.NetConn()
		if raw == nil {
			return conn
		}
		conn = raw
	}
}

type capturingResponseWriter struct {
	http.ResponseWriter
	transport net.Conn
}

func (w *capturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.transport = conn
	}
	return conn, readWriter, err
}

// Option configures an optional Proxy dependency.
type Option func(*Proxy)

// WithShimMonitorClock replaces only the Hook overrun monitor's clock. It is a
// deterministic transport-test seam; production uses shim's monotonic clock.
func WithShimMonitorClock(clock shim.Clock) Option {
	return func(p *Proxy) {
		p.shimMonitorOptions = append(p.shimMonitorOptions, shim.WithClock(clock))
	}
}

// New returns a WebSocket Proxy with an independently cancellable session
// context. dialClient must not impose a total client timeout.
func New(caller *upstream.Caller, dialClient *http.Client, dialTimeout, writeTimeout time.Duration, maxMessageBytes int64, registry shim.Registry, logger, shimLogger *slog.Logger, hookOverrunThreshold time.Duration, metrics WsMetrics, options ...Option) *Proxy {
	baseCtx, cancel := context.WithCancel(context.Background())
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	proxy := &Proxy{
		caller:                   caller,
		dialClient:               dialClient,
		dialTimeout:              dialTimeout,
		writeTimeout:             writeTimeout,
		maxMessageBytes:          maxMessageBytes,
		logger:                   logger,
		shimLogger:               shimLogger,
		shimHookOverrunThreshold: hookOverrunThreshold,
		metrics:                  metrics,
		registry:                 append(shim.Registry(nil), registry...),
		baseCtx:                  baseCtx,
		cancel:                   cancel,
		drainCtx:                 drainCtx,
		cancelDrain:              cancelDrain,
		sessions:                 make(map[*activeSession]struct{}),
	}
	for _, configure := range options {
		configure(proxy)
	}
	proxy.shimMonitor = shim.NewMonitor(proxy.shimLogger, proxy.shimHookOverrunThreshold, proxy.shimMonitorOptions...)
	return proxy
}

// Handler returns the WebSocket forwarding handler for one endpoint contract.
func (p *Proxy) Handler(ep endpoint.WSForward) http.HandlerFunc {
	surface := ep.Surface()
	upstreamRoute := ep.Upstream()
	return func(w http.ResponseWriter, r *http.Request) {
		handshakeStart := time.Now()
		p.wg.Add(1)
		defer p.wg.Done()
		phaseCtx, cancelPhase := context.WithCancel(r.Context())
		stopForceCancel := context.AfterFunc(p.baseCtx, cancelPhase)
		defer stopForceCancel()
		defer cancelPhase()
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

		outReq, failure := p.caller.Prepare(phaseCtx, upstream.Call{
			Route:        upstreamRoute,
			Method:       http.MethodGet,
			Query:        r.URL.RawQuery,
			ForceQuery:   r.URL.ForceQuery,
			ClientHeader: r.Header,
		})
		if failure != nil {
			if failure.RespondTo(w, surface) {
				p.metrics.observeAccept(acceptOutcome(failure))
			}
			return
		}
		dialCtx, cancelDial := context.WithTimeout(phaseCtx, p.dialTimeout)
		var upstreamTransport net.Conn
		dialCtx = httptrace.WithClientTrace(dialCtx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				upstreamTransport = rawNetworkConn(info.Conn)
			},
		})
		upstreamConn, response, err := websocket.Dial(dialCtx, outReq.URL.String(), &websocket.DialOptions{
			HTTPClient:      p.dialClient,
			HTTPHeader:      outReq.Header,
			CompressionMode: websocket.CompressionDisabled,
		})
		dialDeadlineExceeded := dialCtx.Err() == context.DeadlineExceeded
		cancelDial()
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			// Preserve a deadline that fired before cleanup; otherwise classify
			// against the still-live phase context so cancelDial cannot manufacture
			// ClientGone for an ordinary execution error.
			classificationCtx := phaseCtx
			if dialDeadlineExceeded {
				classificationCtx = dialCtx
			}
			if p.caller.Classify(classificationCtx, err).RespondTo(w, surface) {
				p.metrics.observeAccept(AcceptDialFailed)
			}
			return
		}
		responseCtx := p.caller.Correlate(r.Context(), response.Header)
		defer func() { _ = upstreamConn.CloseNow() }()

		clientResponseWriter := &capturingResponseWriter{ResponseWriter: w}
		client, err := websocket.Accept(clientResponseWriter, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			p.metrics.observeAccept(AcceptRejected)
			return
		}
		defer func() { _ = client.CloseNow() }()
		chain := p.registry.NewChain(responseCtx, surface, upstreamRoute)
		p.logger.LogAttrs(responseCtx, slog.LevelInfo, "websocket established",
			slog.Int(logging.StatusKey, http.StatusSwitchingProtocols),
			slog.Duration(logging.HandshakeDurationKey, time.Since(handshakeStart)),
		)
		p.metrics.observeAccept(AcceptEstablished)

		session := &activeSession{
			client:            client,
			upstream:          upstreamConn,
			clientTransport:   clientResponseWriter.transport,
			upstreamTransport: upstreamTransport,
		}
		p.trackSession(session)
		defer p.untrackSession(session)

		result := runSession(p.drainCtx, p.baseCtx, client, upstreamConn, p.writeTimeout, p.maxMessageBytes,
			chain.WSClientAdapter(responseCtx, p.shimMonitor), chain.WSServerAdapter(responseCtx, p.shimMonitor))
		requestsummary.RecordWebSocket(r.Context(), requestsummary.WebSocketResult{
			Terminal:  requestsummary.WebSocketTerminal(result.terminal),
			CloseCode: int(result.closeCode),
			MsgsC2U:   result.messagesClientToUpstream,
			MsgsU2C:   result.messagesUpstreamToClient,
			BytesC2U:  result.bytesClientToUpstream,
			BytesU2C:  result.bytesUpstreamToClient,
		})
		p.metrics.observeSessionTerminal(result.terminal)
	}
}

func acceptOutcome(failure *upstream.Failure) AcceptOutcome {
	if failure.Kind == apierror.NotReady {
		return AcceptRejected
	}
	return AcceptDialFailed
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
