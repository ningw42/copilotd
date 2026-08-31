package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/requestsummary"
	"github.com/ningw42/copilotd/internal/upstream"
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
		requestsummary.RecordBinding(r.Context(), requestsummary.Binding{Context: ctx, Probe: probe})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog emits the sole terminal request summary after the registered
// handler returns. Registration scope is absent when no registered handler ran.
func accessLog(logger *slog.Logger, streamOutcomes StreamOutcomeObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{
			ResponseWriter:    w,
			status:            http.StatusOK,
			suppressBodyBytes: r.Method == http.MethodHead,
		}

		ctx, summary := requestsummary.Begin(r.Context(), streamOutcomes)
		next.ServeHTTP(sw, r.WithContext(ctx))

		publication := summary.Finish(requestsummary.ResponseResult{
			Method:   r.Method,
			Status:   sw.status,
			Bytes:    sw.bytes,
			Duration: time.Since(start),
		})
		logger.LogAttrs(publication.Context, publication.Level, "access", publication.Attrs...)
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
				if matched, ok := requestsummary.MatchedContext(r.Context()); ok {
					logCtx = matched
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
