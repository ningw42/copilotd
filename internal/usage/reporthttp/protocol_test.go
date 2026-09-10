package reporthttp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func nonemptyReportJSON(t *testing.T) (string, report.Query) {
	t.Helper()
	count := func(n int64) *int64 { return &n }
	model := "模型"
	openAITotal := report.Total{Turns: 2, Usage: map[string]report.Metric{
		"input_tokens":       {Sum: count(9007199254740993), ReportedTurns: 2},
		"output_tokens":      {Sum: count(9), ReportedTurns: 2},
		"cached_tokens":      {Sum: count(4), ReportedTurns: 1},
		"cache_write_tokens": {},
		"reasoning_tokens":   {Sum: count(0), ReportedTurns: 1},
		"total_tokens":       {},
	}}
	anthropicTotal := report.Total{Turns: 2, Usage: map[string]report.Metric{
		"input_tokens":                {Sum: count(12), ReportedTurns: 2},
		"output_tokens":               {Sum: count(9), ReportedTurns: 2},
		"cache_creation_input_tokens": {Sum: count(4), ReportedTurns: 1},
		"cache_read_input_tokens":     {},
		"ephemeral_5m_input_tokens":   {Sum: count(0), ReportedTurns: 1},
		"ephemeral_1h_input_tokens":   {},
		"thinking_tokens":             {},
	}}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	result := report.Report{
		SchemaVersion: 1,
		GeneratedAt:   start.Add(12 * time.Hour),
		Timezone:      "UTC",
		Period:        "day",
		Since:         "2026-09-01",
		Until:         "2026-09-02",
		WindowStart:   start,
		WindowEnd:     end,
		Scope:         "configured_database",
		Collection:    "best_effort",
		Surface:       "all",
		Model:         &model,
		Buckets: []report.Bucket{{
			StartDate:  "2026-09-01",
			UntilDate:  "2026-09-02",
			RangeStart: start,
			RangeEnd:   end,
		}},
	}
	openAIModel := report.ModelTotal{Model: model, Total: openAITotal}
	anthropicModel := report.ModelTotal{Model: model, Total: anthropicTotal}
	result.OpenAI = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: openAIModel}}, Models: []report.ModelTotal{openAIModel}, Total: openAITotal}
	result.Anthropic = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: anthropicModel}}, Models: []report.ModelTotal{anthropicModel}, Total: anthropicTotal}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), report.Query{Surface: "all", Period: "day", Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02", Model: &model}
}

func TestClientValidatesCompleteWireContract(t *testing.T) {
	base, query := nonemptyReportJSON(t)
	queryWithoutModel := query
	queryWithoutModel.Model = nil
	type example struct {
		name, body string
		query      report.Query
		valid      bool
	}
	cases := []example{
		{name: "complete native report", body: base, query: query, valid: true},
		{name: "unsolicited effective model filter", body: base, query: queryWithoutModel},
		{name: "malformed", body: `{`, query: query},
		{name: "trailing document", body: base + ` {}`, query: query},
	}
	mutate := func(name string, q report.Query, valid bool, change func(map[string]any)) {
		var root map[string]any
		if err := json.Unmarshal([]byte(base), &root); err != nil {
			t.Fatal(err)
		}
		change(root)
		body, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, example{name: name, body: string(body), query: q, valid: valid})
	}
	invalidMutation := func(name string, change func(map[string]any)) {
		mutate(name, query, false, change)
	}

	for _, key := range []string{"schema_version", "generated_at", "timezone", "period", "since", "until", "window_start", "window_end", "scope", "collection", "surface", "model", "buckets", "anthropic", "openai"} {
		invalidMutation("missing "+key, func(root map[string]any) { delete(root, key) })
		invalidMutation("case is not "+key, func(root map[string]any) {
			root[strings.ToUpper(key)] = root[key]
			delete(root, key)
		})
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"schema_version", 2}, {"schema_version", nil},
		{"timezone", "Etc/UTC"}, {"period", "week"}, {"since", "2026-08-01"}, {"until", "2026-09-03"},
		{"model", nil}, {"model", "模型 "}, {"surface", "openai"},
		{"buckets", nil}, {"anthropic", nil}, {"openai", nil},
	} {
		invalidMutation("contradictory "+tc.key, func(root map[string]any) { root[tc.key] = tc.value })
	}

	for _, native := range []string{"anthropic", "openai"} {
		for _, field := range []string{"rows", "models"} {
			invalidMutation(native+" NULL "+field, func(root map[string]any) { root[native].(map[string]any)[field] = nil })
			invalidMutation(native+" wrong filtered "+field, func(root map[string]any) {
				root[native].(map[string]any)[field].([]any)[0].(map[string]any)["model"] = "different"
			})
		}
		names := report.OpenAIMetrics()
		if native == "anthropic" {
			names = report.AnthropicMetrics()
		}
		for _, level := range []string{"rows", "models", "total"} {
			getTotal := func(root map[string]any) map[string]any {
				section := root[native].(map[string]any)
				if level == "total" {
					return section[level].(map[string]any)
				}
				return section[level].([]any)[0].(map[string]any)
			}
			for _, name := range names {
				for _, kind := range []string{"missing", "case variant", "NULL", "missing sum", "missing coverage", "coverage too high", "sum overflow"} {
					invalidMutation(native+"/"+level+"/"+name+"/"+kind, func(root map[string]any) {
						metrics := getTotal(root)["usage"].(map[string]any)
						metric := metrics[name].(map[string]any)
						switch kind {
						case "missing":
							delete(metrics, name)
						case "case variant":
							metrics[strings.ToUpper(name)] = metric
							delete(metrics, name)
						case "NULL":
							metrics[name] = nil
						case "missing sum":
							delete(metric, "sum")
						case "missing coverage":
							delete(metric, "reported_turns")
						case "coverage too high":
							metric["reported_turns"] = "3"
						case "sum overflow":
							metric["sum"] = "9223372036854775808"
						}
					})
				}
			}
			for _, bad := range []any{"-1", "+1", "01", "1.0", "1e0", "9223372036854775808", float64(2), nil} {
				invalidMutation(native+"/"+level+" invalid turns", func(root map[string]any) { getTotal(root)["turns"] = bad })
			}
			for _, tc := range []struct {
				name     string
				sum      any
				coverage string
			}{
				{"input_tokens", nil, "2"},
				{"output_tokens", "9", "1"},
				{names[2], nil, "1"},
				{names[2], "0", "0"},
			} {
				invalidMutation(native+"/"+level+" sum coverage contradiction", func(root map[string]any) {
					getTotal(root)["usage"].(map[string]any)[tc.name] = map[string]any{"sum": tc.sum, "reported_turns": tc.coverage}
				})
			}
		}
	}

	for _, tc := range []struct {
		name, old, replacement string
		valid                  bool
	}{
		{"duplicate root", `"schema_version":1`, `"schema_version":1,"schema_version":1`, false},
		{"escaped duplicate root", `"schema_version":1`, `"schema_version":1,"schema_versi\u006fn":1`, false},
		{"duplicate section", `"rows":`, `"rows":[],"rows":`, false},
		{"escaped duplicate row", `"bucket_start":`, `"bucket_start":"2026-09-01","bucket_\u0073tart":`, false},
		{"escaped duplicate metric member", `"sum":`, `"sum":null,"\u0073um":`, false},
		{"escaped duplicate native metric", `"thinking_tokens":`, `"thinking_tokens":{},"thinking_\u0074okens":`, false},
		{"duplicate additive nested", `"schema_version":1`, `"schema_version":1,"future":[{"x":1,"\u0078":2}]`, false},
		{"invalid UTF8", `"schema_version":1`, "\"schema_version\":1,\"future\":\"\xff\"", false},
		{"unpaired high", `"schema_version":1`, `"schema_version":1,"future":"\ud800"`, false},
		{"unpaired low", `"schema_version":1`, `"schema_version":1,"future":"\udc00"`, false},
		{"high non-low pair", `"schema_version":1`, `"schema_version":1,"future":"\ud800\u0061"`, false},
		{"surrogate key", `"schema_version":1`, `"schema_version":1,"\ud800":1`, false},
		{"valid pair and additive case", `"schema_version":1`, `"schema_version":1,"SCHEMA_VERSION":999,"future":{"pair":"\ud83d\ude00","x":1,"X":2}`, true},
	} {
		if !strings.Contains(base, tc.old) {
			t.Fatal("invalid mutation fixture")
		}
		cases = append(cases, example{name: tc.name, body: strings.Replace(base, tc.old, tc.replacement, 1), query: query, valid: tc.valid})
	}

	defaultQuery := query
	defaultQuery.Surface = ""
	mutate("default cannot select one Surface", defaultQuery, false, func(root map[string]any) {
		root["surface"] = "openai"
		delete(root, "anthropic")
	})
	invalidMutation("unknown effective Surface", func(root map[string]any) { root["surface"] = "unknown" })
	invalidMutation("optional sum requires coverage", func(root map[string]any) {
		root["anthropic"].(map[string]any)["total"].(map[string]any)["usage"].(map[string]any)["thinking_tokens"].(map[string]any)["sum"] = "0"
	})
	invalidMutation("coverage requires optional sum", func(root map[string]any) {
		root["anthropic"].(map[string]any)["total"].(map[string]any)["usage"].(map[string]any)["thinking_tokens"].(map[string]any)["reported_turns"] = "1"
	})
	anthropicQuery := query
	anthropicQuery.Surface = "anthropic"
	mutate("unselected known NULL", anthropicQuery, false, func(root map[string]any) {
		root["surface"] = "anthropic"
		root["openai"] = nil
	})
	mutate("unselected known section", anthropicQuery, false, func(root map[string]any) { root["surface"] = "anthropic" })
	mutate("unknown additive native case", anthropicQuery, true, func(root map[string]any) {
		root["surface"] = "anthropic"
		delete(root, "openai")
		root["OpenAI"] = nil
	})
	mutate("unknown native metric remains additive", query, true, func(root map[string]any) {
		metrics := root["anthropic"].(map[string]any)["total"].(map[string]any)["usage"].(map[string]any)
		metrics["future_native_tokens"] = map[string]any{"sum": "999"}
	})

	var body atomic.Value
	body.Store(base)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body.Store(tc.body)
			got, err := client.Query(context.Background(), tc.query)
			if (err == nil) != tc.valid {
				t.Fatalf("protocol valid=%t: %v", tc.valid, err)
			}
			if tc.valid && string(got.JSON) != tc.body {
				t.Fatal("original additive bytes lost")
			}
		})
	}
	if calls.Load() != int32(len(cases)) {
		t.Fatalf("protocol matrix made %d calls for %d cases", calls.Load(), len(cases))
	}
}

func TestClientValidatesEmptySectionContract(t *testing.T) {
	nonempty, _ := nonemptyReportJSON(t)
	var nonemptyRoot map[string]any
	if err := json.Unmarshal([]byte(nonempty), &nonemptyRoot); err != nil {
		t.Fatal(err)
	}
	var body atomic.Value
	body.Store(emptyJSON)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer server.Close()
	client, err := reporthttp.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	expectedCalls := 0
	for _, surface := range []string{"anthropic", "openai", "all"} {
		for _, kind := range []string{"valid", "NULL rows", "NULL models", "required NULL", "required positive", "optional zero", "optional coverage", "empty with rows", "unselected section"} {
			if surface == "all" && kind == "unselected section" {
				continue
			}
			expectedCalls++
			t.Run(surface+"/"+kind, func(t *testing.T) {
				var root map[string]any
				if err := json.Unmarshal([]byte(emptyJSON), &root); err != nil {
					t.Fatal(err)
				}
				var anthropic map[string]any
				if err := json.Unmarshal([]byte(emptyAnthropicSection), &anthropic); err != nil {
					t.Fatal(err)
				}
				root["surface"] = surface
				switch surface {
				case "anthropic":
					root["anthropic"] = anthropic
					delete(root, "openai")
				case "all":
					root["anthropic"] = anthropic
				}
				for _, native := range []string{"anthropic", "openai"} {
					if surface != "all" && surface != native {
						continue
					}
					section := root[native].(map[string]any)
					metrics := section["total"].(map[string]any)["usage"].(map[string]any)
					names := report.OpenAIMetrics()
					if native == "anthropic" {
						names = report.AnthropicMetrics()
					}
					switch kind {
					case "NULL rows":
						section["rows"] = nil
					case "NULL models":
						section["models"] = nil
					case "required NULL":
						metrics["input_tokens"].(map[string]any)["sum"] = nil
					case "required positive":
						metrics["input_tokens"].(map[string]any)["sum"] = "1"
					case "optional zero":
						metrics[names[2]].(map[string]any)["sum"] = "0"
					case "optional coverage":
						metric := metrics[names[2]].(map[string]any)
						metric["sum"] = "0"
						metric["reported_turns"] = "1"
					case "empty with rows":
						section["rows"] = nonemptyRoot[native].(map[string]any)["rows"]
					}
				}
				if kind == "unselected section" {
					if surface == "anthropic" {
						root["openai"] = root["anthropic"]
					} else {
						root["anthropic"] = root["openai"]
					}
				}
				raw, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				body.Store(string(raw))
				query := clientQuery()
				query.Surface = surface
				got, err := client.Query(context.Background(), query)
				valid := kind == "valid"
				if (err == nil) != valid {
					t.Fatalf("protocol valid=%t: %v", valid, err)
				}
				if valid && string(got.JSON) != string(raw) {
					t.Fatal("empty report bytes changed")
				}
			})
		}
	}
	if calls.Load() != int32(expectedCalls) {
		t.Fatalf("empty-section matrix made %d calls for %d cases", calls.Load(), expectedCalls)
	}
}
