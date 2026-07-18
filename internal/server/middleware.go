package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
)

const requestIDHeader = "X-Request-Id"

// requestID resolves the correlation id — honoring a well-formed inbound value,
// otherwise generating one — stores it in the request context (so logs pick it
// up), and echoes it in the response header before the request is served, so
// even a panic-produced response still carries it.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := logging.ResolveRequestID(r.Header.Get(requestIDHeader))
		w.Header().Set(requestIDHeader, id)
		ctx := logging.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog emits exactly one structured line per request. It labels by route
// template (r.Pattern) to keep the label low-cardinality, with an "unmatched"
// fallback on 404. For streamed responses it adds the pump summary and observes
// the terminal-outcome metric. The quiet health route logs at debug so constant
// polling does not flood info. The request_id attribute is injected by the
// logging context handler.
func accessLog(logger *slog.Logger, streamOutcomes StreamOutcomeObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{
			ResponseWriter:    w,
			status:            http.StatusOK,
			suppressBodyBytes: r.Method == http.MethodHead,
		}

		ctx := forward.WithStreamResultHolder(r.Context())
		requestWithHolder := r.WithContext(ctx)
		next.ServeHTTP(sw, requestWithHolder)

		route := requestWithHolder.Pattern
		if route == "" {
			route = "unmatched"
		}
		level := slog.LevelInfo
		if r.URL.Path == healthPath {
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.Int("status", sw.status),
			slog.Int64("bytes", sw.bytes),
			slog.Duration("duration", time.Since(start)),
		}
		if result, ok := forward.StreamResultFromContext(ctx); ok {
			streamOutcomes.ObserveStreamOutcome(result.Surface, result.Outcome)
			switch result.Outcome {
			case sse.OutcomeSynthesized, sse.OutcomeStall, sse.OutcomeUpstreamError, sse.OutcomeShimError:
				level = slog.LevelWarn
			}
			attrs = append(attrs,
				slog.String("outcome", string(result.Outcome)),
				slog.Int("frames", result.Frames),
				slog.Int("fallbacks", result.Fallbacks),
			)
		}
		logger.LogAttrs(r.Context(), level, "access", attrs...)
	})
}

// recoverMW turns a handler panic into a generic 500 with no stack and no JSON
// envelope, logged with its request-id. Client-side correlation is via the
// X-Request-Id response header set by the outer requestID middleware.
func recoverMW(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.Any("panic", rec),
				)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter captures the status code and byte count for the access log.
type statusWriter struct {
	http.ResponseWriter
	status            int
	bytes             int64
	wroteHeader       bool
	suppressBodyBytes bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	// net/http accepts representation writes for HEAD so it can derive the
	// response headers, but suppresses those bytes on the wire. Access logs
	// report downstream body bytes, so only methods that can emit a body count
	// the accepted representation bytes.
	if !w.suppressBodyBytes {
		w.bytes += int64(n)
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer through the
// wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
