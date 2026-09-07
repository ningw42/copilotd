package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/shim"
)

// This test-only earlier Shim makes the original client selection observably
// different from the exact payload sent upstream. The meter must see the latter.
type requestedModelRewrite struct{}

func (requestedModelRewrite) TransformRequest(_ context.Context, request *shim.Request) error {
	request.Body = bytes.ReplaceAll(request.Body, []byte(`"client-before-shim"`), []byte(`"gpt-5.6-sol-fast"`))
	return nil
}

func TestRunBoundServeRequestedModelFourHTTPPathsAndRequestIsolation(t *testing.T) {
	for _, fixture := range []struct {
		name                   string
		path, table, idColumn  string
		reported               string
		transport, contentType string
		copies                 int
		responseFor            func(string) string
	}{
		{
			name: "openai/buffered", path: "/openai/v1/responses", table: "openai_turn", idColumn: "response_id",
			reported: "gpt-5.6-sol", transport: "buffered", contentType: "application/json", copies: 1,
			responseFor: func(id string) string {
				return fmt.Sprintf(`{"id":%q,"model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":12,"output_tokens":6}}`, id)
			},
		},
		{
			name: "openai/sse", path: "/openai/v1/responses", table: "openai_turn", idColumn: "response_id",
			reported: "gpt-5.6-sol", transport: "sse", contentType: "text/event-stream", copies: 2,
			responseFor: func(id string) string {
				response := fmt.Sprintf(`{"id":%q,"model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":12,"output_tokens":6}}`, id)
				frame := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
				return frame + frame // duplicates retain independent submission ordinals
			},
		},
		{
			name: "anthropic/buffered", path: "/anthropic/v1/messages", table: "anthropic_turn", idColumn: "message_id",
			reported: "claude-reported", transport: "buffered", contentType: "application/json", copies: 1,
			responseFor: func(id string) string {
				return fmt.Sprintf(`{"id":%q,"type":"message","model":"claude-reported","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6}}`, id)
			},
		},
		{
			name: "anthropic/sse", path: "/anthropic/v1/messages", table: "anthropic_turn", idColumn: "message_id",
			reported: "claude-reported", transport: "sse", contentType: "text/event-stream", copies: 2,
			responseFor: func(id string) string {
				completion := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"model\":\"claude-reported\",\"usage\":{\"input_tokens\":12,\"output_tokens\":6}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", id)
				return completion + completion // accumulator reset must not clear request metadata
			},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			type requestCase struct {
				body, upstreamBody string
				want               sql.NullString
			}
			cases := []requestCase{
				{body: `{"model":"client-before-shim","input":"private-prompt-sentinel"}`, upstreamBody: `{"model":"gpt-5.6-sol-fast","input":"private-prompt-sentinel"}`, want: sql.NullString{String: "gpt-5.6-sol-fast", Valid: true}},
				{body: `{}`},
				{body: `{"model":""}`, want: sql.NullString{Valid: true}},
				{body: `{"model":"  GPT.Alias\t\u96ea  "}`, want: sql.NullString{String: "  GPT.Alias\t雪  ", Valid: true}},
				{body: `{"model":null}`},
				{body: `{"model":42}`},
				{body: `{"model":"ambiguous","model":"last"}`},
				{body: `{"model":"malformed",`},
				{body: "{\"model\":\"gpt-\xff\"}"},
				{body: "{\"model\":\"exact\",\"input\":\"\xff\"}"},
				{body: `[{"model":"not-an-object"}]`},
				{body: `{"input":{"model":"nested"},"Model":"cased"}`},
			}
			for i := range cases {
				if cases[i].upstreamBody == "" {
					cases[i].upstreamBody = cases[i].body
				}
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var index int
				id := r.URL.Query().Get("case")
				if _, err := fmt.Sscanf(id, "case-%d", &index); err != nil || index < 0 || index >= len(cases)*2 {
					t.Errorf("unexpected upstream case %q", id)
					http.Error(w, "invalid test case", http.StatusBadRequest)
					return
				}
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != cases[index%len(cases)].upstreamBody {
					t.Errorf("upstream request %s = %q, %v, want %q", id, got, err, cases[index%len(cases)].upstreamBody)
				}
				w.Header().Set("Content-Type", fixture.contentType)
				_, _ = io.WriteString(w, fixture.responseFor(id))
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, func(registry shim.Registry) shim.Registry {
				return append(shim.Registry{{Name: "test-request-rewrite", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any { return requestedModelRewrite{} }}}, registry...)
			})
			// Concurrent keep-alive dials can leave unused new connections in
			// net/http's pool past the harness's short shutdown deadline. Give
			// each fixture request its own connection; keep drain bounds intact.
			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			t.Cleanup(client.CloseIdleConnections)
			send := func(index int) {
				id := fmt.Sprintf("case-%d", index)
				req, err := http.NewRequest(http.MethodPost, harness.baseURL+fixture.path+"?case="+id, strings.NewReader(cases[index%len(cases)].body))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
				req.Header.Set("X-Request-Id", "reused-request-id")
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != fixture.responseFor(id) {
					t.Errorf("response %s changed: status=%d body=%q err=%v", id, resp.StatusCode, body, err)
				}
			}
			for i := range cases {
				send(i)
			}
			var concurrent sync.WaitGroup
			for i := range cases {
				concurrent.Go(func() { send(len(cases) + i) })
			}
			concurrent.Wait()
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			var count int
			if err := db.QueryRow("SELECT count(*) FROM " + fixture.table).Scan(&count); err != nil || count != len(cases)*2*fixture.copies {
				t.Fatalf("row count = %d, %v, want %d", count, err, len(cases)*2*fixture.copies)
			}
			for i := range len(cases) * 2 {
				for ordinal := range fixture.copies {
					var requestID, model, gotTransport string
					var requested sql.NullString
					var input, output int64
					err := db.QueryRow("SELECT request_id, model, requested_model, transport, input_tokens, output_tokens FROM "+fixture.table+" WHERE "+fixture.idColumn+"=? AND turn_index=?", fmt.Sprintf("case-%d", i), ordinal).Scan(&requestID, &model, &requested, &gotTransport, &input, &output)
					if err != nil || requestID != "reused-request-id" || model != fixture.reported || requested != cases[i%len(cases)].want || gotTransport != fixture.transport || input != 12 || output != 6 {
						t.Errorf("case %d ordinal %d: requested=%+v want=%+v reported=%q transport=%q usage=%d/%d correlation=%q err=%v", i, ordinal, requested, cases[i%len(cases)].want, model, gotTransport, input, output, requestID, err)
					}
				}
			}
		})
	}
}
