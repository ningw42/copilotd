package sse

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Outcome describes how an SSE stream ended.
type Outcome string

const (
	OutcomeClean         Outcome = "clean"
	OutcomeSynthesized   Outcome = "synthesized"
	OutcomeStall         Outcome = "stall"
	OutcomeClientCancel  Outcome = "client_cancel"
	OutcomeUpstreamError Outcome = "upstream_error"
)

// Result is the request-level summary returned by Pump. Frames counts
// successfully forwarded upstream frames; synthesized frames are not upstream
// frames and are therefore not included. Fallbacks counts frames classified via
// data.type because their event line was absent or empty.
type Result struct {
	Outcome   Outcome
	Frames    int
	Fallbacks int
}

// Policy supplies the surface-specific decisions while Pump owns only stream
// mechanics. RenderError must write surface-native bytes to the supplied
// deadline-bounded ResponseWriter.
type Policy struct {
	Terminal     func(eventType string) bool
	RenderError  func(http.ResponseWriter, Outcome) error
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	// KeepaliveInterval is an idle-gap interval. Zero disables injection.
	KeepaliveInterval time.Duration
	Clock             Clock
	OnFallback        func()
}

type readResult struct {
	frame      Frame
	err        error
	receivedAt time.Time
}

// Pump forwards complete upstream SSE frames verbatim and flushes each one.
// Every exit follows cancel-then-join: cancel the upstream context, close the
// response body, then wait for the reader goroutine to exit.
func Pump(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser, dst http.ResponseWriter, policy Policy) (result Result) {
	clock := policy.Clock
	if clock == nil {
		clock = RealClock{}
	}
	writer := NewWriter(dst, policy.WriteTimeout, clock.Now)
	controller := http.NewResponseController(writer)
	var stall Timer
	var stallC <-chan time.Time
	if policy.IdleTimeout > 0 {
		// Pump is entered immediately after the forwarder commits the response,
		// so creating the timer here arms the stopwatch at the commit point.
		stall = clock.NewTimer(policy.IdleTimeout)
		stallC = stall.C()
		defer stopTimer(stall)
	}
	var keepalive Timer
	var keepaliveC <-chan time.Time
	if policy.KeepaliveInterval > 0 {
		keepalive = clock.NewTimer(policy.KeepaliveInterval)
		keepaliveC = keepalive.C()
		defer stopTimer(keepalive)
	}

	reads := make(chan readResult)
	readerExited := make(chan struct{})
	fallbacks := 0
	reader := NewReader(body, func() {
		fallbacks++
		if policy.OnFallback != nil {
			policy.OnFallback()
		}
	})
	go func() {
		defer close(readerExited)
		defer close(reads)
		for {
			frame, err := reader.Read()
			select {
			case reads <- readResult{frame: frame, err: err, receivedAt: clock.Now()}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = body.Close()
		<-readerExited
		result.Fallbacks = fallbacks
	}()

	sawTerminal := false
	for {
		var read readResult
		haveRead := false
		stallFired := false
		select {
		case <-ctx.Done():
			result.Outcome = OutcomeClientCancel
			return result
		case stallAt := <-stallC:
			// Client cancellation is authoritative when it races a stall tick.
			if ctx.Err() != nil {
				result.Outcome = OutcomeClientCancel
				return result
			}
			// A frame received no later than the stall tick proves upstream
			// progress even when a slow downstream keepalive write delayed this
			// loop from accepting the reader's handoff.
			select {
			case next, ok := <-reads:
				if !ok {
					if ctx.Err() != nil {
						result.Outcome = OutcomeClientCancel
					}
					return result
				}
				if next.receivedAt.After(stallAt) {
					stallFired = true
				} else {
					read = next
					haveRead = true
				}
			default:
				stallFired = true
			}
		case <-keepaliveC:
			// Client cancellation is authoritative when it races a keepalive tick.
			if ctx.Err() != nil {
				result.Outcome = OutcomeClientCancel
				return result
			}
			// If a real frame is already waiting, it ended the idle gap before
			// this tick. Prefer it even when select chose both ready arms fairly.
			select {
			case next, ok := <-reads:
				if !ok {
					if ctx.Err() != nil {
						result.Outcome = OutcomeClientCancel
					}
					return result
				}
				read = next
				haveRead = true
			default:
				if sawTerminal {
					result.Outcome = OutcomeClean
					return result
				}
				if _, err := writer.Write([]byte(":\n\n")); err != nil {
					result.Outcome = OutcomeClientCancel
					return result
				}
				if err := controller.Flush(); err != nil {
					result.Outcome = OutcomeClientCancel
					return result
				}
				resetTimer(keepalive, policy.KeepaliveInterval)
				continue
			}
		case next, ok := <-reads:
			if !ok {
				if ctx.Err() != nil {
					result.Outcome = OutcomeClientCancel
				}
				return result
			}
			read = next
			haveRead = true
		}
		// If a read and stall tick were both ready and select chose the read,
		// order them by when the reader completed the frame rather than by the
		// scheduler's random case choice.
		if haveRead && stall != nil {
			select {
			case stallAt := <-stallC:
				if read.receivedAt.After(stallAt) {
					stallFired = true
					haveRead = false
				}
			default:
			}
		}
		if stallFired {
			if ctx.Err() != nil {
				result.Outcome = OutcomeClientCancel
				return result
			}
			if sawTerminal {
				result.Outcome = OutcomeClean
				return result
			}
			result.Outcome = OutcomeStall
			if err := policy.RenderError(writer, result.Outcome); err != nil {
				result.Outcome = OutcomeClientCancel
			}
			return result
		}
		if read.err == nil {
			// Stop the stopwatch at receipt, before any downstream work.
			stopTimer(stall)
			// A real upstream frame ends the keepalive idle gap. Re-arm only
			// after the frame is successfully delivered.
			stopTimer(keepalive)
		}
		// Cancellation can make both select arms ready at once. Re-checking it
		// here keeps the client-disconnect outcome authoritative and prevents a
		// concurrent upstream read error from rendering a terminal afterward.
		if ctx.Err() != nil {
			result.Outcome = OutcomeClientCancel
			return result
		}
		if read.err != nil {
			if sawTerminal {
				result.Outcome = OutcomeClean
				return result
			}
			if read.err == io.EOF {
				result.Outcome = OutcomeSynthesized
				if err := policy.RenderError(writer, result.Outcome); err != nil {
					result.Outcome = OutcomeClientCancel
				}
				return result
			}
			result.Outcome = OutcomeUpstreamError
			if err := policy.RenderError(writer, result.Outcome); err != nil {
				result.Outcome = OutcomeClientCancel
			}
			return result
		}
		if _, err := writer.Write(read.frame.Raw); err != nil {
			result.Outcome = OutcomeClientCancel
			return result
		}
		if err := controller.Flush(); err != nil {
			result.Outcome = OutcomeClientCancel
			return result
		}
		result.Frames++
		if policy.Terminal(read.frame.Type) {
			sawTerminal = true
		}
		if stall != nil {
			resetTimer(stall, policy.IdleTimeout)
		}
		if keepalive != nil {
			resetTimer(keepalive, policy.KeepaliveInterval)
		}
	}
}

func stopTimer(timer Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
}

func resetTimer(timer Timer, d time.Duration) {
	stopTimer(timer)
	timer.Reset(d)
}
