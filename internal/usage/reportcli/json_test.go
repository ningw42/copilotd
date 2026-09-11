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

func TestCommandRejectsInvalidReportsBeforeEitherPresentation(t *testing.T) {
	valid, err := json.Marshal(commandReport())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body string
	}{
		{"malformed", `{"`},
		{"unsupported schema", strings.Replace(string(valid), `"schema_version":1`, `"schema_version":2`, 1)},
		{"contradictory selection", strings.Replace(string(valid), `"timezone":"UTC"`, `"timezone":"Etc/UTC"`, 1)},
	}
	var body atomic.Value
	body.Store(string(valid))
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
	utc := "UTC"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body.Store(tc.body)
			for _, jsonMode := range []bool{false, true} {
				var out bytes.Buffer
				err := reportcli.Run(context.Background(), client, reportcli.Options{
					Endpoint: server.URL,
					Timezone: &utc,
					Query:    report.Query{Surface: "openai", Period: "day", Since: "2026-09-01", Until: "2026-09-02"},
					JSON:     jsonMode,
					Details:  true,
					Timeout:  time.Second,
				}, &out)
				if err == nil {
					t.Fatalf("JSON=%t accepted invalid report", jsonMode)
				}
				if out.Len() != 0 {
					t.Fatalf("JSON=%t protocol failure emitted partial output", jsonMode)
				}
			}
		})
	}
	if calls.Load() != int32(2*len(cases)) {
		t.Fatalf("invalid reports made %d calls", calls.Load())
	}
}

func TestCommandPresentsEmptySelectedSections(t *testing.T) {
	emptyTotal := func(names []string) report.Total {
		metrics := make(map[string]report.Metric, len(names))
		for _, name := range names {
			metrics[name] = report.Metric{}
		}
		metrics["input_tokens"] = report.Metric{Sum: number(0)}
		metrics["output_tokens"] = report.Metric{Sum: number(0)}
		return report.Total{Usage: metrics}
	}
	r := commandReport()
	r.Surface = "all"
	r.OpenAI = &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}, Total: emptyTotal(report.OpenAIMetrics())}
	r.Anthropic = &report.Section{Rows: []report.Row{}, Models: []report.ModelTotal{}, Total: emptyTotal(report.AnthropicMetrics())}
	for _, jsonMode := range []bool{false, true} {
		body, output, err := commandResult(t, r, true, jsonMode)
		if err != nil {
			t.Fatalf("JSON=%t: %v", jsonMode, err)
		}
		if jsonMode {
			if output != string(body)+"\n" {
				t.Fatal("empty JSON changed")
			}
		} else if strings.Count(output, "No stored Turns in the selected range.") != 2 {
			t.Fatal(output)
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
