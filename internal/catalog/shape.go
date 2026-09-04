package catalog

import (
	"net/http"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// Shape identifies a successfully rendered OpenAI Catalog shape.
type Shape string

const (
	// ShapeOpenAI is the provider-shaped OpenAI Catalog.
	ShapeOpenAI Shape = "openai"
	// ShapeCodex is the client-shaped Codex catalog.
	ShapeCodex Shape = "codex"
)

// servesCodexShape reports whether this descriptor's opt-in gates are open for the
// OpenAI Surface and the given request.
func (d CodexDescriptor) servesCodexShape(ep endpoint.Catalog, r *http.Request) bool {
	return ep.Surface() == endpoint.OpenAI &&
		r.URL.Query().Has("client_version") &&
		d.Enabled &&
		d.RenderConfig.mutates()
}
