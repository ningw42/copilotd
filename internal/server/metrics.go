package server

import (
	"sync"

	"github.com/ningw42/copilotd/internal/sse"
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
