package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/sse"
)

func TestRunBoundServeUsageMeterComposesWithConfiguredItemIDStabilizer(t *testing.T) {
	const stream = "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"item-first"}}` + "\n\n" +
		"event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","output_index":0,"item_id":"item-delta","delta":"hello"}` + "\n\n" +
		"event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"response-native","model":"model-native","status":"completed","output":[{"id":"item-final"}],"usage":{"input_tokens":8012,"input_tokens_details":{"cached_tokens":6000,"cache_write_tokens":2000},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":8021}}}` + "\n\n"
	for _, enabled := range []bool{false, true} {
		name := "stabilizer-off"
		if enabled {
			name = "stabilizer-on"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) {
				cfg.ShimResponsesItemIDStabilizerEnabled = enabled
			}, nil)
			resp, body := doUsagePOST(t, context.Background(), harness.baseURL, "/openai/v1/responses", "composed-stabilizer", `{"model":"requested-model","stream":true}`)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("SSE response = %d %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
			if !enabled && string(body) != stream {
				t.Error("disabled stabilizer did not preserve the upstream SSE bytes")
			}

			reader := sse.NewReader(bytes.NewReader(body), nil)
			for i, wantType := range []string{"response.output_item.added", "response.output_text.delta", "response.completed"} {
				frame, err := reader.Read()
				if err != nil || frame.Type != wantType {
					t.Fatalf("frame %d = %q, %v; want %s", i, frame.Type, err, wantType)
				}
				data, present := frame.Data()
				var event struct {
					Item struct {
						ID string `json:"id"`
					} `json:"item"`
					ItemID   string `json:"item_id"`
					Delta    string `json:"delta"`
					Response struct {
						ID     string `json:"id"`
						Model  string `json:"model"`
						Output []struct {
							ID string `json:"id"`
						} `json:"output"`
					} `json:"response"`
				}
				if err := json.Unmarshal(data, &event); err != nil || !present {
					t.Fatalf("frame %d data = %q, %v", i, data, err)
				}
				id := event.Item.ID
				if i == 1 {
					id = event.ItemID
					if event.Delta != "hello" {
						t.Errorf("delta = %q, want unchanged text", event.Delta)
					}
				}
				if i == 2 {
					if len(event.Response.Output) != 1 || event.Response.ID != "response-native" || event.Response.Model != "model-native" {
						t.Fatalf("downstream completion identity/output = %s", data)
					}
					id = event.Response.Output[0].ID
				}
				wantID := []string{"item-first", "item-delta", "item-final"}[i]
				if enabled {
					wantID = "item-first"
				}
				if id != wantID {
					t.Errorf("frame %d item id = %q, want %q", i, id, wantID)
				}
			}
			if frame, err := reader.Read(); err != io.EOF {
				t.Errorf("unexpected extra SSE frame = %q, %v", frame.Raw, err)
			}

			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			if got := queryUsageCount(t, db, "openai_turn", ""); got != 1 {
				t.Fatalf("usage rows = %d, want one completion", got)
			}
			var requestID, responseID, model, transport string
			var turnIndex, input, cached, cacheWrite, output, reasoning, total int64
			err := db.QueryRow(`SELECT request_id, response_id, model, transport, turn_index,
				input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens, total_tokens FROM openai_turn`).Scan(
				&requestID, &responseID, &model, &transport, &turnIndex, &input, &cached, &cacheWrite, &output, &reasoning, &total)
			if err != nil {
				t.Fatal(err)
			}
			if requestID != "composed-stabilizer" || responseID != "response-native" || model != "model-native" || transport != "sse" || turnIndex != 0 ||
				input != 8012 || cached != 6000 || cacheWrite != 2000 || output != 9 || reasoning != 4 || total != 8021 {
				t.Errorf("persisted native completion = %q %q %q %q turn=%d counts=[%d %d %d %d %d %d]",
					requestID, responseID, model, transport, turnIndex, input, cached, cacheWrite, output, reasoning, total)
			}
		})
	}
}
