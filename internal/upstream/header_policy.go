package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/ningw42/copilotd/internal/logging"
)

// hopByHop is the standard set of connection-scoped headers a proxy must not
// forward in either direction. Names use http.CanonicalHeaderKey form.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// requestStrip is hopByHop plus values that belong only to copilotd's inbound
// request. Sec-WebSocket-* is handled as a prefix rule by stripRequestHeader.
var requestStrip = withExtra(hopByHop, "Authorization", "X-Api-Key", "Host", "Content-Length")

// responseStrip suppresses Copilot's request id because the outer middleware
// has already installed copilotd's resolved correlation value.
var responseStrip = withExtra(hopByHop, RequestIDHeader)

func withExtra(base map[string]bool, extra ...string) map[string]bool {
	result := make(map[string]bool, len(base)+len(extra))
	for name := range base {
		result[name] = true
	}
	for _, name := range extra {
		result[http.CanonicalHeaderKey(name)] = true
	}
	return result
}

func authenticatedOutboundHeaders(ctx context.Context, call Call, token string, credentialHeaders http.Header) http.Header {
	result := make(http.Header, len(call.ClientHeader)+len(credentialHeaders)+3)
	connection := connectionTokens(call.ClientHeader)
	for name, values := range call.ClientHeader {
		canonicalName := http.CanonicalHeaderKey(name)
		if stripRequestHeader(canonicalName) || connection[canonicalName] {
			continue
		}
		result[canonicalName] = append([]string(nil), values...)
	}

	result.Set("Authorization", "Bearer "+token)
	for name, values := range credentialHeaders {
		result[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	if requestID, ok := logging.RequestIDFrom(ctx); ok {
		result.Set(RequestIDHeader, requestID)
	}
	if call.AcceptIdentityEncoding {
		result.Set("Accept-Encoding", "identity")
	}
	return result
}

func stripRequestHeader(canonicalName string) bool {
	return requestStrip[canonicalName] || strings.HasPrefix(strings.ToLower(canonicalName), "sec-websocket-")
}

// CopyResponseHeaders copies response headers minus the response strip set and
// any names listed in the upstream Connection header.
func CopyResponseHeaders(destination, source http.Header) {
	connection := connectionTokens(source)
	for name, values := range source {
		canonicalName := http.CanonicalHeaderKey(name)
		if responseStrip[canonicalName] || connection[canonicalName] {
			continue
		}
		for _, value := range values {
			destination.Add(canonicalName, value)
		}
	}
}

func connectionTokens(header http.Header) map[string]bool {
	var result map[string]bool
	for name, values := range header {
		if http.CanonicalHeaderKey(name) != "Connection" {
			continue
		}
		for _, value := range values {
			for _, token := range strings.Split(value, ",") {
				if token = strings.TrimSpace(token); token != "" {
					if result == nil {
						result = make(map[string]bool)
					}
					result[http.CanonicalHeaderKey(token)] = true
				}
			}
		}
	}
	return result
}

// singleAttemptBody differs from http.NoBody only in identity. Its non-nil Body
// and nil GetBody make an otherwise bodyless request non-replayable.
type singleAttemptBody struct {
	io.ReadCloser
}
