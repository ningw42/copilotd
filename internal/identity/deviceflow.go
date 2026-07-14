package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Device-flow endpoints and defaults for `copilotd login` (§6.6). login obtains
// ONLY the GitHub OAuth token; it performs no exchange (no copilot_internal/v2/
// token) and needs no impersonation headers.
const (
	// defaultGitHubWebBaseURL hosts the device-code and access-token endpoints
	// (github.com), distinct from the API host (api.github.com) used for /user.
	defaultGitHubWebBaseURL = "https://github.com"

	deviceCodePath  = "/login/device/code"
	accessTokenPath = "/login/oauth/access_token"
	userPath        = "/user"

	// grantTypeDeviceCode is the OAuth 2.0 device-flow grant type sent when
	// polling for the access token.
	grantTypeDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"

	// loginUserAgent is sent on the GitHub API /user request; GitHub rejects API
	// requests without a User-Agent.
	loginUserAgent = "copilotd"

	// defaultDeviceFlowClientID / defaultDeviceFlowScope are safety-net defaults
	// applied when DeviceFlowConfig leaves them empty; production always passes
	// the resolved config values.
	defaultDeviceFlowClientID = "Iv1.b507a08c87ecfe98"
	defaultDeviceFlowScope    = "read:user"

	// minPollInterval floors the poll cadence when GitHub omits interval;
	// slowDownIncrement is the minimum back-off added on a slow_down response.
	minPollInterval   = 5 * time.Second
	slowDownIncrement = 5 * time.Second
)

// DeviceFlowConfig carries the injected dependencies for the login device flow.
// Every external edge — the two GitHub base URLs, the HTTP client, and the
// inter-poll sleep — is injectable so the whole flow runs against httptest stubs
// with a fast, deterministic clock.
type DeviceFlowConfig struct {
	// GitHubBaseURL hosts device-code + access-token (default https://github.com).
	GitHubBaseURL string
	// APIBaseURL hosts the /user confirmation call (default https://api.github.com).
	APIBaseURL string
	// HTTPClient performs every request (default http.DefaultClient).
	HTTPClient *http.Client
	// ClientID / Scope parameterize the device-code request.
	ClientID string
	Scope    string
	// TokenFilePath is where the raw OAuth token is written (0600, atomic).
	TokenFilePath string
	// Stdout receives the user-facing prompts and confirmations.
	Stdout io.Writer
	// Logger records each step (default slog.Default()); tokens are never logged.
	Logger *slog.Logger
	// Sleep waits d honoring ctx (default sleepCtx); injected in tests to make
	// polling fast and deterministic and to record the intervals used.
	Sleep func(ctx context.Context, d time.Duration) error
}

// deviceCodeResponse is the JSON returned by the device-code endpoint.
type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// accessTokenResponse is the JSON returned by the access-token endpoint. On a
// successful authorization AccessToken is set; otherwise Error carries one of
// authorization_pending / slow_down / expired_token / access_denied.
type accessTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// githubUser is the slice of GitHub's /user response login needs.
type githubUser struct {
	Login string `json:"login"`
}

// Login runs the GitHub OAuth device flow and writes the resulting raw token to
// the OAuth-token file (§6.6). It obtains ONLY the GitHub OAuth token; it
// performs no exchange. Zero DeviceFlowConfig fields take documented defaults.
//
// Steps: request a device code; print the verification URI + user code; poll for
// the access token at the returned interval (handling authorization_pending,
// slow_down back-off, and the terminal expired_token / access_denied errors);
// confirm the authenticated username via GET <api>/user; then write the token
// (0600, atomic) via WriteTokenFile, noting when an existing file is replaced.
func Login(ctx context.Context, cfg DeviceFlowConfig) error {
	cfg = cfg.withDefaults()

	dc, err := requestDeviceCode(ctx, cfg)
	if err != nil {
		return err
	}
	cfg.Logger.Info("device code requested",
		slog.String("verification_uri", dc.VerificationURI),
		slog.Int("expires_in", dc.ExpiresIn))

	fmt.Fprintf(cfg.Stdout, "To authorize copilotd, visit:\n\n    %s\n\nand enter the code: %s\n\nWaiting for authorization...\n",
		dc.VerificationURI, dc.UserCode)

	token, err := pollForToken(ctx, cfg, dc)
	if err != nil {
		return err
	}

	username, err := fetchUsername(ctx, cfg, token)
	if err != nil {
		return err
	}
	cfg.Logger.Info("device flow authorized", slog.String("login", username))
	fmt.Fprintf(cfg.Stdout, "Authorized as %s.\n", username)

	if _, err := os.Stat(cfg.TokenFilePath); err == nil {
		fmt.Fprintf(cfg.Stdout, "replacing existing token at %s\n", cfg.TokenFilePath)
	}
	if err := WriteTokenFile(cfg.TokenFilePath, token); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	cfg.Logger.Info("wrote github oauth token", slog.String("path", cfg.TokenFilePath))
	fmt.Fprintf(cfg.Stdout, "Wrote GitHub OAuth token to %s\n", cfg.TokenFilePath)
	return nil
}

// withDefaults returns a copy of cfg with every zero field replaced by its
// documented default, so Login never dereferences a nil dependency.
func (cfg DeviceFlowConfig) withDefaults() DeviceFlowConfig {
	if cfg.GitHubBaseURL == "" {
		cfg.GitHubBaseURL = defaultGitHubWebBaseURL
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = defaultGitHubBaseURL // https://api.github.com
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.ClientID == "" {
		cfg.ClientID = defaultDeviceFlowClientID
	}
	if cfg.Scope == "" {
		cfg.Scope = defaultDeviceFlowScope
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	return cfg
}

// requestDeviceCode performs step 1: POST device/code and parse the response.
func requestDeviceCode(ctx context.Context, cfg DeviceFlowConfig) (deviceCodeResponse, error) {
	form := url.Values{"client_id": {cfg.ClientID}, "scope": {cfg.Scope}}
	body, status, err := postForm(ctx, cfg.HTTPClient, strings.TrimRight(cfg.GitHubBaseURL, "/")+deviceCodePath, form)
	if err != nil {
		return deviceCodeResponse{}, fmt.Errorf("request device code: %w", err)
	}
	if status != http.StatusOK {
		return deviceCodeResponse{}, fmt.Errorf("request device code: status %d: %s", status, strings.TrimSpace(string(body)))
	}
	var dc deviceCodeResponse
	if err := json.Unmarshal(body, &dc); err != nil {
		return deviceCodeResponse{}, fmt.Errorf("decode device code response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return deviceCodeResponse{}, fmt.Errorf("device code response missing required fields")
	}
	return dc, nil
}

// pollForToken performs steps 2-3: poll the access-token endpoint at the
// device-code interval, honoring authorization_pending / slow_down and
// terminating cleanly (with a non-nil error) on expired_token / access_denied.
// The caller's ctx is respected via the injected Sleep.
func pollForToken(ctx context.Context, cfg DeviceFlowConfig, dc deviceCodeResponse) (string, error) {
	interval := time.Duration(dc.Interval) * time.Second
	if interval < minPollInterval {
		interval = minPollInterval
	}
	form := url.Values{
		"client_id":   {cfg.ClientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {grantTypeDeviceCode},
	}
	for {
		if err := cfg.Sleep(ctx, interval); err != nil {
			return "", err // ctx cancelled while waiting
		}
		body, status, err := postForm(ctx, cfg.HTTPClient, strings.TrimRight(cfg.GitHubBaseURL, "/")+accessTokenPath, form)
		if err != nil {
			return "", fmt.Errorf("poll for access token: %w", err)
		}
		var at accessTokenResponse
		if err := json.Unmarshal(body, &at); err != nil {
			return "", fmt.Errorf("decode access token response (status %d): %w", status, err)
		}
		if at.AccessToken != "" {
			return at.AccessToken, nil
		}
		switch at.Error {
		case "authorization_pending":
			// The user has not yet authorized; keep polling at the interval.
			cfg.Logger.Debug("authorization pending", slog.Duration("interval", interval))
		case "slow_down":
			// Back off: add the increment, and honor a larger interval if GitHub
			// returned one.
			interval += slowDownIncrement
			if ni := time.Duration(at.Interval) * time.Second; ni > interval {
				interval = ni
			}
			cfg.Logger.Debug("slow down; backing off", slog.Duration("interval", interval))
		case "expired_token":
			return "", fmt.Errorf("device code expired before authorization; re-run `copilotd login`")
		case "access_denied":
			return "", fmt.Errorf("authorization was denied; re-run `copilotd login` to try again")
		default:
			return "", fmt.Errorf("device authorization failed: %s", describeTokenError(at))
		}
	}
}

// fetchUsername performs step 4: GET <api>/user with the new token and returns
// the authenticated login (used only for confirmation; never persisted).
func fetchUsername(ctx context.Context, cfg DeviceFlowConfig, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.APIBaseURL, "/")+userPath, nil)
	if err != nil {
		return "", fmt.Errorf("build /user request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", loginUserAgent)

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch authenticated user: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch authenticated user: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var u githubUser
	if err := json.Unmarshal(body, &u); err != nil {
		return "", fmt.Errorf("decode /user response: %w", err)
	}
	if u.Login == "" {
		return "", fmt.Errorf("/user response missing login")
	}
	return u.Login, nil
}

// postForm POSTs form-encoded values with Accept: application/json and returns
// the (bounded) response body and status code.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// describeTokenError renders an unexpected access-token error for a message.
func describeTokenError(at accessTokenResponse) string {
	if at.ErrorDescription != "" {
		return fmt.Sprintf("%s (%s)", at.Error, at.ErrorDescription)
	}
	if at.Error != "" {
		return at.Error
	}
	return "no access token and no error in response"
}
