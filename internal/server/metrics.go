package server

import (
	"sync"

	"github.com/ningw42/copilotd/internal/sse"
	"github.com/ningw42/copilotd/internal/wsforward"
)

// StreamOutcomeObserver is the backend seam for the per-surface terminal
// outcome metric.
type StreamOutcomeObserver interface {
	ObserveStreamOutcome(surface string, outcome sse.Outcome)
}

const (
	streamSurfaceAnthropicIndex = iota
	streamSurfaceOpenAIIndex
	streamSurfaceCount
)

const (
	streamOutcomeCleanIndex = iota
	streamOutcomeSynthesizedIndex
	streamOutcomeStallIndex
	streamOutcomeClientCancelIndex
	streamOutcomeUpstreamErrorIndex
	streamOutcomeShimErrorIndex
	streamOutcomeCount
)

// StreamOutcomeCounter is the zero-dependency metrics backend used until a
// process-wide exporter is selected. Its fixed arrays enforce bounded labels.
type StreamOutcomeCounter struct {
	mu     sync.RWMutex
	counts [streamSurfaceCount][streamOutcomeCount]uint64
}

// NewStreamOutcomeCounter returns an empty terminal-outcome counter.
func NewStreamOutcomeCounter() *StreamOutcomeCounter { return &StreamOutcomeCounter{} }

// ObserveStreamOutcome increments one canonical surface/outcome series.
// Unknown labels are ignored so this metric cannot become unbounded.
func (c *StreamOutcomeCounter) ObserveStreamOutcome(surface string, outcome sse.Outcome) {
	surfaceIndex, outcomeIndex, ok := streamOutcomeIndexes(surface, outcome)
	if !ok {
		return
	}
	c.mu.Lock()
	c.counts[surfaceIndex][outcomeIndex]++
	c.mu.Unlock()
}

// Count returns the observed count for one canonical surface/outcome series.
func (c *StreamOutcomeCounter) Count(surface string, outcome sse.Outcome) uint64 {
	surfaceIndex, outcomeIndex, ok := streamOutcomeIndexes(surface, outcome)
	if !ok {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[surfaceIndex][outcomeIndex]
}

func streamOutcomeIndexes(surface string, outcome sse.Outcome) (surfaceIndex, outcomeIndex int, ok bool) {
	switch surface {
	case "anthropic":
		surfaceIndex = streamSurfaceAnthropicIndex
	case "openai":
		surfaceIndex = streamSurfaceOpenAIIndex
	default:
		return 0, 0, false
	}
	switch outcome {
	case sse.OutcomeClean:
		outcomeIndex = streamOutcomeCleanIndex
	case sse.OutcomeSynthesized:
		outcomeIndex = streamOutcomeSynthesizedIndex
	case sse.OutcomeStall:
		outcomeIndex = streamOutcomeStallIndex
	case sse.OutcomeClientCancel:
		outcomeIndex = streamOutcomeClientCancelIndex
	case sse.OutcomeUpstreamError:
		outcomeIndex = streamOutcomeUpstreamErrorIndex
	case sse.OutcomeShimError:
		outcomeIndex = streamOutcomeShimErrorIndex
	default:
		return 0, 0, false
	}
	return surfaceIndex, outcomeIndex, true
}

const (
	wsAcceptEstablishedIndex = iota
	wsAcceptRejectedIndex
	wsAcceptDialFailedIndex
	wsAcceptOutcomeCount
)

// WsAcceptCounter is the zero-dependency WebSocket handshake metric. Its fixed
// array keeps the outcome label bounded.
type WsAcceptCounter struct {
	mu     sync.RWMutex
	counts [wsAcceptOutcomeCount]uint64
}

// NewWsAcceptCounter returns an empty WebSocket handshake counter.
func NewWsAcceptCounter() *WsAcceptCounter { return &WsAcceptCounter{} }

// ObserveAccept increments one canonical handshake outcome. Unknown outcomes
// are ignored so request-derived values cannot create metric series.
func (c *WsAcceptCounter) ObserveAccept(outcome wsforward.AcceptOutcome) {
	index, ok := wsAcceptOutcomeIndex(outcome)
	if !ok {
		return
	}
	c.mu.Lock()
	c.counts[index]++
	c.mu.Unlock()
}

// Count returns the observed count for one canonical handshake outcome.
func (c *WsAcceptCounter) Count(outcome wsforward.AcceptOutcome) uint64 {
	index, ok := wsAcceptOutcomeIndex(outcome)
	if !ok {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[index]
}

func wsAcceptOutcomeIndex(outcome wsforward.AcceptOutcome) (int, bool) {
	switch outcome {
	case wsforward.AcceptEstablished:
		return wsAcceptEstablishedIndex, true
	case wsforward.AcceptRejected:
		return wsAcceptRejectedIndex, true
	case wsforward.AcceptDialFailed:
		return wsAcceptDialFailedIndex, true
	default:
		return 0, false
	}
}

const (
	wsSessionClientClosedIndex = iota
	wsSessionUpstreamClosedIndex
	wsSessionErrorIndex
	wsSessionTerminalCount
)

// WsSessionTerminalCounter is the zero-dependency established-session terminal
// metric. Its fixed array keeps the terminal label bounded.
type WsSessionTerminalCounter struct {
	mu     sync.RWMutex
	counts [wsSessionTerminalCount]uint64
}

// NewWsSessionTerminalCounter returns an empty session-terminal counter.
func NewWsSessionTerminalCounter() *WsSessionTerminalCounter {
	return &WsSessionTerminalCounter{}
}

// ObserveSessionTerminal increments one canonical terminal outcome. Unknown
// outcomes are ignored so request-derived values cannot create metric series.
func (c *WsSessionTerminalCounter) ObserveSessionTerminal(terminal wsforward.SessionTerminal) {
	index, ok := wsSessionTerminalIndex(terminal)
	if !ok {
		return
	}
	c.mu.Lock()
	c.counts[index]++
	c.mu.Unlock()
}

// Count returns the observed count for one canonical session terminal.
func (c *WsSessionTerminalCounter) Count(terminal wsforward.SessionTerminal) uint64 {
	index, ok := wsSessionTerminalIndex(terminal)
	if !ok {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[index]
}

func wsSessionTerminalIndex(terminal wsforward.SessionTerminal) (int, bool) {
	switch terminal {
	case wsforward.SessionClientClosed:
		return wsSessionClientClosedIndex, true
	case wsforward.SessionUpstreamClosed:
		return wsSessionUpstreamClosedIndex, true
	case wsforward.SessionError:
		return wsSessionErrorIndex, true
	default:
		return 0, false
	}
}
