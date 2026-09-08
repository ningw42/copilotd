package sse

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestPumpTimelyFrameSurvivesDelayedTimestampReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"unknown\":true}\n\n"
		upstream, send := io.Pipe()
		defer send.Close()
		dst := newBlockingWriteResponseWriter()
		clock := newPausingNowClock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan Result, 1)
		go func() {
			done <- Pump(ctx, cancel, upstream, dst, discardLogger(), Policy{
				Terminal: func(eventType string) bool { return eventType == "response.completed" },
				RenderError: func(w http.ResponseWriter, _ Outcome) error {
					_, err := io.WriteString(w, "stalled")
					return err
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
		// The keepalive's deadline has already been sampled. The next Now
		// belongs to the complete upstream frame, not a stale writer call.
		clock.PauseNext()
		if _, err := io.WriteString(send, terminal); err != nil {
			t.Fatal(err)
		}
		if err := send.Close(); err != nil {
			t.Fatal(err)
		}
		if got := <-clock.captured; !got.Equal(time.Date(2026, time.July, 16, 12, 0, 10, 0, time.UTC)) {
			t.Errorf("frame timestamp = %v, want ten seconds before stall", got)
		}

		// Freeze the reader after time capture, allowing the pump to handle
		// the expired stall while the clock call has not yet returned. Wait
		// for quiescence, not a sleep or scheduler-probability retry loop.
		clock.Advance(10 * time.Second)
		close(dst.releaseWrite)
		synctest.Wait()
		if got := string(dst.written); got != ":\n\n" {
			t.Errorf("before timestamp return, written = %q, want only keepalive", got)
		}
		close(clock.resume)
		result := <-done
		if result.Outcome != OutcomeClean || result.Frames != 1 {
			t.Errorf("Result = %#v, want clean with one terminal", result)
		}
		if got := string(dst.written); got != ":\n\n"+terminal {
			t.Errorf("written = %q, want keepalive then original terminal", got)
		}
	})
}

func TestPumpDelayedTimestampDoesNotAcceptLateOrCanceledFrames(t *testing.T) {
	for _, tc := range []struct {
		name        string
		arrival     time.Duration
		cancel      bool
		wantOutcome Outcome
		wantWire    string
	}{
		{"at_deadline", 20 * time.Second, false, OutcomeClean, ":\n\nevent: response.completed\ndata: {}\n\n"},
		{"late", 21 * time.Second, false, OutcomeStall, ":\n\nstalled"},
		{"client_cancel", 10 * time.Second, true, OutcomeClientCancel, ":\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				upstream, send := io.Pipe()
				defer send.Close()
				dst := newBlockingWriteResponseWriter()
				clock := newPausingNowClock()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan Result, 1)
				go func() {
					done <- Pump(ctx, cancel, upstream, dst, discardLogger(), Policy{
						Terminal: func(eventType string) bool { return eventType == "response.completed" },
						RenderError: func(w http.ResponseWriter, _ Outcome) error {
							_, err := io.WriteString(w, "stalled")
							return err
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
				if tc.arrival >= 20*time.Second {
					clock.Advance(10 * time.Second) // deliver the stall tick at its deadline
				}
				if tc.arrival > 20*time.Second {
					clock.Advance(tc.arrival - 20*time.Second)
				}
				clock.PauseNext()
				if _, err := io.WriteString(send, "event: response.completed\ndata: {}\n\n"); err != nil {
					t.Fatal(err)
				}
				if err := send.Close(); err != nil {
					t.Fatal(err)
				}
				<-clock.captured
				if tc.cancel {
					clock.Advance(10 * time.Second)
					cancel()
				}
				close(dst.releaseWrite)
				synctest.Wait()
				if got := string(dst.written); got != ":\n\n" {
					t.Errorf("before timestamp return, written = %q, want only keepalive", got)
				}
				close(clock.resume)
				result := <-done
				if result.Outcome != tc.wantOutcome {
					t.Errorf("Outcome = %q, want %q", result.Outcome, tc.wantOutcome)
				}
				if got := string(dst.written); got != tc.wantWire {
					t.Errorf("written = %q, want %q", got, tc.wantWire)
				}
			})
		})
	}
}

func TestPumpPendingFrameKeepsReadAheadBoundedAndJoinsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const frame = "data: {\"type\":\"response.output_text.delta\",\"unknown\":true}\n\n"
		upstream, send := io.Pipe()
		dst := newBlockingWriteResponseWriter()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan Result, 1)
		go func() {
			done <- Pump(ctx, cancel, upstream, dst, discardLogger(), Policy{
				Terminal: func(string) bool { return false },
				RenderError: func(http.ResponseWriter, Outcome) error {
					t.Error("client cancellation rendered a terminal")
					return nil
				},
				WriteTimeout: time.Minute,
			}, nil)
		}()
		thirdWrite := make(chan error, 1)
		go func() {
			defer send.Close()
			for range 2 {
				if _, err := io.WriteString(send, frame); err != nil {
					t.Error(err)
					return
				}
			}
			_, err := io.WriteString(send, frame)
			thirdWrite <- err
		}()
		<-dst.writeStarted
		synctest.Wait()
		select {
		case err := <-thirdWrite:
			t.Errorf("third upstream write completed (%v) while the first downstream write was blocked", err)
		default:
		}
		cancel()
		close(dst.releaseWrite)
		result := <-done
		if result.Outcome != OutcomeClientCancel || result.Frames != 1 || result.Fallbacks != 2 {
			t.Errorf("Result = %#v, want client_cancel, one written frame and two observed fallbacks", result)
		}
		if got := string(dst.written); got != frame {
			t.Errorf("written = %q, want only the already-started original frame", got)
		}
		synctest.Wait()
		select {
		case err := <-thirdWrite:
			if err != io.ErrClosedPipe {
				t.Errorf("blocked upstream write = %v, want closed pipe after Pump joined", err)
			}
		default:
			t.Error("Pump returned without releasing the blocked upstream writer")
		}
	})
}

// pausingNowClock widens the external clock's capture-to-return interval. Only
// the explicitly armed call pauses; downstream deadline calls remain usable.
// It models descheduling after a timestamp is captured without inspecting the
// pump's reader handoff or changing any production timeout.
type pausingNowClock struct {
	*manualClock
	pauseMu  sync.Mutex
	pause    bool
	captured chan time.Time
	resume   chan struct{}
}

func newPausingNowClock() *pausingNowClock {
	return &pausingNowClock{
		manualClock: newManualClock(),
		captured:    make(chan time.Time, 1),
		resume:      make(chan struct{}),
	}
}

func (c *pausingNowClock) PauseNext() {
	c.pauseMu.Lock()
	c.pause = true
	c.pauseMu.Unlock()
}

func (c *pausingNowClock) Now() time.Time {
	c.pauseMu.Lock()
	pause := c.pause
	c.pause = false
	c.pauseMu.Unlock()
	now := c.manualClock.Now()
	if pause {
		c.captured <- now
		<-c.resume
	}
	return now
}
