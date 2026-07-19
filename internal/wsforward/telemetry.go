package wsforward

// AcceptOutcome is the bounded result of one downstream WebSocket handshake.
type AcceptOutcome string

const (
	AcceptEstablished AcceptOutcome = "established"
	AcceptRejected    AcceptOutcome = "rejected"
	AcceptDialFailed  AcceptOutcome = "dial_failed"
)

// SessionTerminal is the bounded reason an established session ended.
type SessionTerminal string

const (
	SessionClientClosed   SessionTerminal = "client_closed"
	SessionUpstreamClosed SessionTerminal = "upstream_closed"
	SessionError          SessionTerminal = "error"
)

// AcceptObserver records one bounded handshake outcome.
type AcceptObserver interface {
	ObserveAccept(AcceptOutcome)
}

// SessionTerminalObserver records one bounded terminal outcome for an
// established session.
type SessionTerminalObserver interface {
	ObserveSessionTerminal(SessionTerminal)
}

// WsMetrics bundles the two independent, single-axis WebSocket observers.
// Either observer may be nil when telemetry is intentionally disabled.
type WsMetrics struct {
	Accept          AcceptObserver
	SessionTerminal SessionTerminalObserver
}

func (m WsMetrics) observeAccept(outcome AcceptOutcome) {
	if m.Accept != nil {
		m.Accept.ObserveAccept(outcome)
	}
}

func (m WsMetrics) observeSessionTerminal(terminal SessionTerminal) {
	if m.SessionTerminal != nil {
		m.SessionTerminal.ObserveSessionTerminal(terminal)
	}
}
