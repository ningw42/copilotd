package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func requestedModelFrom(body []byte) *string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var model *string
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		// Decode one value without interpreting unrelated fields or retaining
		// a request object. Token iteration preserves duplicate top-level keys.
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
		if key == "model" {
			if seen || json.Unmarshal(value, &model) != nil {
				return nil
			}
			seen = true
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil
	}
	return model
}

func (r *turnRecorder) record(responseID, model string, transport usage.Transport, native usage.Usage) {
	requestedModel := r.requestedModel
	// The request hook also runs for WebSocket handshakes. Neither handshake
	// bodies nor client Messages attribute self-contained WebSocket Turns.
	if transport == usage.TransportWebSocket {
		requestedModel = nil
	}
	turnIndex := r.turnIndex
	r.turnIndex++
	r.sink.Record(usage.Turn{
		At:             time.Now(),
		RequestID:      r.requestID,
		ResponseID:     responseID,
		Model:          model,
		RequestedModel: requestedModel,
		Transport:      transport,
		TurnIndex:      turnIndex,
		Usage:          native,
	})
}
