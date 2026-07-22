package impersonation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
)

const (
	cacheNameVSCode      = "vscode"
	cacheNameCopilotChat = "copilot_chat"
)

// Config supplies the embedded fallbacks, refresh cadence, and static
// identifiers used by a Set. The version values are bare; Set derives every
// version-bearing header through the same path whether discovery has succeeded
// or not.
type Config struct {
	VSCodeVersionFallback string
	PluginVersionFallback string
	CopilotIntegrationID  string
	GithubAPIVersion      string
	RefreshInterval       time.Duration
}

// Set assembles live impersonation headers from two independently cached
// version facts and two static identifiers.
type Set struct {
	vscode        *cache.Value[string]
	plugin        *cache.Value[string]
	integrationID string
	apiVersion    string
}

// New constructs a live impersonation set backed by the supplied public
// discovery edge and registers both cached values with registry. Discovery is
// inert until the registry is primed or started.
func New(cfg Config, edge Edge, registry *cache.Registry, logger *slog.Logger) *Set {
	newVersion := func(name, fallback string, discover func(context.Context) (string, error)) *cache.Value[string] {
		value := cache.New(cache.Cacheable[string]{
			Fallback:        fallback,
			FallbackVersion: fallback,
			TTL:             cfg.RefreshInterval,
			Fetch: func(ctx context.Context) (string, string, error) {
				version, err := discover(ctx)
				return version, version, err
			},
			Hash:     hashVersion,
			Validate: validateVersion,
			Name:     name,
		}, cache.WithLogger(logger))
		registry.Register(value)
		return value
	}

	return &Set{
		vscode:        newVersion(cacheNameVSCode, cfg.VSCodeVersionFallback, edge.discoverVSCode),
		plugin:        newVersion(cacheNameCopilotChat, cfg.PluginVersionFallback, edge.discoverCopilotChat),
		integrationID: cfg.CopilotIntegrationID,
		apiVersion:    cfg.GithubAPIVersion,
	}
}

// Header returns a fresh map containing the currently effective impersonation
// headers. Callers may mutate the returned map without changing the Set.
func (s *Set) Header() http.Header {
	vscode, _ := s.vscode.Current()
	plugin, _ := s.plugin.Current()
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

func hashVersion(version string) string {
	sum := sha256.Sum256([]byte(version))
	return hex.EncodeToString(sum[:])
}

func validateVersion(version string) error {
	if !IsBareVersion(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	return nil
}
