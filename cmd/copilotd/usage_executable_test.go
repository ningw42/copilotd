package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/catalog"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/server"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/pricing"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// This is an OS-process acceptance test, not a call to run or a renderer. The
// public writer seeds synthetic prior history; live qualifying inference remains
// covered by usage_report_e2e_test.go and usage_report_concurrency_e2e_test.go.
func TestUsageExecutableAcceptance(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, logging.ForComponent(discardLogger(t), "internal/usage/sqlitestore"))
	if err != nil {
		t.Fatal(err)
	}
	number := func(n int64) *int64 { return &n }
	for _, model := range []string{"Model", " model ", "模型"} {
		for _, counts := range []usage.Usage{
			usage.AnthropicUsage{InputTokens: 12, OutputTokens: 9, CacheCreationInputTokens: number(2000), CacheReadInputTokens: number(6000), Ephemeral5mInputTokens: number(750), Ephemeral1hInputTokens: number(1250), ThinkingTokens: number(4)},
			usage.OpenAIUsage{InputTokens: 8012, OutputTokens: 9, CacheWriteTokens: number(2000), CachedTokens: number(6000), ReasoningTokens: number(4), TotalTokens: number(8021)},
		} {
			store.Record(usage.Turn{At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Model: model, Transport: usage.TransportBuffered, Usage: counts})
		}
	}
	for _, cached := range []*int64{number(0), nil} {
		store.Record(usage.Turn{At: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Model: "partial", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: cached}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := store.Close(ctx); !result.DriverCleanupCompleted || result.FinalFlushLosses != 0 {
		t.Fatalf("history setup: %+v", result)
	}
	endpoint := startUsageExecutable(t, binary, path, true)
	base := []string{"usage", "--endpoint", endpoint, "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02"}
	t.Run("compact", func(t *testing.T) {
		out := usageExec(t, binary, nil, 0, append(base, "--model", "Model")...)
		for _, want := range []string{" Anthropic \n", " OpenAI \n", "│ Day", "│ Model(s)", "Uncached input", "8,012"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q: %s", want, out)
			}
		}
		assertTextExcludes(t, out, "Reasoning", "Grand total", `"Model"`, `├─ "`, `└─ "`, "│ All ", "Model totals", "Section total", "│ Range", "Persisted successful Turns observed by the Usage meter", "Optional-count coverage refers only to stored Turns")
	})
	t.Run("periods_surfaces_and_exact_filters", func(t *testing.T) {
		for _, period := range []struct{ name, start string }{{"day", "2026-09-01"}, {"week", "2026-08-31"}, {"month", "2026-09-01"}, {"year", "2026-01-01"}} {
			for _, surface := range []string{"all", "anthropic", "openai"} {
				for _, model := range []string{"Model", " model ", "模型", "model"} {
					t.Run(period.name+"_"+surface+"_"+model, func(t *testing.T) {
						out := usageExec(t, binary, nil, 0, append(base, "--period", period.name, "--surface", surface, "--model", model, "--json")...)
						r := usageExecutableReport(t, out)
						if r.Period != period.name || r.Surface != surface || r.Model == nil || *r.Model != model || len(r.Buckets) != 1 || r.Buckets[0].StartDate != period.start {
							t.Fatalf("effective selections: %+v", r)
						}
						for _, native := range []struct {
							name    string
							section *report.Section
							input   int64
						}{{"anthropic", r.Anthropic, 12}, {"openai", r.OpenAI, 8012}} {
							if surface != "all" && surface != native.name {
								if native.section != nil {
									t.Fatal("unselected section")
								}
								continue
							}
							if native.section == nil {
								t.Fatal("missing selected section")
							}
							if model == "model" {
								if native.section.Total.Turns != 0 || len(native.section.Rows) != 0 {
									t.Fatal("filter lost case sensitivity")
								}
								continue
							}
							if native.section.Total.Turns != 1 || len(native.section.Rows) != 1 || native.section.Rows[0].Model != model || *native.section.Total.Usage["input_tokens"].Sum != native.input {
								t.Fatalf("native section: %+v", native.section)
							}
						}
					})
				}
			}
		}
	})
	t.Run("stored_turn_coverage", func(t *testing.T) {
		out := usageExec(t, binary, nil, 0, append(base, "--model", "partial", "--surface", "openai")...)
		for _, want := range []string{"0*", "1/2 stored Turns", "—"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q: %s", want, out)
			}
		}
		assertTextExcludes(t, out, "best-effort and potentially incomplete", "Optional-count coverage refers only to stored Turns")
	})
	t.Run("observed_inference_to_executable", func(t *testing.T) {
		openAIFixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "shim", "testdata", "usage", "openai-responses-buffered.recorded.json"))
		if err != nil {
			t.Fatal(err)
		}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "messages") {
				_, _ = io.WriteString(w, `{"id":"synthetic","type":"message","stop_reason":"end_turn","model":"observed","usage":{"input_tokens":12,"output_tokens":9}}`)
			} else {
				_, _ = w.Write(openAIFixture)
			}
		}))
		defer upstream.Close()
		h := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
		client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
		defer client.CloseIdleConnections()
		before := time.Now().UTC()
		for _, path := range []string{"/anthropic/v1/messages", "/openai/v1/responses"} {
			r, _ := http.NewRequest("POST", h.baseURL+path, strings.NewReader(`{"model":"requested-only"}`))
			r.Header.Set("Authorization", "Bearer "+testAPIKey)
			r.Header.Set("Content-Type", "application/json")
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("inference=%d", response.StatusCode)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			out := usageExec(t, binary, nil, 0, "usage", "--endpoint", h.baseURL, "--timezone", "UTC", "--since", before.Format(time.DateOnly), "--until", time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly), "--json")
			r := usageExecutableReport(t, out)
			if r.Anthropic.Total.Turns == 1 && r.OpenAI.Total.Turns == 1 {
				openAI := r.OpenAI.Total
				if *r.Anthropic.Total.Usage["input_tokens"].Sum != 12 || r.OpenAI.Rows[0].Model != "gpt-5.6-sol" ||
					*openAI.Usage["input_tokens"].Sum != 12 || *openAI.Usage["output_tokens"].Sum != 6 ||
					*openAI.Usage["cached_tokens"].Sum != 0 || *openAI.Usage["cache_write_tokens"].Sum != 0 ||
					*openAI.Usage["reasoning_tokens"].Sum != 0 || *openAI.Usage["total_tokens"].Sum != 18 {
					t.Fatalf("recorded native counts/identity changed: %+v", r.OpenAI)
				}
				var priced usageCostExecutableWire
				if err := json.Unmarshal([]byte(out), &priced); err != nil {
					t.Fatal(err)
				}
				cost := priced.OpenAI.Total.Cost
				if cost.Amount == nil || *cost.Amount != "0.000168" || cost.PricedTurns != "1" {
					t.Fatalf("recorded 12-token context did not use gpt-5.6-sol base rates: %+v", cost)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("normal asynchronous persistence not visible")
			}
			time.Sleep(25 * time.Millisecond)
		}
		if err := h.stopAfterClient(client); err != nil {
			t.Fatal(err)
		}
		t.Log("recorded September OpenAI fixture and synthetic Anthropic inference -> in-process production daemon/meter/writer -> actual usage executable; gpt-5.6-sol short context uses embedded base rates and preserves every native aggregate")
	})
	t.Run("details", func(t *testing.T) {
		out := usageExec(t, binary, nil, 0, append(base, "--model", "Model", "--details")...)
		for _, want := range []string{"Thinking", "Reasoning", "750", "1,250", "8,021"} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q: %s", want, out)
			}
		}
	})
	t.Run("default_and_independent_ranges", func(t *testing.T) {
		now := time.Now().UTC()
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for _, bounds := range [][]string{nil, {"--since", month.AddDate(0, 0, -1).Format(time.DateOnly)}, {"--until", month.AddDate(0, 1, 1).Format(time.DateOnly)}} {
			// Use the query's captured clock to avoid a wall-clock month rollover
			// race. This assertion pins calendar-month defaults, not rolling days.
			out := usageExec(t, binary, nil, 0, append([]string{"usage", "--endpoint", endpoint, "--timezone", "UTC", "--json"}, bounds...)...)
			r := usageExecutableReport(t, out)
			now := r.GeneratedAt
			first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			since, until := first.Format(time.DateOnly), first.AddDate(0, 1, 0).Format(time.DateOnly)
			if len(bounds) > 0 {
				if bounds[0] == "--since" {
					since = bounds[1]
				} else {
					until = bounds[1]
				}
			}
			if r.Since != since || r.Until != until || r.Period != "day" || r.Surface != "all" || r.Model != nil {
				t.Fatalf("defaults: %+v", r)
			}
		}
	})
	t.Run("config_scope_and_zone_precedence", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "shared.toml")
		if err := os.WriteFile(file, []byte("timezone = 'Europe/Berlin'\nmodel = 'Model'\nlog-level = 'not-a-level'\nusage-db-path = ''\napikey = ''\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			zone  string
			env   map[string]string
			flags []string
		}{
			{"Europe/Berlin", nil, nil}, {"UTC", map[string]string{"COPILOTD_TIMEZONE": "UTC"}, nil},
			{"Europe/Berlin", map[string]string{"COPILOTD_TIMEZONE": "UTC"}, []string{"--timezone", "Europe/Berlin"}},
		} {
			args := append([]string{"usage", "--endpoint", endpoint, "--config", file, "--since", "2026-09-01", "--until", "2026-09-02", "--json"}, tc.flags...)
			r := usageExecutableReport(t, usageExec(t, binary, tc.env, 0, args...))
			if r.Timezone != tc.zone || r.Model == nil || *r.Model != "Model" || r.OpenAI.Total.Turns != 1 {
				t.Fatalf("config: %+v", r)
			}
		}
		for _, flag := range []string{"--apikey", "--github-oauth-token", "--usage-db-path", "--log-file", "--addr"} {
			usageExec(t, binary, nil, 1, "usage", flag, "irrelevant")
		}
	})
	t.Run("process_timezone", func(t *testing.T) {
		args := []string{"usage", "--endpoint", endpoint, "--since", "2026-09-01", "--until", "2026-09-02", "--json"}
		for _, zone := range []string{"Europe/Berlin", ""} {
			code := 0
			if runtime.GOOS == "windows" {
				code = 1
			}
			out := usageExec(t, binary, map[string]string{"TZ": zone}, code, args...)
			if runtime.GOOS == "windows" {
				if !strings.Contains(out, "--timezone Area/City") {
					t.Fatal(out)
				}
				continue
			}
			r := usageExecutableReport(t, out)
			want := zone
			if want == "" {
				want = "UTC"
			}
			if r.Timezone != want {
				t.Fatalf("process timezone: %q", r.Timezone)
			}
		}
		out := usageExec(t, binary, map[string]string{"TZ": ":"}, 1, args...)
		if !strings.Contains(out, "--timezone Area/City") {
			t.Fatal(out)
		}
		if runtime.GOOS == "windows" {
			absent := filepath.Join(t.TempDir(), "absent")
			for _, zone := range []string{"Europe/Berlin", "UTC"} {
				r := usageExecutableReport(t, usageExec(t, binary, map[string]string{"GOROOT": absent, "ZONEINFO": absent, "TZ": "ignored"}, 0, append(args, "--timezone", zone)...))
				if r.Timezone != zone {
					t.Fatal("Windows embedded zone changed")
				}
			}
			if _, err := os.Stat(absent); !os.IsNotExist(err) {
				t.Fatalf("runtime timezone sources not absent: %v", err)
			}
			t.Log("native Windows explicit embedded loading with absent runtime GOROOT/ZONEINFO; no Unix platform fallback exists")
		}
	})
	t.Run("system_timezone", func(t *testing.T) {
		observations := map[string]any{"runtime_os": runtime.GOOS, "runtime_arch": runtime.GOARCH, "TZ": "absent", "TZDIR": "absent", "ZONEINFO": "absent", "configuration": os.Getenv("COPILOTD_TEST_SYSTEM_CONFIGURATION")}
		if runtime.GOOS != "windows" {
			for _, path := range []string{"/etc/localtime", "/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo", "/var/db/timezone/zoneinfo"} {
				link, linkErr := os.Readlink(path)
				resolved, resolveErr := filepath.EvalSymlinks(path)
				observations[path] = map[string]string{"link": link, "link_error": fmt.Sprint(linkErr), "resolved": resolved, "resolve_error": fmt.Sprint(resolveErr)}
			}
		}
		usageExecEvidence(t, "system-observation", observations)
		out := usageExec(t, binary, nil, -1, "usage", "--endpoint", endpoint, "--json")
		if json.Valid([]byte(out)) {
			r := usageExecutableReport(t, out)
			if runtime.GOOS == "windows" {
				t.Fatal("Windows must not auto-discover")
			}
			if want := os.Getenv("COPILOTD_TEST_SYSTEM_ZONE"); want != "" && r.Timezone != want {
				t.Fatalf("system zone=%q want=%q", r.Timezone, want)
			}
			t.Logf("native system-link discovery succeeded: %s", r.Timezone)
		} else {
			if !strings.Contains(out, "--timezone Area/City") {
				t.Fatal(out)
			}
			if os.Getenv("COPILOTD_TEST_SYSTEM_ZONE") != "" {
				t.Fatal("controlled native system discovery is mandatory")
			}
			t.Log("untouched configuration intentionally unsupported; controlled native CI run must separately demonstrate supported system-link discovery")
		}
	})
	t.Run("informational_and_local_errors", func(t *testing.T) {
		var calls atomic.Int32
		edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
		defer edge.Close()
		root := t.TempDir()
		env := map[string]string{"COPILOTD_CONFIG": filepath.Join(root, "missing.toml"), "COPILOTD_ENDPOINT": edge.URL, "TZ": ":", "ZONEINFO": "/absent", "HOME": root, "LOCALAPPDATA": root, "XDG_CONFIG_HOME": root}
		for _, args := range [][]string{nil, {"--help"}, {"help", "usage"}, {"usage", "--help"}, {"version"}, {"serve", "--help"}} {
			out := usageExec(t, binary, env, 0, args...)
			if len(args) > 0 && args[0] == "usage" && (!strings.Contains(out, "native Windows requires explicit") || strings.Contains(out, "--apikey")) {
				t.Fatal(out)
			}
		}
		for _, args := range [][]string{{"usage", "operand"}, {"usage", "--help", "operand"}} {
			usageExec(t, binary, env, 1, args...)
		}
		for _, name := range []string{"timezone", "model"} {
			out := usageExec(t, binary, map[string]string{"COPILOTD_ENDPOINT": edge.URL, "COPILOTD_TIMEZONE": "UTC"}, 1, "usage", "--"+name, "")
			if !strings.Contains(out, name) {
				t.Fatal(out)
			}
		}
		if calls.Load() != 0 {
			t.Fatal("informational/local errors contacted HTTP")
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("informational command created files: %v %v", entries, err)
		}
	})
	t.Run("prefix_and_original_json", func(t *testing.T) {
		target, _ := url.Parse(endpoint)
		proxy := httputil.NewSingleHostReverseProxy(target)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		proxy.Transport = transport
		defer transport.CloseIdleConnections()
		var calls atomic.Int32
		edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/prefix/usage/v1/report" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("prefix or credential contract")
				w.WriteHeader(400)
				return
			}
			r.URL.Path = "/usage/v1/report"
			proxy.ServeHTTP(w, r)
		}))
		defer edge.Close()
		args := []string{"usage", "--endpoint", edge.URL + "/prefix/", "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02", "--model", "Model", "--json"}
		raw := usageExec(t, binary, nil, 0, args...)
		usageExecutableReport(t, raw)
		if calls.Load() != 1 {
			t.Fatal("not exactly one request")
		}
		// Owned HTTP boundary for original bytes/additive fields, not a claim
		// that the production daemon currently emits the future additive field.
		original := " {\"future\":{\"exact\":9007199254740993}," + strings.TrimSpace(raw)[1:] + " \n"
		frozen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, original)
		}))
		defer frozen.Close()
		args[2] = frozen.URL
		if out := usageExec(t, binary, nil, 0, append(args, "--details")...); out != original+"\n" {
			t.Fatal("original JSON changed")
		}
	})
	t.Run("closed_stdout_pipe", func(t *testing.T) {
		for _, view := range []string{"compact", "--details", "--json"} {
			t.Run(view, func(t *testing.T) {
				readEnd, writeEnd, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = writeEnd.Close() })
				if err := readEnd.Close(); err != nil {
					t.Fatal(err)
				}
				args := append([]string(nil), base...)
				if view != "compact" {
					args = append(args, view)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, binary, args...)
				command.Env = usageExecEnv(nil)
				// An inherited *os.File is real stdout fd 1, not os/exec's writer
				// copying goroutine. On Unix its EPIPE normally raises SIGPIPE.
				command.Stdout = writeEnd
				var stderr bytes.Buffer
				command.Stderr = &stderr
				err = command.Run()
				exit, ok := err.(*exec.ExitError)
				usageExecEvidence(t, "closed-stdout-pipe", map[string]any{"argv": args, "result": fmt.Sprint(err), "stderr": stderr.String(), "runtime_os": runtime.GOOS, "stdout": "inherited pipe with no readers"})
				if !ok || exit.ExitCode() != 1 || !strings.HasPrefix(stderr.String(), "copilotd: write ") || !strings.HasSuffix(stderr.String(), "\n") || strings.Contains(stderr.String(), "schema_version") || strings.Contains(stderr.String(), "Model") {
					t.Fatalf("closed stdout pipe must return CLI exit 1 with safe error: result=%v stderr=%q", err, stderr.String())
				}
				// Error reporting itself may meet a second broken pipe. Keep the
				// policy active through that attempt; do not claim stderr delivery
				// when it is unavailable, or allow it to turn exit 1 into SIGPIPE.
				command = exec.CommandContext(ctx, binary, args...)
				command.Env = usageExecEnv(nil)
				command.Stdout, command.Stderr = writeEnd, writeEnd
				err = command.Run()
				exit, ok = err.(*exec.ExitError)
				usageExecEvidence(t, "closed-both-pipes", map[string]any{"argv": args, "result": fmt.Sprint(err), "stdout_and_stderr": "inherited pipe with no readers"})
				if !ok || exit.ExitCode() != 1 {
					t.Fatalf("signal handling ended before error reporting: %v", err)
				}
			})
		}
	})
	t.Run("disabled_empty_unreachable_protocol_and_output", func(t *testing.T) {
		empty := usageExec(t, binary, nil, 0, append(base, "--model", "absent")...)
		if strings.Count(empty, "No stored Turns in the selected range.") != 2 {
			t.Fatal(empty)
		}
		missing := filepath.Join(t.TempDir(), "never-created", "usage.db")
		disabled := startUsageExecutable(t, binary, missing, false)
		out := usageExec(t, binary, nil, 1, "usage", "--endpoint", disabled, "--timezone", "UTC")
		if !strings.Contains(out, "usage_meter_disabled") {
			t.Fatal(out)
		}
		if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
			t.Fatalf("disabled touched usage path: %v", err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := "http://" + listener.Addr().String()
		_ = listener.Close()
		usageExec(t, binary, nil, 1, "usage", "--endpoint", dead, "--timezone", "UTC")
		for _, response := range []struct {
			status            int
			contentType, body string
		}{{404, "text/html", "<h1>private proxy detail</h1>"}, {200, "application/json", `{"schema_version":99}`}, {302, "text/plain", "redirect"}} {
			edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", response.contentType)
				w.Header().Set("Location", endpoint)
				w.WriteHeader(response.status)
				_, _ = io.WriteString(w, response.body)
			}))
			out := usageExec(t, binary, nil, 1, "usage", "--endpoint", edge.URL, "--timezone", "UTC", "--json")
			edge.Close()
			if strings.Contains(out, "private proxy detail") {
				t.Fatal("untrusted body leaked")
			}
		}
		// Retain ordinary file-write failures separately from the actual pipe
		// and Unix SIGPIPE behavior exercised by closed_stdout_pipe.
		file := filepath.Join(t.TempDir(), "read-only")
		if err := os.WriteFile(file, nil, 0600); err != nil {
			t.Fatal(err)
		}
		stdout, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer stdout.Close()
		for _, view := range []string{"--json", "--details"} {
			command := exec.Command(binary, append(base, view)...)
			command.Env = usageExecEnv(nil)
			command.Stdout = stdout
			var stderr bytes.Buffer
			command.Stderr = &stderr
			err := command.Run()
			exit, ok := err.(*exec.ExitError)
			usageExecEvidence(t, "output-error", map[string]any{"argv": command.Args[1:], "result": fmt.Sprint(err), "stderr": stderr.String(), "stdout": "read-only inherited file"})
			if !ok || exit.ExitCode() != 1 || stderr.Len() == 0 {
				t.Fatalf("output error: %v %s", err, &stderr)
			}
		}
	})
}

const usageCostArtifactFirst = `{
  "openai":{"id":"openai","models":{
    "gpt-tiered":{"id":"gpt-tiered","cost":{"input":1,"output":1,"cache_read":1,"cache_write":1,"tiers":[{"input":3,"output":5,"cache_read":2,"cache_write":7,"tier":{"type":"context","size":200}},{"input":2,"output":4,"cache_read":1,"cache_write":6,"tier":{"type":"context","size":100}}]}},
    "rematch":{"id":"rematch","cost":{"input":1,"output":2}},
    "shared":{"id":"shared","cost":{"input":99,"output":99}}
  }},
  "anthropic":{"id":"anthropic","models":{
    "claude-tiered":{"id":"claude-tiered","cost":{"input":1,"output":1,"cache_read":1,"cache_write":1,"tiers":[{"input":2,"output":4,"cache_read":1,"cache_write":6,"tier":{"type":"context","size":100}},{"input":3,"output":5,"cache_read":2,"cache_write":7,"tier":{"type":"context","size":200}}]}},
    "shared":{"id":"shared","cost":{"input":88,"output":88}}
  }},
  "google":{"id":"google","models":{}},
  "xai":{"id":"xai","models":{}},
  "github-copilot":{"id":"github-copilot","models":{"gpt-tiered":{"id":"gpt-tiered","cost":{"input":999,"output":999}}}}
}`

const usageCostArtifactSecond = `{
  "openai":{"id":"openai","models":{
    "gpt-tiered":{"id":"gpt-tiered","cost":{"input":1,"output":1,"cache_read":1,"cache_write":1,"tiers":[{"input":6,"output":10,"cache_read":4,"cache_write":14,"tier":{"type":"context","size":200}},{"input":2,"output":4,"cache_read":1,"cache_write":6,"tier":{"type":"context","size":100}}]}},
    "rematch-20260901":{"id":"rematch-20260901","cost":{"input":4,"output":6}},
    "shared":{"id":"shared","cost":{"input":99,"output":99}}
  }},
  "anthropic":{"id":"anthropic","models":{
    "claude-tiered":{"id":"claude-tiered","cost":{"input":1,"output":1,"cache_read":1,"cache_write":1,"tiers":[{"input":2,"output":4,"cache_read":1,"cache_write":6,"tier":{"type":"context","size":100}},{"input":6,"output":10,"cache_read":4,"cache_write":14,"tier":{"type":"context","size":200}}]}},
    "shared":{"id":"shared","cost":{"input":88,"output":88}}
  }},
  "google":{"id":"google","models":{}},
  "xai":{"id":"xai","models":{}}
}`

func TestUsageCostExecutableAcceptance(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	var artifact atomic.Value
	artifact.Store(usageCostArtifactFirst)
	var priceCalls atomic.Int32
	refreshEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	prices := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := priceCalls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Editor-Version") != "" || r.Header.Get("Editor-Plugin-Version") != "" || r.Header.Get("Copilot-Integration-Id") != "" {
			t.Errorf("pricing source request carried method/credentials/impersonation: %s %#v", r.Method, r.Header)
		}
		if call == 2 {
			close(refreshEntered)
			<-releaseRefresh
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, artifact.Load().(string))
	}))
	t.Cleanup(prices.Close)

	h := startUsageMeterServeHarnessWithPricing(t, "http://127.0.0.1:1", discardLogger(t), func(cfg *config.ServeConfig) {
		cfg.UsagePricingRefreshInterval = time.Hour
	}, nil, pricing.NewRemote(prices.URL, prices.Client().Transport))
	// Registered after the harness so a fatal assertion releases a blocked
	// refresh before server/cache cleanup waits for its goroutines.
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRefresh) }) })
	waitForUsagePriceSource(t, h, "fetched")
	if priceCalls.Load() != 1 {
		t.Fatalf("startup pricing fetch calls = %d, want 1", priceCalls.Load())
	}

	million, zero := int64(1_000_000), int64(0)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, turn := range []usage.Turn{
		{At: at, Model: "gpt-tiered", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 3 * million, OutputTokens: million, CachedTokens: &million, CacheWriteTokens: &million}},
		// This short-context Turn resolves to the same Pricing model but must use
		// its base vector rather than either structured context tier.
		{At: at, Model: "gpt-tiered-fast", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 100, CachedTokens: &zero, CacheWriteTokens: &zero}},
		{At: at, Model: "rematch", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: million, OutputTokens: million, CachedTokens: &zero, CacheWriteTokens: &zero}},
		{At: at, Model: "shared", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: million, OutputTokens: million, CachedTokens: &zero, CacheWriteTokens: &zero}},
		{At: at, Model: "unknown", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: million, OutputTokens: million, CachedTokens: &zero, CacheWriteTokens: &zero}},
		{At: at, Model: "claude-tiered", Transport: usage.TransportBuffered, Usage: usage.AnthropicUsage{InputTokens: million, OutputTokens: million, CacheCreationInputTokens: &million, CacheReadInputTokens: &million, Ephemeral5mInputTokens: usageInt64(400_000), Ephemeral1hInputTokens: usageInt64(600_000)}},
		// Surface does not select the benchmark provider: this Anthropic-native Turn
		// intentionally resolves the OpenAI original-provider identity.
		{At: at, Model: "gpt-tiered", Transport: usage.TransportBuffered, Usage: usage.AnthropicUsage{InputTokens: million, OutputTokens: million, CacheCreationInputTokens: &million, CacheReadInputTokens: &million}},
	} {
		h.store.Record(turn)
	}

	query := report.Query{Timezone: "UTC", Since: "2026-09-01", Until: "2026-09-02", Surface: "all"}
	waitForUsageTurns(t, h.baseURL, query, 2, 5)
	argsFor := func(surface string) []string {
		return []string{"usage", "--endpoint", h.baseURL, "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02", "--surface", surface, "--json"}
	}
	args := argsFor("all")
	first := decodeUsageCostExecutable(t, usageExec(t, binary, nil, 0, args...))
	assertUsageCostRevision(t, first, "17", "3", "17", "rematch", "exact", "20.0001", "34")
	openAIOnly := decodeUsageCostExecutable(t, usageExec(t, binary, nil, 0, argsFor("openai")...))
	if len(openAIOnly.OpenAI.Rows) != 5 || len(openAIOnly.Anthropic.Rows) != 0 || openAIOnly.OpenAI.Total.Cost.Amount == nil || *openAIOnly.OpenAI.Total.Cost.Amount != "20.0001" {
		t.Fatalf("OpenAI-only actual executable history = %+v", openAIOnly)
	}
	anthropicOnly := decodeUsageCostExecutable(t, usageExec(t, binary, nil, 0, argsFor("anthropic")...))
	if len(anthropicOnly.Anthropic.Rows) != 2 || len(anthropicOnly.OpenAI.Rows) != 0 || anthropicOnly.Anthropic.Total.Cost.Amount == nil || *anthropicOnly.Anthropic.Total.Cost.Amount != "34" {
		t.Fatalf("Anthropic-only actual executable history = %+v", anthropicOnly)
	}

	text := usageExec(t, binary, nil, 0, "usage", "--endpoint", h.baseURL, "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02", "--details")
	for _, want := range []string{"Est. USD", "20.000*", "34.000", "Pricing: original-provider / models.dev standard + context rates / single cache-write rate", "Pricing model resolutions (Reported → Pricing)", "gpt-tiered → openai/gpt-tiered (exact)", "gpt-tiered-fast → openai/gpt-tiered (suffix)", "shared → ambiguous", "unknown → unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("actual executable text missing %q: %s", want, text)
		}
	}

	artifact.Store(usageCostArtifactSecond)
	primeDone := make(chan struct{})
	go func() {
		h.caches.Prime(context.Background())
		close(primeDone)
	}()
	select {
	case <-refreshEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement pricing refresh did not reach local source")
	}
	// A report never joins the blocked fetch and stays entirely on the previously
	// captured revision. The source call count also proves report-time no-network.
	during := decodeUsageCostExecutable(t, usageExec(t, binary, nil, 0, args...))
	assertUsageCostRevision(t, during, "17", "3", "17", "rematch", "exact", "20.0001", "34")
	if priceCalls.Load() != 2 {
		t.Fatalf("report triggered pricing network calls: %d", priceCalls.Load())
	}
	releaseOnce.Do(func() { close(releaseRefresh) })
	select {
	case <-primeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement pricing refresh did not finish")
	}
	waitForUsagePriceSource(t, h, "fetched")
	if priceCalls.Load() != 2 {
		t.Fatalf("replacement pricing fetch calls = %d, want 2", priceCalls.Load())
	}
	second := decodeUsageCostExecutable(t, usageExec(t, binary, nil, 0, args...))
	assertUsageCostRevision(t, second, "34", "10", "34", "rematch-20260901", "dated", "44.0001", "68")
	if first.Pricing.Version == second.Pricing.Version || first.OpenAI.Total.Turns != second.OpenAI.Total.Turns || first.Anthropic.Total.Turns != second.Anthropic.Total.Turns {
		t.Fatalf("repricing did not change only the captured tariff/match revision: first=%+v second=%+v", first, second)
	}
	t.Log("local synthetic models.dev source -> shared cache lifecycle -> production reporter/HTTP -> actual CLI; per-Turn context tiers, aggregate cache-write, original-provider, unknown/ambiguous, report-time no-network and price/identity repricing verified without new database writes")
}

type usageCostExecutableWire struct {
	Pricing struct {
		Version     string     `json:"version"`
		Source      string     `json:"source"`
		LastSuccess *time.Time `json:"last_success"`
	} `json:"pricing"`
	Anthropic usageCostExecutableSection `json:"anthropic"`
	OpenAI    usageCostExecutableSection `json:"openai"`
}

type usageCostExecutableSection struct {
	Rows  []usageCostExecutableRow `json:"rows"`
	Total struct {
		Turns string   `json:"turns"`
		Cost  wireCost `json:"cost"`
	} `json:"total"`
}

type usageCostExecutableRow struct {
	Model        string    `json:"model"`
	Cost         wireCost  `json:"cost"`
	PricingMatch wireMatch `json:"pricing_match"`
}

type wireCost struct {
	Amount      *string `json:"amount"`
	PricedTurns string  `json:"priced_turns"`
	Unpriced    struct {
		UnknownModel   string `json:"unknown_model"`
		AmbiguousModel string `json:"ambiguous_model"`
	} `json:"unpriced"`
}

type wireMatch struct {
	Status   string `json:"status"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Method   string `json:"method"`
}

func decodeUsageCostExecutable(t *testing.T, output string) usageCostExecutableWire {
	t.Helper()
	var wire usageCostExecutableWire
	if err := json.Unmarshal([]byte(output), &wire); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, `"context_policy"`) {
		t.Fatal("actual executable response retained presentation-only context_policy")
	}
	if wire.Pricing.Source != "fetched" || wire.Pricing.Version == "" || wire.Pricing.LastSuccess == nil {
		t.Fatalf("actual executable pricing provenance = %+v", wire.Pricing)
	}
	return wire
}

func assertUsageCostRevision(t *testing.T, wire usageCostExecutableWire, tiered, rematch, anthropicTiered, rematchModel, rematchMethod, openAITotal, anthropicTotal string) {
	t.Helper()
	openAI := usageCostRowsByModel(wire.OpenAI.Rows)
	anthropic := usageCostRowsByModel(wire.Anthropic.Rows)
	for model, want := range map[string]string{"gpt-tiered": tiered, "gpt-tiered-fast": "0.0001", "rematch": rematch} {
		row, ok := openAI[model]
		if !ok || row.Cost.Amount == nil || *row.Cost.Amount != want || row.Cost.PricedTurns != "1" || row.PricingMatch.Status != "matched" || row.PricingMatch.Provider != "openai" {
			t.Fatalf("OpenAI %s revision row = %+v", model, row)
		}
	}
	if row := openAI["gpt-tiered-fast"]; row.PricingMatch.Model != "gpt-tiered" || row.PricingMatch.Method != "suffix" {
		t.Fatalf("short-context resolution = %+v, want gpt-tiered/suffix", row.PricingMatch)
	}
	if row := openAI["rematch"]; row.PricingMatch.Model != rematchModel || row.PricingMatch.Method != rematchMethod {
		t.Fatalf("rematch resolution = %+v, want %s/%s", row.PricingMatch, rematchModel, rematchMethod)
	}
	if row := openAI["shared"]; row.Cost.Amount != nil || row.Cost.Unpriced.AmbiguousModel != "1" || row.PricingMatch.Status != "ambiguous" {
		t.Fatalf("ambiguous row = %+v", row)
	}
	if row := openAI["unknown"]; row.Cost.Amount != nil || row.Cost.Unpriced.UnknownModel != "1" || row.PricingMatch.Status != "unknown" {
		t.Fatalf("unknown row = %+v", row)
	}
	for model, provider := range map[string]string{"claude-tiered": "anthropic", "gpt-tiered": "openai"} {
		row, ok := anthropic[model]
		if !ok || row.Cost.Amount == nil || *row.Cost.Amount != anthropicTiered || row.PricingMatch.Provider != provider || row.PricingMatch.Status != "matched" {
			t.Fatalf("Anthropic %s original-provider row = %+v", model, row)
		}
	}
	if wire.OpenAI.Total.Cost.Amount == nil || *wire.OpenAI.Total.Cost.Amount != openAITotal || wire.OpenAI.Total.Cost.PricedTurns != "3" || wire.OpenAI.Total.Turns != "5" || wire.Anthropic.Total.Cost.Amount == nil || *wire.Anthropic.Total.Cost.Amount != anthropicTotal || wire.Anthropic.Total.Cost.PricedTurns != "2" || wire.Anthropic.Total.Turns != "2" {
		t.Fatalf("combined section totals = OpenAI %+v Anthropic %+v", wire.OpenAI.Total, wire.Anthropic.Total)
	}
}

func usageCostRowsByModel(rows []usageCostExecutableRow) map[string]usageCostExecutableRow {
	result := make(map[string]usageCostExecutableRow, len(rows))
	for _, row := range rows {
		result[row.Model] = row
	}
	return result
}

func usageInt64(value int64) *int64 { return &value }

func waitForUsageTurns(t *testing.T, endpoint string, query report.Query, anthropic, openAI int64) {
	t.Helper()
	client, err := reporthttp.NewClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		result, err := client.Query(context.Background(), query)
		if err == nil && result.Report.Anthropic.Total.Turns == anthropic && result.Report.OpenAI.Total.Turns == openAI {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("synthetic history did not become visible: result=%+v err=%v", result.Report, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForUsagePriceSource(t *testing.T, h *usageMeterServeHarness, source string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		statuses := h.caches.Observe()
		for _, status := range statuses {
			if status.Name == "usage_prices" && status.Source == source && status.LastSuccess != nil && status.LastAttemptResult != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pricing source did not reach %s: %+v", source, statuses)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUsageExecutableReadsGenericRecovery(t *testing.T) {
	binary := usageAcceptanceBinary(t)
	cfg := e2eConfig("unused-recovery-oauth-token")
	logger := discardLogger(t)
	provider := identity.NewStatic(identity.Credential{BaseURL: "http://127.0.0.1:1", Token: "unused-recovery-copilot-token"}, true)
	forwarder := newTestForwarderWithLogger(
		provider,
		forward.NewClient(cfg.ResponseHeaderTimeout),
		cfg.OutboundTimeout,
		cfg.WriteTimeout,
		cfg.StreamIdleTimeout,
		cfg.StreamKeepaliveInterval,
		cfg.MaxRequestBytes,
		cfg.MaxBufferedResponseBytes,
		logger,
		configuredShimRegistry(cfg, nil),
	)
	const panicSentinel = "private-usage-recovery-panic-sentinel"
	observed := make(chan string, 3)
	reportHandler := reporthttp.Handler(func(ctx context.Context, _ report.Query) (report.Report, error) {
		id, ok := logging.RequestIDFrom(ctx)
		if !ok {
			id = "missing-request-id"
		}
		observed <- id
		panic(panicSentinel)
	})
	base := startTestServer(t, server.New(
		cfg,
		logging.ForComponent(logger, "internal/server"),
		logging.ForComponent(logger, "internal/catalog"),
		newTestDependencyErrorLog(),
		provider,
		newTestReadyObservers(),
		forwarder,
		newTestCatalogSource(provider),
		newTestWSProxy(provider),
		server.NewStreamOutcomeCounter(),
		catalog.RenderDescriptors{},
		reportHandler,
	))

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "text"},
		{name: "json", args: []string{"--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"usage", "--endpoint", base, "--timezone", "UTC"}, tc.args...)
			stderr := usageExec(t, binary, nil, 1, args...)
			var requestID string
			select {
			case requestID = <-observed:
			case <-time.After(time.Second):
				t.Fatal("report callback request ID observation exceeded bound")
			}
			if requestID == "missing-request-id" || !logging.ValidRequestID(requestID) {
				t.Fatalf("callback request ID=%q", requestID)
			}
			if len(stderr) > 512 || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "500") || !strings.Contains(stderr, requestID) {
				t.Fatalf("recovery diagnostic is unbounded or uncorrelated: %q", stderr)
			}
			for _, r := range stderr {
				if r != '\n' && (r < 0x20 || r > 0x7e) {
					t.Fatalf("recovery diagnostic contains non-ASCII/control character %U: %q", r, stderr)
				}
			}
			for _, forbidden := range []string{"internal server error", panicSentinel, "goroutine ", ".go:", "runtime/debug", "runtime.gopanic", "\x1b"} {
				if strings.Contains(stderr, forbidden) {
					t.Fatalf("recovery diagnostic leaked %q: %q", forbidden, stderr)
				}
			}
		})
	}
	select {
	case id := <-observed:
		t.Fatalf("unexpected extra report query with request ID %q", id)
	default:
	}
}

func usageExecutableReport(t *testing.T, output string) report.Report {
	t.Helper()
	var r report.Report
	if err := json.Unmarshal([]byte(output), &r); err != nil {
		t.Fatal(err)
	}
	if r.SchemaVersion != 1 || r.Collection != "best_effort" || r.Scope != "configured_database" || r.GeneratedAt.IsZero() || !strings.HasSuffix(output, "\n") {
		t.Fatalf("report metadata: %+v", r)
	}
	// Independent wire check: integers must actually be strings, not merely
	// values recoverable by a permissive numeric decoder.
	if !strings.Contains(output, `"reported_turns":"`) || !strings.Contains(output, `"turns":"`) {
		t.Fatal("counts are not exact decimal strings")
	}
	return r
}

func usageAcceptanceBinary(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("COPILOTD_TEST_BINARY"); binary != "" {
		return binary
	}
	binary := filepath.Join(t.TempDir(), "copilotd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	// The enclosing test is run through nix develop locally, or setup-go on CI.
	command := exec.Command("go", "build", "-o", binary, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, out)
	}
	return binary
}

// Keep only OS process prerequisites. Never pass a developer's COPILOTD_*,
// credentials, proxy bypasses, or timezone overrides to the synthetic executable.
func usageExecEnv(extra map[string]string) []string {
	var env []string
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

func usageExec(t *testing.T, binary string, env map[string]string, code int, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = usageExecEnv(env)
	command.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	got := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			got = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	usageExecEvidence(t, "invocation", map[string]any{"argv": args, "exit": got, "stdout": stdout.String(), "stderr": stderr.String()})
	if (code != -1 && got != code) || (got != 0 && got != 1) || (got == 0 && stderr.Len() != 0) || (got != 0 && (stdout.Len() != 0 || stderr.Len() == 0)) {
		t.Fatalf("argv=%q exit=%d want=%d err=%v stdout=%s stderr=%s", args, got, code, err, &stdout, &stderr)
	}
	if got != 0 {
		return stderr.String()
	}
	return stdout.String()
}

func usageExecEvidence(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", name, data)
	if dir := os.Getenv("COPILOTD_TEST_EVIDENCE"); dir != "" {
		dir = filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		file, err := os.CreateTemp(dir, name+"-*.json")
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("evidence: %v %v", writeErr, closeErr)
		}
	}
}

func startUsageExecutable(t *testing.T, binary, database string, enabled bool) string {
	t.Helper()
	// Real HTTPS CONNECT boundary: startup mint is refused locally. No tunnel,
	// real credential or public network is used; discovery/refresh is disabled.
	mint := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "CONNECT" && r.Host == "api.github.com:443" {
			select {
			case mint <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	_ = reservation.Close()
	args := []string{"serve", "--addr", addr, "--apikey", "synthetic-inbound-key", "--github-oauth-token", "synthetic-not-a-github-token", "--usage-db-path", database,
		fmt.Sprintf("--shim-usage-meter-enabled=%t", enabled), "--impersonation-refresh-interval=0", "--codex-catalog-refresh-interval=0", "--usage-pricing-refresh-interval=0", "--startup-mint-retries=0", "--shutdown-timeout=2s"}
	command := exec.Command(binary, args...)
	env := map[string]string{"HTTPS_PROXY": proxy.URL, "HTTP_PROXY": proxy.URL}
	if runtime.GOOS == "windows" {
		absent := filepath.Join(t.TempDir(), "absent")
		env["GOROOT"], env["ZONEINFO"] = absent, absent
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("Windows runtime source must be absent: %v", err)
		}
	}
	command.Env = usageExecEnv(env)
	command.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		// Windows os.Process cannot send an interrupt; cleanup there is a kill,
		// not evidence of graceful signal shutdown. In-process drain tests apply.
		if runtime.GOOS == "windows" {
			_ = command.Process.Kill()
		} else {
			_ = command.Process.Signal(os.Interrupt)
		}
		select {
		case err := <-done:
			usageExecEvidence(t, "daemon", map[string]any{"argv": args, "result": fmt.Sprint(err), "stdout": stdout.String(), "stderr": stderr.String(), "windows_cleanup_kill": runtime.GOOS == "windows"})
			if runtime.GOOS != "windows" && err != nil {
				t.Errorf("daemon stop: %v\n%s", err, &stderr)
			}
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
			t.Error("daemon cleanup exceeded bound")
		}
	})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("executable did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-mint:
	case <-time.After(5 * time.Second):
		t.Fatal("startup exchange did not reach the rejecting local proxy")
	}
	return "http://" + addr
}
