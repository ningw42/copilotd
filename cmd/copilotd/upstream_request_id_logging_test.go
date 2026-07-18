package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/identity"
)

func TestForwardedResponsesLogDifferentUpstreamRequestID(t *testing.T) {
	const (
		requestBodySecret  = "request-body-must-not-be-logged"
		responseBodySecret = "response-body-must-not-be-logged"
		responseHeaderData = "unrelated-header-must-not-be-logged"
	)

	upstreamIDs := map[string]string{
		"POST /v1/messages":              "upstream-anthropic-messages-request-id",
		"POST /v1/messages/count_tokens": "upstream-anthropic-token-count-request-id",
		"POST /responses":                "upstream-openai-streaming-request-id",
		"GET /models":                    "upstream-support-get-request-id",
		"HEAD /models":                   "upstream-support-head-request-id",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", upstreamIDs[r.Method+" "+r.URL.Path])
		w.Header().Set("X-Unrelated-Response-Header", responseHeaderData)
		switch r.URL.Path {
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"secret\":\""+responseBodySecret+"\"}\n\n")
		case "/models":
			w.WriteHeader(http.StatusTeapot)
		case "/v1/messages", "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"secret":"`+responseBodySecret+`"}`)
		default:
			t.Errorf("unexpected upstream Route %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   phase4CopilotToken,
	}, true)
	cfg := e2eConfig("unused-oauth-token")
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	logger := newPhase4Logger(t, &logs)
	base := startPhase4Server(t, cfg, provider, logger)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		requestID  string
		upstreamID string
		wantStatus int
	}{
		{
			name:       "Anthropic Messages buffered inference",
			method:     http.MethodPost,
			path:       "/anthropic/v1/messages",
			body:       `{"input":"` + requestBodySecret + `"}`,
			requestID:  "resolved-anthropic-messages-request-id",
			upstreamID: upstreamIDs["POST /v1/messages"],
			wantStatus: http.StatusOK,
		},
		{
			name:       "Anthropic token counting buffered inference",
			method:     http.MethodPost,
			path:       "/anthropic/v1/messages/count_tokens",
			body:       `{"input":"` + requestBodySecret + `"}`,
			requestID:  "resolved-anthropic-token-count-request-id",
			upstreamID: upstreamIDs["POST /v1/messages/count_tokens"],
			wantStatus: http.StatusOK,
		},
		{
			name:       "OpenAI Responses streaming inference",
			method:     http.MethodPost,
			path:       "/openai/v1/responses",
			body:       `{"stream":true,"input":"` + requestBodySecret + `"}`,
			requestID:  "resolved-openai-streaming-request-id",
			upstreamID: upstreamIDs["POST /responses"],
			wantStatus: http.StatusOK,
		},
		{
			name:       "GitHub Copilot raw support GET",
			method:     http.MethodGet,
			path:       "/models",
			requestID:  "resolved-support-get-request-id",
			upstreamID: upstreamIDs["GET /models"],
			wantStatus: http.StatusTeapot,
		},
		{
			name:       "GitHub Copilot raw support HEAD",
			method:     http.MethodHead,
			path:       "/models",
			requestID:  "resolved-support-head-request-id",
			upstreamID: upstreamIDs["HEAD /models"],
			wantStatus: http.StatusTeapot,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			resp, _ := doPhase4Request(t, nil, tc.method, base+tc.path, body, func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+phase4APIKey)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Request-Id", tc.requestID)
			})
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{tc.requestID}) {
				t.Errorf("downstream X-Request-Id = %q, want sole resolved ID", got)
			}

			lines := phase4LogLinesContaining(logs.String(), "upstream_request_id="+tc.upstreamID)
			if len(lines) != 1 {
				t.Fatalf("upstream correlation records for %q = %d, want one:\n%s", tc.upstreamID, len(lines), logs.String())
			}
			if !strings.Contains(lines[0], "request_id="+tc.requestID) {
				t.Errorf("upstream correlation record lacks resolved ID %q: %s", tc.requestID, lines[0])
			}
		})
	}

	for _, secret := range []string{
		phase4APIKey,
		phase4CopilotToken,
		requestBodySecret,
		responseBodySecret,
		responseHeaderData,
	} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("private material %q appeared in logs:\n%s", secret, logs.String())
		}
	}
}

func TestForwardedResponsesDoNotLogAbsentOrIdenticalUpstreamRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("upstream") == "identical" {
			w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
		}
		switch r.URL.Path {
		case "/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
		case "/models":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected upstream Route %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   phase4CopilotToken,
	}, true)
	cfg := e2eConfig("unused-oauth-token")
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	logger := newPhase4Logger(t, &logs)
	base := startPhase4Server(t, cfg, provider, logger)

	paths := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "buffered inference", method: http.MethodPost, path: "/anthropic/v1/messages", body: `{}`},
		{name: "streaming inference", method: http.MethodPost, path: "/openai/v1/responses", body: `{"stream":true}`},
		{name: "raw support", method: http.MethodGet, path: "/models"},
	}
	for _, mode := range []string{"absent", "identical"} {
		for _, path := range paths {
			t.Run(mode+"/"+path.name, func(t *testing.T) {
				requestID := "resolved-" + mode + "-" + strings.ReplaceAll(path.name, " ", "-")
				var body io.Reader
				if path.body != "" {
					body = strings.NewReader(path.body)
				}
				resp, _ := doPhase4Request(t, nil, path.method, base+path.path+"?upstream="+mode, body, func(req *http.Request) {
					req.Header.Set("Authorization", "Bearer "+phase4APIKey)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Request-Id", requestID)
				})
				if got := resp.Header.Values("X-Request-Id"); !reflect.DeepEqual(got, []string{requestID}) {
					t.Errorf("downstream X-Request-Id = %q, want sole resolved ID", got)
				}
			})
		}
	}

	if strings.Contains(logs.String(), "upstream_request_id=") {
		t.Errorf("absent or identical upstream ID produced a correlation record:\n%s", logs.String())
	}
}

func TestStreamingResponseLogsUpstreamRequestIDBeforeBodyCompletes(t *testing.T) {
	const (
		requestID  = "resolved-open-stream-request-id"
		upstreamID = "upstream-open-stream-request-id"
	)
	releaseTerminal := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", upstreamID)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_ = http.NewResponseController(w).Flush()
		<-releaseTerminal
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	t.Cleanup(upstream.Close)

	provider := identity.NewStatic(identity.Credential{
		BaseURL: upstream.URL,
		Token:   phase4CopilotToken,
	}, true)
	cfg := e2eConfig("unused-oauth-token")
	cfg.APIKey = phase4APIKey
	var logs bytes.Buffer
	logger := newPhase4Logger(t, &logs)
	base := startPhase4Server(t, cfg, provider, logger)

	req, err := http.NewRequest(http.MethodPost, base+"/openai/v1/responses", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatalf("build streaming request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+phase4APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(releaseTerminal)
		t.Fatalf("start streaming request: %v", err)
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "upstream_request_id="+upstreamID) || !strings.Contains(logOutput, "request_id="+requestID) {
		t.Errorf("open stream lacks immediate upstream correlation:\n%s", logOutput)
	}

	close(releaseTerminal)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
}
