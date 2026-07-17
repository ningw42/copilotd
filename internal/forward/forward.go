// Package forward is copilotd's dumb upstream forwarder: it moves a request to
// GitHub Copilot and copies the response back with minimal interpretation. It is
// deliberately Copilot-agnostic — it sees only the identity.Credential seam
// (base URL, bearer token, impersonation headers) and never learns how that
// credential was minted. Per request it bounds and buffers the body, peeks only
// the OpenAI background flag, rewrites headers by a fixed denylist, forwards the
// original bytes, and copies upstream responses frame- or body-verbatim. Only
// copilotd-originated signals are synthesized via apierror.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
)

// requestIDHeader carries copilotd's resolved correlation id onto the upstream
// request. It mirrors the inbound header name and is kept local so forward does
// not import server (which would be a cycle).
const requestIDHeader = "X-Request-Id"

// Forwarder forwards inbound provider-route requests upstream. Its dependencies
// are injected so it stays Copilot-agnostic and unit-testable: the credential
// Provider, the outbound HTTP client, the per-request context deadline, the
// inbound and buffered-response body caps, and the ordered shim registry.
type Forwarder struct {
	provider                 identity.Provider
	client                   *http.Client
	outboundTimeout          time.Duration
	writeTimeout             time.Duration
	streamIdleTimeout        time.Duration
	streamKeepaliveInterval  time.Duration
	clock                    sse.Clock
	fallbacks                *sse.FallbackCounter
	logger                   *slog.Logger
	suppressedShimErrors     *sse.SuppressedShimErrorCounter
	maxRequestBytes          int64
	maxBufferedResponseBytes int64
	registry                 shim.Registry
}

// New builds a Forwarder from its injected dependencies.
func New(provider identity.Provider, client *http.Client, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes, maxBufferedResponseBytes int64, registry shim.Registry) *Forwarder {
	registry = append(shim.Registry(nil), registry...)
	return &Forwarder{
		provider:                 provider,
		client:                   client,
		outboundTimeout:          outboundTimeout,
		writeTimeout:             writeTimeout,
		streamIdleTimeout:        streamIdleTimeout,
		streamKeepaliveInterval:  streamKeepaliveInterval,
		clock:                    sse.RealClock{},
		fallbacks:                sse.NewFallbackCounter(),
		logger:                   slog.Default(),
		suppressedShimErrors:     sse.NewSuppressedShimErrorCounter(),
		maxRequestBytes:          maxRequestBytes,
		maxBufferedResponseBytes: maxBufferedResponseBytes,
		registry:                 registry,
	}
}

// SuppressedShimErrorCount reports stream shim failures hidden from the wire by
// the post-terminal no-double-up rule.
func (f *Forwarder) SuppressedShimErrorCount() uint64 {
	return f.suppressedShimErrors.Count()
}

// NewClient builds the dedicated outbound client: a tuned, connection-pooling
// transport that honors proxy env vars and default TLS verification. The
// response-header timeout bounds time-to-first-byte without imposing a total
// duration on a future streaming response.
func NewClient(responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
	}
}

// Handler returns the http.Handler for one route: it forwards to upstreamPath on
// the credential's base URL and tags any copilotd-originated signal with tag (the
// error dialect). Anthropic requests are forwarded without a peek; the OpenAI
// surface peeks only background:true, which remains unsupported.
// A new route (e.g. OpenAI POST /openai/v1/responses -> /responses,
// apierror.OpenAI) is added by registering another Handler; the forwarding core
// does not change.
func (f *Forwarder) Handler(upstreamPath string, tag apierror.Surface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.readBody(w, r, tag)
		if !ok {
			return
		}
		chain := f.registry.NewChain(r.Context(), tag, shim.Route(upstreamPath))
		header, body, err := chain.RunRequest(r.Context(), r.URL.RawQuery, r.Header, body)
		if err != nil {
			writeShimError(w, tag, err)
			return
		}
		if tag == apierror.OpenAI && peekBackground(body) {
			apierror.Write(w, tag, apierror.BackgroundUnsupported, "background responses are not supported")
			return
		}
		f.forward(w, r, header, body, upstreamPath, tag, chain)
	}
}

func writeShimError(w http.ResponseWriter, surface apierror.Surface, err error) {
	var rejected *apierror.Error
	if errors.As(err, &rejected) {
		apierror.Write(w, surface, rejected.Kind, rejected.Msg)
		return
	}
	apierror.Write(w, surface, apierror.ShimError, "copilotd: shim failed")
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

// peekBackground reads only the OpenAI background field from the buffered body.
// A non-JSON or field-absent body forwards (we are not a JSON validator;
// malformed bodies get Copilot's own 400). A background response's queued
// object needs the Responses management sub-paths, which are not mounted.
func peekBackground(body []byte) bool {
	var p struct {
		Background *bool `json:"background"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	return p.Background != nil && *p.Background
}

// forward mints/reads the credential, builds the outbound request with the
// original bytes and the rewritten headers, calls upstream under the inbound
// cancellation context and response-header bound, and copies the response back
// verbatim. Failures originated by copilotd are classified into 502/504 (and a client
// disconnect is swallowed — the caller has already left).
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, header http.Header, body []byte, upstreamPath string, tag apierror.Surface, chain *shim.Chain) {
	cred, err := f.provider.Current(r.Context())
	if err != nil {
		// A request-time credential failure (the real Manager's on-demand mint
		// failing, #11) leaves nothing to forward; surface it as not-ready. The
		// static stub never errors here.
		apierror.Write(w, tag, apierror.NotReady, "no upstream credential available")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cred.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		apierror.Write(w, tag, apierror.BadGateway, "could not build the upstream request")
		return
	}
	outReq.URL.RawQuery = r.URL.RawQuery
	outReq.URL.ForceQuery = r.URL.ForceQuery
	headerRequest := *r
	headerRequest.Header = header
	outReq.Header = outboundHeaders(&headerRequest, cred)

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

	// A synchronous completion keeps the existing total-duration backstop. SSE
	// responses deliberately do not have a total-duration cap.
	eventStream := isEventStream(resp.Header.Get("Content-Type"))
	var outboundTimer *time.Timer
	if !eventStream {
		outboundTimer = time.AfterFunc(f.outboundTimeout, cancel)
		defer outboundTimer.Stop()
	}

	if eventStream {
		encodings := resp.Header.Values("Content-Encoding")
		switch {
		case len(encodings) == 0:
		case len(encodings) == 1 && strings.EqualFold(strings.TrimSpace(encodings[0]), "identity"):
			resp.Header.Del("Content-Encoding")
		default:
			apierror.Write(w, tag, apierror.BadGateway, "upstream returned unsupported Content-Encoding for an event stream")
			return
		}
	}
	preludeHeader := make(http.Header)
	copyResponseHeaders(preludeHeader, resp.Header)
	status, preludeHeader, err := chain.RunPrelude(r.Context(), resp.StatusCode, preludeHeader)
	if err != nil {
		writeShimError(w, tag, err)
		return
	}
	if eventStream {
		copyResponseHeaders(w.Header(), preludeHeader)
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		policy := streamPolicy(tag, f.writeTimeout, f.streamIdleTimeout, f.streamKeepaliveInterval, f.clock, f.fallbacks.Increment)
		policy.Logger = f.logger
		policy.SuppressedShimErrors = f.suppressedShimErrors
		result := sse.Pump(ctx, cancel, resp.Body, w, policy, chain.StreamAdapter())
		StoreStreamResult(r.Context(), StreamResult{
			Surface:   streamSurface(tag),
			Outcome:   result.Outcome,
			Frames:    result.Frames,
			Fallbacks: result.Fallbacks,
		})
		return
	}
	if !chain.HasBufferedTransformer() || !identityContentEncoding(resp.Header) {
		copyResponseHeaders(w.Header(), preludeHeader)
		w.WriteHeader(status)
		_, _ = io.Copy(sse.NewWriter(w, f.writeTimeout, time.Now), resp.Body)
		return
	}
	reader := io.Reader(resp.Body)
	if f.maxBufferedResponseBytes < math.MaxInt64 {
		reader = io.LimitReader(resp.Body, f.maxBufferedResponseBytes+1)
	}
	buffered, err := io.ReadAll(reader)
	if err != nil {
		apierror.Write(w, tag, apierror.BadGateway, "could not read the upstream response")
		return
	}
	if int64(len(buffered)) > f.maxBufferedResponseBytes {
		apierror.Write(w, tag, apierror.PayloadTooLarge, "upstream response body exceeds the maximum allowed size")
		return
	}
	buffered, err = chain.RunBuffered(r.Context(), buffered)
	if err != nil {
		writeShimError(w, tag, err)
		return
	}
	preludeHeader.Set("Content-Length", strconv.Itoa(len(buffered)))
	copyResponseHeaders(w.Header(), preludeHeader)
	w.WriteHeader(status)
	_, _ = sse.NewWriter(w, f.writeTimeout, time.Now).Write(buffered)
}

func identityContentEncoding(header http.Header) bool {
	values := header.Values("Content-Encoding")
	return len(values) == 0 || len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func streamPolicy(surface apierror.Surface, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, clock sse.Clock, onFallback func()) sse.Policy {
	keepaliveInterval := time.Duration(0)
	if surface == apierror.OpenAI {
		keepaliveInterval = streamKeepaliveInterval
	}
	return sse.Policy{
		Terminal: func(eventType string) bool {
			if eventType == "error" {
				return true
			}
			if surface == apierror.Anthropic {
				return eventType == "message_stop"
			}
			return eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete"
		},
		RenderError: func(w http.ResponseWriter, outcome sse.Outcome) error {
			reason := apierror.StreamEnded
			switch outcome {
			case sse.OutcomeUpstreamError:
				reason = apierror.StreamFailed
			case sse.OutcomeStall:
				reason = apierror.StreamStalled
			case sse.OutcomeShimError:
				reason = apierror.StreamShimFailed
			}
			return apierror.WriteStreamError(w, surface, reason)
		},
		WriteTimeout:      writeTimeout,
		IdleTimeout:       streamIdleTimeout,
		KeepaliveInterval: keepaliveInterval,
		Clock:             clock,
		OnFallback:        onFallback,
	}
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
// credential, the impersonation set, identity response encoding, and the
// resolved correlation id — each replacing any client value. cred.Headers is
// copied onto a fresh map and never mutated.
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
	out.Set("Accept-Encoding", "identity")
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
