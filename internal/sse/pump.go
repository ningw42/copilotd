package sse

import (
	"context"
	"io"
	"log/slog"
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
	// OutcomeShimError means the injected FrameTransformer failed before a
	// terminal reached the client. In production, the transformer is the shim
	// chain's stream adapter.
	OutcomeShimError Outcome = "shim_error"
)

// FrameTransformer is the payload-opaque transform boundary owned by the SSE
// engine. Transform maps one upstream frame to zero or more client-facing
// frames; Finalize releases frames held until a pre-terminal stream teardown.
type FrameTransformer interface {
	Transform(context.Context, Frame) ([]Frame, error)
	Finalize(context.Context) ([]Frame, error)
}

// Result is the request-level summary returned by Pump. Frames counts
// successfully written client-facing frames; synthesized frames are not
// transformer output and are therefore not included. Fallbacks counts upstream
// frames classified via data.type because their event line was absent or empty.
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
	// Logger receives metadata-only warnings for FrameTransformer errors hidden
	// from the wire by no-double-up. Nil uses slog.Default.
	Logger *slog.Logger
	// SuppressedShimErrors counts those same post-terminal errors when non-nil.
	SuppressedShimErrors *SuppressedShimErrorCounter
}

type readResult struct {
	frame      Frame
	err        error
	receivedAt time.Time
}

// Pump transforms complete upstream SSE frames when transformer is non-nil,
// writes and flushes each resulting client-facing frame, and preserves upstream
// frames verbatim when transformer is nil. Every exit follows cancel-then-join:
// cancel the upstream context, close the response body, then wait for the reader
// goroutine to exit.
func Pump(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser, dst http.ResponseWriter, policy Policy, transformer FrameTransformer) (result Result) {
	clock := policy.Clock
	if clock == nil {
		clock = RealClock{}
	}
	logger := policy.Logger
	if logger == nil {
		logger = slog.Default()
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
	writeFrames := func(frames []Frame) bool {
		for _, frame := range frames {
			if _, err := writer.Write(frame.Raw); err != nil {
				result.Outcome = OutcomeClientCancel
				return false
			}
			if err := controller.Flush(); err != nil {
				result.Outcome = OutcomeClientCancel
				return false
			}
			result.Frames++
			if policy.Terminal(frame.Type) {
				sawTerminal = true
			}
			if keepalive != nil {
				resetTimer(keepalive, policy.KeepaliveInterval)
			}
		}
		return true
	}
	renderOutcome := func(outcome Outcome) {
		result.Outcome = outcome
		if err := policy.RenderError(writer, outcome); err != nil {
			result.Outcome = OutcomeClientCancel
		}
	}
	recordSuppressedShimError := func(stage string) {
		if policy.SuppressedShimErrors != nil {
			policy.SuppressedShimErrors.Increment()
		}
		logger.WarnContext(ctx, "suppressed post-terminal shim error", slog.String("stage", stage))
	}
	writeKeepalive := func() bool {
		if _, err := writer.Write([]byte(":\n\n")); err != nil {
			result.Outcome = OutcomeClientCancel
			return false
		}
		if err := controller.Flush(); err != nil {
			result.Outcome = OutcomeClientCancel
			return false
		}
		resetTimer(keepalive, policy.KeepaliveInterval)
		return true
	}
	finishPreTerminal := func(cause Outcome) {
		if transformer != nil {
			frames, err := transformer.Finalize(ctx)
			// A disconnect is authoritative even if it arrives while Finalize is
			// running. Its output is still held and must not reach the client.
			if ctx.Err() != nil {
				result.Outcome = OutcomeClientCancel
				return
			}
			if !writeFrames(frames) {
				return
			}
			if sawTerminal {
				if err != nil {
					recordSuppressedShimError("finalize")
				}
				result.Outcome = OutcomeClean
				return
			}
			if err != nil {
				renderOutcome(OutcomeShimError)
				return
			}
		}
		renderOutcome(cause)
	}
	for {
		var read readResult
		haveRead := false
		stallFired := false
		keepaliveFired := false
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
			keepaliveFired = true
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
				if !writeKeepalive() {
					return result
				}
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
			finishPreTerminal(OutcomeStall)
			return result
		}
		if read.err == nil {
			// Stop the stopwatch at receipt, before any downstream work.
			stopTimer(stall)
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
				finishPreTerminal(OutcomeSynthesized)
				return result
			}
			finishPreTerminal(OutcomeUpstreamError)
			return result
		}
		frames := []Frame{read.frame}
		if transformer != nil {
			var err error
			frames, err = transformer.Transform(ctx, read.frame)
			if ctx.Err() != nil {
				result.Outcome = OutcomeClientCancel
				return result
			}
			if err != nil {
				result.Outcome = OutcomeShimError
				if sawTerminal {
					recordSuppressedShimError("transform")
					result.Outcome = OutcomeClean
				} else if renderErr := policy.RenderError(writer, result.Outcome); renderErr != nil {
					result.Outcome = OutcomeClientCancel
				}
				return result
			}
		}
		if !writeFrames(frames) {
			return result
		}
		if keepaliveFired && len(frames) == 0 {
			if sawTerminal {
				result.Outcome = OutcomeClean
				return result
			}
			if !writeKeepalive() {
				return result
			}
		}
		if stall != nil {
			resetTimer(stall, policy.IdleTimeout)
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
