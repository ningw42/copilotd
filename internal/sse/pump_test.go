package sse

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPumpSilentUpstreamStallsAndJoinsReader(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(w http.ResponseWriter, outcome Outcome) error {
				if outcome != OutcomeStall {
					t.Errorf("renderer outcome = %q, want %q", outcome, OutcomeStall)
				}
				if _, err := io.WriteString(w, "native stalled terminal"); err != nil {
					return err
				}
				return http.NewResponseController(w).Flush()
			},
			WriteTimeout: time.Minute,
			IdleTimeout:  90 * time.Second,
			Clock:        clock,
		}, nil)
	}()

	<-body.readStarted
	<-clock.timerCreated
	clock.Advance(90 * time.Second)
	result := <-done

	if result.Outcome != OutcomeStall {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeStall)
	}
	if result.Frames != 0 {
		t.Errorf("Frames = %d, want 0", result.Frames)
	}
	if got := string(dst.written); got != "native stalled terminal" {
		t.Errorf("written = %q, want native stalled terminal", got)
	}
	if dst.flushes != 1 || len(dst.deadlines) != 1 {
		t.Errorf("flushes = %d, deadlines = %d; want bounded and flushed terminal", dst.flushes, len(dst.deadlines))
	}
	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the upstream reader exited")
	}
	select {
	case <-body.closed:
	default:
		t.Error("Pump returned without closing the upstream body")
	}
}

func TestPumpSlowDownstreamWriteIsExcludedFromStallStopwatch(t *testing.T) {
	const first = "event: message_start\ndata: {}\n\n"
	const terminal = "event: message_stop\ndata: {}\n\n"
	upstream, send := io.Pipe()
	dst := newBlockingWriteResponseWriter()
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, upstream, dst, Policy{
			Terminal:     func(eventType string) bool { return eventType == "message_stop" },
			RenderError:  func(http.ResponseWriter, Outcome) error { return errors.New("unexpected synthesized terminal") },
			WriteTimeout: time.Minute,
			IdleTimeout:  10 * time.Second,
			Clock:        clock,
		}, nil)
	}()
	<-clock.timerCreated

	if _, err := io.WriteString(send, first); err != nil {
		t.Fatalf("write first upstream frame: %v", err)
	}
	<-dst.writeStarted
	clock.Advance(30 * time.Second)
	select {
	case <-clock.timerFired:
		t.Error("stall stopwatch fired while a downstream frame write was in progress")
	default:
	}
	close(dst.releaseWrite)

	if _, err := io.WriteString(send, terminal); err != nil {
		t.Fatalf("write terminal upstream frame: %v", err)
	}
	if err := send.Close(); err != nil {
		t.Fatalf("close upstream: %v", err)
	}
	result := <-done

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if got := string(dst.written); got != first+terminal {
		t.Errorf("written = %q, want %q", got, first+terminal)
	}
	if clock.ResetCount() < 1 {
		t.Error("stall stopwatch was not re-armed after the slow write completed")
	}
}

func TestPumpAnthropicPingFramesResetStallStopwatch(t *testing.T) {
	const ping = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	const terminal = "event: message_stop\ndata: {}\n\n"
	upstream, send := io.Pipe()
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, upstream, dst, Policy{
			Terminal:     func(eventType string) bool { return eventType == "message_stop" || eventType == "error" },
			RenderError:  func(http.ResponseWriter, Outcome) error { return errors.New("unexpected synthesized terminal") },
			WriteTimeout: time.Minute,
			IdleTimeout:  10 * time.Second,
			Clock:        clock,
		}, nil)
	}()
	<-clock.timerCreated

	for range 2 {
		if _, err := io.WriteString(send, ping); err != nil {
			t.Fatalf("write ping frame: %v", err)
		}
		<-clock.timerReset
		clock.Advance(9 * time.Second)
		select {
		case <-clock.timerFired:
			t.Fatal("stall stopwatch fired despite a recent upstream ping frame")
		default:
		}
	}
	if _, err := io.WriteString(send, terminal); err != nil {
		t.Fatalf("write terminal frame: %v", err)
	}
	if err := send.Close(); err != nil {
		t.Fatalf("close upstream: %v", err)
	}
	result := <-done

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 3 {
		t.Errorf("Frames = %d, want two pings plus one terminal", result.Frames)
	}
	if got := string(dst.written); got != ping+ping+terminal {
		t.Errorf("written = %q, want pings forwarded verbatim before terminal", got)
	}
}

func TestPumpInjectsKeepaliveAtEachOpenAIIdleGap(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("keepalive-only idle gap unexpectedly synthesized an error")
				return nil
			},
			WriteTimeout:      time.Minute,
			KeepaliveInterval: 15 * time.Second,
			Clock:             clock,
		}, nil)
	}()

	<-body.readStarted
	<-clock.timerCreated
	for range 2 {
		clock.Advance(15 * time.Second)
		<-clock.timerReset
	}
	cancel()
	result := <-done

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q after cancellation", result.Outcome, OutcomeClientCancel)
	}
	if result.Frames != 0 {
		t.Errorf("Frames = %d, want injected comments excluded from upstream frame count", result.Frames)
	}
	if got := string(dst.written); got != ":\n\n:\n\n" {
		t.Errorf("written = %q, want two keepalive comments", got)
	}
	if dst.flushes != 2 || len(dst.deadlines) != 2 {
		t.Errorf("flushes = %d, deadlines = %d; want each keepalive bounded and flushed", dst.flushes, len(dst.deadlines))
	}
}

func TestPumpRealFramesResetOpenAIKeepaliveSchedule(t *testing.T) {
	const first = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"}\n\n"
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	upstream, send := io.Pipe()
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, upstream, dst, Policy{
			Terminal:          func(eventType string) bool { return eventType == "response.completed" },
			RenderError:       func(http.ResponseWriter, Outcome) error { return errors.New("unexpected synthesized terminal") },
			WriteTimeout:      time.Minute,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, identityFrameTransformer{})
	}()
	<-clock.timerCreated

	clock.Advance(6 * time.Second)
	if _, err := io.WriteString(send, first); err != nil {
		t.Fatalf("write first upstream frame: %v", err)
	}
	<-clock.timerReset
	clock.Advance(9 * time.Second)
	if got := string(dst.written); got != first {
		t.Fatalf("written after nine-second post-frame gap = %q, want only real frame", got)
	}
	clock.Advance(time.Second)
	<-clock.timerReset

	if _, err := io.WriteString(send, terminal); err != nil {
		t.Fatalf("write terminal upstream frame: %v", err)
	}
	if err := send.Close(); err != nil {
		t.Fatalf("close upstream: %v", err)
	}
	result := <-done
	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if got := string(dst.written); got != first+":\n\n"+terminal {
		t.Errorf("written = %q, want frame-reset idle gap followed by one keepalive and terminal", got)
	}
}

func TestPumpOpenAIKeepalivesNeverDelayStall(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(w http.ResponseWriter, outcome Outcome) error {
				if outcome != OutcomeStall {
					t.Errorf("renderer outcome = %q, want %q", outcome, OutcomeStall)
				}
				if _, err := io.WriteString(w, "native stalled terminal"); err != nil {
					return err
				}
				return http.NewResponseController(w).Flush()
			},
			WriteTimeout:      time.Minute,
			IdleTimeout:       25 * time.Second,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, nil)
	}()
	<-body.readStarted
	<-clock.timerCreated
	<-clock.timerCreated

	for range 2 {
		clock.Advance(10 * time.Second)
		<-clock.timerReset
	}
	clock.Advance(5 * time.Second)
	result := <-done

	if result.Outcome != OutcomeStall {
		t.Errorf("Outcome = %q, want %q at original stall deadline", result.Outcome, OutcomeStall)
	}
	if got := string(dst.written); got != ":\n\n:\n\nnative stalled terminal" {
		t.Errorf("written = %q, want two keepalives followed by stalled terminal", got)
	}
}

func TestPumpFrameArrivingDuringSlowKeepaliveBeatsStall(t *testing.T) {
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"

	for iteration := range 50 {
		upstream, send := io.Pipe()
		dst := newBlockingWriteResponseWriter()
		clock := newManualClock()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)

		go func() {
			done <- Pump(ctx, cancel, upstream, dst, Policy{
				Terminal: func(eventType string) bool { return eventType == "response.completed" },
				RenderError: func(w http.ResponseWriter, _ Outcome) error {
					if _, err := io.WriteString(w, "stalled"); err != nil {
						return err
					}
					return http.NewResponseController(w).Flush()
				},
				WriteTimeout:      time.Minute,
				IdleTimeout:       20 * time.Second,
				KeepaliveInterval: 10 * time.Second,
				Clock:             clock,
			}, nil)
		}()

		<-clock.timerCreated
		<-clock.timerCreated
		clock.Advance(10 * time.Second)
		<-dst.writeStarted
		<-clock.nowCalled // keepalive write deadline

		writeDone := make(chan error, 1)
		go func() {
			_, err := io.WriteString(send, terminal)
			if err == nil {
				err = send.Close()
			}
			writeDone <- err
		}()
		select {
		case err := <-writeDone:
			if err != nil {
				t.Fatalf("iteration %d: write terminal upstream: %v", iteration, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: reader did not receive terminal while keepalive write was blocked", iteration)
		}
		select {
		case <-clock.nowCalled: // reader timestamped the complete frame
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: reader did not timestamp terminal before stall", iteration)
		}

		clock.Advance(10 * time.Second)
		close(dst.releaseWrite)

		var result Result
		select {
		case result = <-done:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Pump did not return", iteration)
		}
		if result.Outcome != OutcomeClean {
			t.Fatalf("iteration %d: Outcome = %q, want %q", iteration, result.Outcome, OutcomeClean)
		}
		if got := string(dst.written); got != ":\n\n"+terminal {
			t.Fatalf("iteration %d: written = %q, want keepalive then queued terminal", iteration, got)
		}
	}
}

func TestPumpKeepaliveWriteFailureIsClientCancelWithNoFurtherOutput(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{
		header:   make(http.Header),
		writeErr: errors.New("client stopped draining keepalive"),
	}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("keepalive write failure unexpectedly synthesized an error")
				return nil
			},
			WriteTimeout:      time.Minute,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, nil)
	}()
	<-body.readStarted
	<-clock.timerCreated
	clock.Advance(10 * time.Second)
	result := <-done

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("failed keepalive left output %q with %d flushes, want no wire output", dst.written, dst.flushes)
	}
	clock.Advance(time.Minute)
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("output continued after keepalive failure: %q with %d flushes", dst.written, dst.flushes)
	}
}

func TestPumpCancellationSuppressesReadyKeepaliveAndFurtherOutput(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("cancellation unexpectedly synthesized an error")
				return nil
			},
			WriteTimeout:      time.Minute,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, nil)
	}()
	<-body.readStarted
	<-clock.timerCreated
	cancel()
	clock.Advance(10 * time.Second)
	result := <-done

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("cancellation race wrote %q with %d flushes, want no wire output", dst.written, dst.flushes)
	}
	clock.Advance(time.Minute)
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("output continued after cancellation: %q with %d flushes", dst.written, dst.flushes)
	}
}

func TestPumpForwardsUpstreamErrorTerminalWithoutSynthesis(t *testing.T) {
	const terminal = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(terminal)), dst, Policy{
		Terminal: func(eventType string) bool {
			return eventType == "message_stop" || eventType == "error"
		},
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("upstream error terminal was doubled with a synthesized error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 1 || string(dst.written) != terminal {
		t.Errorf("Result = %#v, written = %q; want one verbatim upstream terminal", result, dst.written)
	}
}

func TestPumpTerminalFollowedByReadErrorRemainsClean(t *testing.T) {
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := io.NopCloser(&readerEndingWithError{
		data: []byte(terminal),
		err:  errors.New("upstream failed after terminal"),
	})
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(eventType string) bool { return eventType == "message_stop" },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("read error after an upstream terminal was doubled with a synthesized error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 1 || string(dst.written) != terminal {
		t.Errorf("Result = %#v, written = %q; want one verbatim terminal", result, dst.written)
	}
}

func TestPumpTerminalFollowedByIdleTimeoutRemainsClean(t *testing.T) {
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := newBlockingAfterFrameBody(terminal)
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(eventType string) bool { return eventType == "message_stop" },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("idle timeout after an upstream terminal was doubled with a synthesized error")
				return nil
			},
			WriteTimeout: time.Minute,
			IdleTimeout:  10 * time.Second,
			Clock:        clock,
		}, nil)
	}()

	<-clock.timerCreated
	<-clock.timerReset
	clock.Advance(10 * time.Second)
	result := <-done

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 1 || string(dst.written) != terminal {
		t.Errorf("Result = %#v, written = %q; want one verbatim terminal", result, dst.written)
	}
}

func TestPumpTerminalFollowedByKeepaliveTickRemainsClean(t *testing.T) {
	const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	body := newBlockingAfterFrameBody(terminal)
	dst := &deadlineResponseWriter{header: make(http.Header)}
	clock := newManualClock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)

	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(eventType string) bool { return eventType == "response.completed" },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("keepalive tick after an upstream terminal synthesized an error")
				return nil
			},
			WriteTimeout:      time.Minute,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, nil)
	}()

	<-clock.timerCreated
	<-clock.timerReset
	clock.Advance(10 * time.Second)
	result := <-done

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 1 || string(dst.written) != terminal {
		t.Errorf("Result = %#v, written = %q; want one terminal and no keepalive", result, dst.written)
	}
	if dst.flushes != 1 {
		t.Errorf("flushes = %d, want only the upstream terminal flush", dst.flushes)
	}
}

func TestPumpClientContextCancellationStopsUpstreamAndJoinsReader(t *testing.T) {
	body := newBlockingAfterFrameBody("")
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan Result, 1)
	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("client cancellation unexpectedly synthesized an error")
				return nil
			},
			WriteTimeout: time.Second,
			Clock:        RealClock{},
		}, nil)
	}()

	select {
	case <-body.readStarted:
	case <-time.After(time.Second):
		_ = body.Close()
		t.Fatal("reader did not start")
	}
	cancel()

	var result Result
	select {
	case result = <-done:
	case <-time.After(time.Second):
		_ = body.Close()
		<-done
		t.Fatal("Pump did not return promptly after client context cancellation")
	}
	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("client cancellation wrote %q with %d flushes, want no wire output", dst.written, dst.flushes)
	}
	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the upstream reader exited")
	}
	select {
	case <-body.closed:
	default:
		t.Error("Pump returned without closing the upstream body")
	}
}

func TestPumpDownstreamWriteFailureIsClientCancelAndJoinsReader(t *testing.T) {
	body := newBlockingAfterFrameBody("event: message_start\ndata: {}\n\n")
	dst := &deadlineResponseWriter{
		header:   make(http.Header),
		writeErr: errors.New("downstream write failed"),
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("write failure unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)
	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("downstream write failure left output %q with %d flushes, want none", dst.written, dst.flushes)
	}

	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the blocked reader exited")
	}
	select {
	case <-body.closed:
	default:
		t.Error("Pump returned without closing the upstream body")
	}
}

func TestPumpDownstreamFlushFailureIsClientCancelAndJoinsReader(t *testing.T) {
	body := newBlockingAfterFrameBody("event: message_start\ndata: {}\n\n")
	dst := &deadlineResponseWriter{
		header:   make(http.Header),
		flushErr: errors.New("downstream flush failed"),
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("flush failure unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if got := string(dst.written); got != "event: message_start\ndata: {}\n\n" {
		t.Errorf("written bytes = %q, want only the frame written before Flush failed", got)
	}
	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the blocked reader exited")
	}
	select {
	case <-body.closed:
	default:
		t.Error("Pump returned without closing the upstream body")
	}
}

func TestPumpSetWriteDeadlineFailureIsClientCancelWithNoWireOutput(t *testing.T) {
	body := newBlockingAfterFrameBody("event: message_start\ndata: {}\n\n")
	dst := &deadlineResponseWriter{
		header:      make(http.Header),
		deadlineErr: errors.New("write deadline exceeded"),
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("write-deadline failure unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("write-deadline failure left output %q with %d flushes, want none", dst.written, dst.flushes)
	}
	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the blocked reader exited")
	}
	select {
	case <-body.closed:
	default:
		t.Error("Pump returned without closing the upstream body")
	}
}

func TestPumpExceededWriteDeadlineIsClientCancelWithNoWireOutput(t *testing.T) {
	body := newBlockingAfterFrameBody("event: message_start\ndata: {}\n\n")
	dst := &deadlineResponseWriter{
		header:   make(http.Header),
		writeErr: context.DeadlineExceeded,
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("exceeded write deadline unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("exceeded write deadline left output %q with %d flushes, want none", dst.written, dst.flushes)
	}
	select {
	case <-body.readExited:
	default:
		t.Error("Pump returned before the blocked reader exited")
	}
}

func TestPumpSynthesizedTerminalWriteFailureBecomesClientCancel(t *testing.T) {
	dst := &deadlineResponseWriter{
		header:   make(http.Header),
		writeErr: errors.New("client disconnected before synthesized terminal"),
	}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader("")), dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(w http.ResponseWriter, _ Outcome) error {
			if _, err := io.WriteString(w, "synthesized terminal"); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.written) != 0 || dst.flushes != 0 {
		t.Errorf("failed synthesized terminal left output %q with %d flushes, want none", dst.written, dst.flushes)
	}
}

func TestPumpCancellationWinsConcurrentUpstreamReadError(t *testing.T) {
	for range 100 {
		body := newConcurrentErrorBody()
		dst := &deadlineResponseWriter{header: make(http.Header)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)
		go func() {
			done <- Pump(ctx, cancel, body, dst, Policy{
				Terminal: func(string) bool { return false },
				RenderError: func(http.ResponseWriter, Outcome) error {
					t.Error("concurrent cancellation rendered an upstream error")
					return nil
				},
				WriteTimeout: time.Second,
				Clock:        RealClock{},
			}, nil)
		}()
		<-body.readStarted
		cancel()
		close(body.returnError)
		result := <-done
		if result.Outcome != OutcomeClientCancel {
			t.Fatalf("Outcome = %q, want %q when cancellation races a read error", result.Outcome, OutcomeClientCancel)
		}
		if len(dst.written) != 0 || dst.flushes != 0 {
			t.Fatalf("cancellation race wrote %q with %d flushes, want no wire output", dst.written, dst.flushes)
		}
	}
}

func TestPumpCancellationWinsConcurrentStallTick(t *testing.T) {
	for range 100 {
		body := newBlockingAfterFrameBody("")
		dst := &deadlineResponseWriter{header: make(http.Header)}
		clock := newManualClock()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result, 1)
		go func() {
			done <- Pump(ctx, cancel, body, dst, Policy{
				Terminal: func(string) bool { return false },
				RenderError: func(http.ResponseWriter, Outcome) error {
					t.Error("concurrent cancellation rendered a stalled terminal")
					return nil
				},
				WriteTimeout: time.Minute,
				IdleTimeout:  10 * time.Second,
				Clock:        clock,
			}, nil)
		}()
		<-body.readStarted
		<-clock.timerCreated
		cancel()
		clock.Advance(10 * time.Second)
		result := <-done
		if result.Outcome != OutcomeClientCancel {
			t.Fatalf("Outcome = %q, want %q when cancellation races stall", result.Outcome, OutcomeClientCancel)
		}
		if len(dst.written) != 0 || dst.flushes != 0 {
			t.Fatalf("cancellation race wrote %q with %d flushes, want no wire output", dst.written, dst.flushes)
		}
	}
}

func TestPumpWedgedClientWriteDeadlineIsClientCancelAndReleasesUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamCancelled)
		w.Header().Set("Content-Type", "text/event-stream")
		frame := "event: content_block_delta\ndata: " + strings.Repeat("x", 32<<10) + "\n\n"
		for {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer upstream.Close()

	results := make(chan Result, 1)
	handlerErrors := make(chan error, 1)
	rendered := make(chan struct{}, 1)
	downstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		outReq, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		if err != nil {
			handlerErrors <- err
			return
		}
		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			handlerErrors <- err
			return
		}
		results <- Pump(ctx, cancel, resp.Body, w, Policy{
			Terminal: func(string) bool { return false },
			RenderError: func(http.ResponseWriter, Outcome) error {
				rendered <- struct{}{}
				return nil
			},
			WriteTimeout: 100 * time.Millisecond,
			Clock:        RealClock{},
		}, nil)
	}))
	downstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetWriteBuffer(1024)
			}
		}
	}
	downstream.Start()
	defer downstream.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(downstream.URL, "http://"))
	if err != nil {
		t.Fatalf("dial downstream: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	if _, err := io.WriteString(conn, "GET /stream HTTP/1.1\r\nHost: copilotd.test\r\n\r\n"); err != nil {
		t.Fatalf("write downstream request: %v", err)
	}

	select {
	case err := <-handlerErrors:
		t.Fatalf("stream handler setup: %v", err)
	case result := <-results:
		if result.Outcome != OutcomeClientCancel {
			t.Fatalf("Outcome = %q, want %q for a connected client that stopped draining", result.Outcome, OutcomeClientCancel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Pump did not return after the wedged client's write deadline")
	}
	select {
	case <-rendered:
		t.Error("wedged client caused a synthesized terminal")
	default:
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("Pump returned before the upstream connection was released")
	}
}

func TestPumpForwardsCleanStreamVerbatim(t *testing.T) {
	const first = "event: message_start\ndata: {\"type\":\"message_start\",\"unknown\":true}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(first+terminal)), dst, Policy{
		Terminal: func(eventType string) bool {
			return eventType == "message_stop" || eventType == "error"
		},
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("clean stream unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if result.Frames != 2 {
		t.Errorf("Frames = %d, want 2", result.Frames)
	}
	if got := string(dst.written); got != first+terminal {
		t.Errorf("written bytes = %q, want exact upstream bytes %q", got, first+terminal)
	}
	if dst.flushes != 2 {
		t.Errorf("flushes = %d, want one per frame (2)", dst.flushes)
	}
	if len(dst.deadlines) != 2 {
		t.Errorf("write deadlines = %d, want one per frame (2)", len(dst.deadlines))
	}
	select {
	case <-ctx.Done():
	default:
		t.Error("upstream context was not cancelled before Pump returned")
	}
}

func TestPumpSynthesizesTerminalOnUpstreamReadError(t *testing.T) {
	const upstream = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	const synthesized = "native upstream failure terminal"
	boom := errors.New("upstream read failed")
	body := io.NopCloser(&readerEndingWithError{
		data: []byte(upstream + "event: incomplete\ndata: partial"),
		err:  boom,
	})
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, body, dst, Policy{
		Terminal: func(eventType string) bool {
			return eventType == "message_stop" || eventType == "error"
		},
		RenderError: func(w http.ResponseWriter, outcome Outcome) error {
			if outcome != OutcomeUpstreamError {
				t.Errorf("renderer outcome = %q, want %q", outcome, OutcomeUpstreamError)
			}
			if _, err := io.WriteString(w, synthesized); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeUpstreamError {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeUpstreamError)
	}
	if result.Frames != 1 {
		t.Errorf("Frames = %d, want 1", result.Frames)
	}
	if got := string(dst.written); got != upstream+synthesized {
		t.Errorf("written bytes = %q, want complete upstream frame then renderer bytes %q", got, upstream+synthesized)
	}
	if dst.flushes != 2 || len(dst.deadlines) != 2 {
		t.Errorf("flushes = %d, deadlines = %d; want both writes flushed and bounded", dst.flushes, len(dst.deadlines))
	}
}

func TestPumpSynthesizesTerminalWhenEOFHasNoTerminal(t *testing.T) {
	const upstream = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	const synthesized = "native synthesized terminal"
	dst := &deadlineResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(upstream)), dst, Policy{
		Terminal: func(eventType string) bool {
			return eventType == "message_stop" || eventType == "error"
		},
		RenderError: func(w http.ResponseWriter, outcome Outcome) error {
			if outcome != OutcomeSynthesized {
				t.Errorf("renderer outcome = %q, want %q", outcome, OutcomeSynthesized)
			}
			if _, err := io.WriteString(w, synthesized); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, nil)

	if result.Outcome != OutcomeSynthesized {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeSynthesized)
	}
	if result.Frames != 1 {
		t.Errorf("Frames = %d, want 1", result.Frames)
	}
	if got := string(dst.written); got != upstream+synthesized {
		t.Errorf("written bytes = %q, want upstream frame then renderer bytes %q", got, upstream+synthesized)
	}
	if dst.flushes != 2 {
		t.Errorf("flushes = %d, want upstream frame and synthesized terminal flushed", dst.flushes)
	}
	if len(dst.deadlines) != 2 {
		t.Errorf("write deadlines = %d, want upstream frame and synthesized terminal bounded", len(dst.deadlines))
	}
}

type blockingAfterFrameBody struct {
	frame       []byte
	closed      chan struct{}
	readStarted chan struct{}
	readExited  chan struct{}
}

func newBlockingAfterFrameBody(frame string) *blockingAfterFrameBody {
	return &blockingAfterFrameBody{
		frame:       []byte(frame),
		closed:      make(chan struct{}),
		readStarted: make(chan struct{}),
		readExited:  make(chan struct{}),
	}
}

func (b *blockingAfterFrameBody) Read(p []byte) (int, error) {
	select {
	case <-b.readStarted:
	default:
		close(b.readStarted)
	}
	if len(b.frame) > 0 {
		n := copy(p, b.frame)
		b.frame = b.frame[n:]
		return n, nil
	}
	<-b.closed
	close(b.readExited)
	return 0, io.ErrClosedPipe
}

func (b *blockingAfterFrameBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type concurrentErrorBody struct {
	readStarted chan struct{}
	returnError chan struct{}
	closed      chan struct{}
}

func newConcurrentErrorBody() *concurrentErrorBody {
	return &concurrentErrorBody{
		readStarted: make(chan struct{}),
		returnError: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

func (b *concurrentErrorBody) Read([]byte) (int, error) {
	close(b.readStarted)
	select {
	case <-b.returnError:
		return 0, errors.New("upstream failed as client disconnected")
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}

func (b *concurrentErrorBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type manualClock struct {
	mu           sync.Mutex
	now          time.Time
	timers       []*manualTimer
	nowCalled    chan struct{}
	timerCreated chan struct{}
	timerReset   chan struct{}
	timerFired   chan struct{}
	resetCount   int
}

func newManualClock() *manualClock {
	return &manualClock{
		now:          time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
		nowCalled:    make(chan struct{}, 64),
		timerCreated: make(chan struct{}, 16),
		timerReset:   make(chan struct{}, 16),
		timerFired:   make(chan struct{}, 16),
	}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	select {
	case c.nowCalled <- struct{}{}:
	default:
	}
	return now
}

func (c *manualClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	timer := &manualTimer{
		clock:    c,
		channel:  make(chan time.Time, 1),
		deadline: c.now.Add(d),
		active:   true,
	}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.timerCreated <- struct{}{}
	return timer
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var fired []*manualTimer
	for _, timer := range c.timers {
		if timer.active && !timer.deadline.After(now) {
			timer.active = false
			fired = append(fired, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range fired {
		timer.channel <- now
		c.timerFired <- struct{}{}
	}
}

func (c *manualClock) ResetCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resetCount
}

type manualTimer struct {
	clock    *manualClock
	channel  chan time.Time
	deadline time.Time
	active   bool
}

func (t *manualTimer) C() <-chan time.Time { return t.channel }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}

func (t *manualTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	wasActive := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	t.clock.resetCount++
	t.clock.mu.Unlock()
	t.clock.timerReset <- struct{}{}
	return wasActive
}

type blockingWriteResponseWriter struct {
	*deadlineResponseWriter
	writeStarted chan struct{}
	releaseWrite chan struct{}
	blockOnce    sync.Once
}

func newBlockingWriteResponseWriter() *blockingWriteResponseWriter {
	return &blockingWriteResponseWriter{
		deadlineResponseWriter: &deadlineResponseWriter{header: make(http.Header)},
		writeStarted:           make(chan struct{}),
		releaseWrite:           make(chan struct{}),
	}
}

func (w *blockingWriteResponseWriter) Write(p []byte) (int, error) {
	block := false
	w.blockOnce.Do(func() {
		block = true
		close(w.writeStarted)
	})
	if block {
		<-w.releaseWrite
	}
	return w.deadlineResponseWriter.Write(p)
}
