// Package logging builds copilotd's structured logger and owns request-id
// correlation. It deliberately imports no net/http so it stays reusable and
// independently testable; the HTTP layer couples to it only through the context
// helpers below.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"

	"github.com/google/uuid"
	"github.com/ningw42/copilotd/internal/build"
	"github.com/ningw42/copilotd/internal/config"
)

// serviceName is emitted as a base attribute on every record.
const serviceName = "copilotd"

// maxRequestIDLen bounds an honored inbound request-id so a malformed or
// oversized correlation header can never become a DoS lever.
const maxRequestIDLen = 128

// requestIDPattern is the charset an inbound X-Request-Id must match to be
// honored. UUID hyphens fall inside it, so generated ids validate cleanly.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,` + fmt.Sprint(maxRequestIDLen) + `}$`)

// New builds the shared logger from cfg, writing to the configured log file or
// stderr. The returned io.Closer closes the log file (a no-op for stderr) and
// should be closed by the caller on shutdown.
func New(cfg config.ServeConfig) (*slog.Logger, io.Closer, error) {
	w, closer, err := openSink(cfg.LogFile)
	if err != nil {
		return nil, nil, err
	}
	logger, err := NewWithWriter(w, cfg)
	if err != nil {
		_ = closer.Close()
		return nil, nil, err
	}
	return logger, closer, nil
}

// openSink returns the writer and its closer for the configured destination.
func openSink(logFile string) (io.Writer, io.Closer, error) {
	if logFile == "" {
		return os.Stderr, io.NopCloser(nil), nil
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %q: %w", logFile, err)
	}
	return f, f, nil
}

// NewWithWriter constructs a logger writing to w. It is the testing seam:
// emitted bytes are asserted against an injected buffer, and other packages'
// tests reuse it to capture structured output. New delegates to it.
func NewWithWriter(w io.Writer, cfg config.ServeConfig) (*slog.Logger, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{
		Level: level,
		// Source locations are debugging signal but noise/overhead at info+.
		AddSource: level == slog.LevelDebug,
	}

	var base slog.Handler
	switch cfg.LogFormat {
	case "json":
		base = slog.NewJSONHandler(w, opts)
	default:
		base = slog.NewTextHandler(w, opts)
	}

	logger := slog.New(&contextHandler{inner: base}).With(
		slog.String("service", serviceName),
		slog.String("version", build.Version),
	)
	return logger, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", s)
	}
}

// contextHandler injects the request-id carried in the record's context as a
// request_id attribute. It must implement all four slog.Handler methods and
// re-wrap on WithAttrs/WithGroup: a wrapper that returned the inner handler
// there would silently drop attributes and groups.
type contextHandler struct {
	inner slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := RequestIDFrom(ctx); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}

// ctxKey is a private context key type so request-id values never collide with
// other packages' context entries.
type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a context carrying the request-id, so any *Context log
// call made under it emits request_id with no explicit plumbing.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request-id stored in ctx, if any.
func RequestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// NewRequestID returns a fresh UUIDv4 string.
func NewRequestID() string {
	return uuid.NewString()
}

// ValidRequestID reports whether an inbound request-id is safe to honor:
// non-empty, at most maxRequestIDLen characters, and within the charset.
func ValidRequestID(id string) bool {
	return requestIDPattern.MatchString(id)
}

// ResolveRequestID honors a well-formed inbound id and otherwise generates a
// fresh one. A malformed/oversized value is never rejected — it is regenerated.
func ResolveRequestID(inbound string) string {
	if ValidRequestID(inbound) {
		return inbound
	}
	return NewRequestID()
}
