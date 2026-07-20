// Package endpoint holds copilotd's served-endpoint contracts as
// dependency-light typed facts. Patterns returns strings in net/http
// ServeMux's "METHOD /path" grammar, the package's sole router concession.
package endpoint

import "net/http"

// Surface identifies the inbound API dialect copilotd speaks on a route.
type Surface int

const (
	Anthropic Surface = iota
	OpenAI
	GitHubCopilot
)

// String returns the Surface's canonical lowercase name.
func (s Surface) String() string {
	switch s {
	case Anthropic:
		return "anthropic"
	case OpenAI:
		return "openai"
	case GitHubCopilot:
		return "github-copilot"
	default:
		return "unknown"
	}
}

// Route is one exact upstream path. Route values are never normalized.
type Route string

const (
	RouteAnthropicMessages    Route = "/v1/messages"
	RouteAnthropicCountTokens Route = "/v1/messages/count_tokens"
	RouteOpenAIResponses      Route = "/responses"
	RouteModels               Route = "/models"
)

// SSEMode declares whether an HTTP-forward endpoint may serve an SSE response.
type SSEMode int

const (
	// NeverSSE marks a JSON-only endpoint. A text/event-stream upstream is
	// buffered rather than pumped.
	NeverSSE SSEMode = iota
	// JSONorSSE marks an endpoint that may stream when its upstream response is
	// text/event-stream.
	JSONorSSE
)

func (m SSEMode) allowsSSE() bool { return m == JSONorSSE }

type endpointID uint8

const (
	anthropicMessagesID endpointID = iota
	anthropicCountTokensID
	openAIResponsesHTTPID
	openAIResponsesWSID
	modelsID
	anthropicCatalogID
	openAICatalogID
)

// Endpoint is the immutable inbound projection of a served contract. It carries
// no upstream fact and is not accepted by behavior factories. Its zero value is
// the canonical Anthropic Messages binding.
type Endpoint struct {
	id endpointID
}

// Surface returns the inbound API dialect owned by the binding.
func (e Endpoint) Surface() Surface {
	switch e.id {
	case openAIResponsesHTTPID, openAIResponsesWSID, openAICatalogID:
		return OpenAI
	case modelsID:
		return GitHubCopilot
	default:
		return Anthropic
	}
}

// Patterns returns one "METHOD /path" ServeMux pattern per served method.
func (e Endpoint) Patterns() []string {
	switch e.id {
	case anthropicCountTokensID:
		return []string{http.MethodPost + " /anthropic/v1/messages/count_tokens"}
	case openAIResponsesHTTPID:
		return []string{http.MethodPost + " /openai/v1/responses"}
	case openAIResponsesWSID:
		return []string{http.MethodGet + " /openai/v1/responses"}
	case modelsID:
		return []string{http.MethodGet + " /models", http.MethodHead + " /models"}
	case anthropicCatalogID:
		return []string{http.MethodGet + " /anthropic/v1/models", http.MethodHead + " /anthropic/v1/models"}
	case openAICatalogID:
		return []string{http.MethodGet + " /openai/v1/models", http.MethodHead + " /openai/v1/models"}
	default:
		return []string{http.MethodPost + " /anthropic/v1/messages"}
	}
}

// HTTPForward is an immutable HTTP-forward contract. Its zero value is
// AnthropicMessages; the other canonical values come from parameterless
// accessors in this package.
type HTTPForward struct {
	id endpointID
}

// Endpoint returns the contract's inbound projection.
func (h HTTPForward) Endpoint() Endpoint {
	switch h.id {
	case anthropicCountTokensID, openAIResponsesHTTPID:
		return Endpoint{id: h.id}
	default:
		return Endpoint{id: anthropicMessagesID}
	}
}

// Surface returns the contract's inbound API dialect.
func (h HTTPForward) Surface() Surface { return h.Endpoint().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (h HTTPForward) Patterns() []string { return h.Endpoint().Patterns() }

// Upstream returns the exact path forwarded upstream.
func (h HTTPForward) Upstream() Route {
	switch h.Endpoint().id {
	case anthropicCountTokensID:
		return RouteAnthropicCountTokens
	case openAIResponsesHTTPID:
		return RouteOpenAIResponses
	default:
		return RouteAnthropicMessages
	}
}

// AllowsSSE reports whether the endpoint may serve an SSE response.
func (h HTTPForward) AllowsSSE() bool {
	if h.Endpoint().id == anthropicCountTokensID {
		return NeverSSE.allowsSSE()
	}
	return JSONorSSE.allowsSSE()
}

// AnthropicMessages returns the canonical Anthropic Messages HTTP contract.
func AnthropicMessages() HTTPForward { return HTTPForward{} }

// AnthropicCountTokens returns the canonical Anthropic Count Tokens contract.
func AnthropicCountTokens() HTTPForward { return HTTPForward{id: anthropicCountTokensID} }

// OpenAIResponsesHTTP returns the canonical OpenAI Responses HTTP contract.
func OpenAIResponsesHTTP() HTTPForward { return HTTPForward{id: openAIResponsesHTTPID} }

// WSForward is the immutable OpenAI Responses WebSocket contract. Its zero value
// is canonical; no other WebSocket forward is served.
type WSForward struct{}

// Endpoint returns the contract's inbound projection.
func (WSForward) Endpoint() Endpoint { return Endpoint{id: openAIResponsesWSID} }

// Surface returns the contract's inbound API dialect.
func (w WSForward) Surface() Surface { return w.Endpoint().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (w WSForward) Patterns() []string { return w.Endpoint().Patterns() }

// Upstream returns the exact WebSocket path forwarded upstream.
func (WSForward) Upstream() Route { return RouteOpenAIResponses }

// OpenAIResponsesWS returns the canonical OpenAI Responses WebSocket contract.
func OpenAIResponsesWS() WSForward { return WSForward{} }

// Passthrough is the immutable raw Models passthrough contract. Its zero value
// is canonical; no other raw passthrough is served.
type Passthrough struct{}

// Endpoint returns the contract's inbound projection.
func (Passthrough) Endpoint() Endpoint { return Endpoint{id: modelsID} }

// Surface returns the contract's inbound API dialect.
func (p Passthrough) Surface() Surface { return p.Endpoint().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (p Passthrough) Patterns() []string { return p.Endpoint().Patterns() }

// Upstream returns the exact path served as a raw passthrough.
func (Passthrough) Upstream() Route { return RouteModels }

// Models returns the canonical raw GitHub Copilot model source contract.
func Models() Passthrough { return Passthrough{} }

// Catalog is an immutable provider-shaped model catalog contract. Its zero
// value is AnthropicCatalog; OpenAICatalog is the other canonical value.
type Catalog struct {
	id endpointID
}

// Endpoint returns the contract's inbound projection.
func (c Catalog) Endpoint() Endpoint {
	if c.id == openAICatalogID {
		return Endpoint{id: openAICatalogID}
	}
	return Endpoint{id: anthropicCatalogID}
}

// Surface returns the contract's inbound API dialect.
func (c Catalog) Surface() Surface { return c.Endpoint().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (c Catalog) Patterns() []string { return c.Endpoint().Patterns() }

// Upstream returns the exact path of the catalog's Copilot model source.
func (Catalog) Upstream() Route { return RouteModels }

// RequiredRoute returns the route a model must advertise for catalog inclusion.
func (c Catalog) RequiredRoute() Route {
	if c.Endpoint().id == openAICatalogID {
		return RouteOpenAIResponses
	}
	return RouteAnthropicMessages
}

// AnthropicCatalog returns the canonical Anthropic provider-shaped model catalog.
func AnthropicCatalog() Catalog { return Catalog{} }

// OpenAICatalog returns the canonical OpenAI provider-shaped model catalog.
func OpenAICatalog() Catalog { return Catalog{id: openAICatalogID} }
