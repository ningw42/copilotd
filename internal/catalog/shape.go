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

// servesCodexShape reports whether this descriptor serves the Codex shape for
// the OpenAI Surface and the given request. A present models source is the
// Codex catalog's enable signal; render add-ons only alter what is served.
func (d CodexDescriptor) servesCodexShape(ep endpoint.Catalog, r *http.Request) bool {
	return ep.Surface() == endpoint.OpenAI &&
		r.URL.Query().Has("client_version") &&
		d.Models != nil
}
