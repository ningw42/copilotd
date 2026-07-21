package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// defaultGitHubBaseURL is the host the token exchange targets unless overridden
// (injectable so tests can point it at an httptest server).
const defaultGitHubBaseURL = "https://api.github.com"

// exchangePath is the GitHub endpoint that trades a GitHub OAuth token for a
// short-lived Copilot token.
const exchangePath = "/copilot_internal/v2/token"

// defaultUpstreamBase is the Copilot inference origin used only when the
// exchange response omits endpoints.api.
const defaultUpstreamBase = "https://api.githubcopilot.com"

// mintKey is the single singleflight key shared by the startup mint and every
// on-demand mint, so at most one exchange is ever in flight globally.
const mintKey = "mint"

// Default timings, applied by NewManager when a ManagerConfig field is zero.
const (
	defaultExchangeTimeout = 30 * time.Second
	defaultSafetyMargin    = 2 * time.Minute
	maxExchangeBodyBytes   = 1 << 20 // 1 MiB: bounds the exchange response read.
)

// copilotToken is the internal, parsed result of one exchange. raw is opaque —
// it is passed upstream verbatim and never parsed for auth logic.
type copilotToken struct {
	raw       string
	expiresAt time.Time
	refreshIn time.Duration
	baseURL   string // from endpoints.api; may be empty
}

// fresh reports whether the token is still usable at now, i.e. now is before a
// safety margin ahead of hard expiry. A token within the margin is stale and
// must be re-minted so upstream never receives a token that could die mid-call.
func (t copilotToken) fresh(now time.Time, margin time.Duration) bool {
	return now.Before(t.expiresAt.Add(-margin))
}

// exchangeResponse mirrors the JSON body the token endpoint returns.
type exchangeResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	RefreshIn int64  `json:"refresh_in"`
	Endpoints struct {
		API string `json:"api"`
	} `json:"endpoints"`
}

// exchangeError classifies an exchange failure. transient failures (429, 5xx,
// network, timeout) are worth retrying; non-transient (auth-class: 401/403/404,
// or any other permanent condition) are not — they short-circuit startup-mint
// retries and signal the operator to check the Copilot subscription.
type exchangeError struct {
	transient bool
	authClass bool  // true for 401/403/404 — used only to shape the log message
	status    int   // HTTP status, or 0 for a transport-level failure
	err       error // underlying transport/decode error, if any
}

func (e *exchangeError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("copilot token exchange: status %d", e.status)
	}
	return "copilot token exchange failed"
}

func (e *exchangeError) Unwrap() error { return e.err }

// isTransient reports whether an error returned by mint is worth retrying. Only
// a classified transient exchangeError is; anything else (auth-class, decode
// failure, or an unknown error) is treated as permanent.
func isTransient(err error) bool {
	var ee *exchangeError
	if errors.As(err, &ee) {
		return ee.transient
	}
	return false
}

// ManagerConfig carries the injected dependencies for a Manager. Every external
// edge — GitHub host, HTTP client, clock, backoff — is injectable so the whole
// minting engine is deterministically testable.
type ManagerConfig struct {
	// OAuthToken is the long-lived GitHub OAuth token, the sole input to the
	// exchange. Opaque and secret; never logged.
	OAuthToken string
	// GitHubBaseURL is the scheme+host the exchange targets (default
	// https://api.github.com); injected in tests to point at an httptest server.
	GitHubBaseURL string
	// HTTPClient performs the exchange (default http.DefaultClient). NewManager
	// shallow-copies it and disables redirects so one exchange is one wire request.
	HTTPClient *http.Client
	// Impersonation supplies the current header set applied to the exchange
	// request and carried on Credential.Headers for inference requests.
	Impersonation Impersonation
	// StartupMintRetries bounds transient-failure retries of the startup mint
	// (total attempts = 1 + N). Auth-class failures short-circuit regardless.
	StartupMintRetries int
	// ExchangeTimeout bounds a single exchange on its own background context
	// (default 30s), independent of any request/outbound timeout.
	ExchangeTimeout time.Duration
	// SafetyMargin re-mints a token this long before hard expiry (default 2m).
	SafetyMargin time.Duration
	// Clock returns the current time (default time.Now); injected for staleness.
	Clock func() time.Time
	// Backoff returns the delay before startup-mint retry attempt n (n >= 1);
	// default is capped exponential. Injected as a constant zero in tests to keep
	// startup-retry tests fast and deterministic.
	Backoff func(attempt int) time.Duration
	// Logger records mint triggers, outcomes, and startup retry lifecycle
	// (default slog.Default()). Secrets are never passed to it.
	Logger *slog.Logger
}

// Manager mints and caches the short-lived Copilot token, implementing the
// Provider interface. It mints on demand (inside Current when the cache is
// missing or stale) plus once at startup (StartupMint), with at most one
// exchange in flight globally. There is no scheduled refresh (ADR-0001).
type Manager struct {
	oauthToken      string
	githubBaseURL   string
	httpClient      *http.Client
	impersonation   Impersonation
	startupRetries  int
	exchangeTimeout time.Duration
	safetyMargin    time.Duration
	clock           func() time.Time
	backoff         func(attempt int) time.Duration
	logger          *slog.Logger

	group singleflight.Group

	mu        sync.Mutex
	cached    copilotToken
	hasCached bool

	// localReady records whether construction received the locally resolved
	// GitHub OAuth token. Network exchange outcomes never change it: they are
	// request-scoped and remain observable through mint logs.
	localReady bool
}

// Verify Manager is a drop-in for the Provider seam the forwarder/server use.
var _ Provider = (*Manager)(nil)

// NewManager builds a Manager from cfg, applying defaults for any zero field.
func NewManager(cfg ManagerConfig) *Manager {
	m := &Manager{
		oauthToken:      cfg.OAuthToken,
		githubBaseURL:   cfg.GitHubBaseURL,
		httpClient:      cfg.HTTPClient,
		impersonation:   cfg.Impersonation,
		startupRetries:  cfg.StartupMintRetries,
		exchangeTimeout: cfg.ExchangeTimeout,
		safetyMargin:    cfg.SafetyMargin,
		clock:           cfg.Clock,
		backoff:         cfg.Backoff,
		logger:          cfg.Logger,
		localReady:      strings.TrimSpace(cfg.OAuthToken) != "",
	}
	if m.githubBaseURL == "" {
		m.githubBaseURL = defaultGitHubBaseURL
	}
	if m.httpClient == nil {
		m.httpClient = http.DefaultClient
	}
	exchangeClient := *m.httpClient
	exchangeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	m.httpClient = &exchangeClient
	if m.impersonation == nil {
		m.impersonation = StaticImpersonation(nil)
	}
	if m.exchangeTimeout <= 0 {
		m.exchangeTimeout = defaultExchangeTimeout
	}
	if m.safetyMargin <= 0 {
		m.safetyMargin = defaultSafetyMargin
	}
	if m.clock == nil {
		m.clock = time.Now
	}
	if m.backoff == nil {
		m.backoff = defaultBackoff
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	if m.startupRetries < 0 {
		m.startupRetries = 0
	}
	return m
}

// Current returns the credential for an outbound request: the cached Copilot
// token when it is still fresh, otherwise a freshly minted one. It first reads
// the cache under the mutex (a fresh hit returns without touching singleflight);
// only a missing or stale cache mints, collapsing concurrent callers onto one
// exchange.
func (m *Manager) Current(ctx context.Context) (Credential, error) {
	if tok, ok := m.freshCached(); ok {
		return m.credentialFrom(tok), nil
	}
	return m.mint(ctx, "on-demand")
}

// Ready reports whether the Manager has the local prerequisite needed to
// attempt an exchange: a resolved GitHub OAuth token. Mint success, failure,
// and cached-token expiry do not change readiness or request admission.
func (m *Manager) Ready() bool { return m.localReady }

// StartupMint performs the boot warm-up mint. It runs once, retrying transient
// failures with capped backoff up to StartupMintRetries and short-circuiting on
// an auth-class failure. Its outcome never gates requests: the next request can
// always mint on demand. Intended to be run in a goroutine.
func (m *Manager) StartupMint(ctx context.Context) {
	attempts := m.startupRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, m.backoff(attempt)); err != nil {
				return // shutting down mid-backoff
			}
		}
		_, err := m.mint(ctx, "startup")
		if err == nil {
			return // success; the closure cached and logged the token
		}
		if ctx.Err() != nil {
			return // caller cancelled; the background exchange (if any) runs on
		}
		if !isTransient(err) {
			// Auth-class / permanent: retrying cannot help. The closure already
			// logged the distinct "check the Copilot subscription" error.
			m.logger.Warn("startup mint short-circuited on a permanent failure; on-demand mint remains available")
			return
		}
		m.logger.Debug("startup mint attempt failed (transient), will retry",
			slog.Int("attempt", attempt+1), slog.Int("attempts", attempts))
	}
	m.logger.Warn("startup mint exhausted its retries; a later request can mint on demand",
		slog.Int("attempts", attempts))
}

// mint routes both triggers through the single singleflight key. The exchange
// runs on a background-scoped context bounded by exchangeTimeout — deliberately
// NOT the caller's ctx — so a disconnecting caller cannot cancel the shared
// exchange other waiters depend on. The caller waits via DoChan and selects on
// its own ctx, returning promptly on cancellation without poisoning the mint.
func (m *Manager) mint(ctx context.Context, trigger string) (Credential, error) {
	ch := m.group.DoChan(mintKey, func() (any, error) {
		exCtx, cancel := context.WithTimeout(context.Background(), m.exchangeTimeout)
		defer cancel()

		tok, err := m.exchange(exCtx)
		if err == nil {
			m.store(tok)
		}
		m.logMint(trigger, tok, err)
		return tok, err
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return Credential{}, res.Err
		}
		return m.credentialFrom(res.Val.(copilotToken)), nil
	case <-ctx.Done():
		// The caller left; the shared exchange keeps running for other waiters.
		return Credential{}, ctx.Err()
	}
}

// exchange performs one token exchange and classifies the outcome. It carries
// Authorization: token <oauth> plus the impersonation headers, which the token
// endpoint's client/user-agent allowlist requires even for the exchange itself.
func (m *Manager) exchange(ctx context.Context) (copilotToken, error) {
	exchangeURL := strings.TrimRight(m.githubBaseURL, "/") + exchangePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exchangeURL, &singleAttemptExchangeBody{ReadCloser: http.NoBody})
	if err != nil {
		return copilotToken{}, &exchangeError{transient: false, err: err}
	}
	for k, vs := range m.impersonation.Header() {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Authorization", "token "+m.oauthToken)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		// Network/timeout: transient.
		return copilotToken{}, &exchangeError{transient: true, err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBodyBytes))

	switch {
	case resp.StatusCode == http.StatusOK:
		var parsed exchangeResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			// A malformed 200 body looks like a truncated/flaky read: transient.
			return copilotToken{}, &exchangeError{transient: true, status: resp.StatusCode, err: err}
		}
		if strings.TrimSpace(parsed.Token) == "" {
			return copilotToken{}, &exchangeError{transient: true, status: resp.StatusCode, err: errors.New("empty token in exchange response")}
		}
		baseURL, err := normalizeCopilotOrigin(parsed.Endpoints.API)
		if err != nil {
			return copilotToken{}, &exchangeError{transient: true, status: resp.StatusCode, err: err}
		}
		return copilotToken{
			raw:       parsed.Token,
			expiresAt: time.Unix(parsed.ExpiresAt, 0),
			refreshIn: time.Duration(parsed.RefreshIn) * time.Second,
			baseURL:   baseURL,
		}, nil
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound:
		return copilotToken{}, &exchangeError{transient: false, authClass: true, status: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return copilotToken{}, &exchangeError{transient: true, status: resp.StatusCode}
	default:
		// Any other status (e.g. an unexpected 4xx): retrying will not help.
		return copilotToken{}, &exchangeError{transient: false, status: resp.StatusCode}
	}
}

// singleAttemptExchangeBody differs from http.NoBody only in identity. Its
// non-nil Body and nil GetBody make Go's Transport treat the otherwise bodyless
// GET as non-replayable; Transport's empty-body probe still emits no wire body.
type singleAttemptExchangeBody struct {
	io.ReadCloser
}

// normalizeCopilotOrigin validates an exchange-provided endpoints.api value and
// returns its canonical origin. An empty value is preserved so credentialFrom
// can apply the built-in missing-value fallback. A single trailing slash is
// accepted but removed; paths, queries, fragments, userinfo, and non-HTTP(S)
// schemes are not origins and must never reach a published Credential.
func normalizeCopilotOrigin(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid endpoints.api origin: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("invalid endpoints.api origin: scheme must be http or https")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid endpoints.api origin: host is required")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("invalid endpoints.api origin: userinfo is not allowed")
	}
	if path := parsed.EscapedPath(); path != "" && path != "/" {
		return "", fmt.Errorf("invalid endpoints.api origin: path is not allowed")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", fmt.Errorf("invalid endpoints.api origin: query is not allowed")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("invalid endpoints.api origin: fragment is not allowed")
	}

	return scheme + "://" + parsed.Host, nil
}

// logMint records the single mint outcome, with a distinct auth-class message so
// a permanent credential problem is not mistaken for a flaky exchange. Tokens
// (OAuth and Copilot), raw response bodies, and underlying errors are never
// included.
func (m *Manager) logMint(trigger string, tok copilotToken, err error) {
	if err == nil {
		m.logger.Info("minted copilot token",
			slog.String("trigger", trigger),
			slog.Time("expires_at", tok.expiresAt),
			slog.Duration("refresh_in", tok.refreshIn))
		return
	}
	var ee *exchangeError
	if !errors.As(err, &ee) {
		m.logger.Warn("copilot token exchange failed",
			slog.String("trigger", trigger))
		return
	}
	if ee.authClass {
		m.logger.Error("copilot token exchange failed: not transient — check the Copilot subscription",
			slog.String("trigger", trigger),
			slog.Int("status", ee.status))
		return
	}
	attrs := []any{
		slog.String("trigger", trigger),
		slog.Int("status", ee.status),
	}
	if ee.transient {
		m.logger.Warn("copilot token exchange failed (transient)", attrs...)
		return
	}
	m.logger.Error("copilot token exchange failed (permanent)", attrs...)
}

// freshCached returns the cached token if present and still fresh at clock().
func (m *Manager) freshCached() (copilotToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasCached && m.cached.fresh(m.clock(), m.safetyMargin) {
		return m.cached, true
	}
	return copilotToken{}, false
}

// store caches a freshly minted token under the mutex.
func (m *Manager) store(tok copilotToken) {
	m.mu.Lock()
	m.cached = tok
	m.hasCached = true
	m.mu.Unlock()
}

// credentialFrom builds the immutable Credential snapshot. The validated
// exchange origin wins whenever present; the built-in origin is only its
// missing-value fallback. Headers is the shared impersonation set (the forwarder
// copies it onto a fresh map, so sharing is race-free).
func (m *Manager) credentialFrom(tok copilotToken) Credential {
	base := tok.baseURL
	if base == "" {
		base = defaultUpstreamBase
	}
	return Credential{
		BaseURL: base,
		Token:   tok.raw,
		Headers: m.impersonation.Header(),
	}
}

// defaultBackoff is a capped exponential schedule for startup-mint retries:
// 500ms, 1s, 2s, … capped at 8s. attempt is the 1-based retry index.
func defaultBackoff(attempt int) time.Duration {
	const base = 500 * time.Millisecond
	const cap = 8 * time.Second
	if attempt < 1 {
		return 0
	}
	d := base << (attempt - 1)
	if d <= 0 || d > cap { // <=0 guards the shift overflowing at large attempts
		return cap
	}
	return d
}

// sleepCtx sleeps for d, returning early with ctx.Err() if ctx is cancelled. A
// non-positive d is an immediate return that still honors an already-cancelled
// ctx, so an injected zero backoff stays deterministic without spinning.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
