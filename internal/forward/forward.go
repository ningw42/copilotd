// Package forward is copilotd's dumb upstream forwarder: it moves a request to
// GitHub Copilot and copies the response back with minimal interpretation. It is
// deliberately Copilot-agnostic — it sees only the identity.Credential seam
// (base URL, bearer token, impersonation headers) and never learns how that
// credential was minted. Inference requests use the bounded shim/SSE path;
// support requests use a focused streaming passthrough path. Both rewrite
// headers by a fixed denylist and copy upstream responses body-verbatim. Only
// copilotd-originated signals are synthesized via apierror.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
)

// requestIDHeader carries copilotd's resolved correlation id onto the upstream
// request. It mirrors the inbound header name and is kept local so forward does
// not import server (which would be a cycle).
const requestIDHeader = "X-Request-Id"

// Forwarder forwards inbound Surface endpoint requests upstream. Its dependencies
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

// Option configures an optional Forwarder dependency.
type Option func(*Forwarder)

// WithLogger routes Forwarder-owned records through logger. A nil logger keeps
// the process default captured by New.
func WithLogger(logger *slog.Logger) Option {
	return func(f *Forwarder) {
		if logger != nil {
			f.logger = logger
		}
	}
}

// New builds a Forwarder from its injected dependencies.
func New(provider identity.Provider, client *http.Client, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes, maxBufferedResponseBytes int64, registry shim.Registry, options ...Option) *Forwarder {
	registry = append(shim.Registry(nil), registry...)
	f := &Forwarder{
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
	for _, configure := range options {
		configure(f)
	}
	return f
}

// SuppressedShimErrorCount reports stream shim failures hidden from the wire by
// the post-terminal no-double-up rule.
func (f *Forwarder) SuppressedShimErrorCount() uint64 {
	return f.suppressedShimErrors.Count()
}

// NewClient builds the dedicated outbound client: a tuned, connection-pooling
// transport that honors proxy env vars and default TLS verification. It returns
// the first upstream response, leaves compression negotiation and decoding to
// callers, and bounds time-to-first-byte without imposing a total duration on a
// future streaming response.
func NewClient(responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
	}
}

// Handler returns the handler for one HTTP-forward endpoint contract. The
// contract supplies both the upstream route and the Surface error dialect.
// Anthropic requests are forwarded without a peek; the OpenAI surface peeks
// only background:true, which remains unsupported.
func (f *Forwarder) Handler(ep endpoint.HTTPForward) http.HandlerFunc {
	upstream := ep.Upstream()
	surface := ep.Surface()
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.readBody(w, r, surface)
		if !ok {
			return
		}
		chain := f.registry.NewChain(r.Context(), surface, upstream)
		header, body, err := chain.RunRequest(r.Context(), r.URL.RawQuery, r.Header, body)
		if err != nil {
			writeShimError(w, surface, err)
			return
		}
		if surface == endpoint.OpenAI && peekBackground(body) {
			apierror.Write(w, surface, apierror.BackgroundUnsupported, "background responses are not supported")
			return
		}
		f.forward(w, r, header, body, ep, chain)
	}
}

// PassthroughHandler returns the raw support-route handler for one passthrough
// endpoint contract. It streams both request and response bodies, and
// deliberately bypasses request peeking, body caps, shims, and SSE
// classification. The inbound request method is preserved upstream.
func (f *Forwarder) PassthroughHandler(ep endpoint.Passthrough) http.HandlerFunc {
	upstream := ep.Upstream()
	surface := ep.Surface()
	return func(w http.ResponseWriter, r *http.Request) {
		cred, err := f.provider.Current(r.Context())
		if err != nil {
			apierror.Write(w, surface, apierror.NotReady, "no upstream credential available")
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		outboundBody := r.Body
		if outboundBody == nil || outboundBody == http.NoBody {
			// net/http transparently retries bodyless GET and HEAD requests after
			// some failures on reused connections. Preserve an empty body on the
			// wire while making this request explicitly single-attempt.
			outboundBody = &singleAttemptBody{ReadCloser: http.NoBody}
		}
		outReq, err := http.NewRequestWithContext(ctx, r.Method, cred.BaseURL+string(upstream), outboundBody)
		if err != nil {
			apierror.Write(w, surface, apierror.BadGateway, "could not build the upstream request")
			return
		}
		outReq.URL.RawQuery = r.URL.RawQuery
		outReq.URL.ForceQuery = r.URL.ForceQuery
		outReq.ContentLength = r.ContentLength
		outReq.Header = authenticatedOutboundHeaders(r, cred)
		resp, err := f.client.Do(outReq)
		if err != nil {
			switch {
			case errors.Is(r.Context().Err(), context.Canceled):
				return
			case errors.Is(err, context.DeadlineExceeded):
				apierror.Write(w, surface, apierror.GatewayTimeout, "the upstream request timed out")
			default:
				apierror.Write(w, surface, apierror.BadGateway, "could not reach the upstream")
			}
			return
		}
		f.logUpstreamRequestID(r.Context(), resp.Header)
		// A committed response can only be terminated, never replaced. Cancel
		// upstream work before closing its body so every post-commit exit path
		// releases a body whose Close may itself wait for cancellation.
		defer func() {
			cancel()
			_ = resp.Body.Close()
		}()

		outboundTimer := time.AfterFunc(f.outboundTimeout, cancel)
		defer outboundTimer.Stop()

		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(sse.NewWriter(w, f.writeTimeout, time.Now), resp.Body)
	}
}

// FetchModels obtains the current account-authorized Copilot model Catalog as
// one bounded, buffered response. The Forwarder deliberately does not decode or
// reshape the body; callers own any provider-shaped representation.
func (f *Forwarder) FetchModels(parent context.Context, upstream endpoint.Route) (int, []byte, error) {
	cred, err := f.provider.Current(parent)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", catalog.ErrNoCredential, err)
	}

	ctx, cancel := context.WithCancelCause(parent)
	outReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cred.BaseURL+string(upstream), &singleAttemptBody{ReadCloser: http.NoBody})
	if err != nil {
		cancel(nil)
		return 0, nil, fmt.Errorf("%w: %v", catalog.ErrBuildUpstream, err)
	}
	outReq.Header = authenticatedOutboundHeaders(outReq, cred)
	outReq.Header.Set("Accept-Encoding", "identity")

	resp, err := f.client.Do(outReq)
	if err != nil {
		cancel(nil)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("%w: %v", catalog.ErrUpstreamTimeout, err)
		}
		return 0, nil, fmt.Errorf("%w: %v", catalog.ErrUpstreamUnreachable, err)
	}
	f.logUpstreamRequestID(parent, resp.Header)
	defer func() {
		cancel(nil)
		_ = resp.Body.Close()
	}()

	timeoutCause := errors.New("models fetch outbound timeout")
	outboundTimer := time.AfterFunc(f.outboundTimeout, func() { cancel(timeoutCause) })
	defer outboundTimer.Stop()

	reader := io.Reader(resp.Body)
	if f.maxBufferedResponseBytes < math.MaxInt64 {
		reader = io.LimitReader(resp.Body, f.maxBufferedResponseBytes+1)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, timeoutCause) || errors.Is(cause, context.DeadlineExceeded) {
			return 0, nil, fmt.Errorf("%w: %v", catalog.ErrUpstreamTimeout, err)
		}
		return 0, nil, fmt.Errorf("%w: %v", catalog.ErrUpstreamRead, err)
	}
	if int64(len(body)) > f.maxBufferedResponseBytes {
		return 0, nil, fmt.Errorf("%w: upstream response body exceeds the maximum allowed size", catalog.ErrUpstreamRead)
	}
	return resp.StatusCode, body, nil
}

var _ catalog.Fetcher = (*Forwarder)(nil)

// singleAttemptBody differs from http.NoBody only in identity. Its non-nil Body
// and nil GetBody make Go's Transport treat an otherwise bodyless GET or HEAD as
// non-replayable; Transport's empty-body probe still emits no body on the wire.
type singleAttemptBody struct {
	io.ReadCloser
}

func writeShimError(w http.ResponseWriter, surface endpoint.Surface, err error) {
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
func (f *Forwarder) readBody(w http.ResponseWriter, r *http.Request, surface endpoint.Surface) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, f.maxRequestBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apierror.Write(w, surface, apierror.PayloadTooLarge, "request body exceeds the maximum allowed size")
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
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, header http.Header, body []byte, ep endpoint.HTTPForward, chain *shim.Chain) {
	upstream := ep.Upstream()
	surface := ep.Surface()
	cred, err := f.provider.Current(r.Context())
	if err != nil {
		// A request-time credential failure (the real Manager's on-demand mint
		// failing, #11) leaves nothing to forward; surface it as not-ready. The
		// static stub never errors here.
		apierror.Write(w, surface, apierror.NotReady, "no upstream credential available")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cred.BaseURL+string(upstream), bytes.NewReader(body))
	if err != nil {
		apierror.Write(w, surface, apierror.BadGateway, "could not build the upstream request")
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
			apierror.Write(w, surface, apierror.GatewayTimeout, "the upstream request timed out")
		default:
			apierror.Write(w, surface, apierror.BadGateway, "could not reach the upstream")
		}
		return
	}
	f.logUpstreamRequestID(r.Context(), resp.Header)
	defer func() { _ = resp.Body.Close() }()

	// A synchronous completion keeps the existing total-duration backstop. SSE
	// responses deliberately do not have a total-duration cap.
	eventStream := ep.AllowsSSE() && isEventStream(resp.Header.Get("Content-Type"))
	var outboundTimer *time.Timer
	if !eventStream {
		outboundTimer = time.AfterFunc(f.outboundTimeout, cancel)
		defer outboundTimer.Stop()
	}

	if eventStream {
		if !identityContentEncoding(resp.Header) {
			apierror.Write(w, surface, apierror.BadGateway, "upstream returned unsupported Content-Encoding for an event stream")
			return
		}
		resp.Header.Del("Content-Encoding")
	}
	preludeHeader := make(http.Header)
	copyResponseHeaders(preludeHeader, resp.Header)
	status, preludeHeader, err := chain.RunPrelude(r.Context(), resp.StatusCode, preludeHeader)
	if err != nil {
		writeShimError(w, surface, err)
		return
	}
	if eventStream {
		copyResponseHeaders(w.Header(), preludeHeader)
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		policy := streamPolicy(ep.Surface(), f.writeTimeout, f.streamIdleTimeout, f.streamKeepaliveInterval, f.clock, f.fallbacks.Increment)
		policy.Logger = f.logger
		policy.SuppressedShimErrors = f.suppressedShimErrors
		result := sse.Pump(ctx, cancel, resp.Body, w, policy, chain.StreamAdapter())
		StoreStreamResult(r.Context(), StreamResult{
			Surface:   ep.Surface().String(),
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
		apierror.Write(w, surface, apierror.BadGateway, "could not read the upstream response")
		return
	}
	if int64(len(buffered)) > f.maxBufferedResponseBytes {
		apierror.Write(w, surface, apierror.PayloadTooLarge, "upstream response body exceeds the maximum allowed size")
		return
	}
	buffered, err = chain.RunBuffered(r.Context(), buffered)
	if err != nil {
		writeShimError(w, surface, err)
		return
	}
	preludeHeader.Set("Content-Length", strconv.Itoa(len(buffered)))
	copyResponseHeaders(w.Header(), preludeHeader)
	w.WriteHeader(status)
	_, _ = sse.NewWriter(w, f.writeTimeout, time.Now).Write(buffered)
}

func (f *Forwarder) logUpstreamRequestID(ctx context.Context, header http.Header) {
	requestID, ok := logging.RequestIDFrom(ctx)
	if !ok {
		return
	}
	upstreamRequestID := header.Get(requestIDHeader)
	if upstreamRequestID == "" || upstreamRequestID == requestID {
		return
	}
	f.logger.InfoContext(ctx, "upstream response correlation",
		slog.String("upstream_request_id", upstreamRequestID))
}

func identityContentEncoding(header http.Header) bool {
	values := header.Values("Content-Encoding")
	return len(values) == 0 || len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")
}

func isEventStream(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func streamPolicy(surface endpoint.Surface, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, clock sse.Clock, onFallback func()) sse.Policy {
	keepaliveInterval := time.Duration(0)
	if surface == endpoint.OpenAI {
		keepaliveInterval = streamKeepaliveInterval
	}
	return sse.Policy{
		Terminal: func(eventType string) bool {
			if eventType == "error" {
				return true
			}
			if surface == endpoint.Anthropic {
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

// responseStrip also suppresses Copilot's request id: the outer server
// middleware has already installed copilotd's resolved correlation value.
var responseStrip = withExtra(hopByHop, requestIDHeader)

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

// outboundHeaders builds the inference request headers and forces identity
// response encoding on top of the shared authenticated-header policy.
func outboundHeaders(r *http.Request, cred identity.Credential) http.Header {
	out := authenticatedOutboundHeaders(r, cred)
	out.Set("Accept-Encoding", "identity")
	return out
}

// authenticatedOutboundHeaders copies every inbound header except the strip-set
// (and any header named in Connection), then replaces credentials,
// impersonation, and correlation values. cred.Headers is copied onto a fresh map
// and never mutated.
func authenticatedOutboundHeaders(r *http.Request, cred identity.Credential) http.Header {
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

// copyResponseHeaders copies response headers minus the response strip-set (and
// any named in the upstream Connection header) downstream verbatim.
func copyResponseHeaders(dst, src http.Header) {
	conn := connectionTokens(src)
	for name, vals := range src {
		cn := http.CanonicalHeaderKey(name)
		if responseStrip[cn] || conn[cn] {
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
