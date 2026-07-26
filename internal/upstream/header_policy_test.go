package upstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/apierror"
	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/identity"
	"github.com/ningw42/copilotd/internal/logging"
)

func TestCallerPrepareAppliesHeaderPolicy(t *testing.T) {
	stripTests := []struct {
		name       string
		header     string
		wantValues []string
	}{
		{name: "connection", header: "Connection"},
		{name: "keep alive", header: "Keep-Alive"},
		{name: "proxy authenticate", header: "Proxy-Authenticate"},
		{name: "proxy authorization", header: "Proxy-Authorization"},
		{name: "TE", header: "TE"},
		{name: "trailer", header: "Trailer"},
		{name: "transfer encoding", header: "Transfer-Encoding"},
		{name: "upgrade", header: "Upgrade"},
		{name: "authorization", header: "Authorization", wantValues: []string{"Bearer copilot-token"}},
		{name: "API key", header: "X-Api-Key"},
		{name: "host", header: "Host"},
		{name: "content length", header: "Content-Length"},
		{name: "WebSocket protocol", header: "Sec-WebSocket-Protocol"},
		{name: "WebSocket extensions", header: "Sec-WebSocket-Extensions"},
		{name: "arbitrary WebSocket header", header: "Sec-WebSocket-Future"},
	}

	for _, tc := range stripTests {
		t.Run("strips client "+tc.name, func(t *testing.T) {
			clientHeader := http.Header{tc.header: {"client value"}}
			request := prepareRequest(t, context.Background(), identity.Credential{
				BaseURL: "https://upstream.example",
				Token:   "copilot-token",
			}, Call{Route: endpoint.RouteModels, Method: http.MethodGet, ClientHeader: clientHeader})

			if got := request.Header.Values(tc.header); !reflect.DeepEqual(got, tc.wantValues) {
				t.Errorf("%s = %q, want %q", tc.header, got, tc.wantValues)
			}
		})
	}

	t.Run("strips names listed by Connection", func(t *testing.T) {
		request := prepareRequest(t, context.Background(), identity.Credential{
			BaseURL: "https://upstream.example",
			Token:   "copilot-token",
		}, Call{
			Route:  endpoint.RouteModels,
			Method: http.MethodGet,
			ClientHeader: http.Header{
				"Connection":     {"X-Remove-One, x-remove-two", "X-Remove-Three"},
				"X-Remove-One":   {"one"},
				"X-Remove-Two":   {"two"},
				"X-Remove-Three": {"three"},
				"X-Keep":         {"kept"},
			},
		})

		for _, name := range []string{"Connection", "X-Remove-One", "X-Remove-Two", "X-Remove-Three"} {
			if got := request.Header.Values(name); len(got) != 0 {
				t.Errorf("%s = %q, want absent", name, got)
			}
		}
		if got := request.Header.Values("X-Keep"); !reflect.DeepEqual(got, []string{"kept"}) {
			t.Errorf("X-Keep = %q, want [kept]", got)
		}
	})

	t.Run("applies overlay order without mutating credential headers", func(t *testing.T) {
		credentialHeaders := http.Header{
			"x-shared":               {"credential"},
			"x-credential-only":      {"one", "two"},
			"authorization":          {"credential authorization"},
			"x-request-id":           {"credential request id"},
			"accept-encoding":        {"gzip"},
			"connection":             {"credential connection"},
			"sec-websocket-protocol": {"credential subprotocol"},
		}
		before := credentialHeaders.Clone()
		ctx := logging.WithRequestID(context.Background(), "resolved request id")
		request := prepareRequest(t, ctx, identity.Credential{
			BaseURL: "https://upstream.example",
			Token:   "copilot-token",
			Headers: credentialHeaders,
		}, Call{
			Route:                  endpoint.RouteModels,
			Method:                 http.MethodGet,
			AcceptIdentityEncoding: true,
			ClientHeader: http.Header{
				"X-Shared":        {"client"},
				"X-Client-Only":   {"client one", "client two"},
				"Authorization":   {"client authorization"},
				"X-Request-Id":    {"client request id"},
				"Accept-Encoding": {"br"},
			},
		})

		want := map[string][]string{
			"X-Shared":               {"credential"},
			"X-Credential-Only":      {"one", "two"},
			"X-Client-Only":          {"client one", "client two"},
			"Authorization":          {"credential authorization"},
			RequestIDHeader:          {"resolved request id"},
			"Accept-Encoding":        {"identity"},
			"Connection":             {"credential connection"},
			"Sec-Websocket-Protocol": {"credential subprotocol"},
		}
		for name, values := range want {
			if got := request.Header.Values(name); !reflect.DeepEqual(got, values) {
				t.Errorf("%s = %q, want %q", name, got, values)
			}
		}
		if !reflect.DeepEqual(credentialHeaders, before) {
			t.Errorf("credential headers mutated:\n got: %#v\nwant: %#v", credentialHeaders, before)
		}
		request.Header["X-Credential-Only"][0] = "request mutation"
		if got := credentialHeaders["x-credential-only"][0]; got != "one" {
			t.Errorf("credential header aliases request header: got %q, want one", got)
		}
	})

	t.Run("sets bearer authorization after stripping client authorization", func(t *testing.T) {
		request := prepareRequest(t, context.Background(), identity.Credential{
			BaseURL: "https://upstream.example",
			Token:   "copilot-token",
		}, Call{
			Route:        endpoint.RouteModels,
			Method:       http.MethodGet,
			ClientHeader: http.Header{"Authorization": {"client secret"}},
		})
		if got := request.Header.Get("Authorization"); got != "Bearer copilot-token" {
			t.Errorf("Authorization = %q, want bearer Copilot token", got)
		}
	})

	t.Run("request id is absent without a resolved value", func(t *testing.T) {
		request := prepareRequest(t, context.Background(), identity.Credential{
			BaseURL: "https://upstream.example",
			Token:   "copilot-token",
		}, Call{Route: endpoint.RouteModels, Method: http.MethodGet})
		if got := request.Header.Values(RequestIDHeader); len(got) != 0 {
			t.Errorf("%s = %q, want absent", RequestIDHeader, got)
		}
	})

	t.Run("identity encoding is opt in", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			enabled bool
			want    string
		}{
			{name: "disabled", enabled: false},
			{name: "enabled", enabled: true, want: "identity"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				request := prepareRequest(t, context.Background(), identity.Credential{
					BaseURL: "https://upstream.example",
					Token:   "copilot-token",
				}, Call{
					Route:                  endpoint.RouteModels,
					Method:                 http.MethodGet,
					AcceptIdentityEncoding: tc.enabled,
				})
				if got := request.Header.Get("Accept-Encoding"); got != tc.want {
					t.Errorf("Accept-Encoding = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("nil client header forwards no client header", func(t *testing.T) {
		request := prepareRequest(t, context.Background(), identity.Credential{
			BaseURL: "https://upstream.example",
			Token:   "copilot-token",
		}, Call{Route: endpoint.RouteModels, Method: http.MethodGet})
		if got := request.Header; !reflect.DeepEqual(got, http.Header{"Authorization": {"Bearer copilot-token"}}) {
			t.Errorf("Header = %#v, want only authorization", got)
		}
	})
}

func TestCallerPreparePreservesBodyReplayAndLengthSemantics(t *testing.T) {
	tests := []struct {
		name              string
		body              io.Reader
		contentLength     int64
		wantContentLength int64
		wantGetBody       bool
		wantWrappedEmpty  bool
	}{
		{name: "nil body", wantWrappedEmpty: true},
		{name: "http NoBody", body: http.NoBody, wantWrappedEmpty: true},
		{
			name:              "sized bytes reader retains derived replay",
			body:              bytes.NewReader([]byte("sized")),
			wantContentLength: 5,
			wantGetBody:       true,
		},
		{
			name:              "unknown length round trips",
			body:              strings.NewReader("stream"),
			contentLength:     -1,
			wantContentLength: -1,
			wantGetBody:       true,
		},
		{
			name:              "explicit nonzero length is assigned",
			body:              strings.NewReader("stream"),
			contentLength:     3,
			wantContentLength: 3,
			wantGetBody:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := prepareRequest(t, context.Background(), identity.Credential{
				BaseURL: "https://upstream.example",
				Token:   "copilot-token",
			}, Call{
				Route:         endpoint.RouteModels,
				Method:        http.MethodGet,
				Body:          tc.body,
				ContentLength: tc.contentLength,
			})

			if request.ContentLength != tc.wantContentLength {
				t.Errorf("ContentLength = %d, want %d", request.ContentLength, tc.wantContentLength)
			}
			if got := request.GetBody != nil; got != tc.wantGetBody {
				t.Errorf("GetBody present = %v, want %v", got, tc.wantGetBody)
			}
			if tc.wantWrappedEmpty {
				if request.Body == nil || request.Body == http.NoBody {
					t.Errorf("Body = %#v, want distinct empty single-attempt wrapper", request.Body)
				}
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatalf("read wrapped body: %v", err)
				}
				if len(body) != 0 {
					t.Errorf("wrapped body = %q, want empty", body)
				}
			}
		})
	}
}

func TestCallerPreparePreservesURLFidelity(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		query      string
		forceQuery bool
		wantURL    string
	}{
		{
			name:    "trims trailing base URL slashes",
			baseURL: "https://upstream.example///",
			wantURL: "https://upstream.example/models",
		},
		{
			name:    "forwards raw query verbatim",
			baseURL: "https://upstream.example",
			query:   "z=%2f&a=one+two&a=%41",
			wantURL: "https://upstream.example/models?z=%2f&a=one+two&a=%41",
		},
		{
			name:       "preserves bare question mark",
			baseURL:    "https://upstream.example/",
			forceQuery: true,
			wantURL:    "https://upstream.example/models?",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := prepareRequest(t, context.Background(), identity.Credential{
				BaseURL: tc.baseURL,
				Token:   "copilot-token",
			}, Call{
				Route:      endpoint.RouteModels,
				Method:     http.MethodGet,
				Query:      tc.query,
				ForceQuery: tc.forceQuery,
			})

			if got := request.URL.String(); got != tc.wantURL {
				t.Errorf("URL = %q, want %q", got, tc.wantURL)
			}
			if request.URL.RawQuery != tc.query {
				t.Errorf("RawQuery = %q, want verbatim %q", request.URL.RawQuery, tc.query)
			}
			if request.URL.ForceQuery != tc.forceQuery {
				t.Errorf("ForceQuery = %v, want %v", request.URL.ForceQuery, tc.forceQuery)
			}
		})
	}
}

func TestCallerPrepareClassifiesCredentialAndBuildFailures(t *testing.T) {
	credentialCause := errors.New("mint failed with secret detail")
	tests := []struct {
		name        string
		provider    identity.Provider
		call        Call
		wantKind    apierror.Kind
		wantMessage string
		wantErr     error
	}{
		{
			name: "credential failure",
			provider: func() identity.Provider {
				provider := identity.NewStatic(identity.Credential{}, true)
				provider.SetError(credentialCause)
				return provider
			}(),
			call:        Call{Route: endpoint.RouteModels, Method: http.MethodGet},
			wantKind:    apierror.NotReady,
			wantMessage: "no upstream credential available",
			wantErr:     credentialCause,
		},
		{
			name: "request build failure",
			provider: identity.NewStatic(identity.Credential{
				BaseURL: "://invalid",
				Token:   "copilot-token",
			}, true),
			call:        Call{Route: endpoint.RouteModels, Method: http.MethodGet},
			wantKind:    apierror.BadGateway,
			wantMessage: "could not build the upstream request",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			caller := &Caller{
				provider: tc.provider,
				logger:   slog.New(slog.NewTextHandler(&logs, nil)),
			}

			request, failure := caller.Prepare(context.Background(), tc.call)

			if request != nil {
				t.Errorf("request = %#v, want nil", request)
			}
			if failure == nil {
				t.Fatal("failure = nil, want classified failure")
			}
			if failure.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", failure.Kind, tc.wantKind)
			}
			if failure.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", failure.Message, tc.wantMessage)
			}
			if tc.wantErr != nil && failure.Err != tc.wantErr {
				t.Errorf("Err = %v, want original error %v", failure.Err, tc.wantErr)
			}
			if failure.Err == nil {
				t.Error("Err = nil, want underlying cause")
			}
			if got := strings.Count(logs.String(), "\n"); got != 1 {
				t.Errorf("failure log records = %d, want 1: %q", got, logs.String())
			}
		})
	}
}

func TestCopyResponseHeadersAppliesResponsePolicy(t *testing.T) {
	source := http.Header{
		"Connection":             {"X-Connection-Only"},
		"Keep-Alive":             {"timeout=5"},
		"Proxy-Authenticate":     {"challenge"},
		"Proxy-Authorization":    {"secret"},
		"TE":                     {"trailers"},
		"Trailer":                {"Digest"},
		"Transfer-Encoding":      {"chunked"},
		"Upgrade":                {"websocket"},
		RequestIDHeader:          {"upstream id"},
		"X-Connection-Only":      {"strip me"},
		"Authorization":          {"response value is allowed"},
		"Sec-WebSocket-Protocol": {"response value is allowed"},
		"X-Keep":                 {"one", "two"},
	}
	destination := http.Header{"X-Keep": {"existing"}}

	CopyResponseHeaders(destination, source)

	for _, name := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		RequestIDHeader,
		"X-Connection-Only",
	} {
		if got := destination.Values(name); len(got) != 0 {
			t.Errorf("%s = %q, want absent", name, got)
		}
	}
	for name, want := range map[string][]string{
		"Authorization":          {"response value is allowed"},
		"Sec-Websocket-Protocol": {"response value is allowed"},
		"X-Keep":                 {"existing", "one", "two"},
	} {
		if got := destination.Values(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func prepareRequest(t *testing.T, ctx context.Context, credential identity.Credential, call Call) *http.Request {
	t.Helper()
	caller := &Caller{
		provider: identity.NewStatic(credential, true),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request, failure := caller.Prepare(ctx, call)
	if failure != nil {
		t.Fatalf("Prepare failure: kind=%v message=%q err=%v", failure.Kind, failure.Message, failure.Err)
	}
	if request == nil {
		t.Fatal("Prepare request = nil, want request")
	}
	return request
}
