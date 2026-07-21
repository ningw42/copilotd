package impersonation

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

const primeTimeout = 5 * time.Second

// Config supplies the embedded fallbacks and static identifiers used by a Set.
// The version values are bare; Set derives every version-bearing header through
// the same path whether discovery has succeeded or not.
type Config struct {
	VSCodeVersionFallback string
	PluginVersionFallback string
	CopilotIntegrationID  string
	GithubAPIVersion      string
}

// Observed is a non-secret snapshot of the effective impersonation set and its
// two discovery facts. It deliberately omits attempt errors and other runtime
// details that are not safe for an unauthenticated readiness response.
type Observed struct {
	EffectiveHeaders http.Header
	Discovery        ObservedDiscovery
}

// ObservedDiscovery groups the two independently discovered version facts.
type ObservedDiscovery struct {
	VSCode      ObservedFact
	CopilotChat ObservedFact
}

// ObservedFact reports whether a version has ever been discovered and when the
// most recent successful discovery occurred. LastSuccess is nil while the
// embedded fallback is in use.
type ObservedFact struct {
	Source      string
	LastSuccess *time.Time
}

// Set assembles live impersonation headers from two independently discovered
// version facts and two static identifiers.
type Set struct {
	vscode        *versionFact
	plugin        *versionFact
	integrationID string
	apiVersion    string

	primeTimeout time.Duration
	newTicker    func(time.Duration) setTicker
}

// New constructs a live impersonation set backed by the supplied public
// discovery edge. Discovery is inert until Prime or Run is called.
func New(cfg Config, edge Edge, logger *slog.Logger) *Set {
	return &Set{
		vscode: newVersionFact(
			cfg.VSCodeVersionFallback,
			edge.discoverVSCode,
			withLogger(logger),
		),
		plugin: newVersionFact(
			cfg.PluginVersionFallback,
			edge.discoverCopilotChat,
			withLogger(logger),
		),
		integrationID: cfg.CopilotIntegrationID,
		apiVersion:    cfg.GithubAPIVersion,
		primeTimeout:  primeTimeout,
		newTicker:     newRealSetTicker,
	}
}

// Header returns a fresh map containing the currently effective impersonation
// headers. Callers may mutate the returned map without changing the Set.
func (s *Set) Header() http.Header {
	vscode, _ := s.vscode.current()
	plugin, _ := s.plugin.current()
	return s.header(vscode, plugin)
}

func (s *Set) header(vscode, plugin string) http.Header {
	header := make(http.Header, 5)
	header.Set("Editor-Version", "vscode/"+vscode)
	header.Set("Editor-Plugin-Version", "copilot-chat/"+plugin)
	header.Set("User-Agent", "GitHubCopilotChat/"+plugin)
	header.Set("Copilot-Integration-Id", s.integrationID)
	header.Set("X-GitHub-Api-Version", s.apiVersion)
	return header
}

// Observe returns a fresh, non-secret snapshot suitable for readiness
// observation. Raw discovery failures and attempt details remain package-local.
func (s *Set) Observe() Observed {
	vscodeVersion, vscode := s.vscode.current()
	pluginVersion, plugin := s.plugin.current()
	return Observed{
		EffectiveHeaders: s.header(vscodeVersion, pluginVersion),
		Discovery: ObservedDiscovery{
			VSCode:      observeFact(vscode),
			CopilotChat: observeFact(plugin),
		},
	}
}

// Prime concurrently attempts both discoveries and waits for them to settle,
// bounded by five seconds overall. Failure leaves each fact on its fallback (or
// on its last-good value after an earlier success).
func (s *Set) Prime(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.primeTimeout)
	defer cancel()
	s.discoverBoth(ctx)
}

// Run attempts discovery for both facts on one shared interval. It deliberately
// waits for the first tick because Prime owns the startup attempt.
func (s *Set) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	ticker := s.newTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			s.discoverBoth(ctx)
		}
	}
}

func (s *Set) discoverBoth(ctx context.Context) {
	done := make(chan struct{}, 2)
	attempt := func(fact *versionFact) {
		_ = fact.attemptDiscovery(ctx)
		done <- struct{}{}
	}
	go attempt(s.vscode)
	go attempt(s.plugin)

	for range 2 {
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
	}
}

func observeFact(state snapshot) ObservedFact {
	observed := ObservedFact{Source: string(state.source)}
	if !state.lastSuccess.IsZero() {
		lastSuccess := state.lastSuccess
		observed.LastSuccess = &lastSuccess
	}
	return observed
}

type setTicker interface {
	C() <-chan time.Time
	Stop()
}

type realSetTicker struct {
	*time.Ticker
}

func newRealSetTicker(interval time.Duration) setTicker {
	return realSetTicker{Ticker: time.NewTicker(interval)}
}

func (t realSetTicker) C() <-chan time.Time { return t.Ticker.C }
