package sse

import (
	"net/http"
	"testing"
	"time"
)

type deadlineResponseWriter struct {
	header          http.Header
	deadlines       []time.Time
	deadlineAtWrite []time.Time
	currentDeadline time.Time
	written         []byte
	flushes         int
	writeErr        error
	flushErr        error
	deadlineErr     error
}

func (w *deadlineResponseWriter) Header() http.Header { return w.header }

func (w *deadlineResponseWriter) WriteHeader(int) {}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.currentDeadline = deadline
	w.deadlines = append(w.deadlines, deadline)
	return w.deadlineErr
}

func (w *deadlineResponseWriter) Write(p []byte) (int, error) {
	w.deadlineAtWrite = append(w.deadlineAtWrite, w.currentDeadline)
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.written = append(w.written, p...)
	return len(p), nil
}

func (w *deadlineResponseWriter) Flush() { w.flushes++ }

func (w *deadlineResponseWriter) FlushError() error {
	w.flushes++
	return w.flushErr
}

func TestWriterResetsDeadlineBeforeEveryWrite(t *testing.T) {
	base := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(3 * time.Second)}
	clockCalls := 0
	clock := func() time.Time {
		got := times[clockCalls]
		clockCalls++
		return got
	}

	dst := &deadlineResponseWriter{header: make(http.Header)}
	w := NewWriter(dst, 5*time.Second, clock)
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatalf("first Write() error = %v", err)
	}
	if _, err := w.Write([]byte("second")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}

	want := []time.Time{base.Add(5 * time.Second), base.Add(8 * time.Second)}
	if len(dst.deadlines) != 2 {
		t.Fatalf("SetWriteDeadline calls = %d, want 2", len(dst.deadlines))
	}
	for i := range want {
		if !dst.deadlines[i].Equal(want[i]) {
			t.Errorf("deadline[%d] = %v, want %v", i, dst.deadlines[i], want[i])
		}
		if !dst.deadlineAtWrite[i].Equal(want[i]) {
			t.Errorf("deadline observed by Write[%d] = %v, want %v (deadline must be set first)", i, dst.deadlineAtWrite[i], want[i])
		}
	}
	if got := string(dst.written); got != "firstsecond" {
		t.Errorf("written bytes = %q, want firstsecond", got)
	}
}
