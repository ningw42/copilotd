package shim

import (
	"context"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
)

// turnRecorder owns the immutable metadata shared by the two native Usage
// meter parsers. Surface-specific validation and accumulation remain with the
// parser; only the common Turn submission envelope is composed here.
type turnRecorder struct {
	sink      usage.Sink
	requestID string
	turnIndex int
}

func newTurnRecorder(ctx context.Context, sink usage.Sink) turnRecorder {
	requestID, _ := logging.RequestIDFrom(ctx)
	return turnRecorder{sink: sink, requestID: requestID}
}

func (r *turnRecorder) record(responseID, model string, transport usage.Transport, native usage.Usage) {
	turnIndex := r.turnIndex
	r.turnIndex++
	r.sink.Record(usage.Turn{
		At:         time.Now(),
		RequestID:  r.requestID,
		ResponseID: responseID,
		Model:      model,
		Transport:  transport,
		TurnIndex:  turnIndex,
		Usage:      native,
	})
}
