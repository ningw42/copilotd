package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// noopDrainer is a WebSocket drainer with nothing to drain.
type noopDrainer struct{}

func (noopDrainer) StartDrain()                    {}
func (noopDrainer) Shutdown(context.Context) error { return nil }

// constructionPanic runs build and returns its panic message, or "" when it
// returned normally.
func constructionPanic(build func()) (message string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			message = fmt.Sprint(recovered)
		}
	}()
	build()
	return ""
}

func TestMountsRejectAMissingHandlerBeforeWrapping(t *testing.T) {
	missing := []struct {
		name    string
		handler http.Handler
	}{
		{name: "nil interface", handler: nil},
		{name: "nil HandlerFunc", handler: http.HandlerFunc(nil)},
	}
	mounts := []struct {
		name  string
		build func(http.Handler) Mount
	}{
		{name: "HTTP forward", build: func(h http.Handler) Mount { return MountHTTPForward(endpoint.AnthropicMessages(), h) }},
		{name: "passthrough", build: func(h http.Handler) Mount { return MountPassthrough(endpoint.Models(), h) }},
		{name: "Catalog", build: func(h http.Handler) Mount { return MountCatalog(endpoint.OpenAICatalog(), h) }},
		{name: "WebSocket", build: func(h http.Handler) Mount {
			return MountWebSocket(endpoint.OpenAIResponsesWS(), h, noopDrainer{})
		}},
		{name: "report", build: MountReport},
	}
	for _, mount := range mounts {
		for _, handler := range missing {
			t.Run(mount.name+"/"+handler.name, func(t *testing.T) {
				message := constructionPanic(func() { mount.build(handler.handler) })
				if !strings.Contains(message, "nil handler") {
					t.Errorf("construction panic = %q, want a nil handler rejection", message)
				}
			})
		}
	}
}

func TestWebSocketMountRejectsAMissingDrainer(t *testing.T) {
	message := constructionPanic(func() {
		MountWebSocket(endpoint.OpenAIResponsesWS(), http.NotFoundHandler(), nil)
	})
	if !strings.Contains(message, "no drainer") {
		t.Errorf("construction panic = %q, want a missing drainer rejection", message)
	}
}

func TestMountsAcceptBuiltHandlers(t *testing.T) {
	handler := http.NotFoundHandler()
	for name, build := range map[string]func(){
		"HTTP forward": func() { MountHTTPForward(endpoint.AnthropicMessages(), handler) },
		"passthrough":  func() { MountPassthrough(endpoint.Models(), handler) },
		"Catalog":      func() { MountCatalog(endpoint.AnthropicCatalog(), handler) },
		"WebSocket":    func() { MountWebSocket(endpoint.OpenAIResponsesWS(), handler, noopDrainer{}) },
		"report":       func() { MountReport(handler) },
	} {
		if message := constructionPanic(build); message != "" {
			t.Errorf("%s mount panicked: %s", name, message)
		}
	}
}
