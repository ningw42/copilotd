package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/config"
	"github.com/ningw42/copilotd/internal/forward"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

type catalogFailureScenario struct {
	name          string
	ready         bool
	authorize     bool
	credentialErr error
	baseURL       string
	responseLimit int64
	roundTrip     serverRoundTripFunc
	wantStatus    int
	wantCalls     int32
	wantErrorType func(apierror.Surface) string
}

func TestCatalogLocalFailuresHaveGETEquivalentHEADFramingOverRealListener(t *testing.T) {
	apiError := func(apierror.Surface) string { return "api_error" }
	authError := func(surface apierror.Surface) string {
		if surface == apierror.OpenAI {
			return "invalid_request_error"
		}
		return "authentication_error"
	}
	freshResponse := func(status int, body func() io.ReadCloser) serverRoundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: body(), Request: r}, nil
		}
	}
	scenarios := []catalogFailureScenario{
		{name: "authentication wins over readiness", ready: false, wantStatus: 401, wantErrorType: authError},
		{name: "readiness gate", ready: false, authorize: true, wantStatus: 503, wantErrorType: apiError},
		{name: "fetch-time credential", ready: true, authorize: true, credentialErr: errors.New("credential-secret"), wantStatus: 503, wantErrorType: apiError},
		{name: "request construction", ready: true, authorize: true, baseURL: ":", wantStatus: 502, wantErrorType: apiError},
		{name: "reachability", ready: true, authorize: true, roundTrip: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network-secret")
		}, wantStatus: 502, wantCalls: 2, wantErrorType: apiError},
		{name: "response read", ready: true, authorize: true, roundTrip: freshResponse(200, func() io.ReadCloser {
			return io.NopCloser(catalogErrorReader{err: errors.New("read-secret")})
		}), wantStatus: 502, wantCalls: 2, wantErrorType: apiError},
		{name: "response size boundary", ready: true, authorize: true, responseLimit: 4, roundTrip: freshResponse(200, func() io.ReadCloser {
			return io.NopCloser(strings.NewReader("oversized-secret"))
		}), wantStatus: 502, wantCalls: 2, wantErrorType: apiError},
		{name: "timeout", ready: true, authorize: true, roundTrip: func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		}, wantStatus: 504, wantCalls: 2, wantErrorType: apiError},
		{name: "upstream non-200", ready: true, authorize: true, roundTrip: freshResponse(429, func() io.ReadCloser {
			return io.NopCloser(strings.NewReader(`{"copilot":"body-secret"}`))
		}), wantStatus: 502, wantCalls: 2, wantErrorType: apiError},
		{name: "malformed catalog", ready: true, authorize: true, roundTrip: freshResponse(200, func() io.ReadCloser {
			return io.NopCloser(strings.NewReader(`<body-secret>`))
		}), wantStatus: 502, wantCalls: 2, wantErrorType: apiError},
	}
	surfaces := []struct {
		name    string
		path    string
		surface apierror.Surface
	}{
		{name: "Anthropic", path: "/anthropic/v1/models", surface: apierror.Anthropic},
		{name: "OpenAI", path: "/openai/v1/models", surface: apierror.OpenAI},
	}

	for _, surface := range surfaces {
		for _, scenario := range scenarios {
			t.Run(surface.name+"/"+scenario.name, func(t *testing.T) {
				baseURL := scenario.baseURL
				if baseURL == "" {
					baseURL = "https://upstream.invalid"
				}
				provider := identity.NewStatic(identity.Credential{BaseURL: baseURL, Token: "copilot-token-secret"}, scenario.ready)
				provider.SetError(scenario.credentialErr)
				var calls atomic.Int32
				roundTrip := scenario.roundTrip
				if roundTrip == nil {
					roundTrip = func(*http.Request) (*http.Response, error) {
						return nil, errors.New("unexpected upstream call")
					}
				}
				client := &http.Client{Transport: serverRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return roundTrip(r)
				})}
				responseLimit := scenario.responseLimit
				if responseLimit == 0 {
					responseLimit = 1 << 20
				}
				forwarder := forward.New(provider, client, time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, responseLimit, nil)
				base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, NewStreamOutcomeCounter()))

				do := func(method string) (*http.Response, []byte) {
					t.Helper()
					req, err := http.NewRequest(method, base+surface.path, nil)
					if err != nil {
						t.Fatalf("build %s: %v", method, err)
					}
					if scenario.authorize {
						req.Header.Set("Authorization", "Bearer "+testAPIKey)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatalf("%s: %v", method, err)
					}
					defer func() { _ = resp.Body.Close() }()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatalf("read %s: %v", method, err)
					}
					return resp, body
				}

				getResponse, getBody := do(http.MethodGet)
				headResponse, headBody := do(http.MethodHead)
				if getResponse.StatusCode != scenario.wantStatus || headResponse.StatusCode != scenario.wantStatus {
					t.Errorf("GET/HEAD statuses = %d/%d, want %d", getResponse.StatusCode, headResponse.StatusCode, scenario.wantStatus)
				}
				if got := catalogErrorType(t, surface.surface, getBody); got != scenario.wantErrorType(surface.surface) {
					t.Errorf("error type = %q, want %q", got, scenario.wantErrorType(surface.surface))
				}
				if getResponse.Header.Get("Content-Type") != "application/json" || headResponse.Header.Get("Content-Type") != "application/json" {
					t.Errorf("GET/HEAD content types = %q/%q, want application/json", getResponse.Header.Get("Content-Type"), headResponse.Header.Get("Content-Type"))
				}
				if headResponse.Header.Get("Content-Length") != getResponse.Header.Get("Content-Length") || getResponse.Header.Get("Content-Length") != stringInt(len(getBody)) {
					t.Errorf("GET/HEAD content lengths = %q/%q, want %d", getResponse.Header.Get("Content-Length"), headResponse.Header.Get("Content-Length"), len(getBody))
				}
				if len(headBody) != 0 {
					t.Errorf("HEAD wire body = %q, want empty", headBody)
				}
				if strings.Contains(string(getBody), "secret") {
					t.Errorf("failure leaked upstream detail: %s", getBody)
				}
				if got := calls.Load(); got != scenario.wantCalls {
					t.Errorf("upstream calls = %d, want %d for GET+HEAD", got, scenario.wantCalls)
				}
			})
		}
	}
}

type catalogErrorReader struct{ err error }

func (r catalogErrorReader) Read([]byte) (int, error) { return 0, r.err }

func catalogErrorType(t *testing.T, surface apierror.Surface, body []byte) string {
	t.Helper()
	if surface == apierror.OpenAI {
		return openaiErrorType(t, body)
	}
	return anthropicErrorType(t, body)
}

func stringInt(value int) string {
	// strconv.Itoa in a tiny helper keeps the table assertion readable.
	return strconv.Itoa(value)
}

func TestCatalogClientCancellationPropagatesToCopilotOverRealListener(t *testing.T) {
	for _, path := range []string{"/anthropic/v1/models", "/openai/v1/models"} {
		t.Run(path, func(t *testing.T) {
			reached := make(chan struct{})
			upstreamCancelled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(reached)
				<-r.Context().Done()
				close(upstreamCancelled)
			}))
			defer upstream.Close()

			provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: "copilot-token"}, true)
			forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil)
			base := startServer(t, New(testConfig(), discardLogger(t), provider, forwarder, NewStreamOutcomeCounter()))
			ctx, cancel := context.WithCancel(context.Background())
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			clientDone := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				clientDone <- err
			}()

			select {
			case <-reached:
			case <-time.After(time.Second):
				t.Fatal("Catalog request did not reach stub Copilot")
			}
			cancel()
			select {
			case <-upstreamCancelled:
			case <-time.After(time.Second):
				t.Fatal("stub Copilot did not observe client cancellation")
			}
			select {
			case err := <-clientDone:
				if err == nil {
					t.Error("cancelled Catalog client unexpectedly received a replacement response")
				}
			case <-time.After(time.Second):
				t.Fatal("Catalog client did not return after cancellation")
			}
		})
	}
}

func TestCatalogCorrelationAccessLogsAndSecretRedaction(t *testing.T) {
	const (
		copilotToken      = "copilot-token-secret-52"
		modelData         = "model-data-secret-52"
		vendorData        = "vendor-data-secret-52"
		queryData         = "query-data-secret-52"
		upstreamPrimary   = "upstream-primary-id-52"
		upstreamSecondary = "upstream-secondary-id-52"
		resolvedGET       = "resolved-catalog-get-52"
		resolvedHEAD      = "resolved-catalog-head-52"
	)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		wantRequestID := resolvedGET
		if calls.Load() == 2 {
			wantRequestID = resolvedHEAD
		}
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/models" || r.Header.Get("Authorization") != "Bearer "+copilotToken || r.Header.Get("X-Request-Id") != wantRequestID {
			t.Errorf("upstream request = %s %s auth=%q request-id=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Request-Id"))
		}
		w.Header().Add("X-Request-Id", upstreamPrimary)
		w.Header().Add("X-Request-Id", upstreamSecondary)
		_, _ = io.WriteString(w, `{"data":[{"id":"`+modelData+`","vendor":"`+vendorData+`","model_picker_enabled":true,"supported_endpoints":["/responses"]}]}`)
	}))
	defer upstream.Close()

	var logBuffer bytes.Buffer
	logger, err := logging.NewWithWriter(&logBuffer, config.ServeConfig{LogLevel: "info", LogFormat: "text"})
	if err != nil {
		t.Fatalf("build logger: %v", err)
	}
	provider := identity.NewStatic(identity.Credential{BaseURL: upstream.URL, Token: copilotToken}, true)
	forwarder := forward.New(provider, forward.NewClient(time.Second), time.Second, time.Second, 90*time.Second, 15*time.Second, 1<<20, 1<<20, nil, forward.WithLogger(logger))
	base := startServer(t, New(testConfig(), logger, provider, forwarder, NewStreamOutcomeCounter()))

	do := func(method, requestID string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, base+"/openai/v1/models?limit="+queryData, nil)
		if err != nil {
			t.Fatalf("build %s: %v", method, err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("X-Request-Id", requestID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", method, err)
		}
		return resp, body
	}

	getResponse, getBody := do(http.MethodGet, resolvedGET)
	headResponse, headBody := do(http.MethodHead, resolvedHEAD)
	if getResponse.StatusCode != 200 || headResponse.StatusCode != 200 || len(headBody) != 0 {
		t.Fatalf("GET/HEAD = %d/%d bodies %q/%q", getResponse.StatusCode, headResponse.StatusCode, getBody, headBody)
	}
	if got := getResponse.Header.Values("X-Request-Id"); len(got) != 1 || got[0] != resolvedGET {
		t.Errorf("GET downstream request IDs = %q, want sole resolved ID", got)
	}
	if got := headResponse.Header.Values("X-Request-Id"); len(got) != 1 || got[0] != resolvedHEAD {
		t.Errorf("HEAD downstream request IDs = %q, want sole resolved ID", got)
	}
	if calls.Load() != 2 {
		t.Errorf("GET+HEAD upstream calls = %d, want 2 independent calls", calls.Load())
	}

	logs := logBuffer.String()
	for _, want := range []string{
		`msg="upstream response correlation"`, "upstream_request_id=" + upstreamPrimary,
		`route="GET /openai/v1/models"`, `route="HEAD /openai/v1/models"`,
		"status=200", "bytes=" + strconv.Itoa(len(getBody)), "bytes=0", "duration=", "request_id=" + resolvedGET, "request_id=" + resolvedHEAD,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if count := strings.Count(logs, `msg="upstream response correlation"`); count != 2 {
		t.Errorf("correlation log count = %d, want 2:\n%s", count, logs)
	}
	for _, forbidden := range []string{testAPIKey, copilotToken, modelData, vendorData, queryData, upstreamSecondary} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("logs leaked %q:\n%s", forbidden, logs)
		}
	}
}
