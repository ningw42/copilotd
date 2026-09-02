// Package forward is copilotd's dumb upstream forwarder: it moves a request to
// GitHub Copilot and copies the response back with minimal interpretation. It is
// deliberately Copilot-agnostic — it sees only the shared upstream Caller and
// never learns how its credential was minted. Inference requests use the
// bounded shim/SSE path;
// support requests use a focused streaming passthrough path. Both apply the
// centrally governed upstream header policy and copy responses body-verbatim.
// Only copilotd-originated signals are synthesized via apierror.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/shim"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/upstream"
)

// Forwarder forwards inbound Surface endpoint requests upstream. Its dependencies
// are injected so it stays Copilot-agnostic and unit-testable: the shared
// upstream Caller, the per-request context deadline, the inbound body cap, the
// ordered shim registry, and the internal/sse-owned pump logger.
type Forwarder struct {
	caller                  *upstream.Caller
	outboundTimeout         time.Duration
	writeTimeout            time.Duration
	streamIdleTimeout       time.Duration
	streamKeepaliveInterval time.Duration
	clock                   sse.Clock
	fallbacks               *sse.FallbackCounter
	// sseLogger belongs to internal/sse, the only package that emits through it.
	sseLogger            *slog.Logger
	shimMonitor          *shim.Monitor
	suppressedShimErrors *sse.SuppressedShimErrorCounter
	maxRequestBytes      int64
	registry             shim.Registry
}

// Option configures an optional Forwarder dependency.
type Option func(*Forwarder)

// New builds a Forwarder from its injected dependencies.
func New(caller *upstream.Caller, outboundTimeout, writeTimeout, streamIdleTimeout, streamKeepaliveInterval time.Duration, maxRequestBytes int64, registry shim.Registry, sseLogger, shimLogger *slog.Logger, hookOverrunThreshold time.Duration, options ...Option) *Forwarder {
	registry = append(shim.Registry(nil), registry...)
	f := &Forwarder{
		caller:                  caller,
		outboundTimeout:         outboundTimeout,
		writeTimeout:            writeTimeout,
		streamIdleTimeout:       streamIdleTimeout,
		streamKeepaliveInterval: streamKeepaliveInterval,
		clock:                   sse.RealClock{},
		fallbacks:               sse.NewFallbackCounter(),
		sseLogger:               sseLogger,
		shimMonitor:             shim.NewMonitor(shimLogger, hookOverrunThreshold),
		suppressedShimErrors:    sse.NewSuppressedShimErrorCounter(),
		maxRequestBytes:         maxRequestBytes,
		registry:                registry,
	}
	for _, configure := range options {
		configure(f)
	}
	return f
}

// SuppressedShimErrorCount reports stream shim panics hidden from the wire by
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
	upstreamRoute := ep.Upstream()
	surface := ep.Surface()
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.readBody(w, r, surface)
		if !ok {
			return
		}
		chain := f.registry.NewChain(r.Context(), surface, upstreamRoute)
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
	upstreamRoute := ep.Upstream()
	surface := ep.Surface()
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		resp, _, failure := f.caller.Do(ctx, upstream.Call{
			Route:                  upstreamRoute,
			Method:                 r.Method,
			Query:                  r.URL.RawQuery,
			ForceQuery:             r.URL.ForceQuery,
			ClientHeader:           r.Header,
			Body:                   r.Body,
			ContentLength:          r.ContentLength,
			AcceptIdentityEncoding: false,
		})
		if failure != nil {
			failure.RespondTo(w, surface)
			return
		}
		// A committed response can only be terminated, never replaced. Cancel
		// upstream work before closing its body so every post-commit exit path
		// releases a body whose Close may itself wait for cancellation.
		defer func() {
			cancel()
			_ = resp.Body.Close()
		}()

		outboundTimer := time.AfterFunc(f.outboundTimeout, cancel)
		defer outboundTimer.Stop()

		upstream.CopyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(sse.NewWriter(w, f.writeTimeout, time.Now), resp.Body)
	}
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

// forward performs one upstream call with the original bytes and rewritten
// headers, then copies the response back verbatim. Copilotd-originated failures
// are rendered through the call's classification (and a client disconnect is
// swallowed because the caller has already left).
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, header http.Header, body []byte, ep endpoint.HTTPForward, chain *shim.Chain) {
	upstreamRoute := ep.Upstream()
	surface := ep.Surface()
	ctx, cancelCause := context.WithCancelCause(r.Context())
	cancel := context.CancelFunc(func() { cancelCause(context.Canceled) })

	resp, responseCtx, failure := f.caller.Do(ctx, upstream.Call{
		Route:                  upstreamRoute,
		Method:                 http.MethodPost,
		Query:                  r.URL.RawQuery,
		ForceQuery:             r.URL.ForceQuery,
		ClientHeader:           header,
		Body:                   bytes.NewReader(body),
		ContentLength:          int64(len(body)),
		AcceptIdentityEncoding: true,
	})
	if failure != nil {
		cancel()
		failure.RespondTo(w, surface)
		return
	}
	// A committed response can only be terminated, never replaced. Cancel
	// upstream work before closing its body so every exit path releases a body
	// whose Close may itself wait for cancellation.
	defer func() {
		cancel()
		_ = resp.Body.Close()
	}()

	// A synchronous completion keeps the existing total-duration backstop. SSE
	// responses deliberately do not have a total-duration cap.
	eventStream := ep.AllowsSSE() && isEventStream(resp.Header.Get("Content-Type"))
	var outboundTimer *time.Timer
	if !eventStream {
		outboundTimer = time.AfterFunc(f.outboundTimeout, func() {
			cancelCause(context.DeadlineExceeded)
		})
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
	upstream.CopyResponseHeaders(preludeHeader, resp.Header)
	status, preludeHeader, err := chain.RunPrelude(r.Context(), resp.StatusCode, preludeHeader)
	if err != nil {
		writeShimError(w, surface, err)
		return
	}
	if eventStream {
		upstream.CopyResponseHeaders(w.Header(), preludeHeader)
		w.Header().Del("Content-Length")
		w.WriteHeader(status)
		policy := streamPolicy(ep.Surface(), f.writeTimeout, f.streamIdleTimeout, f.streamKeepaliveInterval, f.clock, f.fallbacks.Increment)
		policy.SuppressedShimErrors = f.suppressedShimErrors
		result := sse.Pump(responseCtx, cancel, resp.Body, w, f.sseLogger, policy, chain.StreamAdapter(f.shimMonitor, responseCtx))
		requestsummary.RecordStream(r.Context(), requestsummary.StreamResult{
			Surface:   ep.Surface().String(),
			Outcome:   result.Outcome,
			Frames:    result.Frames,
			Fallbacks: result.Fallbacks,
		})
		return
	}
	if !chain.HasBufferedTransformer() || !identityContentEncoding(resp.Header) {
		upstream.CopyResponseHeaders(w.Header(), preludeHeader)
		w.WriteHeader(status)
		_, _ = io.Copy(sse.NewWriter(w, f.writeTimeout, time.Now), resp.Body)
		return
	}
	buffered, failure := f.caller.ReadBounded(resp.Body)
	if failure != nil {
		failure.RespondTo(w, surface)
		return
	}
	buffered, err = chain.RunBuffered(r.Context(), buffered)
	if err != nil {
		writeShimError(w, surface, err)
		return
	}
	preludeHeader.Set("Content-Length", strconv.Itoa(len(buffered)))
	upstream.CopyResponseHeaders(w.Header(), preludeHeader)
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
