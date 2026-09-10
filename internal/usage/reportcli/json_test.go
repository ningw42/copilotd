package reportcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reportcli"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

// This matrix characterizes the inherited shared validator through BOTH public
// command presentations. Validation never reconstructs calendar rules or
// replaces server totals; text may derive presentation-only period totals.
func TestCommandValidatesCompleteWireContractBeforeEitherPresentation(t *testing.T) {
	r := commandReport()
	r.Surface = "all"
	model := "模型"
	r.Model = &model
	r.OpenAI.Rows[0].Model = model
	r.OpenAI.Models[0].Model = model
	a := report.ModelTotal{Model: model, Total: report.Total{Turns: 2, Usage: map[string]report.Metric{
		"input_tokens": {Sum: number(12), ReportedTurns: 2}, "output_tokens": {Sum: number(9), ReportedTurns: 2},
		"cache_creation_input_tokens": {}, "cache_read_input_tokens": {}, "ephemeral_5m_input_tokens": {}, "ephemeral_1h_input_tokens": {}, "thinking_tokens": {Sum: number(0), ReportedTurns: 1},
	}}}
	r.Anthropic = &report.Section{Rows: []report.Row{{BucketStart: "2026-09-01", ModelTotal: a}}, Models: []report.ModelTotal{a}, Total: a.Total}
	base, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	type example struct {
		name, body string
		valid      bool
	}
	cases := []example{{"complete native report", string(base), true}, {"malformed", `{"`, false}, {"trailing", string(base) + ` {}`, false}}
	mutate := func(name string, change func(map[string]any)) {
		var root map[string]any
		if err := json.Unmarshal(base, &root); err != nil {
			t.Fatal(err)
		}
		change(root)
		body, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, example{name, string(body), false})
	}
	for _, key := range []string{"schema_version", "generated_at", "timezone", "period", "since", "until", "window_start", "window_end", "scope", "collection", "surface", "model", "buckets", "anthropic", "openai"} {
		mutate("missing "+key, func(root map[string]any) { delete(root, key) })
		mutate("case is not "+key, func(root map[string]any) { root[strings.ToUpper(key)] = root[key]; delete(root, key) })
	}
	for _, tc := range []struct {
		key   string
		value any
	}{{"schema_version", 2}, {"schema_version", nil}, {"timezone", "Etc/UTC"}, {"model", nil}, {"model", "模型 "}, {"surface", "openai"}, {"buckets", nil}, {"anthropic", nil}, {"openai", nil}} {
		mutate("contradictory "+tc.key, func(root map[string]any) { root[tc.key] = tc.value })
	}
	for _, native := range []string{"anthropic", "openai"} {
		for _, field := range []string{"rows", "models"} {
			mutate(native+" NULL "+field, func(root map[string]any) { root[native].(map[string]any)[field] = nil })
			mutate(native+" wrong filtered "+field, func(root map[string]any) {
				root[native].(map[string]any)[field].([]any)[0].(map[string]any)["model"] = "different"
			})
		}
		names := []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens"}
		if native == "anthropic" {
			names = []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens", "thinking_tokens"}
		}
		for _, level := range []string{"rows", "models", "total"} {
			getTotal := func(root map[string]any) map[string]any {
				s := root[native].(map[string]any)
				if level == "total" {
					return s[level].(map[string]any)
				}
				return s[level].([]any)[0].(map[string]any)
			}
			for _, name := range names {
				for _, kind := range []string{"missing", "case variant", "NULL", "missing sum", "missing coverage", "coverage too high", "sum overflow"} {
					mutate(native+"/"+level+"/"+name+"/"+kind, func(root map[string]any) {
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
				mutate(native+"/"+level+" invalid turns", func(root map[string]any) { getTotal(root)["turns"] = bad })
			}
			for _, tc := range []struct {
				name     string
				sum      any
				coverage string
			}{{"input_tokens", nil, "2"}, {"output_tokens", "9", "1"}, {names[2], nil, "1"}, {names[2], "0", "0"}} {
				mutate(native+"/"+level+" sum coverage contradiction", func(root map[string]any) {
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
		{"escaped duplicate metric", `"sum":`, `"sum":null,"\u0073um":`, false},
		{"duplicate additive nested", `"schema_version":1`, `"schema_version":1,"future":[{"x":1,"\u0078":2}]`, false},
		{"invalid UTF8", `"schema_version":1`, "\"schema_version\":1,\"future\":\"\xff\"", false},
		{"unpaired high", `"schema_version":1`, `"schema_version":1,"future":"\ud800"`, false},
		{"unpaired low", `"schema_version":1`, `"schema_version":1,"future":"\udc00"`, false},
		{"high non-low pair", `"schema_version":1`, `"schema_version":1,"future":"\ud800\u0061"`, false},
		{"surrogate key", `"schema_version":1`, `"schema_version":1,"\ud800":1`, false},
		{"valid pair and additive case", `"schema_version":1`, `"schema_version":1,"SCHEMA_VERSION":999,"future":{"pair":"\ud83d\ude00","x":1,"X":2}`, true},
	} {
		if !strings.Contains(string(base), tc.old) {
			t.Fatal("invalid mutation fixture")
		}
		cases = append(cases, example{tc.name, strings.Replace(string(base), tc.old, tc.replacement, 1), tc.valid})
	}
	var body atomic.Value
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body.Store(tc.body)
			for _, jsonMode := range []bool{false, true} {
				options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "all", Period: "day", Since: "2026-09-01", Until: "2026-09-02", Model: &model}, JSON: jsonMode, Details: true, Timeout: time.Second}
				var out bytes.Buffer
				err := reportcli.Run(context.Background(), client, options, &out)
				if (err == nil) != tc.valid {
					t.Fatalf("JSON=%t valid=%t err=%v", jsonMode, tc.valid, err)
				}
				if !tc.valid && out.Len() != 0 {
					t.Fatal("protocol failure emitted partial output")
				}
				if tc.valid && jsonMode && out.String() != tc.body+"\n" {
					t.Fatal("original bytes changed")
				}
			}
		})
	}
	if calls.Load() != int32(2*len(cases)) {
		t.Fatal("protocol validation retried HTTP")
	}
}

func TestCommandEmptySectionCoverageExceptionInTextAndJSON(t *testing.T) {
	for _, surface := range []string{"anthropic", "openai", "all"} {
		for _, kind := range []string{"valid", "NULL rows", "NULL models", "required NULL", "required positive", "optional zero", "optional coverage", "empty with rows", "unselected section"} {
			if surface == "all" && kind == "unselected section" {
				continue
			}
			r := commandReport()
			r.Surface = surface
			r.OpenAI = nil
			for _, native := range []string{"anthropic", "openai"} {
				if surface != "all" && surface != native {
					continue
				}
				names := report.OpenAIMetrics()
				if native == "anthropic" {
					names = report.AnthropicMetrics()
				}
				metrics := map[string]report.Metric{}
				for _, name := range names {
					metrics[name] = report.Metric{}
				}
				metrics["input_tokens"] = report.Metric{Sum: number(0)}
				metrics["output_tokens"] = report.Metric{Sum: number(0)}
				s := &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}, Total: report.Total{Usage: metrics}}
				switch kind {
				case "NULL rows":
					s.Rows = nil
				case "NULL models":
					s.Models = nil
				case "required NULL":
					metrics["input_tokens"] = report.Metric{}
				case "required positive":
					metrics["input_tokens"] = report.Metric{Sum: number(1)}
				case "optional zero":
					metrics[names[2]] = report.Metric{Sum: number(0)}
				case "optional coverage":
					metrics[names[2]] = report.Metric{Sum: number(0), ReportedTurns: 1}
				case "empty with rows":
					s.Rows = commandReport().OpenAI.Rows
				}
				if native == "anthropic" {
					r.Anthropic = s
				} else {
					r.OpenAI = s
				}
			}
			if kind == "unselected section" {
				if surface == "anthropic" {
					r.OpenAI = r.Anthropic
				} else {
					r.Anthropic = r.OpenAI
				}
			}
			body, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			client, _ := reporthttp.NewClient(server.URL)
			utc := "UTC"
			for _, jsonMode := range []bool{false, true} {
				var out bytes.Buffer
				err := reportcli.Run(context.Background(), client, reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: surface}, JSON: jsonMode, Details: true, Timeout: time.Second}, &out)
				if (err == nil) != (kind == "valid") {
					t.Fatalf("%s/%s JSON=%t: %v", surface, kind, jsonMode, err)
				}
				if err != nil && out.Len() != 0 {
					t.Fatal("invalid empty report emitted output")
				}
				if err == nil {
					if jsonMode {
						if out.String() != string(body)+"\n" {
							t.Fatal("empty JSON changed")
						}
					} else {
						want := 1
						if surface == "all" {
							want = 2
						}
						if strings.Count(out.String(), "No stored Turns in the selected range.") != want {
							t.Fatal(out.String())
						}
					}
				}
			}
			server.Close()
		}
	}
}

// A writer may fail after accepting some bytes, or short-write without an error.
// In JSON mode the final newline is part of successful output, not best effort.
func TestCommandReturnsPartialShortAndNewlineOutputFailures(t *testing.T) {
	raw, err := json.Marshal(commandReport())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	for _, jsonMode := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			limit int
			err   error
		}{{"body failure", 0, io.ErrClosedPipe}, {"partial body failure", 5, io.ErrClosedPipe}, {"short body", 5, nil}, {"newline failure", len(raw), io.ErrClosedPipe}, {"short newline", len(raw), nil}} {
			// Newline boundary belongs to original-byte JSON.
			if !jsonMode && tc.limit == len(raw) {
				continue
			}
			t.Run(tc.name+map[bool]string{true: "/JSON", false: "/text"}[jsonMode], func(t *testing.T) {
				out := &limitedOutput{remaining: tc.limit, err: tc.err}
				err := reportcli.Run(context.Background(), client, reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai"}, JSON: jsonMode, Details: true, Timeout: time.Second}, out)
				if err == nil {
					t.Fatal("partial output succeeded")
				}
				if jsonMode && tc.limit == len(raw) && out.buffer.String() != string(raw) {
					t.Fatal("newline failure did not retain original complete body")
				}
			})
		}
	}
}

type limitedOutput struct {
	remaining int
	err       error
	buffer    bytes.Buffer
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := min(w.remaining, len(p))
	w.buffer.Write(p[:n])
	w.remaining -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestCommandJSONPreservesOriginalBytesAndPresentationOnlySelection(t *testing.T) {
	r := commandReport()
	model := " 模型é\u202e😀 "
	r.Model = &model
	r.OpenAI.Rows[0].Model = model
	r.OpenAI.Models[0].Model = model
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Unknown case variants are additive, not substitutes. Preserve whitespace,
	// native Unicode, exact decimal strings, and a valid escaped surrogate pair.
	raw := " \n\t" + strings.Replace(string(body), "{", "{\n  \"SCHEMA_VERSION\": 999, \"additive\": {\"number\":9007199254740993123456789,\"pair\":\"\\ud83d\\ude00\"},", 1) + " \n\t"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		want := url.Values{"timezone": {"UTC"}, "surface": {"openai"}, "since": {"2026-09-01"}, "until": {"2026-09-02"}, "model": {model}}
		if request.URL.RawQuery != want.Encode() || request.Method != "GET" || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Errorf("selection/credentials: %s %v", request.URL, request.Header)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, raw)
	}))
	defer server.Close()
	client, _ := reporthttp.NewClient(server.URL)
	utc := "UTC"
	options := reportcli.Options{Endpoint: server.URL, Timezone: &utc, Query: report.Query{Surface: "openai", Since: "2026-09-01", Until: "2026-09-02", Model: &model}, JSON: true, Timeout: time.Second}
	for _, details := range []bool{false, true} {
		options.Details = details
		var out bytes.Buffer
		if err := reportcli.Run(context.Background(), client, options, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != raw+"\n" {
			t.Fatalf("original JSON changed: %q", out.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("presentation made %d requests", calls.Load())
	}
}
