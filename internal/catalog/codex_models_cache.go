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
	modelsRequestTimeout    = 5 * time.Second
	modelsEdgeResponseLimit = 8 << 20
	latestCodexReleasePath  = "/repos/openai/codex/releases/latest"
	codexReleaseCommitPath  = "/repos/openai/codex/commits/"
	codexModelsContentPath  = "/repos/openai/codex/contents/codex-rs/models-manager/models.json"
	githubRawMediaType      = "application/vnd.github.raw+json"
	githubSHA1MediaType     = "application/vnd.github.sha"
	codexModelsCacheName    = "codex_models"
)

var embeddedCodexModelsVersion = embeddedCodexRelease.Release.Tag

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
func NewModelsCache(cfg ModelsCacheConfig, edge ModelsEdge, registry *cache.Registry, cacheLogger *slog.Logger) *cache.Value[[]byte] {
	value := cache.New(cacheLogger, cache.Cacheable[[]byte]{
		Fallback:        embeddedCodexModels,
		FallbackVersion: embeddedCodexModelsVersion,
		TTL:             cfg.RefreshInterval,
		Version:         edge.latestReleaseTag,
		Fetch:           edge.fetchLatestModels,
		Hash:            hashModels,
		Validate: func(currentBytes []byte) error {
			_, err := validateCodexModels(currentBytes)
			return err
		},
		Name: codexModelsCacheName,
	})
	registry.Register(value)
	return value
}

func (e ModelsEdge) latestReleaseTag(ctx context.Context) (string, error) {
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := e.get(ctx, latestCodexReleasePath, "", &release); err != nil {
		return "", fmt.Errorf("discover latest Codex release: %w", err)
	}
	tag := strings.TrimSpace(release.TagName)
	if tag == "" {
		return "", errors.New("discover latest Codex release: response contains no tag_name")
	}
	if release.Draft || release.Prerelease {
		return "", fmt.Errorf("discover latest Codex release: %q is not stable", tag)
	}
	if !isStableCodexReleaseTag(tag) {
		return "", fmt.Errorf("discover latest Codex release: %q is not a stable rust-vX.Y.Z tag", tag)
	}
	return tag, nil
}

func isStableCodexReleaseTag(tag string) bool {
	version, ok := strings.CutPrefix(tag, "rust-v")
	if !ok {
		return false
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

func (e ModelsEdge) fetchLatestModels(ctx context.Context) ([]byte, string, error) {
	tag, err := e.latestReleaseTag(ctx)
	if err != nil {
		return nil, "", err
	}
	commit, err := e.releaseCommit(ctx, tag)
	if err != nil {
		return nil, "", err
	}
	query := url.Values{"ref": []string{commit}}.Encode()
	body, err := e.getBytes(ctx, codexModelsContentPath+"?"+query, githubRawMediaType)
	if err != nil {
		return nil, "", fmt.Errorf("fetch Codex models at %s: %w", tag, err)
	}
	return body, tag, nil
}

func (e ModelsEdge) releaseCommit(ctx context.Context, tag string) (string, error) {
	body, err := e.getBytes(ctx, codexReleaseCommitPath+url.PathEscape(tag), githubSHA1MediaType)
	if err != nil {
		return "", fmt.Errorf("resolve Codex release %s commit: %w", tag, err)
	}
	sha := strings.TrimSpace(string(body))
	if !isGitCommitSHA(sha) {
		return "", fmt.Errorf("resolve Codex release %s commit: invalid SHA %q", tag, sha)
	}
	return sha, nil
}

func isGitCommitSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	for _, digit := range sha {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') && (digit < 'A' || digit > 'F') {
			return false
		}
	}
	return true
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsEdgeResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > modelsEdgeResponseLimit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", modelsEdgeResponseLimit)
	}
	return body, nil
}

func hashModels(currentBytes []byte) string {
	sum := sha256.Sum256(currentBytes)
	return hex.EncodeToString(sum[:])
}
