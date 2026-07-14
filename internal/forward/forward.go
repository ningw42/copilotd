// Package forward is copilotd's dumb upstream forwarder: it moves a request to
// GitHub Copilot and copies the response back with minimal interpretation. It is
// deliberately Copilot-agnostic — it sees only the identity.Credential seam
// (base URL, bearer token, impersonation headers) and never learns how that
// credential was minted. Per request it bounds and buffers the body, peeks the
// synchronous-only fields, rewrites headers by a fixed denylist, forwards the
// original bytes, and copies the upstream response (any status) back verbatim;
// only proxy-originated errors are synthesized via apierror.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

// requestIDHeader carries copilotd's resolved correlation id onto the upstream
// request. It mirrors the inbound header name and is kept local so forward does
// not import server (which would be a cycle).
const requestIDHeader = "X-Request-Id"

// Forwarder forwards inbound provider-route requests upstream. Its dependencies
// are injected so it stays Copilot-agnostic and unit-testable: the credential
// Provider, the outbound HTTP client, the per-request context deadline, and the
// inbound body cap.
type Forwarder struct {
	provider        identity.Provider
	client          *http.Client
	outboundTimeout time.Duration
	maxRequestBytes int64
}

// New builds a Forwarder from its injected dependencies.
func New(provider identity.Provider, client *http.Client, outboundTimeout time.Duration, maxRequestBytes int64) *Forwarder {
	return &Forwarder{
		provider:        provider,
		client:          client,
		outboundTimeout: outboundTimeout,
		maxRequestBytes: maxRequestBytes,
	}
}

// NewClient builds the dedicated outbound client: a tuned, connection-pooling
// transport that honors proxy env vars and default TLS verification, with no
// blunt client timeout (each call is bounded by the per-request context deadline
// instead, so a legitimately slow completion is not killed).
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// Handler returns the http.Handler for one route: it forwards to upstreamPath on
// the credential's base URL and tags any proxy-originated error with tag (the
// error dialect). tag also selects the synchronous-only peek — every surface
// rejects stream:true; the OpenAI surface additionally rejects background:true.
// A new route (e.g. OpenAI POST /openai/v1/responses -> /responses,
// apierror.OpenAI) is added by registering another Handler; the forwarding core
// does not change.
func (f *Forwarder) Handler(upstreamPath string, tag apierror.Surface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.readBody(w, r, tag)
		if !ok {
			return
		}
		if kind, reject := peek(body, tag); reject {
			apierror.Write(w, tag, kind, rejectMessage(kind))
			return
		}
		f.forward(w, r, body, upstreamPath, tag)
	}
}

// readBody bounds r.Body to maxRequestBytes and reads it fully into memory,
// returning false (after a 413) if the cap is exceeded. A different read error
// means the client vanished mid-body, so nothing useful can be sent and it
// returns false without a response.
func (f *Forwarder) readBody(w http.ResponseWriter, r *http.Request, tag apierror.Surface) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, f.maxRequestBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apierror.Write(w, tag, apierror.PayloadTooLarge, "request body exceeds the maximum allowed size")
		}
		return nil, false
	}
	return body, true
}

// peek reads only the synchronous-only fields from the buffered body. A non-JSON
// or field-absent body forwards (we are not a JSON validator; malformed bodies
// get Copilot's own 400). stream:true is rejected on every surface; background:true
// only on the OpenAI surface (its queued object needs the Responses management
// sub-paths, which Phase 1 does not mount).
func peek(body []byte, tag apierror.Surface) (apierror.Kind, bool) {
	var p struct {
		Stream     *bool `json:"stream"`
		Background *bool `json:"background"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, false
	}
	if p.Stream != nil && *p.Stream {
		return apierror.StreamUnsupported, true
	}
	if tag == apierror.OpenAI && p.Background != nil && *p.Background {
		return apierror.BackgroundUnsupported, true
	}
	return 0, false
}

func rejectMessage(kind apierror.Kind) string {
	switch kind {
	case apierror.StreamUnsupported:
		return "streaming responses are not supported"
	case apierror.BackgroundUnsupported:
		return "background responses are not supported"
	default:
		return "unsupported request"
	}
}

// forward mints/reads the credential, builds the outbound request with the
// original bytes and the rewritten headers, calls upstream under a per-request
// deadline, and copies the response back verbatim. Proxy-origin failures are
// classified into 502/504 (and a client disconnect is swallowed — the caller has
// already left).
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, body []byte, upstreamPath string, tag apierror.Surface) {
	cred, err := f.provider.Current(r.Context())
	if err != nil {
		// A request-time credential failure (the real Manager's on-demand mint
		// failing, #11) leaves nothing to forward; surface it as not-ready. The
		// static stub never errors here.
		apierror.Write(w, tag, apierror.NotReady, "no upstream credential available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), f.outboundTimeout)
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cred.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		apierror.Write(w, tag, apierror.BadGateway, "could not build the upstream request")
		return
	}
	outReq.Header = outboundHeaders(r, cred)

	resp, err := f.client.Do(outReq)
	if err != nil {
		switch {
		case errors.Is(r.Context().Err(), context.Canceled):
			// The client disconnected; deriving ctx from r.Context() already
			// cancelled the upstream call, and there is no one left to answer.
			return
		case errors.Is(err, context.DeadlineExceeded):
			apierror.Write(w, tag, apierror.GatewayTimeout, "the upstream request timed out")
		default:
			apierror.Write(w, tag, apierror.BadGateway, "could not reach the upstream")
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// hopByHop is the standard set of connection-scoped headers a proxy must not
// forward in either direction. Names are in http.CanonicalHeaderKey form
// (note "Te" is the canonical form of "TE").
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// requestStrip is hopByHop plus the headers copilotd must never leak upstream:
// the inbound API key (both schemes), the client Host, and the recomputed
// Content-Length.
var requestStrip = withExtra(hopByHop, "Authorization", "X-Api-Key", "Host", "Content-Length")

func withExtra(base map[string]bool, extra ...string) map[string]bool {
	m := make(map[string]bool, len(base)+len(extra))
	for k := range base {
		m[k] = true
	}
	for _, k := range extra {
		m[http.CanonicalHeaderKey(k)] = true
	}
	return m
}

// outboundHeaders builds the upstream request headers by the denylist/passthrough
// policy (§7.3): copy every inbound header except the strip-set (and any header
// named in the inbound Connection header), then set ours — Authorization from the
// credential, the impersonation set, and the resolved correlation id — each
// replacing any client value. cred.Headers is copied onto a fresh map and never
// mutated.
func outboundHeaders(r *http.Request, cred identity.Credential) http.Header {
	out := make(http.Header, len(r.Header))
	conn := connectionTokens(r.Header)
	for name, vals := range r.Header {
		cn := http.CanonicalHeaderKey(name)
		if requestStrip[cn] || conn[cn] {
			continue
		}
		out[cn] = append([]string(nil), vals...)
	}
	out.Set("Authorization", "Bearer "+cred.Token)
	for name, vals := range cred.Headers {
		out[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
	}
	if id, ok := logging.RequestIDFrom(r.Context()); ok {
		out.Set(requestIDHeader, id)
	}
	return out
}

// copyResponseHeaders copies response headers minus hop-by-hop (and any named in
// the upstream Connection header) downstream verbatim.
func copyResponseHeaders(dst, src http.Header) {
	conn := connectionTokens(src)
	for name, vals := range src {
		cn := http.CanonicalHeaderKey(name)
		if hopByHop[cn] || conn[cn] {
			continue
		}
		for _, v := range vals {
			dst.Add(cn, v)
		}
	}
}

// connectionTokens returns the set of header names listed in the Connection
// header, which are themselves hop-by-hop for this message.
func connectionTokens(h http.Header) map[string]bool {
	var tokens map[string]bool
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if name := strings.TrimSpace(tok); name != "" {
				if tokens == nil {
					tokens = make(map[string]bool)
				}
				tokens[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return tokens
}
