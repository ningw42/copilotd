package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/config"
	"time"
)

func TestDailyOpenAIUsageCommandThroughProductionListener(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"report-turn","status":"completed","model":"reported-not-requested","usage":{"input_tokens":8012,"output_tokens":9,"input_tokens_details":{"cached_tokens":6000,"cache_write_tokens":2000},"output_tokens_details":{"reasoning_tokens":4},"total_tokens":8021}}`)
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	h := startUsageMeterServeHarness(t, upstream.URL, newPhase4Logger(t, &logs), nil, nil)
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
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("X-Request-Id") != "report-access" {
			t.Fatalf("unauthenticated report key=%q: %d %s", key, response.StatusCode, body)
		}
	}
	unauth, err := http.Post(h.baseURL+"/openai/v1/responses", "application/json", strings.NewReader(`{"model":"requested"}`))
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
	response, err := http.DefaultClient.Do(request)
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
	if err := h.stop(); err != nil {
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
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("disabled report touched database path: %v", err)
	}
	var out, stderr bytes.Buffer
	args := []string{"usage", "--endpoint", h.baseURL, "--surface", "openai", "--timezone", "UTC", "--since", "2026-09-01", "--until", "2026-09-02"}
	if code := run(args, noEnv(), &out, &stderr); code != 1 || out.Len() != 0 || !strings.Contains(stderr.String(), "usage_meter_disabled") {
		t.Fatalf("disabled command: %d stdout=%s stderr=%s", code, out.String(), stderr.String())
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
