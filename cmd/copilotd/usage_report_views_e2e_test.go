package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/report"
)

func TestUsageInvalidResolvedModelNeverMakesHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "usage.toml")
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		flags      []string
	}{
		{"empty file", "model = ''", nil, nil},
		{"empty env", "model = 'valid'", map[string]string{"COPILOTD_MODEL": ""}, nil},
		{"empty flag", "model = 'valid'", map[string]string{"COPILOTD_MODEL": "valid"}, []string{"--model", ""}},
		{"invalid env", "model = 'valid'", map[string]string{"COPILOTD_MODEL": "bad\xff"}, nil},
		{"invalid flag", "model = 'valid'", map[string]string{"COPILOTD_MODEL": "valid"}, []string{"--model", "bad\xff"}},
	} {
		if err := os.WriteFile(path, []byte(tc.file), 0600); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"usage", "--endpoint", server.URL, "--timezone", "UTC", "--config", path, "--json"}, tc.flags...)
		lookup := func(key string) (string, bool) { v, ok := tc.env[key]; return v, ok }
		var out, stderr bytes.Buffer
		if code := run(args, lookup, &out, &stderr); code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "model") {
			t.Fatalf("%s exit=%d stdout=%q stderr=%q", tc.name, code, out.String(), stderr.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid resolved model made %d HTTP requests", calls.Load())
	}
}

func TestUsageDaemonErrorsStayBoundedEscapedAndOffStdout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "id-\u202e"+strings.Repeat("r", 4096)+"ID-END")
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "error": map[string]string{"code": "usage_unavailable\x1b", "message": "bad\n\x1b[31m\u202e" + strings.Repeat("m", 4096) + "MESSAGE-END"}})
	}))
	defer server.Close()
	for _, view := range [][]string{nil, {"--details"}, {"--json"}, {"--details", "--json"}} {
		var out, stderr bytes.Buffer
		args := append([]string{"usage", "--endpoint", server.URL, "--timezone", "UTC"}, view...)
		if code := run(args, noEnv(), &out, &stderr); code != 1 || out.Len() != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
		text := stderr.String()
		for _, want := range []string{"503", "usage_unavailable", `\x1b`, `\n`, `\u202e`, "request ID"} {
			if !strings.Contains(text, want) {
				t.Errorf("missing %q: %s", want, text)
			}
		}
		if len(text) > 1500 || strings.Count(text, "\n") != 1 || strings.ContainsAny(text, "\x1b\u202e") || strings.Contains(text, "MESSAGE-END") || strings.Contains(text, "ID-END") {
			t.Fatalf("unsafe/unbounded error: %q", text)
		}
	}
}

func TestUsageOutputFailuresExitOneIncludingFinalJSONNewline(t *testing.T) {
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), nil, nil)
	// Freeze the daemon bytes at the owned HTTP seam to identify the exact final
	// newline boundary independently of the command's chosen write strategy.
	response, err := http.Get(h.baseURL + "/usage/v1/report?timezone=UTC&since=2026-09-01&until=2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	for _, jsonMode := range []bool{false, true} {
		limits := []int{0, 5}
		if jsonMode {
			limits = append(limits, len(raw))
		}
		for _, limit := range limits {
			for _, failure := range []error{nil, io.ErrClosedPipe} {
				out := &usageLimitedWriter{left: limit, err: failure}
				var stderr bytes.Buffer
				args := []string{"usage", "--endpoint", server.URL, "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02"}
				if jsonMode {
					args = append(args, "--json")
				}
				if code := run(args, noEnv(), out, &stderr); code != 1 || stderr.Len() == 0 {
					t.Fatalf("output failure exit=%d stderr=%q", code, stderr.String())
				}
			}
		}
	}
}

type usageLimitedWriter struct {
	left int
	err  error
}

func (w *usageLimitedWriter) Write(p []byte) (int, error) {
	n := min(w.left, len(p))
	w.left -= n
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestUsageExactModelConfigurationThroughProductionListener(t *testing.T) {
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), nil, nil)
	number := func(n int64) *int64 { return &n }
	for _, model := range []string{"Model", "model", " model ", "é", "e\u0301", "模型", " ", "model\x00suffix"} {
		requested := "requested-only"
		for _, counts := range []usage.Usage{usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9, ReasoningTokens: number(4), TotalTokens: number(8021)}, usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9, ThinkingTokens: number(4), CacheCreationInputTokens: number(2000), Ephemeral5mInputTokens: number(750), Ephemeral1hInputTokens: number(1250)}} {
			h.store.Record(usage.Turn{At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Model: model, RequestedModel: &requested, Transport: usage.TransportBuffered, Usage: counts})
		}
	}
	for _, counts := range []usage.Usage{usage.OpenAIUsage{InputTokens: 1}, usage.AnthropicUsage{InputTokens: 1}} {
		h.store.Record(usage.Turn{At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Model: strings.Repeat("x", report.MaxModelBytes+1), Transport: usage.TransportBuffered, Usage: counts})
	}
	h.closeStore() // Complete fixture persistence; the reporter itself never flushes.
	response, err := http.Get(h.baseURL + "/usage/v1/report?timezone=UTC&since=2026-09-01&until=2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 422 || !strings.Contains(string(body), "report_too_large") {
		t.Fatalf("unfiltered oversized identity: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	// Each exact selection below must exclude BOTH unrelated oversized values.
	path := filepath.Join(t.TempDir(), "usage.toml")
	if err := os.WriteFile(path, []byte("timezone = 'UTC'\nsince = '2026-09-01'\nuntil = '2026-09-02'\nmodel = 'Model'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"anthropic", "openai", "all"} {
		for _, tc := range []struct {
			model string
			flags []string
			env   map[string]string
		}{
			{"Model", nil, nil},
			{"model", nil, map[string]string{"COPILOTD_MODEL": "model"}},
			{" model ", []string{"--model", " model "}, map[string]string{"COPILOTD_MODEL": "model"}},
			{"é", []string{"--model", "é"}, nil}, {"e\u0301", []string{"--model", "e\u0301"}, nil}, {"模型", []string{"--model", "模型"}, nil},
			{" ", []string{"--model", " "}, nil}, {"model\x00suffix", []string{"--model", "model\x00suffix"}, nil},
			{"requested-only", []string{"--model", "requested-only"}, nil}, {"MODEL", []string{"--model", "MODEL"}, nil},
		} {
			for _, view := range [][]string{nil, {"--details"}, {"--json"}, {"--details", "--json"}} {
				args := append([]string{"usage", "--endpoint", h.baseURL, "--config", path, "--surface", surface}, tc.flags...)
				args = append(args, view...)
				lookup := func(key string) (string, bool) { v, ok := tc.env[key]; return v, ok }
				var stdout, stderr bytes.Buffer
				if code := run(args, lookup, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
					t.Fatalf("%s/%q exit=%d stderr=%s", surface, tc.model, code, stderr.String())
				}
				text := stdout.String()
				if len(view) > 0 && view[len(view)-1] == "--json" {
					var r report.Report
					if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
						t.Fatal(err)
					}
					if r.Model == nil || *r.Model != tc.model || r.Surface != surface {
						t.Fatalf("effective metadata: %+v", r)
					}
					for _, native := range []struct {
						name  string
						s     *report.Section
						input int64
					}{{"anthropic", r.Anthropic, 12}, {"openai", r.OpenAI, 8012}} {
						if surface != "all" && surface != native.name {
							if native.s != nil {
								t.Fatal("unselected JSON section")
							}
							continue
						}
						if native.s == nil {
							t.Fatal("selected JSON section missing")
						}
						if tc.model == "requested-only" || tc.model == "MODEL" {
							if native.s.Total.Turns != 0 || len(native.s.Rows) != 0 {
								t.Fatal("unknown filter is not empty")
							}
							continue
						}
						if len(native.s.Rows) != 1 || native.s.Rows[0].Model != tc.model || *native.s.Total.Usage["input_tokens"].Sum != native.input {
							t.Fatalf("filtered native JSON: %+v", native.s)
						}
						if native.name == "anthropic" {
							if *native.s.Total.Usage["thinking_tokens"].Sum != 4 || *native.s.Total.Usage["ephemeral_5m_input_tokens"].Sum != 750 || *native.s.Total.Usage["ephemeral_1h_input_tokens"].Sum != 1250 {
								t.Fatal("Anthropic secondary JSON")
							}
						} else {
							if *native.s.Total.Usage["reasoning_tokens"].Sum != 4 || *native.s.Total.Usage["total_tokens"].Sum != 8021 {
								t.Fatal("OpenAI secondary JSON")
							}
						}
					}
					continue
				}
				if tc.model == "requested-only" || tc.model == "MODEL" {
					want := 1
					if surface == "all" {
						want = 2
					}
					if strings.Count(text, "No stored Turns in the selected range.") != want {
						t.Fatal(text)
					}
				} else {
					if !strings.Contains(text, strconv.QuoteToASCII(tc.model)) || strings.Contains(text, "No stored Turns") {
						t.Fatal(text)
					}
					if surface != "anthropic" && !strings.Contains(text, "8,012") {
						t.Fatal(text)
					}
					if surface != "openai" && !strings.Contains(text, "Uncached input") {
						t.Fatal(text)
					}
					if len(view) > 0 {
						if surface != "openai" && (!strings.Contains(text, "Thinking") || !strings.Contains(text, "750") || !strings.Contains(text, "1,250")) {
							t.Fatal(text)
						}
						if surface != "anthropic" && (!strings.Contains(text, "Reasoning") || !strings.Contains(text, "8,021")) {
							t.Fatal(text)
						}
					}
				}
				if (surface == "openai" && strings.Contains(text, "Anthropic\n")) || (surface == "anthropic" && strings.Contains(text, "OpenAI\n")) {
					t.Fatal("unselected section")
				}
			}
		}
	}
}
