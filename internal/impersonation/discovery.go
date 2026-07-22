package impersonation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	discoveryTimeout = 5 * time.Second

	vscodeStableReleasesPath            = "/api/releases/stable"
	marketplaceQueryPath                = "/_apis/public/gallery/extensionquery"
	marketplaceAccept                   = "application/json;api-version=7.2-preview.1"
	marketplaceExtensionID              = "GitHub.copilot-chat"
	marketplaceFilterType               = 7
	marketplaceIncludeVersions          = 0x1
	marketplaceIncludeVersionProperties = 0x10
	marketplaceQueryFlags               = marketplaceIncludeVersions | marketplaceIncludeVersionProperties
	marketplacePrereleaseKey            = "Microsoft.VisualStudio.Code.PreRelease"
)

// Edge is the public, unauthenticated HTTP edge used for version discovery.
// Client is used directly; callers should supply a plain client with no Copilot
// credentials or impersonation headers configured on its transport.
type Edge struct {
	VSCodeBaseURL      string
	MarketplaceBaseURL string
	Client             *http.Client
}

func (e Edge) discoverVSCode(ctx context.Context) (string, error) {
	if e.Client == nil {
		return "", errors.New("discover VS Code version: nil HTTP client")
	}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(e.VSCodeBaseURL, "/")+vscodeStableReleasesPath,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("discover VS Code version: build request: %w", err)
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discover VS Code version: request: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSuccess(resp.StatusCode); err != nil {
		return "", fmt.Errorf("discover VS Code version: %w", err)
	}

	var releases []string
	if err := decodeJSON(resp.Body, &releases); err != nil {
		return "", fmt.Errorf("discover VS Code version: decode response: %w", err)
	}
	if len(releases) == 0 {
		return "", errors.New("discover VS Code version: response contains no stable releases")
	}
	return releases[0], nil
}

func (e Edge) discoverCopilotChat(ctx context.Context) (string, error) {
	if e.Client == nil {
		return "", errors.New("discover Copilot Chat version: nil HTTP client")
	}

	body, err := json.Marshal(marketplaceQuery{
		Filters: []marketplaceFilter{{
			Criteria: []marketplaceCriterion{{
				FilterType: marketplaceFilterType,
				Value:      marketplaceExtensionID,
			}},
			PageNumber: 1,
			PageSize:   1,
		}},
		AssetTypes: []string{},
		Flags:      marketplaceQueryFlags,
	})
	if err != nil {
		return "", fmt.Errorf("discover Copilot Chat version: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(e.MarketplaceBaseURL, "/")+marketplaceQueryPath,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("discover Copilot Chat version: build request: %w", err)
	}
	req.Header.Set("Accept", marketplaceAccept)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discover Copilot Chat version: request: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSuccess(resp.StatusCode); err != nil {
		return "", fmt.Errorf("discover Copilot Chat version: %w", err)
	}

	var result marketplaceResponse
	if err := decodeJSON(resp.Body, &result); err != nil {
		return "", fmt.Errorf("discover Copilot Chat version: decode response: %w", err)
	}
	if len(result.Results) == 0 ||
		len(result.Results[0].Extensions) == 0 ||
		len(result.Results[0].Extensions[0].Versions) == 0 {
		return "", errors.New("discover Copilot Chat version: response contains no extension versions")
	}

	for _, candidate := range result.Results[0].Extensions[0].Versions {
		if candidate.isPrerelease() {
			continue
		}
		return candidate.Version, nil
	}
	return "", errors.New("discover Copilot Chat version: response contains no stable extension versions")
}

type marketplaceQuery struct {
	Filters    []marketplaceFilter `json:"filters"`
	AssetTypes []string            `json:"assetTypes"`
	Flags      int                 `json:"flags"`
}

type marketplaceFilter struct {
	Criteria   []marketplaceCriterion `json:"criteria"`
	PageNumber int                    `json:"pageNumber"`
	PageSize   int                    `json:"pageSize"`
}

type marketplaceCriterion struct {
	FilterType int    `json:"filterType"`
	Value      string `json:"value"`
}

type marketplaceResponse struct {
	Results []struct {
		Extensions []struct {
			Versions []marketplaceVersion `json:"versions"`
		} `json:"extensions"`
	} `json:"results"`
}

type marketplaceVersion struct {
	Version    string                `json:"version"`
	Properties []marketplaceProperty `json:"properties"`
}

type marketplaceProperty struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (v marketplaceVersion) isPrerelease() bool {
	for _, property := range v.Properties {
		if property.Key == marketplacePrereleaseKey && property.Value == "true" {
			return true
		}
	}
	return false
}

func requireSuccess(statusCode int) error {
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected HTTP status %d", statusCode)
	}
	return nil
}

func decodeJSON(body io.Reader, dst any) error {
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values in response")
		}
		return fmt.Errorf("trailing response data: %w", err)
	}
	return nil
}
