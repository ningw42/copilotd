package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Manager must satisfy the same Provider seam the Static stub does.
var _ Provider = (*Manager)(nil)

// --- test helpers -----------------------------------------------------------

// stub is an httptest-backed fake of GitHub's token endpoint. It counts hits,
// captures the last request's headers, and delegates the response to a per-test
// handler (set before use).
type stub struct {
	server  *httptest.Server
	hits    atomic.Int32
	mu      sync.Mutex
	lastHdr http.Header
	handler func(w http.ResponseWriter, r *http.Request)
}

func newStub(t *testing.T) *stub {
	s := &stub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		s.lastHdr = r.Header.Clone()
		s.mu.Unlock()
		s.handler(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHdr
}

// writeToken renders a successful exchange response.
func writeToken(w http.ResponseWriter, token string, expiresAt time.Time, refreshIn time.Duration, apiURL string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"expires_at": expiresAt.Unix(),
		"refresh_in": int64(refreshIn / time.Second),
		"endpoints":  map[string]any{"api": apiURL},
	})
}

// fakeClock is a goroutine-safe, manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testImpersonation() http.Header {
	return http.Header{
		"Copilot-Integration-Id": {"vscode-chat"},
		"Editor-Version":         {"vscode/1.104.1"},
		"User-Agent":             {"GitHubCopilotChat/0.26.7"},
		"X-Github-Api-Version":   {"2025-04-01"},
	}
}

// bufLogger returns a debug-level text logger writing to the returned buffer.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	l := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return l, buf
}

// zeroBackoff makes startup-mint retries instant and deterministic.
func zeroBackoff(int) time.Duration { return 0 }

// --- AC #1: exchange builds a credential; request carries auth + headers -----

func TestManagerCurrentMintsCredential(t *testing.T) {
	const (
		oauth    = "gho-oauth-secret"
		copilot  = "copilot-token-abc"
		endpoint = "https://api.individual.githubcopilot.com"
	)
	s := newStub(t)
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		writeToken(w, copilot, time.Now().Add(25*time.Minute), 1500, endpoint)
	}

	t.Run("endpoints.api used when no override", func(t *testing.T) {
		m := NewManager(ManagerConfig{
			OAuthToken:    oauth,
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		cred, err := m.Current(context.Background())
		if err != nil {
			t.Fatalf("Current() error = %v", err)
		}
		if cred.Token != copilot {
			t.Errorf("Token = %q, want %q", cred.Token, copilot)
		}
		if cred.BaseURL != endpoint {
			t.Errorf("BaseURL = %q, want %q", cred.BaseURL, endpoint)
		}
		if cred.Headers.Get("Copilot-Integration-Id") != "vscode-chat" {
			t.Errorf("Headers missing impersonation set: %v", cred.Headers)
		}

		// The exchange request carried the OAuth token and the impersonation set.
		h := s.header()
		if got := h.Get("Authorization"); got != "token "+oauth {
			t.Errorf("exchange Authorization = %q, want %q", got, "token "+oauth)
		}
		if got := h.Get("Copilot-Integration-Id"); got != "vscode-chat" {
			t.Errorf("exchange Copilot-Integration-Id = %q, want vscode-chat", got)
		}
		if got := h.Get("User-Agent"); got != "GitHubCopilotChat/0.26.7" {
			t.Errorf("exchange User-Agent = %q, want the impersonation UA", got)
		}
		if got := h.Get("X-GitHub-Api-Version"); got != "2025-04-01" {
			t.Errorf("exchange X-GitHub-Api-Version = %q, want 2025-04-01", got)
		}
	})

	t.Run("upstream-base override wins over endpoints.api", func(t *testing.T) {
		const override = "https://override.example.invalid"
		m := NewManager(ManagerConfig{
			OAuthToken:    oauth,
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			UpstreamBase:  override,
			Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		cred, err := m.Current(context.Background())
		if err != nil {
			t.Fatalf("Current() error = %v", err)
		}
		if cred.BaseURL != override {
			t.Errorf("BaseURL = %q, want override %q", cred.BaseURL, override)
		}
	})
}

// --- AC #2a: concurrent stale/empty-cache callers collapse to one exchange ---

func TestManagerSingleInFlight(t *testing.T) {
	s := newStub(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // block so concurrent callers pile up on the single key
		writeToken(w, "copilot-token", time.Now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
	}
	m := NewManager(ManagerConfig{
		OAuthToken:    "gho",
		GitHubBaseURL: s.server.URL,
		HTTPClient:    s.server.Client(),
		Impersonation: testImpersonation(),
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	const n = 50
	results := make([]Credential, n)
	errs := make([]error, n)
	var wg sync.WaitGroup

	// The first caller triggers the exchange; wait until it is running (blocked).
	wg.Add(1)
	go func() { defer wg.Done(); results[0], errs[0] = m.Current(context.Background()) }()
	<-entered

	// All remaining callers now find the key in flight and must join it.
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = m.Current(context.Background()) }(i)
	}
	// Let the joiners reach DoChan while the exchange is still blocked, then release.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := s.hits.Load(); got != 1 {
		t.Fatalf("exchange hits = %d, want exactly 1 (single-in-flight collapse)", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error = %v", i, errs[i])
		}
		if results[i].Token != "copilot-token" {
			t.Errorf("caller %d Token = %q, want shared minted token", i, results[i].Token)
		}
	}
}

// --- AC #2b: cancelling one waiter does not cancel the shared exchange -------

func TestManagerCancelOneWaiterDoesNotCancelExchange(t *testing.T) {
	s := newStub(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		writeToken(w, "copilot-token", time.Now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
	}
	m := NewManager(ManagerConfig{
		OAuthToken:    "gho",
		GitHubBaseURL: s.server.URL,
		HTTPClient:    s.server.Client(),
		Impersonation: testImpersonation(),
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	ctxA, cancelA := context.WithCancel(context.Background())
	var crA, crB Credential
	var erA, erB error
	var wgA, wgB sync.WaitGroup

	// Caller A triggers the exchange.
	wgA.Add(1)
	go func() { defer wgA.Done(); crA, erA = m.Current(ctxA) }()
	<-entered

	// Caller B joins the in-flight exchange with a live context.
	wgB.Add(1)
	go func() { defer wgB.Done(); crB, erB = m.Current(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let B reach its select on the shared channel

	// A abandons its wait; this must not cancel the shared exchange.
	cancelA()
	wgA.Wait()
	if !errors.Is(erA, context.Canceled) {
		t.Fatalf("caller A error = %v, want context.Canceled", erA)
	}
	_ = crA

	// The exchange completes; B must still receive the credential.
	close(release)
	wgB.Wait()
	if erB != nil {
		t.Fatalf("caller B error = %v, want nil", erB)
	}
	if crB.Token != "copilot-token" {
		t.Errorf("caller B Token = %q, want the minted token", crB.Token)
	}
	if got := s.hits.Load(); got != 1 {
		t.Errorf("exchange hits = %d, want exactly 1", got)
	}
}

// --- AC #3: cache reuse until stale; on-demand mint is a single attempt ------

func TestManagerCacheReuseAndSingleAttempt(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &fakeClock{t: base}
	s := newStub(t)
	var failMode atomic.Bool
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		if failMode.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeToken(w, "copilot-token", clk.now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
	}
	m := NewManager(ManagerConfig{
		OAuthToken:    "gho",
		GitHubBaseURL: s.server.URL,
		HTTPClient:    s.server.Client(),
		Impersonation: testImpersonation(),
		SafetyMargin:  2 * time.Minute,
		Clock:         clk.now,
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	ctx := context.Background()

	// First call mints.
	if _, err := m.Current(ctx); err != nil {
		t.Fatalf("mint #1 error = %v", err)
	}
	if s.hits.Load() != 1 {
		t.Fatalf("hits = %d after first mint, want 1", s.hits.Load())
	}

	// Well within freshness: served from cache, no new exchange.
	clk.advance(10 * time.Minute)
	if _, err := m.Current(ctx); err != nil {
		t.Fatalf("cached call error = %v", err)
	}
	if s.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (cache reuse, no re-mint)", s.hits.Load())
	}

	// Advance to within the safety margin of expiry (expiry at +25m, margin 2m):
	// +24m is stale, so the next call re-mints.
	clk.advance(14 * time.Minute) // now base+24m
	if _, err := m.Current(ctx); err != nil {
		t.Fatalf("re-mint error = %v", err)
	}
	if s.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (re-mint within safety margin)", s.hits.Load())
	}

	// On-demand mint is a SINGLE attempt: a transient failure does not retry
	// in-path. Force staleness and a 500; one Current -> exactly one new hit.
	failMode.Store(true)
	clk.advance(30 * time.Minute) // past the new token's expiry -> stale
	before := s.hits.Load()
	if _, err := m.Current(ctx); err == nil {
		t.Fatalf("expected error from failing on-demand mint, got nil")
	}
	if delta := s.hits.Load() - before; delta != 1 {
		t.Fatalf("transient on-demand mint made %d exchanges, want exactly 1 (no in-path retry)", delta)
	}

	// The NEXT request re-attempts (still one more exchange).
	before = s.hits.Load()
	if _, err := m.Current(ctx); err == nil {
		t.Fatalf("expected error from second failing mint, got nil")
	}
	if delta := s.hits.Load() - before; delta != 1 {
		t.Fatalf("next on-demand mint made %d exchanges, want exactly 1", delta)
	}
}

// --- AC #4a: startup mint retries transient, short-circuits auth-class -------

func TestStartupMintRetryAndShortCircuit(t *testing.T) {
	t.Run("retries transient failures up to the bound", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable) // 503: transient
		}
		m := NewManager(ManagerConfig{
			OAuthToken:         "gho",
			GitHubBaseURL:      s.server.URL,
			HTTPClient:         s.server.Client(),
			Impersonation:      testImpersonation(),
			StartupMintRetries: 3,
			Backoff:            zeroBackoff,
			Logger:             slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		m.StartupMint(context.Background())
		if got := s.hits.Load(); got != 4 {
			t.Errorf("exchange hits = %d, want 4 (1 + 3 retries)", got)
		}
		if m.Ready() {
			t.Errorf("Ready() = true after exhausted startup mint, want false")
		}
	})

	t.Run("auth-class short-circuits immediately", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized) // 401: auth-class, permanent
		}
		m := NewManager(ManagerConfig{
			OAuthToken:         "gho",
			GitHubBaseURL:      s.server.URL,
			HTTPClient:         s.server.Client(),
			Impersonation:      testImpersonation(),
			StartupMintRetries: 3,
			Backoff:            zeroBackoff,
			Logger:             slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		m.StartupMint(context.Background())
		if got := s.hits.Load(); got != 1 {
			t.Errorf("exchange hits = %d, want 1 (auth-class short-circuit)", got)
		}
		if m.Ready() {
			t.Errorf("Ready() = true after auth-class failure, want false")
		}
	})

	t.Run("success on first attempt", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			writeToken(w, "copilot-token", time.Now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
		}
		m := NewManager(ManagerConfig{
			OAuthToken:         "gho",
			GitHubBaseURL:      s.server.URL,
			HTTPClient:         s.server.Client(),
			Impersonation:      testImpersonation(),
			StartupMintRetries: 3,
			Backoff:            zeroBackoff,
			Logger:             slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		m.StartupMint(context.Background())
		if got := s.hits.Load(); got != 1 {
			t.Errorf("exchange hits = %d, want 1", got)
		}
		if !m.Ready() {
			t.Errorf("Ready() = false after successful startup mint, want true")
		}
	})
}

// --- AC #4b: Ready() tracks the last mint OUTCOME, not token expiry ----------

func TestReadyTracksLastMintOutcome(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clk := &fakeClock{t: base}
	s := newStub(t)
	var failMode atomic.Bool
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		if failMode.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeToken(w, "copilot-token", clk.now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
	}
	m := NewManager(ManagerConfig{
		OAuthToken:    "gho",
		GitHubBaseURL: s.server.URL,
		HTTPClient:    s.server.Client(),
		Impersonation: testImpersonation(),
		SafetyMargin:  2 * time.Minute,
		Clock:         clk.now,
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	ctx := context.Background()

	if m.Ready() {
		t.Fatalf("Ready() = true before any mint, want false")
	}

	if _, err := m.Current(ctx); err != nil {
		t.Fatalf("first mint error = %v", err)
	}
	if !m.Ready() {
		t.Fatalf("Ready() = false after first successful mint, want true")
	}

	// Idle past expiry WITHOUT a request: readiness must NOT flap to false.
	clk.advance(60 * time.Minute)
	if !m.Ready() {
		t.Fatalf("Ready() = false across idle token expiry, want it to stay true")
	}

	// A failing on-demand mint flips readiness to false.
	failMode.Store(true)
	if _, err := m.Current(ctx); err == nil {
		t.Fatalf("expected failing mint error, got nil")
	}
	if m.Ready() {
		t.Fatalf("Ready() = true after a failed mint, want false")
	}

	// The next successful mint flips it back to true.
	failMode.Store(false)
	if _, err := m.Current(ctx); err != nil {
		t.Fatalf("recovery mint error = %v", err)
	}
	if !m.Ready() {
		t.Fatalf("Ready() = false after recovery, want true")
	}
}

// --- AC #5: classified logging; tokens never logged -------------------------

func TestExchangeClassificationAndSecretRedaction(t *testing.T) {
	const (
		oauth   = "gho-super-secret-oauth"
		copilot = "copilot-super-secret-token"
	)

	t.Run("success does not log either token", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			writeToken(w, copilot, time.Now().Add(25*time.Minute), 1500, "https://api.githubcopilot.com")
		}
		logger, buf := bufLogger()
		m := NewManager(ManagerConfig{
			OAuthToken:    oauth,
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			Logger:        logger,
		})
		if _, err := m.Current(context.Background()); err != nil {
			t.Fatalf("Current() error = %v", err)
		}
		out := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte("minted copilot token")) {
			t.Errorf("expected a mint-success log line, got: %s", out)
		}
		assertNoSecret(t, out, oauth, copilot)
	})

	t.Run("transient failure logged distinctly", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway) // 502: transient
		}
		logger, buf := bufLogger()
		m := NewManager(ManagerConfig{
			OAuthToken:    oauth,
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			Logger:        logger,
		})
		if _, err := m.Current(context.Background()); err == nil {
			t.Fatalf("expected error, got nil")
		}
		out := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte("transient")) {
			t.Errorf("expected a transient-failure log line, got: %s", out)
		}
		assertNoSecret(t, out, oauth, copilot)
	})

	t.Run("auth-class failure logged distinctly", func(t *testing.T) {
		s := newStub(t)
		s.handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden) // 403: auth-class
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		}
		logger, buf := bufLogger()
		m := NewManager(ManagerConfig{
			OAuthToken:    oauth,
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			Logger:        logger,
		})
		if _, err := m.Current(context.Background()); err == nil {
			t.Fatalf("expected error, got nil")
		}
		out := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte("check the Copilot subscription")) {
			t.Errorf("expected the distinct auth-class log line, got: %s", out)
		}
		assertNoSecret(t, out, oauth, copilot)
	})
}

func assertNoSecret(t *testing.T, out, oauth, copilot string) {
	t.Helper()
	if bytes.Contains([]byte(out), []byte(oauth)) {
		t.Errorf("log output leaked the OAuth token\nfull: %s", out)
	}
	if bytes.Contains([]byte(out), []byte(copilot)) {
		t.Errorf("log output leaked the Copilot token\nfull: %s", out)
	}
}

// --- classification unit coverage -------------------------------------------

func TestExchangeErrorClassification(t *testing.T) {
	tests := []struct {
		status        int
		wantTransient bool
		wantAuthClass bool
	}{
		{http.StatusUnauthorized, false, true},
		{http.StatusForbidden, false, true},
		{http.StatusNotFound, false, true},
		{http.StatusTooManyRequests, true, false},
		{http.StatusInternalServerError, true, false},
		{http.StatusBadGateway, true, false},
		{http.StatusBadRequest, false, false},
	}
	for _, tc := range tests {
		s := newStub(t)
		status := tc.status
		s.handler = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }
		m := NewManager(ManagerConfig{
			OAuthToken:    "gho",
			GitHubBaseURL: s.server.URL,
			HTTPClient:    s.server.Client(),
			Impersonation: testImpersonation(),
			Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		})
		_, err := m.Current(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		var ee *exchangeError
		if !errors.As(err, &ee) {
			t.Fatalf("status %d: error is not *exchangeError: %v", tc.status, err)
		}
		if ee.transient != tc.wantTransient {
			t.Errorf("status %d: transient = %v, want %v", tc.status, ee.transient, tc.wantTransient)
		}
		if ee.authClass != tc.wantAuthClass {
			t.Errorf("status %d: authClass = %v, want %v", tc.status, ee.authClass, tc.wantAuthClass)
		}
		if isTransient(err) != tc.wantTransient {
			t.Errorf("status %d: isTransient = %v, want %v", tc.status, isTransient(err), tc.wantTransient)
		}
	}
}
