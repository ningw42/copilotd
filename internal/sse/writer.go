package sse

import (
	"net/http"
	"time"
)

// Writer bounds every downstream write with a fresh absolute deadline. The
// clock is injected so callers can test deadline advancement deterministically.
type Writer struct {
	dst        http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
	now        func() time.Time
}

// NewWriter wraps dst with a per-write deadline. The deadline is reset before
// every delegated Write, allowing a client that keeps draining to continually
// push the deadline forward.
func NewWriter(dst http.ResponseWriter, timeout time.Duration, now func() time.Time) *Writer {
	return &Writer{
		dst:        dst,
		controller: http.NewResponseController(dst),
		timeout:    timeout,
		now:        now,
	}
}

func (w *Writer) Write(p []byte) (int, error) {
	if err := w.controller.SetWriteDeadline(w.now().Add(w.timeout)); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
}

// Header and WriteHeader make Writer usable by renderers that accept an
// http.ResponseWriter. Stream responses are already committed before the
// renderer is called; delegating preserves the normal ResponseWriter contract.
func (w *Writer) Header() http.Header { return w.dst.Header() }

func (w *Writer) WriteHeader(statusCode int) { w.dst.WriteHeader(statusCode) }

// Unwrap lets ResponseController reach Flush and other optional capabilities on
// the real downstream writer while all byte writes still pass through Writer.
func (w *Writer) Unwrap() http.ResponseWriter { return w.dst }
