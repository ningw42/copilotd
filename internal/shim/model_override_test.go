package shim

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// modelOverrideChain builds the request or session chain exactly as the
// forwarders do, with only the canonical model-override registration enabled.
func modelOverrideChain(t *testing.T, rules map[string]string, surface endpoint.Surface, route endpoint.Route) *Chain {
	t.Helper()
	registry := CanonicalRegistry(nil, rules)
	found := false
	for i := range registry {
		if registry[i].Name == "model-override" {
			registry[i].Enabled = true
			found = true
		}
	}
	if !found {
		t.Fatal("CanonicalRegistry has no model-override registration")
	}
	return registry.NewChain(context.Background(), surface, route)
}

// overrideHTTPBody runs one inbound HTTP request body through the enabled
// Model override on surface/route and returns the upstream-bound body.
func overrideHTTPBody(t *testing.T, rules map[string]string, surface endpoint.Surface, route endpoint.Route, body string) string {
	t.Helper()
	chain := modelOverrideChain(t, rules, surface, route)
	_, got, err := chain.RunRequest(context.Background(), "", http.Header{}, []byte(body))
	if err != nil {
		t.Fatalf("RunRequest() error = %v, want the Model override to never reject", err)
	}
	return string(got)
}

func TestModelOverrideSubstitutesMappedAnthropicMessagesModel(t *testing.T) {
	got := overrideHTTPBody(t, map[string]string{"gpt-5.6-sol": "gpt-5.6-sol-fast"}, endpoint.Anthropic, endpoint.RouteAnthropicMessages,
		`{"model":"gpt-5.6-sol","max_tokens":16}`)
	if want := `{"model":"gpt-5.6-sol-fast","max_tokens":16}`; got != want {
		t.Fatalf("upstream body = %s, want %s", got, want)
	}
}

// assertOverride runs body through the HTTP request hook on Anthropic Messages
// and requires the exact upstream-bound bytes.
func assertOverride(t *testing.T, rules map[string]string, body, want string) {
	t.Helper()
	if got := overrideHTTPBody(t, rules, endpoint.Anthropic, endpoint.RouteAnthropicMessages, body); got != want {
		t.Errorf("upstream body for %q =\n  %q\nwant\n  %q", body, got, want)
	}
}

func TestModelOverrideUsesOneExactCaseSensitiveLookup(t *testing.T) {
	rules := map[string]string{
		"A": "B", "B": "C", // chained rules apply once: A reaches B, never C
		"X": "Y", "Y": "X", // reciprocal rules cannot loop
		"S": "S", // self-mapping is a no-op
		"é": "precomposed-target",
	}
	for _, tc := range []struct{ name, body, want string }{
		{name: "A=B,B=C sends A to B only", body: `{"model":"A"}`, want: `{"model":"B"}`},
		{name: "A=B,B=C sends B to C", body: `{"model":"B"}`, want: `{"model":"C"}`},
		{name: "reciprocal forward", body: `{"model":"X"}`, want: `{"model":"Y"}`},
		{name: "reciprocal reverse", body: `{"model":"Y"}`, want: `{"model":"X"}`},
		{name: "unmapped target ID", body: `{"model":"C"}`, want: `{"model":"C"}`},
		{name: "case-sensitive", body: `{"model":"a"}`, want: `{"model":"a"}`},
		{name: "leading space is not trimmed", body: `{"model":" A"}`, want: `{"model":" A"}`},
		{name: "trailing tab is not trimmed", body: `{"model":"A\t"}`, want: `{"model":"A\t"}`},
		{name: "Unicode is not normalized", body: `{"model":"e\u0301"}`, want: `{"model":"e\u0301"}`},
		{name: "self-mapping", body: `{"model":"S"}`, want: `{"model":"S"}`},
		{name: "escaped self-mapping stays escaped", body: `{"model":"\u0053"}`, want: `{"model":"\u0053"}`},
		{name: "empty model", body: `{"model":""}`, want: `{"model":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) { assertOverride(t, rules, tc.body, tc.want) })
	}
}

func TestModelOverrideReplacesOnlyTheTopLevelModelValueBytes(t *testing.T) {
	rules := map[string]string{"gpt-5.6-sol": "gpt-5.6-sol-fast", "quoted": `quo"te\back/雪<&>`}
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "escaped key",
			body: `{"mod\u0065l":"gpt-5.6-sol"}`,
			want: `{"mod\u0065l":"gpt-5.6-sol-fast"}`,
		},
		{
			name: "escaped source value",
			body: `{"model":"gpt-5.6-\u0073ol"}`,
			want: `{"model":"gpt-5.6-sol-fast"}`,
		},
		{
			name: "target is properly JSON-encoded",
			body: `{"model":"quoted"}`,
			want: `{"model":"quo\"te\\back/雪<&>"}`,
		},
		{
			name: "nested model fields and prompt occurrences of the source ID",
			body: `{"metadata":{"model":"gpt-5.6-sol"},"input":[{"role":"user","content":"use \"gpt-5.6-sol\" via model gpt-5.6-sol","model":"gpt-5.6-sol"}],"model":"gpt-5.6-sol","tools":[{"model":"gpt-5.6-sol"}]}`,
			want: `{"metadata":{"model":"gpt-5.6-sol"},"input":[{"role":"user","content":"use \"gpt-5.6-sol\" via model gpt-5.6-sol","model":"gpt-5.6-sol"}],"model":"gpt-5.6-sol-fast","tools":[{"model":"gpt-5.6-sol"}]}`,
		},
		{
			name: "unknown fields and whitespace",
			body: "\n\t{ \"x-unknown\" : {\"deep\":[1, 2,{\"a\":null}]} ,\r\n  \"model\"\t:\t\"gpt-5.6-sol\"\t,\n\"stream\" :true }  \n",
			want: "\n\t{ \"x-unknown\" : {\"deep\":[1, 2,{\"a\":null}]} ,\r\n  \"model\"\t:\t\"gpt-5.6-sol-fast\"\t,\n\"stream\" :true }  \n",
		},
		{
			name: "unrelated duplicate keys",
			body: `{"temperature":1,"model":"gpt-5.6-sol","temperature":0.5,"metadata":{"k":1,"k":2}}`,
			want: `{"temperature":1,"model":"gpt-5.6-sol-fast","temperature":0.5,"metadata":{"k":1,"k":2}}`,
		},
		{
			name: "large and unusual numeric literals",
			body: `{"seed":123456789012345678901234567890,"budget":1.0E+400,"tiny":-0.000000000000000000001,"scale":1.50,"model":"gpt-5.6-sol"}`,
			want: `{"seed":123456789012345678901234567890,"budget":1.0E+400,"tiny":-0.000000000000000000001,"scale":1.50,"model":"gpt-5.6-sol-fast"}`,
		},
		{
			name: "escaped and non-ASCII prompt text",
			body: `{"model":"gpt-5.6-sol","input":"\u00e9 \/ \ud83d\ude00 雪 \n"}`,
			want: `{"model":"gpt-5.6-sol-fast","input":"\u00e9 \/ \ud83d\ude00 雪 \n"}`,
		},
		{
			name: "lone surrogate escape in unrelated prompt text",
			body: `{"model":"gpt-5.6-sol","input":"truncated \ud83d"}`,
			want: `{"model":"gpt-5.6-sol-fast","input":"truncated \ud83d"}`,
		},
		{
			name: "HTTP ignores an unrelated type member",
			body: `{"type":"other","type":1,"model":"gpt-5.6-sol"}`,
			want: `{"type":"other","type":1,"model":"gpt-5.6-sol-fast"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertOverride(t, rules, tc.body, tc.want) })
	}
}

func TestModelOverrideDeclinesUninterpretableRequestsByPassthrough(t *testing.T) {
	// The replacement-character spelling is mapped so lossy decoding would be
	// observable as a substitution.
	rules := map[string]string{"A": "B", "A\uFFFD": "B"}
	for _, tc := range []struct{ name, body string }{
		{name: "missing model", body: `{"input":"A"}`},
		{name: "model only nested", body: `{"input":{"model":"A"}}`},
		{name: "differently cased key", body: `{"Model":"A"}`},
		{name: "null model", body: `{"model":null}`},
		{name: "numeric model", body: `{"model":42}`},
		{name: "boolean model", body: `{"model":true}`},
		{name: "array model", body: `{"model":["A"]}`},
		{name: "object model", body: `{"model":{"id":"A"}}`},
		{name: "duplicate model", body: `{"model":"A","model":"A"}`},
		{name: "duplicate model via escaped key", body: `{"model":"A","mod\u0065l":"C"}`},
		{name: "duplicate model with wrong-typed twin", body: `{"model":null,"model":"A"}`},
		{name: "lone surrogate in model", body: `{"model":"A\ud800"}`},
		{name: "invalid UTF-8 in model", body: "{\"model\":\"A\xff\"}"},
		{name: "invalid UTF-8 elsewhere", body: "{\"model\":\"A\",\"input\":\"\xff\"}"},
		{name: "truncated", body: `{"model":"A"`},
		{name: "trailing comma", body: `{"model":"A",}`},
		{name: "single-quoted string", body: `{"model":'A'}`},
		{name: "unquoted key", body: `{model:"A"}`},
		{name: "empty body", body: ``},
		{name: "whitespace body", body: " \n"},
		{name: "array", body: `[{"model":"A"}]`},
		{name: "string", body: `"A"`},
		{name: "null", body: `null`},
		{name: "trailing text", body: `{"model":"A"} x`},
		{name: "trailing second object", body: `{"model":"A"}{"model":"A"}`},
		{name: "trailing close brace", body: `{"model":"A"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) { assertOverride(t, rules, tc.body, tc.body) })
	}
}

func TestModelOverrideRewritesEachResponseCreateTextMessageIndependently(t *testing.T) {
	chain := modelOverrideChain(t, map[string]string{"A": "B", "B": "C"}, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	transform := chain.WSClientAdapter(context.Background(), nil)
	if transform == nil {
		t.Fatal("WSClientAdapter() = nil, want the Model override to participate in client Messages")
	}
	// One session's Messages in order: each is interpreted independently, so a
	// later Message without a model is never given one.
	for _, tc := range []struct {
		name       string
		kind       MessageKind
		data, want string
	}{
		{name: "first response.create", kind: MessageText, data: `{"type":"response.create","model":"A","input":"A"}`, want: `{"type":"response.create","model":"B","input":"A"}`},
		{name: "later turn without model", kind: MessageText, data: `{"type":"response.create","input":"next"}`, want: `{"type":"response.create","input":"next"}`},
		{name: "repeated turn uses one lookup", kind: MessageText, data: `{"model":"B","type":"response.create"}`, want: `{"model":"C","type":"response.create"}`},
		{name: "escaped event type", kind: MessageText, data: `{"type":"response\u002ecreate","model":"A"}`, want: `{"type":"response\u002ecreate","model":"B"}`},
		{name: "different event type", kind: MessageText, data: `{"type":"response.cancel","model":"A"}`, want: `{"type":"response.cancel","model":"A"}`},
		{name: "event type is case-sensitive", kind: MessageText, data: `{"type":"Response.Create","model":"A"}`, want: `{"type":"Response.Create","model":"A"}`},
		{name: "missing event type", kind: MessageText, data: `{"model":"A"}`, want: `{"model":"A"}`},
		{name: "duplicate event type", kind: MessageText, data: `{"type":"response.create","type":"response.create","model":"A"}`, want: `{"type":"response.create","type":"response.create","model":"A"}`},
		{name: "ambiguous escaped event type", kind: MessageText, data: `{"type":"response.create","typ\u0065":"other","model":"A"}`, want: `{"type":"response.create","typ\u0065":"other","model":"A"}`},
		{name: "wrong-typed event type", kind: MessageText, data: `{"type":1,"model":"A"}`, want: `{"type":1,"model":"A"}`},
		{name: "null event type", kind: MessageText, data: `{"type":null,"model":"A"}`, want: `{"type":null,"model":"A"}`},
		{name: "lossy event type", kind: MessageText, data: `{"type":"response.create\ud800","model":"A"}`, want: `{"type":"response.create\ud800","model":"A"}`},
		{name: "duplicate model", kind: MessageText, data: `{"type":"response.create","model":"A","model":"A"}`, want: `{"type":"response.create","model":"A","model":"A"}`},
		{name: "malformed", kind: MessageText, data: `{"type":"response.create","model":"A"`, want: `{"type":"response.create","model":"A"`},
		{name: "binary Message", kind: MessageBinary, data: `{"type":"response.create","model":"A"}`, want: `{"type":"response.create","model":"A"}`},
		{name: "final response.create", kind: MessageText, data: `{"type":"response.create","model":"A"}`, want: `{"type":"response.create","model":"B"}`},
	} {
		message := &Message{Kind: tc.kind, Data: []byte(tc.data)}
		if emit := transform(context.Background(), message); !emit {
			t.Errorf("%s: emit = false, want every Message forwarded", tc.name)
		}
		if message.Kind != tc.kind || string(message.Data) != tc.want {
			t.Errorf("%s: Message = (%v, %q), want (%v, %q)", tc.name, message.Kind, message.Data, tc.kind, tc.want)
		}
	}
}

func TestModelOverrideRegistrationIsDisabledAndScopedToInferenceRequests(t *testing.T) {
	registry := CanonicalRegistry(nil, map[string]string{"A": "B"})
	index := slices.IndexFunc(registry, func(r Registration) bool { return r.Name == "model-override" })
	if index < 0 {
		t.Fatal("CanonicalRegistry has no model-override registration")
	}
	if registry[index].Enabled {
		t.Error("model-override is enabled by default, want opt-in")
	}
	if meter := slices.IndexFunc(registry, func(r Registration) bool { return r.Name == "usage-meter" }); meter <= index {
		t.Errorf("usage-meter at %d, want after model-override at %d so it observes the substituted model", meter, index)
	}
	for _, tc := range []struct {
		name    string
		surface endpoint.Surface
		route   endpoint.Route
		want    bool
	}{
		{name: "Anthropic Messages", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicMessages, want: true},
		{name: "Anthropic Count Tokens", surface: endpoint.Anthropic, route: endpoint.RouteAnthropicCountTokens, want: true},
		{name: "OpenAI Responses", surface: endpoint.OpenAI, route: endpoint.RouteOpenAIResponses, want: true},
		{name: "Anthropic catalog", surface: endpoint.Anthropic, route: endpoint.RouteModels},
		{name: "OpenAI catalog", surface: endpoint.OpenAI, route: endpoint.RouteModels},
		{name: "raw models passthrough", surface: endpoint.GitHubCopilot, route: endpoint.RouteModels},
		{name: "GitHub Copilot Responses route", surface: endpoint.GitHubCopilot, route: endpoint.RouteOpenAIResponses},
		{name: "Anthropic Responses route", surface: endpoint.Anthropic, route: endpoint.RouteOpenAIResponses},
		{name: "OpenAI Messages route", surface: endpoint.OpenAI, route: endpoint.RouteAnthropicMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registry[index].Scope(tc.surface, tc.route); got != tc.want {
				t.Errorf("Scope(%v, %q) = %t, want %t", tc.surface, tc.route, got, tc.want)
			}
		})
	}
}

func TestModelOverrideChangesNoTrafficWhenDisabledOrEmpty(t *testing.T) {
	body := `{"model":"A"}`
	disabled := CanonicalRegistry(nil, map[string]string{"A": "B"}).NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages)
	if _, got, err := disabled.RunRequest(context.Background(), "", nil, []byte(body)); err != nil || string(got) != body {
		t.Errorf("disabled override request = %q, %v, want unchanged", got, err)
	}
	if transform := CanonicalRegistry(nil, map[string]string{"A": "B"}).NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter(context.Background(), nil); transform != nil {
		t.Error("disabled override installed a client Message transform")
	}
	for _, rules := range []map[string]string{nil, {}} {
		assertOverride(t, rules, body, body)
	}
}

func TestModelOverrideHasNoResponseHooks(t *testing.T) {
	chain := modelOverrideChain(t, map[string]string{"A": "B"}, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	if chain.HasBufferedTransformer() {
		t.Error("HasBufferedTransformer() = true, want buffered responses to keep the unbuffered path")
	}
	if chain.StreamAdapter(context.Background(), nil) != nil {
		t.Error("StreamAdapter() != nil, want the SSE byte-verbatim fast path")
	}
	if chain.WSServerAdapter(context.Background(), nil) != nil {
		t.Error("WSServerAdapter() != nil, want upstream Messages untouched")
	}
	status, header, err := chain.RunPrelude(context.Background(), http.StatusBadRequest, http.Header{"X-Upstream": {"kept"}})
	if err != nil || status != http.StatusBadRequest || header.Get("X-Upstream") != "kept" {
		t.Errorf("RunPrelude() = %d, %v, %v, want upstream Prelude unchanged", status, header, err)
	}
}

func TestModelOverrideRunsBeforeUsageMeterSoRequestedModelIsSubstituted(t *testing.T) {
	ctx := context.Background()
	sink := &memoryUsageSink{}
	registry := CanonicalRegistry(sink, map[string]string{"gpt-5.6-sol": "gpt-5.6-sol-fast"})
	for i := range registry {
		registry[i].Enabled = registry[i].Name == "model-override" || registry[i].Name == "usage-meter"
	}
	chain := registry.NewChain(ctx, endpoint.OpenAI, endpoint.RouteOpenAIResponses)
	if _, got, err := chain.RunRequest(ctx, "", nil, []byte(`{"model":"gpt-5.6-sol","service_tier":"priority"}`)); err != nil || string(got) != `{"model":"gpt-5.6-sol-fast","service_tier":"priority"}` {
		t.Fatalf("RunRequest() = %q, %v, want substituted model and preserved service_tier intent", got, err)
	}
	response := []byte(`{"id":"resp","model":"gpt-5.6-sol-reported","status":"completed","service_tier":"default","usage":{"input_tokens":3,"output_tokens":2}}`)
	if got, err := chain.RunBuffered(ctx, response); err != nil || string(got) != string(response) {
		t.Fatalf("RunBuffered() = %q, %v, want unchanged", got, err)
	}
	turns := sink.snapshot()
	if len(turns) != 1 || turns[0].RequestedModel == nil || *turns[0].RequestedModel != "gpt-5.6-sol-fast" ||
		turns[0].Model != "gpt-5.6-sol-reported" || turns[0].OpenAIServiceTier == nil || *turns[0].OpenAIServiceTier != "default" {
		t.Fatalf("Turns = %+v, want post-override Requested model with completion-derived model and tier", turns)
	}
}

func TestModelOverrideCarriesNoStateAcrossConcurrentRequests(t *testing.T) {
	registry := CanonicalRegistry(nil, map[string]string{"A": "B", "C": "D"})
	for i := range registry {
		registry[i].Enabled = registry[i].Name == "model-override"
	}
	cases := []struct{ body, want string }{
		{`{"model":"A"}`, `{"model":"B"}`},
		{`{"model":"C"}`, `{"model":"D"}`},
		{`{"input":"no model"}`, `{"input":"no model"}`},
		{`{"model":"unmapped"}`, `{"model":"unmapped"}`},
	}
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			tc := cases[i%len(cases)]
			chain := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses)
			if _, got, err := chain.RunRequest(context.Background(), "", nil, []byte(tc.body)); err != nil || string(got) != tc.want {
				t.Errorf("request %d = %q, %v, want %q", i, got, err, tc.want)
			}
		})
	}
	wg.Wait()
}

func TestModelOverrideRegistryIsIndependentOfLaterRuleChanges(t *testing.T) {
	rules := map[string]string{"A": "B"}
	registry := CanonicalRegistry(nil, rules)
	rules["A"], rules["X"] = "C", "Y"
	for i := range registry {
		registry[i].Enabled = registry[i].Name == "model-override"
	}
	chain := registry.NewChain(context.Background(), endpoint.Anthropic, endpoint.RouteAnthropicMessages)
	for body, want := range map[string]string{`{"model":"A"}`: `{"model":"B"}`, `{"model":"X"}`: `{"model":"X"}`} {
		if _, got, err := chain.RunRequest(context.Background(), "", nil, []byte(body)); err != nil || string(got) != want {
			t.Errorf("RunRequest(%s) = %q, %v, want %q from the rules at construction", body, got, err, want)
		}
	}
}

func TestModelOverrideNeverSendsAReplacementSpelledTarget(t *testing.T) {
	// Configuration rejects such a rule; the Shim still refuses to mangle it.
	assertOverride(t, map[string]string{"A": "bad-\xff"}, `{"model":"A"}`, `{"model":"A"}`)
}
