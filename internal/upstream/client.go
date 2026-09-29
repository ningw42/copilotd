package upstream

import (
	"net/http"
	"time"
)

// Option configures an optional Caller dependency.
type Option func(*transports)

// transports holds the transports under the Caller's two clients.
type transports struct {
	call      http.RoundTripper
	handshake http.RoundTripper
}

// WithTransport replaces the transport under the client that executes Do and
// Buffered. It is a test seam: the replacement carries its own settings,
// including any response-header timeout, while the Caller still refuses
// redirects on the client it builds around it.
func WithTransport(transport http.RoundTripper) Option {
	return func(t *transports) { t.call = transport }
}

// WithHandshakeTransport replaces the transport under HandshakeClient. It is a
// test seam; the Caller still refuses redirects on the client it builds around
// it.
func WithHandshakeTransport(transport http.RoundTripper) Option {
	return func(t *transports) { t.handshake = transport }
}

// defaultTransports builds separate transports for the two clients. Both honor
// proxy environment variables and default TLS verification. The call transport
// pools connections, attempts HTTP/2, leaves compression negotiation and
// decoding to callers, and bounds time-to-first-byte without imposing a total
// duration on a streaming response. The handshake transport is otherwise
// zero-value: the WebSocket handshake timeout bounds it.
func defaultTransports(responseHeaderTimeout time.Duration) transports {
	return transports{
		call: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
		handshake: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
}

// newClient builds a client for authenticated upstream calls. It has no total
// timeout, which would cut off a streaming response or a live WebSocket, and it
// returns the first upstream response. Following a 3xx would replace Copilot's
// answer with another endpoint's, and net/http re-sends Authorization, and with
// it the Copilot token, to a target on the same hostname or a subdomain of it.
func newClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}
}
