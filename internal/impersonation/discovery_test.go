package impersonation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiscoverVSCode(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/api/releases/stable" {
			t.Errorf("path = %q, want /api/releases/stable", r.URL.Path)
		}
		assertNoCopilotHeaders(t, r)
		_, _ = io.WriteString(w, `["1.129.2","1.129.1"]`)
	}))
	defer server.Close()

	edge := Edge{VSCodeBaseURL: server.URL + "/", Client: server.Client()}
	got, err := edge.discoverVSCode(context.Background())
	if err != nil {
		t.Fatalf("discoverVSCode() error = %v", err)
	}
	if got != "1.129.2" {
		t.Fatalf("discoverVSCode() = %q, want %q", got, "1.129.2")
	}
}

func TestDiscoverCopilotChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/_apis/public/gallery/extensionquery" {
			t.Errorf("path = %q, want /_apis/public/gallery/extensionquery", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != marketplaceAccept {
			t.Errorf("Accept = %q, want %q", got, marketplaceAccept)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		assertNoCopilotHeaders(t, r)

		var query struct {
			Filters []struct {
				Criteria []struct {
					FilterType int    `json:"filterType"`
					Value      string `json:"value"`
				} `json:"criteria"`
			} `json:"filters"`
			Flags int `json:"flags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if len(query.Filters) != 1 || len(query.Filters[0].Criteria) != 1 {
			t.Errorf("filters = %#v, want one criterion", query.Filters)
			return
		}
		criterion := query.Filters[0].Criteria[0]
		if criterion.FilterType != 7 || criterion.Value != "GitHub.copilot-chat" {
			t.Errorf("criterion = %#v, want filterType 7 for GitHub.copilot-chat", criterion)
		}
		if query.Flags != 0x11 {
			t.Errorf("flags = %#x, want %#x", query.Flags, 0x11)
		}

		_, _ = io.WriteString(w, `{
			"results":[{"extensions":[{"versions":[
				{"version":"0.50.0","properties":[{"key":"Microsoft.VisualStudio.Code.PreRelease","value":"true"}]},
				{"version":"0.49.2","properties":[{"key":"Microsoft.VisualStudio.Code.PreRelease","value":"false"}]},
				{"version":"0.49.1","properties":[]}
			]}]}]
		}`)
	}))
	defer server.Close()

	edge := Edge{MarketplaceBaseURL: server.URL + "/", Client: server.Client()}
	got, err := edge.discoverCopilotChat(context.Background())
	if err != nil {
		t.Fatalf("discoverCopilotChat() error = %v", err)
	}
	if got != "0.49.2" {
		t.Fatalf("discoverCopilotChat() = %q, want newest stable %q", got, "0.49.2")
	}
}

func TestDiscoveryRejectsInvalidVersionShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		call func(Edge) (string, error)
	}{
		{
			name: "VS Code",
			body: `["release-1.129.2"]`,
			call: func(edge Edge) (string, error) {
				return edge.discoverVSCode(context.Background())
			},
		},
		{
			name: "Copilot Chat",
			body: `{"results":[{"extensions":[{"versions":[{"version":"0.49","properties":[]}]}]}]}`,
			call: func(edge Edge) (string, error) {
				return edge.discoverCopilotChat(context.Background())
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			edge := Edge{
				VSCodeBaseURL:      server.URL,
				MarketplaceBaseURL: server.URL,
				Client:             server.Client(),
			}
			value, err := test.call(edge)
			if err == nil {
				t.Fatalf("discovery returned value %q, want shape error", value)
			}
			if value != "" {
				t.Errorf("value = %q on error, want empty", value)
			}
		})
	}
}

func TestDiscoveryVersionValidation(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"1.130.0",
		"1.130.0-insider",
		"0.50.0+build.1",
		"1.2.3-rc.1+build.5",
	} {
		version := version
		t.Run("accepts "+version, func(t *testing.T) {
			t.Parallel()
			if err := validateVersion(version); err != nil {
				t.Fatalf("validateVersion(%q) error = %v", version, err)
			}
		})
	}

	for _, version := range []string{
		"",
		"banana",
		"vscode/1.2.3",
		"1.2.3/garbage",
		"1.2.3 beta",
		"1.2.3\nInjected: true",
		"1.2.3-",
		"1.2.3+",
		"1.2.3-rc..1",
		"1.2.3+build/1",
	} {
		version := version
		t.Run("rejects "+version, func(t *testing.T) {
			t.Parallel()
			if err := validateVersion(version); err == nil {
				t.Fatalf("validateVersion(%q) error = nil, want invalid version", version)
			}
		})
	}
}

func TestDiscoveryRejectsMalformedBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		call func(Edge) (string, error)
	}{
		{
			name: "VS Code invalid JSON",
			body: `["1.129.2"`,
			call: func(edge Edge) (string, error) {
				return edge.discoverVSCode(context.Background())
			},
		},
		{
			name: "VS Code empty release list",
			body: `[]`,
			call: func(edge Edge) (string, error) {
				return edge.discoverVSCode(context.Background())
			},
		},
		{
			name: "Marketplace invalid JSON",
			body: `{"results":`,
			call: func(edge Edge) (string, error) {
				return edge.discoverCopilotChat(context.Background())
			},
		},
		{
			name: "Marketplace missing versions",
			body: `{"results":[{"extensions":[]}]}`,
			call: func(edge Edge) (string, error) {
				return edge.discoverCopilotChat(context.Background())
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()

			edge := Edge{
				VSCodeBaseURL:      server.URL,
				MarketplaceBaseURL: server.URL,
				Client:             server.Client(),
			}
			value, err := test.call(edge)
			if err == nil {
				t.Fatalf("discovery returned value %q, want malformed-body error", value)
			}
			if value != "" {
				t.Errorf("value = %q on error, want empty", value)
			}
		})
	}
}

func TestDiscoveryCallsAreBoundedToFiveSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(Edge) (string, error)
	}{
		{
			name: "VS Code",
			call: func(edge Edge) (string, error) {
				return edge.discoverVSCode(context.Background())
			},
		},
		{
			name: "Copilot Chat",
			call: func(edge Edge) (string, error) {
				return edge.discoverCopilotChat(context.Background())
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &deadlineTransport{}
			edge := Edge{
				VSCodeBaseURL:      "https://vscode.invalid",
				MarketplaceBaseURL: "https://marketplace.invalid",
				Client:             &http.Client{Transport: transport},
			}

			value, err := test.call(edge)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want context deadline exceeded", err)
			}
			if value != "" {
				t.Errorf("value = %q on timeout, want empty", value)
			}
			remaining := transport.remaining()
			if remaining <= 4*time.Second || remaining > 5*time.Second {
				t.Errorf("request deadline remaining = %v, want in (4s, 5s]", remaining)
			}
		})
	}
}

func TestDiscoveryUsesInjectedClientWithoutCopilotHeaders(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assertNoCopilotHeaders(t, r)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`["1.129.2"]`)),
			Request:    r,
		}, nil
	})
	edge := Edge{
		VSCodeBaseURL: "https://vscode.invalid",
		Client:        &http.Client{Transport: transport},
	}

	if _, err := edge.discoverVSCode(context.Background()); err != nil {
		t.Fatalf("discoverVSCode() error = %v", err)
	}
}

func assertNoCopilotHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	for _, header := range []string{
		"Authorization",
		"Copilot-Integration-Id",
		"Editor-Version",
		"Editor-Plugin-Version",
		"X-GitHub-Api-Version",
	} {
		if got := r.Header.Get(header); got != "" {
			t.Errorf("unexpected %s header %q", header, got)
		}
	}
	if got := r.Header.Get("User-Agent"); strings.HasPrefix(got, "GitHubCopilotChat/") {
		t.Errorf("unexpected impersonation User-Agent %q", got)
	}
}

type deadlineTransport struct {
	mu       sync.Mutex
	duration time.Duration
}

func (t *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	if !ok {
		return nil, errors.New("request has no deadline")
	}
	t.mu.Lock()
	t.duration = time.Until(deadline)
	t.mu.Unlock()
	return nil, context.DeadlineExceeded
}

func (t *deadlineTransport) remaining() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.duration
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
