package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ningw42/copilotd/internal/cache"
)

const (
	embeddedCodexModelsVersion = "rust-v0.144.5"
	modelsRequestTimeout       = 5 * time.Second
	latestCodexReleasePath     = "/repos/openai/codex/releases/latest"
	codexModelsContentPath     = "/repos/openai/codex/contents/codex-rs/models-manager/models.json"
	githubRawMediaType         = "application/vnd.github.raw+json"
	codexModelsCacheName       = "codex_models"
)

// ModelsCacheConfig supplies the refresh cadence for Codex's models.json
// cached value. A non-positive interval pins the embedded floor.
type ModelsCacheConfig struct {
	RefreshInterval time.Duration
}

// ModelsEdge is the credential-free GitHub HTTP edge used to discover and
// fetch Codex release data. Client must be a dedicated plain client whose
// transport adds no GitHub OAuth or Copilot credentials.
type ModelsEdge struct {
	BaseURL string
	Client  *http.Client
}

// NewModelsCache constructs and registers the Codex models.json cached value.
// The embedded floor remains the guaranteed-parseable vendored snapshot.
func NewModelsCache(cfg ModelsCacheConfig, edge ModelsEdge, registry *cache.Registry, logger *slog.Logger) *cache.Value[[]byte] {
	value := cache.New(cache.Cacheable[[]byte]{
		Fallback:        embeddedCodexModels,
		FallbackVersion: embeddedCodexModelsVersion,
		TTL:             cfg.RefreshInterval,
		Version:         edge.latestReleaseTag,
		Fetch:           edge.fetchLatestModels,
		Hash:            hashModels,
		Validate: func(currentBytes []byte) error {
			_, err := decodeCodexModels(currentBytes)
			return err
		},
		Name: codexModelsCacheName,
	}, cache.WithLogger(logger))
	registry.Register(value)
	return value
}

func (e ModelsEdge) latestReleaseTag(ctx context.Context) (string, error) {
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := e.get(ctx, latestCodexReleasePath, "", &release); err != nil {
		return "", fmt.Errorf("discover latest Codex release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return "", errors.New("discover latest Codex release: response contains no tag_name")
	}
	return release.TagName, nil
}

func (e ModelsEdge) fetchLatestModels(ctx context.Context) ([]byte, string, error) {
	tag, err := e.latestReleaseTag(ctx)
	if err != nil {
		return nil, "", err
	}
	query := url.Values{"ref": []string{tag}}.Encode()
	body, err := e.getBytes(ctx, codexModelsContentPath+"?"+query, githubRawMediaType)
	if err != nil {
		return nil, "", fmt.Errorf("fetch Codex models at %s: %w", tag, err)
	}
	return body, tag, nil
}

func (e ModelsEdge) get(ctx context.Context, path, accept string, dst any) error {
	body, err := e.getBytes(ctx, path, accept)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode response: multiple JSON values")
		}
		return fmt.Errorf("decode trailing response data: %w", err)
	}
	return nil
}

func (e ModelsEdge) getBytes(ctx context.Context, path, accept string) ([]byte, error) {
	if e.Client == nil {
		return nil, errors.New("nil HTTP client")
	}
	requestCtx, cancel := context.WithTimeout(ctx, modelsRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(e.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

func hashModels(currentBytes []byte) string {
	sum := sha256.Sum256(currentBytes)
	return hex.EncodeToString(sum[:])
}
