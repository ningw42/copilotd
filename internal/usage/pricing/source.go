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

func init() {
	if _, err := ParseSnapshot(context.Background(), embeddedArtifact); err != nil {
		panic(fmt.Sprintf("decode embedded models.dev pricing floor: %v", err))
	}
}

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

// Source captures one local immutable pricing snapshot. Current performs no
// network work.
type Source interface {
	Current(context.Context) (*Snapshot, SnapshotStatus, error)
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
// immutable snapshots.
type CachedSource struct {
	value *cache.Value[[]byte]
}

// NewCachedSource constructs and registers the pricing cached value.
func NewCachedSource(cfg CacheConfig, remote Remote, registry *cache.Registry, logger *slog.Logger) *CachedSource {
	value := cache.New(logger, cache.Cacheable[[]byte]{
		Fallback:        embeddedArtifact,
		FallbackVersion: contentVersion(embeddedArtifact),
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
	return &CachedSource{value: value}
}

// Current captures the cache's effective bytes/status atomically and projects
// only that captured value.
func (s *CachedSource) Current(ctx context.Context) (*Snapshot, SnapshotStatus, error) {
	raw, status := s.value.Current()
	snapshot, err := ParseSnapshot(ctx, raw)
	if err != nil {
		return nil, SnapshotStatus{}, err
	}
	return snapshot, SnapshotStatus{
		Source:      status.Source,
		Version:     status.Version,
		LastSuccess: status.LastSuccess,
	}, nil
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
