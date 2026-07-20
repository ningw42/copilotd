package endpoint

import (
	"net/http"
	"reflect"
	"testing"
)

type servedContract interface {
	Surface() Surface
	Patterns() []string
	Upstream() Route
}

// TestServedEndpointContracts asserts the complete served set from one readable
// table so an endpoint fact cannot drift without changing this golden contract.
func TestServedEndpointContracts(t *testing.T) {
	allowsSSE := func(allowed bool) *bool { return &allowed }
	requiredRoute := func(route Route) *Route { return &route }

	tests := []struct {
		name              string
		contract          servedContract
		wantKind          reflect.Type
		wantPatterns      []string
		wantSurface       Surface
		wantUpstream      Route
		wantRequiredRoute *Route
		wantAllowsSSE     *bool
	}{
		{
			name:          "Anthropic Messages is an SSE-capable HTTP forward",
			contract:      AnthropicMessages(),
			wantKind:      reflect.TypeOf(HTTPForward{}),
			wantPatterns:  []string{http.MethodPost + " /anthropic/v1/messages"},
			wantSurface:   Anthropic,
			wantUpstream:  RouteAnthropicMessages,
			wantAllowsSSE: allowsSSE(true),
		},
		{
			name:          "Anthropic Count Tokens is a JSON-only HTTP forward",
			contract:      AnthropicCountTokens(),
			wantKind:      reflect.TypeOf(HTTPForward{}),
			wantPatterns:  []string{http.MethodPost + " /anthropic/v1/messages/count_tokens"},
			wantSurface:   Anthropic,
			wantUpstream:  RouteAnthropicCountTokens,
			wantAllowsSSE: allowsSSE(false),
		},
		{
			name:          "OpenAI Responses HTTP is an SSE-capable HTTP forward",
			contract:      OpenAIResponsesHTTP(),
			wantKind:      reflect.TypeOf(HTTPForward{}),
			wantPatterns:  []string{http.MethodPost + " /openai/v1/responses"},
			wantSurface:   OpenAI,
			wantUpstream:  RouteOpenAIResponses,
			wantAllowsSSE: allowsSSE(true),
		},
		{
			name:         "OpenAI Responses WebSocket is a WebSocket forward",
			contract:     OpenAIResponsesWS(),
			wantKind:     reflect.TypeOf(WSForward{}),
			wantPatterns: []string{http.MethodGet + " /openai/v1/responses"},
			wantSurface:  OpenAI,
			wantUpstream: RouteOpenAIResponses,
		},
		{
			name:     "Models is a GET and HEAD raw passthrough",
			contract: Models(),
			wantKind: reflect.TypeOf(Passthrough{}),
			wantPatterns: []string{
				http.MethodGet + " /models",
				http.MethodHead + " /models",
			},
			wantSurface:  GitHubCopilot,
			wantUpstream: RouteModels,
		},
		{
			name:     "Anthropic Catalog reads models supporting Anthropic Messages",
			contract: AnthropicCatalog(),
			wantKind: reflect.TypeOf(Catalog{}),
			wantPatterns: []string{
				http.MethodGet + " /anthropic/v1/models",
				http.MethodHead + " /anthropic/v1/models",
			},
			wantSurface:       Anthropic,
			wantUpstream:      RouteModels,
			wantRequiredRoute: requiredRoute(RouteAnthropicMessages),
		},
		{
			name:     "OpenAI Catalog reads models supporting OpenAI Responses",
			contract: OpenAICatalog(),
			wantKind: reflect.TypeOf(Catalog{}),
			wantPatterns: []string{
				http.MethodGet + " /openai/v1/models",
				http.MethodHead + " /openai/v1/models",
			},
			wantSurface:       OpenAI,
			wantUpstream:      RouteModels,
			wantRequiredRoute: requiredRoute(RouteOpenAIResponses),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reflect.TypeOf(tc.contract); got != tc.wantKind {
				t.Errorf("contract type = %v, want %v", got, tc.wantKind)
			}
			if got := tc.contract.Patterns(); !reflect.DeepEqual(got, tc.wantPatterns) {
				t.Errorf("Patterns() = %v, want %v", got, tc.wantPatterns)
			}
			if got := tc.contract.Surface(); got != tc.wantSurface {
				t.Errorf("Surface() = %v, want %v", got, tc.wantSurface)
			}
			if got := tc.contract.Upstream(); got != tc.wantUpstream {
				t.Errorf("Upstream() = %q, want %q", got, tc.wantUpstream)
			}

			catalog, isCatalog := tc.contract.(Catalog)
			if wantCatalog := tc.wantRequiredRoute != nil; isCatalog != wantCatalog {
				t.Fatalf("RequiredRoute() availability = %t, want %t", isCatalog, wantCatalog)
			}
			if isCatalog && catalog.RequiredRoute() != *tc.wantRequiredRoute {
				t.Errorf("RequiredRoute() = %q, want %q", catalog.RequiredRoute(), *tc.wantRequiredRoute)
			}

			forward, isForward := tc.contract.(HTTPForward)
			if wantForward := tc.wantAllowsSSE != nil; isForward != wantForward {
				t.Fatalf("AllowsSSE() availability = %t, want %t", isForward, wantForward)
			}
			if isForward && forward.AllowsSSE() != *tc.wantAllowsSSE {
				t.Errorf("AllowsSSE() = %t, want %t", forward.AllowsSSE(), *tc.wantAllowsSSE)
			}
		})
	}
}

func TestCanonicalAccessorsResolveToOnePackageFactsRecordPerOperation(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{name: "Anthropic Messages", got: AnthropicMessages().resolved(), want: &anthropicMessages},
		{name: "Anthropic Count Tokens", got: AnthropicCountTokens().resolved(), want: &anthropicCountTokens},
		{name: "OpenAI Responses HTTP", got: OpenAIResponsesHTTP().resolved(), want: &openAIResponsesHTTP},
		{name: "OpenAI Responses WebSocket", got: OpenAIResponsesWS().resolved(), want: &openAIResponsesWS},
		{name: "Models", got: Models().resolved(), want: &models},
		{name: "Anthropic Catalog", got: AnthropicCatalog().resolved(), want: &anthropicCatalog},
		{name: "OpenAI Catalog", got: OpenAICatalog().resolved(), want: &openAICatalog},
	}

	for _, tc := range tests {
		t.Run(tc.name+" has one canonical facts record", func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("accessor facts = %p, want package record %p", tc.got, tc.want)
			}
		})
	}
}

func TestSurfaceStringReturnsCanonicalNames(t *testing.T) {
	tests := []struct {
		name    string
		surface Surface
		want    string
	}{
		{name: "Anthropic has its canonical name", surface: Anthropic, want: "anthropic"},
		{name: "OpenAI has its canonical name", surface: OpenAI, want: "openai"},
		{name: "GitHub Copilot has its canonical name", surface: GitHubCopilot, want: "github-copilot"},
		{name: "An out-of-range Surface is unknown", surface: Surface(99), want: "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.surface.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
