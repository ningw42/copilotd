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
	sink           usage.Sink
	requestID      string
	requestedModel *string
	turnIndex      int
}

func newTurnRecorder(ctx context.Context, sink usage.Sink) turnRecorder {
	requestID, _ := logging.RequestIDFrom(ctx)
	return turnRecorder{sink: sink, requestID: requestID}
}

// observeRequest runs once at the innermost request hook. Only the decoded
// string survives extraction; the request body is never retained or changed.
func (r *turnRecorder) observeRequest(body []byte) {
	r.requestedModel = requestedModelFrom(body)
}

// requestedModelFrom returns the request's unique, losslessly decoded top-level
// model. Anything else has unknown attribution rather than a guessed or
// replacement-derived model name.
func requestedModelFrom(body []byte) *string {
	modelMember := topLevelMember{name: "model"}
	if !scanTopLevelMembers(body, &modelMember) {
		return nil
	}
	model, ok := modelMember.uniqueString()
	if !ok {
		return nil
	}
	return &model
}

func (r *turnRecorder) record(turn usage.Turn) {
	requestedModel := r.requestedModel
	// WebSocket forwarding does not invoke the HTTP request hook. Keep this
	// boundary defensive: even if an internal caller populated request metadata,
	// self-contained WebSocket Turns remain unattributed.
	if turn.Transport == usage.TransportWebSocket {
		requestedModel = nil
	}
	turn.At = time.Now()
	turn.RequestID = r.requestID
	turn.RequestedModel = requestedModel
	turn.TurnIndex = r.turnIndex
	r.turnIndex++
	r.sink.Record(turn)
}
