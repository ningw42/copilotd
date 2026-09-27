package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage/reporthttp"
)

// Mount is one built handler bound to the server's router. Build it with the
// typed constructor for its Endpoint kind, or with MountReport for the local
// Usage report. Each constructor rejects a missing handler before any
// middleware wraps it, so an omission fails at construction, not on a request.
type Mount struct {
	routes  []mountRoute
	surface endpoint.Surface
	// gated Endpoint routes run auth, then local readiness, before the handler.
	gated   bool
	handler http.Handler
	drainer WebSocketDrainer
}

// mountRoute is one ServeMux pattern with the logging scope its binding owns.
type mountRoute struct {
	pattern string
	scope   []slog.Attr
}

// WebSocketDrainer closes WebSocket admission and drains established sessions
// during shutdown. MountWebSocket binds one to its handler.
type WebSocketDrainer interface {
	StartDrain()
	Shutdown(context.Context) error
}

// noWebSocketDrainer stands in for a server with no WebSocket mount, whose
// shutdown drains HTTP only.
type noWebSocketDrainer struct{}

func (noWebSocketDrainer) StartDrain()                    {}
func (noWebSocketDrainer) Shutdown(context.Context) error { return nil }

// MountHTTPForward mounts an HTTP forwarding Endpoint's built handler.
func MountHTTPForward(ep endpoint.HTTPForward, handler http.Handler) Mount {
	return endpointMount(ep, handler)
}

// MountPassthrough mounts the raw model source passthrough's built handler.
func MountPassthrough(ep endpoint.Passthrough, handler http.Handler) Mount {
	return endpointMount(ep, handler)
}

// MountCatalog mounts a Catalog Endpoint's built handler.
func MountCatalog(ep endpoint.Catalog, handler http.Handler) Mount {
	return endpointMount(ep, handler)
}

// MountWebSocket mounts the WebSocket forwarding Endpoint with ws=true in its
// binding scope. Its handler and drainer are bound together: shutdown closes
// the drainer's admission, then drains its sessions alongside HTTP under one
// grace deadline.
func MountWebSocket(ep endpoint.WSForward, handler http.Handler, drainer WebSocketDrainer) Mount {
	mount := endpointMount(ep, handler, slog.Bool(logging.WSKey, true))
	if drainer == nil {
		panic(fmt.Sprintf("server: WebSocket mount %q has no drainer", ep.Patterns()))
	}
	mount.drainer = drainer
	return mount
}

// MountReport mounts the local Usage report handler, including its disabled
// variant. The report is not an Endpoint: it bypasses auth and readiness but
// keeps ordinary, non-probe access classification.
func MountReport(handler http.Handler) Mount {
	requireHandler(handler, []string{reporthttp.Path})
	return Mount{
		routes:  []mountRoute{{pattern: reporthttp.Path, scope: []slog.Attr{slog.String(logging.InboundKey, reporthttp.Path)}}},
		handler: handler,
	}
}

// endpointMount gates handler by ep's Surface and scopes each of ep's patterns
// with its inbound pattern, Surface and any extra binding attributes.
func endpointMount(ep endpoint.Endpoint, handler http.Handler, extra ...slog.Attr) Mount {
	requireHandler(handler, ep.Patterns())
	mount := Mount{surface: ep.Surface(), gated: true, handler: handler}
	for _, pattern := range ep.Patterns() {
		scope := []slog.Attr{
			slog.String(logging.InboundKey, pattern),
			slog.String(logging.SurfaceKey, ep.Surface().String()),
		}
		mount.routes = append(mount.routes, mountRoute{pattern: pattern, scope: append(scope, extra...)})
	}
	return mount
}

// requireHandler rejects the forms a composition-root omission takes: a nil
// interface or a nil http.HandlerFunc.
func requireHandler(handler http.Handler, patterns []string) {
	if f, isFunc := handler.(http.HandlerFunc); handler == nil || (isFunc && f == nil) {
		panic(fmt.Sprintf("server: mount %q has a nil handler", patterns))
	}
}
