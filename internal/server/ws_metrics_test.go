package server

import (
	"sync"
	"testing"

	"github.com/ningw42/copilotd/internal/wsforward"
)

func TestWsCountersExposeEveryCanonicalAxis(t *testing.T) {
	accepts := NewWsAcceptCounter()
	for _, outcome := range []wsforward.AcceptOutcome{
		wsforward.AcceptEstablished,
		wsforward.AcceptRejected,
		wsforward.AcceptDialFailed,
	} {
		accepts.ObserveAccept(outcome)
		if got := accepts.Count(outcome); got != 1 {
			t.Errorf("accept outcome %q count = %d, want 1", outcome, got)
		}
	}

	terminals := NewWsSessionTerminalCounter()
	for _, terminal := range []wsforward.SessionTerminal{
		wsforward.SessionClientClosed,
		wsforward.SessionUpstreamClosed,
		wsforward.SessionError,
	} {
		terminals.ObserveSessionTerminal(terminal)
		if got := terminals.Count(terminal); got != 1 {
			t.Errorf("session terminal %q count = %d, want 1", terminal, got)
		}
	}
}

func TestWsAcceptCounterIsBoundedAndConcurrent(t *testing.T) {
	counter := NewWsAcceptCounter()
	counter.ObserveAccept(wsforward.AcceptOutcome("request-derived"))

	const workers = 64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter.ObserveAccept(wsforward.AcceptEstablished)
			_ = counter.Count(wsforward.AcceptEstablished)
		}()
	}
	wg.Wait()

	if got := counter.Count(wsforward.AcceptEstablished); got != workers {
		t.Errorf("established count = %d, want %d", got, workers)
	}
	if got := counter.Count(wsforward.AcceptRejected); got != 0 {
		t.Errorf("unknown outcome changed rejected count to %d, want 0", got)
	}
}

func TestWsSessionTerminalCounterIsBoundedAndConcurrent(t *testing.T) {
	counter := NewWsSessionTerminalCounter()
	counter.ObserveSessionTerminal(wsforward.SessionTerminal("request-derived"))

	const workers = 64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter.ObserveSessionTerminal(wsforward.SessionError)
			_ = counter.Count(wsforward.SessionError)
		}()
	}
	wg.Wait()

	if got := counter.Count(wsforward.SessionError); got != workers {
		t.Errorf("error count = %d, want %d", got, workers)
	}
	if got := counter.Count(wsforward.SessionClientClosed); got != 0 {
		t.Errorf("unknown terminal changed client_closed count to %d, want 0", got)
	}
}
