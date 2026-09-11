package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
	"github.com/ningw42/copilotd/internal/usage/pricing/modelsdevdata"
)

const (
	// ModelsDevURL is the public pricing artifact fetched by the remote adapter.
	ModelsDevURL = "https://models.dev/api.json"

	pricingCacheName     = "usage_prices"
	fetchTimeout         = 5 * time.Second
	decodedResponseLimit = 8 << 20
)

var embeddedArtifact = modelsdevdata.Artifact()

var embeddedParsedSnapshot = sync.OnceValue(func() *Snapshot {
	snapshot, err := ParseSnapshot(context.Background(), embeddedArtifact)
	if err != nil {
		panic(fmt.Sprintf("pricing: invalid embedded snapshot: %v", err))
	}
	return snapshot
})

// CacheConfig supplies the memory-only pricing refresh cadence. A nonpositive
// interval pins the embedded floor.
type CacheConfig struct {
	RefreshInterval time.Duration
}

// SnapshotStatus identifies the captured effective bytes. LastSuccess is the
// last successful content fetch, not a tariff effective date.
type SnapshotStatus struct {
	Source      string
	Version     string
	LastSuccess *time.Time
}

// ProjectionLimit is the caller's maximum provider/model identity bytes for
// one snapshot projection. On a content-version miss it is enforced during the
// initial strict JSON member walk, before the complete selected-key map can be
// retained; a parsed-cache hit checks the retained total before returning it.
type ProjectionLimit struct {
	MaxIdentityBytes int
}

// Source captures one local immutable pricing snapshot. Current performs no
// network work. A valid accepted cached value can fail one report's smaller
// projection limit without invalidating or replacing that value.
type Source interface {
	Current(context.Context, ProjectionLimit) (*Snapshot, SnapshotStatus, error)
}

// Remote is the credential-free HTTP adapter used only by cache refreshes.
type Remote struct {
	url    string
	client *http.Client
}

// NewRemote constructs a dedicated redirect-refusing client. The optional
// transport exists for deterministic local HTTP fixtures; nil uses Go's default
// credential-free transport.
func NewRemote(url string, transport http.RoundTripper) Remote {
	return Remote{
		url: url,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// CachedSource exposes raw bytes held by the shared cache registry as parsed
// immutable snapshots. It retains one parsed projection per effective content
// version so reports do not repeatedly parse the multi-megabyte raw artifact.
type CachedSource struct {
	value *cache.Value[[]byte]

	parsedGate chan struct{}
	parsed     *parsedSnapshot
}

type parsedSnapshot struct {
	version  string
	snapshot *Snapshot
}

// NewCachedSource constructs and registers the pricing cached value.
func NewCachedSource(cfg CacheConfig, remote Remote, registry *cache.Registry, logger *slog.Logger) *CachedSource {
	fallbackVersion := contentVersion(embeddedArtifact)
	value := cache.New(logger, cache.Cacheable[[]byte]{
		Fallback:        embeddedArtifact,
		FallbackVersion: fallbackVersion,
		TTL:             cfg.RefreshInterval,
		Fetch:           remote.fetch,
		Hash:            contentVersion,
		Validate: func(raw []byte) error {
			_, err := ParseSnapshot(context.Background(), raw)
			return err
		},
		Name: pricingCacheName,
	})
	registry.Register(value)
	return &CachedSource{
		value:      value,
		parsedGate: make(chan struct{}, 1),
		parsed: &parsedSnapshot{
			version:  fallbackVersion,
			snapshot: embeddedParsedSnapshot(),
		},
	}
}

// Current captures the cache's effective bytes/status atomically and projects
// only that captured value. Projection limits do not alter cache acceptance.
// A successful projection is immutable and reused while the content version is
// unchanged; each caller still applies its own retained-identity limit.
func (s *CachedSource) Current(ctx context.Context, limit ProjectionLimit) (*Snapshot, SnapshotStatus, error) {
	raw, status := s.value.Current()
	if err := ctx.Err(); err != nil {
		return nil, SnapshotStatus{}, err
	}

	select {
	case s.parsedGate <- struct{}{}:
		defer func() { <-s.parsedGate }()
	case <-ctx.Done():
		return nil, SnapshotStatus{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, SnapshotStatus{}, err
	}
	if s.parsed != nil && s.parsed.version == status.Version {
		if limit.MaxIdentityBytes < 0 || s.parsed.snapshot.IdentityBytes() > limit.MaxIdentityBytes {
			return nil, SnapshotStatus{}, ErrProjectionLimit
		}
		return s.parsed.snapshot, snapshotStatus(status), nil
	}

	snapshot, err := parseSnapshot(ctx, raw, limit.MaxIdentityBytes)
	if err != nil {
		return nil, SnapshotStatus{}, err
	}
	// A refresh can publish newer raw bytes while projection is running. Return
	// this call's atomically captured version, but never replace the one-entry
	// parsed cache with a snapshot that is no longer current.
	_, currentStatus := s.value.Current()
	if currentStatus.Version == status.Version {
		s.parsed = &parsedSnapshot{version: status.Version, snapshot: snapshot}
	}
	return snapshot, snapshotStatus(status), nil
}

func snapshotStatus(status cache.Status) SnapshotStatus {
	return SnapshotStatus{
		Source:      status.Source,
		Version:     status.Version,
		LastSuccess: status.LastSuccess,
	}
}

func (r Remote) fetch(ctx context.Context) ([]byte, string, error) {
	if r.client == nil {
		return nil, "", errors.New("pricing remote has no HTTP client")
	}
	requestCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build models.dev request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch models.dev pricing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("fetch models.dev pricing: unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, decodedResponseLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("read models.dev pricing: %w", err)
	}
	if len(body) > decodedResponseLimit {
		return nil, "", fmt.Errorf("models.dev pricing exceeds %d-byte decoded limit", decodedResponseLimit)
	}
	return body, contentVersion(body), nil
}

func contentVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
