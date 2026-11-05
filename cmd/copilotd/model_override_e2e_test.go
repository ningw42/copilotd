package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
)

// modelOverrideHTTPCase is one inbound inference request. overridden is the
// exact body Copilot must receive when the configured Model override applies;
// otherwise Copilot must receive body itself.
type modelOverrideHTTPCase struct {
	name, path, mode string
	body, overridden string
	// table names the Usage meter's Turn table; Count Tokens is not metered.
	table string
	// requested is the persisted Requested model when the override applies.
	requested sql.NullString
}

var modelOverrideHTTPCases = []modelOverrideHTTPCase{
	{
		name: "anthropic messages buffered", path: "/anthropic/v1/messages", mode: "buffered", table: "anthropic_turn",
		body:       `{"model":"claude-src","max_tokens":8,"messages":[{"role":"user","content":"is claude-src \"claude-src\"?"}]}`,
		overridden: `{"model":"claude-target","max_tokens":8,"messages":[{"role":"user","content":"is claude-src \"claude-src\"?"}]}`,
		requested:  sql.NullString{String: "claude-target", Valid: true},
	},
	{
		name: "anthropic messages sse", path: "/anthropic/v1/messages", mode: "sse", table: "anthropic_turn",
		body:       `{"stream":true, "model" : "claude-src", "metadata":{"model":"claude-src"}}`,
		overridden: `{"stream":true, "model" : "claude-target", "metadata":{"model":"claude-src"}}`,
		requested:  sql.NullString{String: "claude-target", Valid: true},
	},
	{
		name: "anthropic count tokens", path: "/anthropic/v1/messages/count_tokens", mode: "buffered",
		body:       `{"model":"claude-src","messages":[{"role":"user","content":"count claude-src"}]}`,
		overridden: `{"model":"claude-target","messages":[{"role":"user","content":"count claude-src"}]}`,
	},
	{
		name: "openai responses buffered", path: "/openai/v1/responses", mode: "buffered", table: "openai_turn",
		body:       `{"model":"gpt-5.6-sol","service_tier":"priority","input":"gpt-5.6-sol prompt","seed":123456789012345678901234567890}`,
		overridden: `{"model":"gpt-5.6-sol-fast","service_tier":"priority","input":"gpt-5.6-sol prompt","seed":123456789012345678901234567890}`,
		requested:  sql.NullString{String: "gpt-5.6-sol-fast", Valid: true},
	},
	{
		name: "openai responses sse", path: "/openai/v1/responses", mode: "sse", table: "openai_turn",
		body:       `{"model":"gpt-5.6-sol","stream":true}`,
		overridden: `{"model":"gpt-5.6-sol-fast","stream":true}`,
		requested:  sql.NullString{String: "gpt-5.6-sol-fast", Valid: true},
	},
	{
		name: "openai unmapped model", path: "/openai/v1/responses", mode: "buffered", table: "openai_turn",
		body:      `{"model":"gpt-5.6-luna"}`,
		requested: sql.NullString{String: "gpt-5.6-luna", Valid: true},
	},
	{
		name: "openai model absent", path: "/openai/v1/responses", mode: "buffered", table: "openai_turn",
		body: `{"input":"gpt-5.6-sol"}`,
	},
	{
		name: "anthropic duplicate model", path: "/anthropic/v1/messages", mode: "buffered", table: "anthropic_turn",
		body: `{"model":"claude-src","model":"claude-src"}`,
	},
}

// modelOverrideCompletion is the synthetic upstream response for one case. Its
// Reported model and service tier deliberately match neither the source nor the
// target, so persisted evidence must be completion-derived.
func modelOverrideCompletion(path, mode, id string) (contentType, body string) {
	openAI := fmt.Sprintf(`{"id":%q,"model":"gpt-5.6-sol-reported","status":"completed","service_tier":"default","usage":{"input_tokens":12,"output_tokens":6}}`, id)
	switch {
	case path == "/v1/messages/count_tokens":
		return "application/json", `{"input_tokens":7}`
	case path == "/v1/messages" && mode == "sse":
		return "text/event-stream", fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"model\":\"claude-reported\",\"usage\":{\"input_tokens\":12,\"output_tokens\":6}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", id)
	case path == "/v1/messages":
		return "application/json", fmt.Sprintf(`{"id":%q,"type":"message","model":"claude-reported","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6}}`, id)
	case mode == "sse":
		return "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + openAI + "}\n\n"
	default:
		return "application/json", openAI
	}
}

// modelOverrideWebSocketCompletion is the upstream reply to the nth client
// Message, with completion-derived model and tier evidence.
func modelOverrideWebSocketCompletion(n int) []byte {
	return fmt.Appendf(nil, `{"type":"response.completed","response":{"id":"ws-%d","model":"gpt-5.6-sol-reported","status":"completed","service_tier":"priority","usage":{"input_tokens":1,"output_tokens":1}}}`, n)
}

type modelOverrideWSMessage struct {
	kind       websocket.MessageType
	data       string
	overridden string
}

// One session's client Messages in order. Each is interpreted independently.
var modelOverrideWebSocketMessages = []modelOverrideWSMessage{
	{kind: websocket.MessageText, data: `{"type":"response.create","model":"gpt-5.6-sol","input":"gpt-5.6-sol"}`, overridden: `{"type":"response.create","model":"gpt-5.6-sol-fast","input":"gpt-5.6-sol"}`},
	{kind: websocket.MessageText, data: `{"type":"response.create","input":"later turn without a model"}`},
	{kind: websocket.MessageBinary, data: `{"type":"response.create","model":"gpt-5.6-sol"}`},
	{kind: websocket.MessageText, data: `{"type":"session.update","model":"gpt-5.6-sol"}`},
	{kind: websocket.MessageText, data: `{"type":"response.create","type":"response.create","model":"gpt-5.6-sol"}`},
	{kind: websocket.MessageText, data: `{"model":"gpt-5.6-sol","previous_response_id":"ws-0","type":"response.create"}`, overridden: `{"model":"gpt-5.6-sol-fast","previous_response_id":"ws-0","type":"response.create"}`},
	{kind: websocket.MessageText, data: `{"type":"response.create","model":"gpt-5.6-luna"}`},
}

// TestServeLifecycleModelOverrideReachesEveryInferenceTransport drives the
// production lifecycle with the Usage meter enabled and no catalog. Configured
// rules substitute only the matched model on every supported HTTP operation and
// WebSocket response.create Message; disabled or empty configuration changes no
// traffic. Every response reaches the client byte-unchanged.
func TestServeLifecycleModelOverrideReachesEveryInferenceTransport(t *testing.T) {
	rules := map[string]string{"gpt-5.6-sol": "gpt-5.6-sol-fast", "claude-src": "claude-target"}
	for _, scenario := range []struct {
		name    string
		enabled bool
		rules   map[string]string
		applies bool
	}{
		{name: "enabled", enabled: true, rules: rules, applies: true},
		{name: "disabled with rules", rules: rules},
		{name: "enabled with empty map", enabled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			upstreamBody := func(body, overridden string) string {
				if scenario.applies && overridden != "" {
					return overridden
				}
				return body
			}
			caseFor := func(id string) (modelOverrideHTTPCase, bool) {
				var index int
				if _, err := fmt.Sscanf(id, "case-%d", &index); err != nil || index < 0 || index >= 2*len(modelOverrideHTTPCases) {
					return modelOverrideHTTPCase{}, false
				}
				return modelOverrideHTTPCases[index%len(modelOverrideHTTPCases)], true
			}
			wsReceived := make(chan modelOverrideWSMessage, len(modelOverrideWebSocketMessages))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Errorf("accept upstream WebSocket: %v", err)
						return
					}
					defer func() { _ = conn.CloseNow() }()
					for n := 0; ; n++ {
						kind, data, err := conn.Read(context.Background())
						if err != nil {
							return
						}
						wsReceived <- modelOverrideWSMessage{kind: kind, data: string(data)}
						if err := conn.Write(context.Background(), websocket.MessageText, modelOverrideWebSocketCompletion(n)); err != nil {
							t.Errorf("write upstream completion %d: %v", n, err)
							return
						}
					}
				}
				id := r.URL.Query().Get("case")
				tc, ok := caseFor(id)
				if !ok {
					t.Errorf("unexpected upstream request %s %s?%s; the Model override must not consult a catalog", r.Method, r.URL.Path, r.URL.RawQuery)
					http.Error(w, "unexpected", http.StatusBadRequest)
					return
				}
				got, err := io.ReadAll(r.Body)
				if want := upstreamBody(tc.body, tc.overridden); err != nil || string(got) != want {
					t.Errorf("%s (%s): upstream body = %q, %v, want %q", tc.name, id, got, err, want)
				}
				contentType, body := modelOverrideCompletion(r.URL.Path, r.URL.Query().Get("mode"), id)
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(upstream.Close)
			harness := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), func(cfg *config.ServeConfig) {
				cfg.ShimModelOverrideEnabled = scenario.enabled
				cfg.ModelOverrides = scenario.rules
			}, nil)

			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			t.Cleanup(client.CloseIdleConnections)
			send := func(index int) {
				tc := modelOverrideHTTPCases[index%len(modelOverrideHTTPCases)]
				id := fmt.Sprintf("case-%d", index)
				req, err := http.NewRequest(http.MethodPost, harness.baseURL+tc.path+"?case="+id+"&mode="+tc.mode, strings.NewReader(tc.body))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("%s: %v", tc.name, err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				upstreamPath := strings.TrimPrefix(strings.TrimPrefix(tc.path, "/anthropic"), "/openai/v1")
				if _, want := modelOverrideCompletion(upstreamPath, tc.mode, id); err != nil || resp.StatusCode != http.StatusOK || string(body) != want {
					t.Errorf("%s: response = %d %q (%v), want upstream bytes %q", tc.name, resp.StatusCode, body, err, want)
				}
			}
			for i := range modelOverrideHTTPCases {
				send(i)
			}
			// A concurrent second pass interleaves mapped and unmapped requests:
			// no request inherits another's substitution.
			var concurrent sync.WaitGroup
			for i := range modelOverrideHTTPCases {
				concurrent.Go(func() { send(len(modelOverrideHTTPCases) + i) })
			}
			concurrent.Wait()

			conn := dialUsageMeterWebSocket(t, harness.baseURL, "model-override-websocket")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for n, message := range modelOverrideWebSocketMessages {
				if err := conn.Write(ctx, message.kind, []byte(message.data)); err != nil {
					t.Fatalf("write client Message %d: %v", n, err)
				}
				select {
				case got := <-wsReceived:
					if want := upstreamBody(message.data, message.overridden); got.kind != message.kind || got.data != want {
						t.Errorf("upstream Message %d = (%v, %q), want (%v, %q)", n, got.kind, got.data, message.kind, want)
					}
				case <-ctx.Done():
					t.Fatalf("client Message %d did not reach Copilot", n)
				}
				kind, reply, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageText || !bytes.Equal(reply, modelOverrideWebSocketCompletion(n)) {
					t.Fatalf("server Message %d = (%v, %q, %v), want upstream bytes unchanged", n, kind, reply, err)
				}
			}
			if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
				t.Errorf("close WebSocket session: %v", err)
			}

			if !scenario.applies {
				return
			}
			db, report := externalUsageDB(t, harness)
			assertCleanUsageReport(t, report)
			for i := range 2 * len(modelOverrideHTTPCases) {
				tc := modelOverrideHTTPCases[i%len(modelOverrideHTTPCases)]
				if tc.table == "" {
					continue
				}
				idColumn, reported, tier := "message_id", "claude-reported", "NULL"
				if tc.table == "openai_turn" {
					idColumn, reported, tier = "response_id", "gpt-5.6-sol-reported", "service_tier"
				}
				var model, transport string
				var requested, serviceTier sql.NullString
				err := db.QueryRow("SELECT model, requested_model, transport, "+tier+" FROM "+tc.table+" WHERE "+idColumn+"=?", fmt.Sprintf("case-%d", i)).Scan(&model, &requested, &transport, &serviceTier)
				wantTier := sql.NullString{}
				if tc.table == "openai_turn" {
					wantTier = sql.NullString{String: "default", Valid: true}
				}
				if err != nil || model != reported || requested != tc.requested || transport != tc.mode || serviceTier != wantTier {
					t.Errorf("%s case-%d Turn: model=%q requested=%+v transport=%q tier=%+v err=%v, want Requested %+v with completion-derived evidence", tc.name, i, model, requested, transport, serviceTier, err, tc.requested)
				}
			}
			for n := range modelOverrideWebSocketMessages {
				var model, transport string
				var requested, serviceTier sql.NullString
				err := db.QueryRow("SELECT model, requested_model, transport, service_tier FROM openai_turn WHERE response_id=?", fmt.Sprintf("ws-%d", n)).Scan(&model, &requested, &transport, &serviceTier)
				if err != nil || model != "gpt-5.6-sol-reported" || requested.Valid || transport != "websocket" || serviceTier != (sql.NullString{String: "priority", Valid: true}) {
					t.Errorf("WebSocket Turn ws-%d: model=%q requested=%+v transport=%q tier=%+v err=%v, want unavailable Requested model", n, model, requested, transport, serviceTier, err)
				}
			}
		})
	}
}

// TestServeLifecycleModelOverrideForwardsUpstreamRejectionWithoutFallback
// proves that an unavailable target is still sent exactly once and Copilot's
// rejection reaches the client unchanged: no availability check, retry, or
// fallback to the source ID.
func TestServeLifecycleModelOverrideForwardsUpstreamRejectionWithoutFallback(t *testing.T) {
	const (
		anthropicRejection = `{"type":"error","error":{"type":"not_found_error","message":"model: claude-gone"}}`
		openAIRejection    = `{"error":{"message":"The requested model is not supported.","code":"model_not_supported","param":"model","type":"invalid_request_error"}}`
		wsRejection        = `{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found","message":"gpt-gone"}}`
	)
	var mu sync.Mutex
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept upstream WebSocket: %v", err)
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for {
				_, data, err := conn.Read(context.Background())
				if err != nil {
					return
				}
				mu.Lock()
				received = append(received, "ws "+string(data))
				mu.Unlock()
				if err := conn.Write(context.Background(), websocket.MessageText, []byte(wsRejection)); err != nil {
					return
				}
			}
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, openAIRejection)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, anthropicRejection)
	}))
	t.Cleanup(upstream.Close)
	cfg := lifecycleConfig("gho-model-override-rejection")
	cfg.WebSocketHandshakeTimeout = 5 * time.Second
	cfg.ShimModelOverrideEnabled = true
	cfg.ModelOverrides = map[string]string{"claude-src": "claude-gone", "gpt-src": "gpt-gone"}
	base := startServedLifecycle(t, discardLogger(t), serveInput{
		Config: cfg,
		Edges:  exchangeServeEdges(lifecycleExchangeStub(t, upstream.URL, make(chan http.Header, 1))),
	})

	for _, tc := range []struct {
		path, body string
		status     int
		want       string
	}{
		{path: "/anthropic/v1/messages", body: `{"model":"claude-src","stream":true}`, status: http.StatusNotFound, want: anthropicRejection},
		{path: "/anthropic/v1/messages/count_tokens", body: `{"model":"claude-src"}`, status: http.StatusNotFound, want: anthropicRejection},
		{path: "/openai/v1/responses", body: `{"model":"gpt-src"}`, status: http.StatusBadRequest, want: openAIRejection},
	} {
		resp, body := post(t, base+tc.path, tc.body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || body != tc.want {
			t.Errorf("%s response = %d %q, want Copilot's %d %q", tc.path, resp.StatusCode, body, tc.status, tc.want)
		}
	}

	conn := dialUsageMeterWebSocket(t, base, "model-override-rejection")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for turn := range 2 {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-src"}`)); err != nil {
			t.Fatalf("write turn %d: %v", turn, err)
		}
		if kind, reply, err := conn.Read(ctx); err != nil || kind != websocket.MessageText || string(reply) != wsRejection {
			t.Fatalf("turn %d reply = (%v, %q, %v), want Copilot's error event and an open session", turn, kind, reply, err)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		`/v1/messages {"model":"claude-gone","stream":true}`,
		`/v1/messages/count_tokens {"model":"claude-gone"}`,
		`/responses {"model":"gpt-gone"}`,
		`ws {"type":"response.create","model":"gpt-gone"}`,
		`ws {"type":"response.create","model":"gpt-gone"}`,
	}
	if strings.Join(received, "\n") != strings.Join(want, "\n") {
		t.Errorf("upstream requests =\n%s\nwant exactly one target attempt each, never the source:\n%s", strings.Join(received, "\n"), strings.Join(want, "\n"))
	}
}

// TestServeLifecycleModelOverrideLeavesCatalogsUnchangedAndShadowsLiveSources
// serves every catalog with and without the override. Outputs are identical;
// meanwhile explicit rules shadow source IDs the catalogs advertise as live,
// including the configured Codex reviewer, with exactly one lookup.
func TestServeLifecycleModelOverrideLeavesCatalogsUnchangedAndShadowsLiveSources(t *testing.T) {
	const (
		tag    = "rust-v3.4.6"
		commit = "0123456789abcdef0123456789abcdef01234567"
	)
	var entries []map[string]any
	for _, slug := range []string{"gpt-5.6-sol", "gpt-source"} {
		var envelope struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(completeCodexModelsBytes(t, slug, slug+" prompt"), &envelope); err != nil {
			t.Fatalf("decode complete Codex entry: %v", err)
		}
		entries = append(entries, envelope.Models[0])
	}
	releaseModels, err := json.Marshal(map[string]any{"models": entries})
	if err != nil {
		t.Fatalf("encode Codex release models: %v", err)
	}
	const liveModels = `{"data":[` +
		`{"id":"gpt-5.6-sol","name":"GPT Sol","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"],"capabilities":{"limits":{"max_prompt_tokens":12345}}},` +
		`{"id":"gpt-5.6-sol-fast","name":"GPT Sol Fast","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]},` +
		`{"id":"gpt-alias","name":"GPT Alias","vendor":"OpenAI","model_picker_enabled":true,"supported_endpoints":["/responses"]},` +
		`{"id":"claude-src","name":"Claude Source","vendor":"Anthropic","model_picker_enabled":true,"supported_endpoints":["/v1/messages"]}]}`
	inference := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, liveModels)
			return
		}
		body, _ := io.ReadAll(r.Body)
		inference <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	edge := newCodexReleaseEdge(t, tag, commit, releaseModels)

	serveCatalogs := func(enabled bool) (string, map[string]string) {
		cfg := lifecycleConfig("gho-model-override-catalogs")
		cfg.CodexCatalogEnabled = true
		cfg.CodexCatalogRefreshInterval = time.Hour
		cfg.CodexCatalogModelAliases = map[string]string{"gpt-alias": "gpt-source"}
		cfg.CodexAutoReviewModel = "gpt-alias"
		cfg.CodexAutoReviewModelOverrides = map[string]string{"gpt-5.6-sol": "gpt-alias"}
		cfg.CodexOverrideLimits = true
		if enabled {
			cfg.ShimModelOverrideEnabled = true
			cfg.ModelOverrides = map[string]string{
				"gpt-5.6-sol": "gpt-5.6-sol-fast",
				"gpt-alias":   "gpt-5.6-sol",
				"claude-src":  "claude-target",
			}
		}
		edges := exchangeServeEdges(lifecycleExchangeStub(t, upstream.URL, make(chan http.Header, 1)))
		edges.CodexModels = catalog.ModelsEdge{BaseURL: edge.URL, Client: edge.Client()}
		base := startServedLifecycle(t, discardLogger(t), serveInput{Config: cfg, Edges: edges})
		awaitCachedValue(t, base, "codex_models", "fetched", tag)
		outputs := make(map[string]string)
		for _, path := range []string{"/models", "/anthropic/v1/models", "/openai/v1/models", "/openai/v1/models?client_version=fixture"} {
			req, err := http.NewRequest(http.MethodGet, base+path, nil)
			if err != nil {
				t.Fatalf("build %s request: %v", path, err)
			}
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d %s (%v), want 200", path, resp.StatusCode, body, err)
			}
			outputs[path] = string(body)
		}
		return base, outputs
	}

	_, baseline := serveCatalogs(false)
	base, overridden := serveCatalogs(true)
	for path, want := range baseline {
		if got := overridden[path]; got != want {
			t.Errorf("GET %s with the override enabled =\n%s\nwant unchanged\n%s", path, got, want)
		}
	}
	codex := overridden["/openai/v1/models?client_version=fixture"]
	for _, advertised := range []string{`"slug":"gpt-5.6-sol"`, `"slug":"gpt-alias"`, `"auto_review_model_override":"gpt-alias"`} {
		if !strings.Contains(codex, advertised) {
			t.Fatalf("Codex catalog %s lacks %s; the comparison would not cover rule sources", codex, advertised)
		}
	}

	for _, tc := range []struct{ body, want string }{
		{body: `{"model":"gpt-5.6-sol"}`, want: `{"model":"gpt-5.6-sol-fast"}`},
		{body: `{"model":"gpt-alias"}`, want: `{"model":"gpt-5.6-sol"}`},
	} {
		resp, body := post(t, base+"/openai/v1/responses", tc.body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || body != `{"ok":true}` {
			t.Errorf("POST %s = %d %q, want forwarded upstream response", tc.body, resp.StatusCode, body)
		}
		select {
		case got := <-inference:
			if got != tc.want {
				t.Errorf("upstream body for %s = %q, want %q", tc.body, got, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %s did not reach Copilot", tc.body)
		}
	}
}

// TestRunServeRejectsMalformedModelOverridesBeforeBinding drives the CLI with a
// disabled override whose map repeats a source after trimming.
func TestRunServeRejectsMalformedModelOverridesBeforeBinding(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{
		"serve",
		"--apikey", "some-key",
		"--github-oauth-token", "gho-unused",
		"--addr", "127.0.0.1:0",
		"--shim-model-override-enabled=false",
		"--model-overrides", "gpt-5.6-sol=gpt-5.6-sol-fast, gpt-5.6-sol =other",
	}, noEnv(), io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `duplicate source model "gpt-5.6-sol"`) {
		t.Errorf("exit = %d, stderr = %q, want startup failure naming the duplicate source", code, stderr.String())
	}
}
