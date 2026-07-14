package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// quietLogger returns a logger writing to io.Discard so device-flow tests stay
// silent while still exercising the logging calls.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingSleep is an injected Sleep that records the intervals it is asked to
// wait and returns immediately (honoring ctx), so polling tests are fast and
// deterministic.
func recordingSleep(slept *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return ctx.Err()
	}
}

// webStub scripts the device-code + access-token endpoints. accessResponses is
// consumed one entry per poll; the last entry repeats if polling outlasts it.
type webStub struct {
	t               *testing.T
	deviceCode      deviceCodeResponse
	accessResponses []map[string]any

	// captured request fields
	pollCount     int
	gotClientID   string
	gotScope      string
	gotDeviceCode string
	gotGrant      string
	gotAccept     string
}

func newWebStub(t *testing.T, dc deviceCodeResponse, access []map[string]any) (*httptest.Server, *webStub) {
	s := &webStub{t: t, deviceCode: dc, accessResponses: access}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case deviceCodePath:
			s.gotClientID = r.PostFormValue("client_id")
			s.gotScope = r.PostFormValue("scope")
			s.gotAccept = r.Header.Get("Accept")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      s.deviceCode.DeviceCode,
				"user_code":        s.deviceCode.UserCode,
				"verification_uri": s.deviceCode.VerificationURI,
				"expires_in":       s.deviceCode.ExpiresIn,
				"interval":         s.deviceCode.Interval,
			})
		case accessTokenPath:
			s.gotDeviceCode = r.PostFormValue("device_code")
			s.gotGrant = r.PostFormValue("grant_type")
			idx := s.pollCount
			if idx >= len(s.accessResponses) {
				idx = len(s.accessResponses) - 1
			}
			s.pollCount++
			_ = json.NewEncoder(w).Encode(s.accessResponses[idx])
		default:
			t.Errorf("unexpected web path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

// user-agent, and accept headers login sends.
func newUserStub(t *testing.T, login string, gotAuth, gotUA, gotAccept *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != userPath {
			t.Errorf("unexpected api path %q", r.URL.Path)
		}
		*gotAuth = r.Header.Get("Authorization")
		*gotUA = r.Header.Get("User-Agent")
		*gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"login": login})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginDeviceFlowSuccess(t *testing.T) {
	const (
		deviceCode = "dev-code-123"
		userCode   = "WDJB-MJHT"
		verifyURI  = "https://github.com/login/device"
		accessTok  = "gho_the_raw_access_token"
		username   = "octocat"
	)
	web, ws := newWebStub(t,
		deviceCodeResponse{DeviceCode: deviceCode, UserCode: userCode, VerificationURI: verifyURI, ExpiresIn: 900, Interval: 5},
		[]map[string]any{
			{"error": "authorization_pending"},
			{"error": "authorization_pending"},
			{"access_token": accessTok, "token_type": "bearer", "scope": "read:user"},
		},
	)

	var userAuth, userUA, userAccept string
	api := newUserStub(t, username, &userAuth, &userUA, &userAccept)

	path := filepath.Join(t.TempDir(), "nested", "github-oauth-token")
	var out bytes.Buffer
	var slept []time.Duration

	err := Login(context.Background(), DeviceFlowConfig{
		GitHubBaseURL: web.URL,
		APIBaseURL:    api.URL,
		HTTPClient:    web.Client(),
		ClientID:      "Iv1.test-client",
		Scope:         "read:user",
		TokenFilePath: path,
		Stdout:        &out,
		Logger:        quietLogger(),
		Sleep:         recordingSleep(&slept),
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	// The raw token is written verbatim, 0600, with parent dirs created.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != accessTok {
		t.Errorf("token file = %q, want the raw access token %q", data, accessTok)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %#o, want 0600", perm)
		}
	}

	// The verification URI, user code, username, and final path are printed.
	o := out.String()
	for _, want := range []string{verifyURI, userCode, username, path} {
		if !strings.Contains(o, want) {
			t.Errorf("stdout missing %q:\n%s", want, o)
		}
	}

	// Request shapes: device-code carried client_id/scope + Accept: json.
	if ws.gotClientID != "Iv1.test-client" {
		t.Errorf("device-code client_id = %q, want Iv1.test-client", ws.gotClientID)
	}
	if ws.gotScope != "read:user" {
		t.Errorf("device-code scope = %q, want read:user", ws.gotScope)
	}
	if ws.gotAccept != "application/json" {
		t.Errorf("device-code Accept = %q, want application/json", ws.gotAccept)
	}
	// Access-token poll carried device_code + the device-code grant type.
	if ws.gotDeviceCode != deviceCode {
		t.Errorf("poll device_code = %q, want %q", ws.gotDeviceCode, deviceCode)
	}
	if ws.gotGrant != grantTypeDeviceCode {
		t.Errorf("poll grant_type = %q, want %q", ws.gotGrant, grantTypeDeviceCode)
	}
	// /user used the new token, a User-Agent, and the GitHub media type.
	if userAuth != "token "+accessTok {
		t.Errorf("/user Authorization = %q, want %q", userAuth, "token "+accessTok)
	}
	if userUA == "" {
		t.Errorf("/user request missing a User-Agent (GitHub requires one)")
	}
	if userAccept != "application/vnd.github+json" {
		t.Errorf("/user Accept = %q, want application/vnd.github+json", userAccept)
	}

	// Polled three times, sleeping the 5s interval before each poll.
	if ws.pollCount != 3 {
		t.Errorf("polls = %d, want 3", ws.pollCount)
	}
	if len(slept) != 3 {
		t.Fatalf("sleeps = %v, want 3", slept)
	}
	for i, d := range slept {
		if d != 5*time.Second {
			t.Errorf("sleep[%d] = %v, want 5s", i, d)
		}
	}
}

func TestLoginDeviceFlowSlowDownBacksOff(t *testing.T) {
	const accessTok = "gho_after_backoff"
	web, _ := newWebStub(t,
		deviceCodeResponse{DeviceCode: "dc", UserCode: "AAAA-BBBB", VerificationURI: "https://gh/dev", ExpiresIn: 900, Interval: 5},
		[]map[string]any{
			{"error": "slow_down"}, // no interval field: default +5s back-off
			{"access_token": accessTok},
		},
	)
	var ua, au, ac string
	api := newUserStub(t, "octocat", &au, &ua, &ac)

	var slept []time.Duration
	err := Login(context.Background(), DeviceFlowConfig{
		GitHubBaseURL: web.URL,
		APIBaseURL:    api.URL,
		HTTPClient:    web.Client(),
		TokenFilePath: filepath.Join(t.TempDir(), "tok"),
		Stdout:        io.Discard,
		Logger:        quietLogger(),
		Sleep:         recordingSleep(&slept),
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if len(slept) != 2 {
		t.Fatalf("sleeps = %v, want 2 (one before each poll)", slept)
	}
	// The second interval must have increased after slow_down (>= +5s).
	if slept[1] <= slept[0] {
		t.Errorf("interval did not back off: %v then %v", slept[0], slept[1])
	}
	if slept[1] != slept[0]+slowDownIncrement {
		t.Errorf("interval[1] = %v, want %v (initial + %v)", slept[1], slept[0]+slowDownIncrement, slowDownIncrement)
	}
}

func TestLoginDeviceFlowHonorsReturnedSlowDownInterval(t *testing.T) {
	web, _ := newWebStub(t,
		deviceCodeResponse{DeviceCode: "dc", UserCode: "AAAA-BBBB", VerificationURI: "https://gh/dev", ExpiresIn: 900, Interval: 5},
		[]map[string]any{
			{"error": "slow_down", "interval": 20}, // GitHub returns a new, larger interval
			{"access_token": "gho_x"},
		},
	)
	var ua, au, ac string
	api := newUserStub(t, "octocat", &au, &ua, &ac)

	var slept []time.Duration
	if err := Login(context.Background(), DeviceFlowConfig{
		GitHubBaseURL: web.URL, APIBaseURL: api.URL, HTTPClient: web.Client(),
		TokenFilePath: filepath.Join(t.TempDir(), "tok"), Stdout: io.Discard,
		Logger: quietLogger(), Sleep: recordingSleep(&slept),
	}); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if len(slept) != 2 || slept[1] != 20*time.Second {
		t.Errorf("sleeps = %v, want the returned 20s interval honored", slept)
	}
}

func TestLoginDeviceFlowTerminalErrors(t *testing.T) {
	tests := []struct {
		name       string
		pollError  string
		wantSubstr string
	}{
		{"expired token", "expired_token", "expired"},
		{"access denied", "access_denied", "denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			web, _ := newWebStub(t,
				deviceCodeResponse{DeviceCode: "dc", UserCode: "AAAA-BBBB", VerificationURI: "https://gh/dev", ExpiresIn: 900, Interval: 5},
				[]map[string]any{{"error": tc.pollError}},
			)
			// The API /user must never be reached on a terminal error.
			apiHit := false
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiHit = true
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(api.Close)

			path := filepath.Join(t.TempDir(), "tok")
			var slept []time.Duration
			err := Login(context.Background(), DeviceFlowConfig{
				GitHubBaseURL: web.URL, APIBaseURL: api.URL, HTTPClient: web.Client(),
				TokenFilePath: path, Stdout: io.Discard, Logger: quietLogger(),
				Sleep: recordingSleep(&slept),
			})
			if err == nil {
				t.Fatalf("Login() error = nil, want a terminal %s error", tc.pollError)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantSubstr)
			}
			if apiHit {
				t.Errorf("/user was reached despite the terminal %s error", tc.pollError)
			}
			// No token file is written on a terminal error.
			if _, statErr := os.Stat(path); statErr == nil {
				t.Errorf("a token file was written despite the terminal %s error", tc.pollError)
			}
		})
	}
}

func TestLoginDeviceFlowReplacesExistingTokenWithNotice(t *testing.T) {
	const accessTok = "gho_replacement"
	web, _ := newWebStub(t,
		deviceCodeResponse{DeviceCode: "dc", UserCode: "AAAA-BBBB", VerificationURI: "https://gh/dev", ExpiresIn: 900, Interval: 5},
		[]map[string]any{{"access_token": accessTok}},
	)
	var ua, au, ac string
	api := newUserStub(t, "octocat", &au, &ua, &ac)

	path := filepath.Join(t.TempDir(), "github-oauth-token")
	if err := WriteTokenFile(path, "gho_old_value"); err != nil {
		t.Fatalf("seed existing token: %v", err)
	}

	var out bytes.Buffer
	var slept []time.Duration
	if err := Login(context.Background(), DeviceFlowConfig{
		GitHubBaseURL: web.URL, APIBaseURL: api.URL, HTTPClient: web.Client(),
		TokenFilePath: path, Stdout: &out, Logger: quietLogger(), Sleep: recordingSleep(&slept),
	}); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if !strings.Contains(out.String(), "replacing existing token") {
		t.Errorf("stdout missing the overwrite notice:\n%s", out.String())
	}
	got, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if got != accessTok {
		t.Errorf("token = %q, want the replacement %q", got, accessTok)
	}
}

func TestLoginDeviceFlowRespectsContextCancellation(t *testing.T) {
	web, _ := newWebStub(t,
		deviceCodeResponse{DeviceCode: "dc", UserCode: "AAAA-BBBB", VerificationURI: "https://gh/dev", ExpiresIn: 900, Interval: 5},
		[]map[string]any{{"error": "authorization_pending"}},
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(api.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first Sleep must return ctx.Err()

	err := Login(ctx, DeviceFlowConfig{
		GitHubBaseURL: web.URL, APIBaseURL: api.URL, HTTPClient: web.Client(),
		TokenFilePath: filepath.Join(t.TempDir(), "tok"), Stdout: io.Discard,
		Logger: quietLogger(),
		// default Sleep (sleepCtx) honors the cancelled ctx immediately
	})
	if err == nil {
		t.Fatalf("Login() error = nil, want context cancellation")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error = %q, want it to reflect context cancellation", err.Error())
	}
}
