package shim

import (
	"context"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
)

func TestTurnRecorderOverwritesOwnedMetadataAndPreservesCompletionEvidence(t *testing.T) {
	sink := &memoryUsageSink{}
	recorder := newTurnRecorder(logging.WithRequestID(context.Background(), "captured-request"), sink)
	recorder.observeRequest([]byte(`{"model":"requested"}`))
	serviceTier := "priority"
	wrongRequestedModel := "wrong"
	before := time.Now()
	recorder.record(usage.Turn{
		At:                time.Unix(1, 0),
		RequestID:         "wrong",
		ResponseID:        "response",
		Model:             "reported",
		RequestedModel:    &wrongRequestedModel,
		OpenAIServiceTier: &serviceTier,
		Transport:         usage.TransportBuffered,
		TurnIndex:         99,
		Usage: usage.OpenAIUsage{
			InputTokens:  1,
			OutputTokens: 2,
		},
	})
	after := time.Now()

	turns := sink.snapshot()
	if len(turns) != 1 {
		t.Fatalf("Turns = %+v, want one", turns)
	}
	turn := turns[0]
	if turn.At.Before(before) || turn.At.After(after) || turn.RequestID != "captured-request" ||
		turn.ResponseID != "response" || turn.Model != "reported" || turn.RequestedModel == nil ||
		*turn.RequestedModel != "requested" || turn.OpenAIServiceTier != &serviceTier ||
		turn.Transport != usage.TransportBuffered || turn.TurnIndex != 0 {
		t.Fatalf("recorded Turn = %+v", turn)
	}
	native, ok := turn.Usage.(usage.OpenAIUsage)
	if !ok || native.InputTokens != 1 || native.OutputTokens != 2 {
		t.Fatalf("recorded native usage = %#v", turn.Usage)
	}
}

func TestTurnRecorderForcesWebSocketRequestedModelUnavailable(t *testing.T) {
	sink := &memoryUsageSink{}
	recorder := newTurnRecorder(context.Background(), sink)
	recorder.observeRequest([]byte(`{"model":"not-websocket-attribution"}`))
	recorder.record(usage.Turn{
		ResponseID: "response",
		Model:      "reported",
		Transport:  usage.TransportWebSocket,
		Usage:      usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2},
	})
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestedModel != nil || turns[0].TurnIndex != 0 {
		t.Fatalf("WebSocket Turns = %+v, want unavailable request attribution", turns)
	}
}
