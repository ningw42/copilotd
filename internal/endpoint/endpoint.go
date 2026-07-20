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

// binding is the inbound half shared by every served contract. Its fields stay
// private so callers can observe patterns and Surface without mutating either.
type binding struct {
	surface Surface
	methods []string
	path    string
}

// Surface returns the inbound API dialect owned by the binding.
func (b binding) Surface() Surface { return b.surface }

// Patterns returns one "METHOD /path" ServeMux pattern per served method.
func (b binding) Patterns() []string {
	patterns := make([]string, len(b.methods))
	for i, method := range b.methods {
		patterns[i] = method + " " + b.path
	}
	return patterns
}

// Endpoint is the inbound projection shared by every complete contract kind.
// It deliberately carries no upstream fact and is not accepted by behavior
// factories, which take the opaque concrete kinds below.
type Endpoint interface {
	Surface() Surface
	Patterns() []string
}

type httpForwardFacts struct {
	binding
	upstream Route
	sse      SSEMode
}

type wsForwardFacts struct {
	binding
	upstream Route
}

type passthroughFacts struct {
	binding
	upstream Route
}

type catalogFacts struct {
	binding
	upstream      Route
	requiredRoute Route
}

// Each operation's complete served facts are declared once here. Public
// contract values are opaque handles to these private canonical records.
var (
	anthropicMessages = httpForwardFacts{
		binding:  binding{surface: Anthropic, methods: []string{http.MethodPost}, path: "/anthropic/v1/messages"},
		upstream: RouteAnthropicMessages,
		sse:      JSONorSSE,
	}
	anthropicCountTokens = httpForwardFacts{
		binding:  binding{surface: Anthropic, methods: []string{http.MethodPost}, path: "/anthropic/v1/messages/count_tokens"},
		upstream: RouteAnthropicCountTokens,
		sse:      NeverSSE,
	}
	openAIResponsesHTTP = httpForwardFacts{
		binding:  binding{surface: OpenAI, methods: []string{http.MethodPost}, path: "/openai/v1/responses"},
		upstream: RouteOpenAIResponses,
		sse:      JSONorSSE,
	}
	openAIResponsesWS = wsForwardFacts{
		binding:  binding{surface: OpenAI, methods: []string{http.MethodGet}, path: "/openai/v1/responses"},
		upstream: RouteOpenAIResponses,
	}
	models = passthroughFacts{
		binding:  binding{surface: GitHubCopilot, methods: []string{http.MethodGet, http.MethodHead}, path: "/models"},
		upstream: RouteModels,
	}
	anthropicCatalog = catalogFacts{
		binding:       binding{surface: Anthropic, methods: []string{http.MethodGet, http.MethodHead}, path: "/anthropic/v1/models"},
		upstream:      RouteModels,
		requiredRoute: RouteAnthropicMessages,
	}
	openAICatalog = catalogFacts{
		binding:       binding{surface: OpenAI, methods: []string{http.MethodGet, http.MethodHead}, path: "/openai/v1/models"},
		upstream:      RouteModels,
		requiredRoute: RouteOpenAIResponses,
	}
)

// HTTPForward is an immutable HTTP-forward contract. Its zero value is
// AnthropicMessages; the other canonical values come from parameterless
// accessors in this package.
type HTTPForward struct {
	facts *httpForwardFacts
}

func (h HTTPForward) resolved() *httpForwardFacts {
	if h.facts == nil {
		return &anthropicMessages
	}
	return h.facts
}

// Surface returns the contract's inbound API dialect.
func (h HTTPForward) Surface() Surface { return h.resolved().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (h HTTPForward) Patterns() []string { return h.resolved().Patterns() }

// Upstream returns the exact path forwarded upstream.
func (h HTTPForward) Upstream() Route { return h.resolved().upstream }

// AllowsSSE reports whether the endpoint may serve an SSE response.
func (h HTTPForward) AllowsSSE() bool { return h.resolved().sse.allowsSSE() }

// AnthropicMessages returns the canonical Anthropic Messages HTTP contract.
func AnthropicMessages() HTTPForward { return HTTPForward{} }

// AnthropicCountTokens returns the canonical Anthropic Count Tokens contract.
func AnthropicCountTokens() HTTPForward { return HTTPForward{facts: &anthropicCountTokens} }

// OpenAIResponsesHTTP returns the canonical OpenAI Responses HTTP contract.
func OpenAIResponsesHTTP() HTTPForward { return HTTPForward{facts: &openAIResponsesHTTP} }

// WSForward is the immutable OpenAI Responses WebSocket contract. Its zero value
// is canonical; no other WebSocket forward is served.
type WSForward struct {
	facts *wsForwardFacts
}

func (w WSForward) resolved() *wsForwardFacts {
	if w.facts == nil {
		return &openAIResponsesWS
	}
	return w.facts
}

// Surface returns the contract's inbound API dialect.
func (w WSForward) Surface() Surface { return w.resolved().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (w WSForward) Patterns() []string { return w.resolved().Patterns() }

// Upstream returns the exact WebSocket path forwarded upstream.
func (w WSForward) Upstream() Route { return w.resolved().upstream }

// OpenAIResponsesWS returns the canonical OpenAI Responses WebSocket contract.
func OpenAIResponsesWS() WSForward { return WSForward{} }

// Passthrough is the immutable raw Models passthrough contract. Its zero value
// is canonical; no other raw passthrough is served.
type Passthrough struct {
	facts *passthroughFacts
}

func (p Passthrough) resolved() *passthroughFacts {
	if p.facts == nil {
		return &models
	}
	return p.facts
}

// Surface returns the contract's inbound API dialect.
func (p Passthrough) Surface() Surface { return p.resolved().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (p Passthrough) Patterns() []string { return p.resolved().Patterns() }

// Upstream returns the exact path served as a raw passthrough.
func (p Passthrough) Upstream() Route { return p.resolved().upstream }

// Models returns the canonical raw GitHub Copilot model source contract.
func Models() Passthrough { return Passthrough{} }

// Catalog is an immutable provider-shaped model catalog contract. Its zero
// value is AnthropicCatalog; OpenAICatalog is the other canonical value.
type Catalog struct {
	facts *catalogFacts
}

func (c Catalog) resolved() *catalogFacts {
	if c.facts == nil {
		return &anthropicCatalog
	}
	return c.facts
}

// Surface returns the contract's inbound API dialect.
func (c Catalog) Surface() Surface { return c.resolved().Surface() }

// Patterns returns the contract's inbound ServeMux patterns.
func (c Catalog) Patterns() []string { return c.resolved().Patterns() }

// Upstream returns the exact path of the catalog's Copilot model source.
func (c Catalog) Upstream() Route { return c.resolved().upstream }

// RequiredRoute returns the route a model must advertise for catalog inclusion.
func (c Catalog) RequiredRoute() Route { return c.resolved().requiredRoute }

// AnthropicCatalog returns the canonical Anthropic provider-shaped model catalog.
func AnthropicCatalog() Catalog { return Catalog{} }

// OpenAICatalog returns the canonical OpenAI provider-shaped model catalog.
func OpenAICatalog() Catalog { return Catalog{facts: &openAICatalog} }
