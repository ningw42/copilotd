package catalog

import (
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

func TestFilterRequiresPickerVisibilityAndExactSurfaceRoute(t *testing.T) {
	models := []Model{
		{ID: "route-less", ModelPickerEnabled: true},
		{ID: "chat-only", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{"/chat/completions"}},
		{ID: "embedding", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{"/embeddings"}},
		// WebSocket-only models are intentionally excluded from the exact /responses catalog.
		{ID: "websocket-only", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{"ws:/responses"}},
		{ID: "hidden-but-forwardable", ModelPickerEnabled: false, SupportedRoutes: []endpoint.Route{endpoint.RouteOpenAIResponses}},
		{ID: "microsoft-visible", Vendor: "Microsoft", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{endpoint.RouteOpenAIResponses}},
		{ID: "anthropic-visible", Vendor: "Anthropic", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{endpoint.RouteAnthropicMessages}},
		{ID: "openai-visible", Vendor: "Azure OpenAI", ModelPickerEnabled: true, SupportedRoutes: []endpoint.Route{"ws:/responses", endpoint.RouteOpenAIResponses}},
	}

	if got := modelIDs(Filter(models, endpoint.RouteOpenAIResponses)); !reflect.DeepEqual(got, []string{"microsoft-visible", "openai-visible"}) {
		t.Errorf("Responses membership = %q, want exact visible Routes in input order", got)
	}
	if got := modelIDs(Filter(models, endpoint.RouteAnthropicMessages)); !reflect.DeepEqual(got, []string{"anthropic-visible"}) {
		t.Errorf("Messages membership = %q, want exact visible Route", got)
	}
}
