package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/usage/report"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

func TestAnthropicAndCombinedUsageCommandThroughProductionListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "messages") {
			_, _ = io.WriteString(w, `{"id":"repeated","type":"message","stop_reason":"end_turn","model":"claude-reported","usage":{"input_tokens":12,"output_tokens":9,"cache_creation_input_tokens":2000,"cache_read_input_tokens":6000,"cache_creation":{"ephemeral_5m_input_tokens":750,"ephemeral_1h_input_tokens":1250},"output_tokens_details":{"thinking_tokens":4}}}`)
		} else {
			_, _ = io.WriteString(w, `{"id":"repeated","status":"completed","model":"openai-reported","usage":{"input_tokens":8012,"output_tokens":9,"input_tokens_details":{"cached_tokens":6000,"cache_write_tokens":2000},"output_tokens_details":{"reasoning_tokens":4},"total_tokens":8021}}`)
		}
	}))
	defer upstream.Close()
	h := startUsageMeterServeHarness(t, upstream.URL, discardLogger(t), nil, nil)
	now := time.Now().UTC()
	since, until := now.Format(time.DateOnly), now.AddDate(0, 0, 1).Format(time.DateOnly)
	base := []string{"usage", "--endpoint", h.baseURL, "--timezone", "UTC", "--since", since, "--until", until}
	invoke := func(surface string) string {
		t.Helper()
		args := append([]string{}, base...)
		if surface != "" {
			args = append(args, "--surface", surface)
		}
		var stdout, stderr bytes.Buffer
		if code := run(args, noEnv(), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("surface=%q exit=%d stderr=%s", surface, code, stderr.String())
		}
		return stdout.String()
	}
	empty := invoke("")
	if strings.Count(empty, "No stored Turns in the selected range.") != 2 || !strings.Contains(empty, "Anthropic\n") || !strings.Contains(empty, "OpenAI\n") {
		t.Fatalf("selected empty sections: %s", empty)
	}
	for _, path := range []string{"/anthropic/v1/messages", "/openai/v1/responses"} {
		request, _ := http.NewRequest("POST", h.baseURL+path, strings.NewReader(`{"model":"requested-alias"}`))
		request.Header.Set("Authorization", "Bearer "+testAPIKey)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("inference %s: %d", path, response.StatusCode)
		}
	}
	client, _ := reporthttp.NewClient(h.baseURL)
	deadline := time.Now().Add(4 * time.Second)
	for {
		// Normal asynchronous persistence, never a reporter-triggered flush.
		result, err := client.Query(context.Background(), report.Query{Timezone: "UTC", Since: since, Until: until})
		if err != nil {
			t.Fatal(err)
		}
		if result.Report.Anthropic.Total.Turns == 1 && result.Report.OpenAI.Total.Turns == 1 {
			if *result.Report.Anthropic.Total.Usage["ephemeral_5m_input_tokens"].Sum != 750 || *result.Report.Anthropic.Total.Usage["ephemeral_1h_input_tokens"].Sum != 1250 || *result.Report.Anthropic.Total.Usage["thinking_tokens"].Sum != 4 {
				t.Fatal("secondary native metrics lost in HTTP")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("normal persistence not visible through production reports")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, surface := range []string{"anthropic", "openai", "all", ""} {
		text := invoke(surface)
		if strings.Contains(text, "requested-alias") || strings.Contains(text, "Grand total") || strings.Contains(text, "No stored Turns") {
			t.Fatalf("invented identity/total/empty data: %s", text)
		}
		if surface != "openai" {
			for _, want := range []string{"Anthropic\n", "Uncached input", "Cache create", "claude-reported", "2,000", "6,000"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
		} else if strings.Contains(text, "Anthropic\n") {
			t.Fatal("unselected Anthropic section")
		}
		if surface != "anthropic" {
			for _, want := range []string{"OpenAI\n", "Cache write", "openai-reported", "8,012"} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q: %s", want, text)
				}
			}
		} else if strings.Contains(text, "OpenAI\n") || strings.Contains(text, "8,012") {
			t.Fatal("unselected OpenAI or synthesized Anthropic input")
		}
		if surface == "" || surface == "all" {
			if strings.Index(text, "Anthropic\n") > strings.Index(text, "OpenAI\n") || strings.Contains(text, `├─ "`) || strings.Contains(text, `└─ "`) || strings.Contains(text, "│ All ") || strings.Contains(text, "Model totals") || strings.Contains(text, "Section total") || strings.Contains(text, "│ Range") {
				t.Fatalf("native period groups: %s", text)
			}
		}
	}
}

func TestDailyOpenAIUsageCommandThroughProductionListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"report-turn","status":"completed","model":"reported-not-requested","usage":{"input_tokens":8012,"output_tokens":9,"input_tokens_details":{"cached_tokens":6000,"cache_write_tokens":2000},"output_tokens_details":{"reasoning_tokens":4},"total_tokens":8021}}`)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, &logs), nil, nil)
	// Own these probe/auth connections, including any unused speculative dial,
	// rather than leaving them in the process-global client's idle pool.
	client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(client.CloseIdleConnections)
	now := time.Now().UTC()
	since := now.Format(time.DateOnly)
	until := now.AddDate(0, 0, 1).Format(time.DateOnly)
	query := "?surface=openai&period=day&timezone=UTC&since=" + since + "&until=" + until
	for _, key := range []string{"", "wrong", testAPIKey} {
		request, _ := http.NewRequest("GET", h.baseURL+"/usage/v1/report"+query, nil)
		request.Header.Set("X-Request-Id", "report-access")
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("X-Request-Id") != "report-access" {
			t.Fatalf("unauthenticated report key=%q: %d %s", key, response.StatusCode, body)
		}
	}
	unauth, err := client.Post(h.baseURL+"/openai/v1/responses", "application/json", strings.NewReader(`{"model":"requested"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = unauth.Body.Close()
	if unauth.StatusCode != 401 {
		t.Fatalf("inference auth bypassed: %d", unauth.StatusCode)
	}
	request, _ := http.NewRequest("POST", h.baseURL+"/openai/v1/responses", strings.NewReader(`{"model":"requested"}`))
	request.Header.Set("Authorization", "Bearer "+testAPIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("inference: %d", response.StatusCode)
	}
	args := []string{"usage", "--endpoint", h.baseURL, "--surface", "openai", "--timezone", "UTC", "--since", since, "--until", until}
	deadline := time.Now().Add(4 * time.Second)
	for {
		var out, stderr bytes.Buffer
		if code := run(args, noEnv(), &out, &stderr); code != 0 {
			t.Fatalf("usage exit=%d stderr=%s", code, stderr.String())
		}
		if strings.Contains(out.String(), "8,012") && strings.Contains(out.String(), "reported-not-requested") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted report not visible: %s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.stopAfterClient(client); err != nil {
		t.Fatal(err)
	}
	h.closeStore()
	access := phase4LogLinesContaining(logs.String(), "msg=access", "request_id=report-access")
	if len(access) != 3 {
		t.Fatalf("report access count=%d", len(access))
	}
	for _, line := range access {
		if !strings.Contains(line, "level=INFO") || !strings.Contains(line, "inbound=/usage/v1/report") || strings.Contains(line, "surface=") || strings.Contains(line, "timezone=") {
			t.Fatalf("report access scope: %s", line)
		}
	}
}

func TestDisabledReportDoesNotOpenHistoryOrValidateTimezone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "usage.db")
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), func(cfg *config.ServeConfig) { cfg.ShimUsageMeterEnabled = false; cfg.UsageDBPath = path }, nil)
	response, err := http.Get(h.baseURL + "/usage/v1/report?timezone=%xx")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || !strings.Contains(string(body), "usage_meter_disabled") {
		t.Fatalf("disabled: %d %s", response.StatusCode, body)
	}
	requestReportStatus(t, h, "POST", "?timezone=%xx", 405, "method_not_allowed")
	requestReportStatus(t, h, "HEAD", "?timezone=%xx", 503, "usage_meter_disabled")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("disabled report touched database path: %v", err)
	}
	var out, stderr bytes.Buffer
	args := []string{"usage", "--endpoint", h.baseURL, "--surface", "openai", "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02"}
	if code := run(args, noEnv(), &out, &stderr); code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "usage_meter_disabled") {
		t.Fatalf("disabled command: %d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestUsageCalendarConfigurationThroughProductionListener(t *testing.T) {
	// Every explicit source bypasses unsupported process-local discovery inputs.
	t.Setenv("TZ", "invalid/rules")
	t.Setenv("TZDIR", "/unsupported")
	t.Setenv("ZONEINFO", "/absent")
	h := startUsageMeterServeHarness(t, "http://127.0.0.1:1", discardLogger(t), nil, nil)
	path := filepath.Join(t.TempDir(), "usage.toml")
	if err := os.WriteFile(path, []byte("timezone = 'Europe/Berlin'\nperiod = 'week'\nsince = '2020-12-31'\nuntil = '2021-01-05'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flags        []string
		env          map[string]string
		zone, period string
	}{
		{nil, nil, "Europe/Berlin", "week"},
		{nil, map[string]string{"COPILOTD_TIMEZONE": "US/Eastern", "COPILOTD_PERIOD": "month"}, "US/Eastern", "month"},
		{[]string{"--timezone", "Etc/UTC", "--period", "year"}, map[string]string{"COPILOTD_TIMEZONE": "US/Eastern", "COPILOTD_PERIOD": "month"}, "Etc/UTC", "year"},
		{[]string{"--timezone", "Asia/Tokyo", "--period", "day"}, nil, "Asia/Tokyo", "day"},
	} {
		args := append([]string{"usage", "--endpoint", h.baseURL, "--config", path}, tc.flags...)
		lookup := func(key string) (string, bool) { value, ok := tc.env[key]; return value, ok }
		var stdout, stderr bytes.Buffer
		code := run(args, lookup, &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `Timezone: "`+tc.zone+`"`) || !strings.Contains(stdout.String(), "Period: "+tc.period) || !strings.Contains(stdout.String(), "Range: 2020-12-31 to 2021-01-05 (exclusive)") || strings.Count(stdout.String(), "No stored Turns in the selected range.") != 2 {
			t.Fatalf("calendar configuration %s/%s: exit=%d stdout=%s stderr=%s", tc.zone, tc.period, code, stdout.String(), stderr.String())
		}
	}
}

func TestUsageHelpDescribesNativeSelectionAndPresentation(t *testing.T) {
	help := runSuccessfully(t, "usage", "--help")
	for _, want := range []string{"Anthropic and OpenAI Turns", "native Surface selection: all, anthropic, openai", "day, week, month, year", "current month's first day", "next month's first day", "terminal-local on supported Unix", "native Windows requires explicit", "exact non-empty UTF-8 Reported model", "secondary native tables for period-grouped model rows", "original validated JSON plus newline"} {
		if !strings.Contains(help, want) {
			t.Errorf("missing %q in usage help: %s", want, help)
		}
	}
	if strings.Contains(help, "explicit openai required") || strings.Contains(help, "no local discovery") || strings.Contains(help, "explicit named timezone)") || strings.Contains(help, "not yet supported") {
		t.Fatal("obsolete rollout restriction")
	}
}

func TestUsageHelpAndOperandsRemainSideEffectFree(t *testing.T) {
	help := runSuccessfully(t, "serve", "--help")
	if !strings.Contains(help, "unauthenticated reports on --addr") {
		t.Error("meter help does not disclose unauthenticated report exposure")
	}
	for _, args := range [][]string{{"usage", "--help"}, {"help", "usage"}} {
		var out, stderr bytes.Buffer
		env := func(key string) (string, bool) {
			if key == "COPILOTD_CONFIG" {
				return "/missing/config/never-open", true
			}
			return "", false
		}
		if code := run(args, env, &out, &stderr); code != 0 {
			t.Fatalf("help exit=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(out.String(), "--endpoint") || strings.Contains(out.String(), "--apikey") {
			t.Fatalf("usage help: %s", out.String())
		}
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"usage", "operand"}, noEnv(), &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "unexpected operand") {
		t.Fatalf("operand exit=%d stderr=%s", code, stderr.String())
	}
}
