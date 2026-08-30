package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/upstream"
	"github.com/ningw42/copilotd/internal/wsforward"
)

// requestID resolves the correlation id — honoring a well-formed inbound value,
// otherwise generating one — stores it in the request context (so logs pick it
// up), and echoes it in the response header before the request is served, so
// even a panic-produced response still carries it.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := logging.ResolveRequestID(r.Header.Get(upstream.RequestIDHeader))
		w.Header().Set(upstream.RequestIDHeader, id)
		ctx := logging.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// scoped derives registration-owned logging scope, publishes it for the
// after-handler access record, and runs next with that immutable child context.
func scoped(attrs []slog.Attr, probe bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.With(r.Context(), attrs...)
		publishMatchedScope(r.Context(), matchedScope{ctx: ctx, probe: probe})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog emits the sole terminal request summary after the registered
// handler returns. Registration scope is absent when no registered handler ran;
// streamed response facts are added from their package-owned result holder.
func accessLog(logger *slog.Logger, streamOutcomes StreamOutcomeObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{
			ResponseWriter:    w,
			status:            http.StatusOK,
			suppressBodyBytes: r.Method == http.MethodHead,
		}

		ctx := forward.WithStreamResultHolder(r.Context())
		ctx = catalog.WithShapeResultHolder(ctx)
		ctx = withMatchedScopeHolder(ctx)
		ctx = wsforward.WithSessionResultHolder(ctx)
		ctx = upstream.WithCorrelationHolder(ctx)
		next.ServeHTTP(sw, r.WithContext(ctx))

		logCtx := r.Context()
		probe := false
		matched, matchedOK := matchedScopeFromContext(ctx)
		if matchedOK {
			probe = matched.probe
		}
		if correlated, ok := upstream.CorrelatedContextFromContext(ctx); ok {
			logCtx = correlated
		} else if matchedOK {
			logCtx = matched.ctx
		}
		level := slog.LevelInfo
		if probe {
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String(logging.MethodKey, r.Method),
			slog.Int(logging.StatusKey, sw.status),
			slog.Int64(logging.BytesKey, sw.bytes),
			slog.Duration(logging.DurationKey, time.Since(start)),
		}
		if result, ok := forward.StreamResultFromContext(ctx); ok {
			streamOutcomes.ObserveStreamOutcome(result.Surface, result.Outcome)
			if !probe {
				switch result.Outcome {
				case sse.OutcomeSynthesized, sse.OutcomeStall, sse.OutcomeUpstreamError, sse.OutcomeShimError:
					level = slog.LevelWarn
				}
			}
			attrs = append(attrs,
				slog.String(logging.OutcomeKey, string(result.Outcome)),
				slog.Int(logging.FramesKey, result.Frames),
				slog.Int(logging.FallbacksKey, result.Fallbacks),
			)
		}
		if shape, ok := catalog.ShapeResultFromContext(ctx); ok {
			attrs = append(attrs, slog.String(logging.CatalogShapeKey, string(shape)))
		}
		if result, ok := wsforward.SessionResultFromContext(ctx); ok {
			if !probe && result.Terminal == wsforward.SessionError {
				level = slog.LevelWarn
			}
			attrs = append(attrs,
				slog.String(logging.TerminalReasonKey, string(result.Terminal)),
				slog.Int(logging.CloseCodeKey, result.CloseCode),
				slog.Int64(logging.MsgsC2UKey, result.MsgsC2U),
				slog.Int64(logging.MsgsU2CKey, result.MsgsU2C),
				slog.Int64(logging.BytesC2UKey, result.BytesC2U),
				slog.Int64(logging.BytesU2CKey, result.BytesU2C),
			)
		}
		if !probe && sw.status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		logger.LogAttrs(logCtx, level, "access", attrs...)
	})
}

// recoverMW turns a handler panic into a generic 500 with no stack and no JSON
// envelope, logged with its request and matched-registration scope. Client-side
// correlation is via the X-Request-Id header set by the outer requestID middleware.
func recoverMW(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logCtx := r.Context()
				if matched, ok := matchedScopeFromContext(r.Context()); ok {
					logCtx = matched.ctx
				}
				logger.LogAttrs(logCtx, slog.LevelError, "panic recovered",
					slog.Any(logging.PanicKey, rec),
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
