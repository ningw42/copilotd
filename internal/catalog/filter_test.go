package catalog

import (
	"reflect"
	"testing"
)

func TestFilterRequiresPickerVisibilityAndExactSurfaceRoute(t *testing.T) {
	models := []Model{
		{ID: "route-less", ModelPickerEnabled: true},
		{ID: "chat-only", ModelPickerEnabled: true, SupportedRoutes: []Route{"/chat/completions"}},
		{ID: "embedding", ModelPickerEnabled: true, SupportedRoutes: []Route{"/embeddings"}},
		{ID: "websocket-only", ModelPickerEnabled: true, SupportedRoutes: []Route{"ws:/responses"}},
		{ID: "hidden-but-forwardable", ModelPickerEnabled: false, SupportedRoutes: []Route{OpenAIResponsesRoute}},
		{ID: "microsoft-visible", Vendor: "Microsoft", ModelPickerEnabled: true, SupportedRoutes: []Route{OpenAIResponsesRoute}},
		{ID: "anthropic-visible", Vendor: "Anthropic", ModelPickerEnabled: true, SupportedRoutes: []Route{AnthropicMessagesRoute}},
		{ID: "openai-visible", Vendor: "Azure OpenAI", ModelPickerEnabled: true, SupportedRoutes: []Route{"ws:/responses", OpenAIResponsesRoute}},
	}

	if got := modelIDs(Filter(models, OpenAIResponsesRoute)); !reflect.DeepEqual(got, []string{"microsoft-visible", "openai-visible"}) {
		t.Errorf("Responses membership = %q, want exact visible Routes in input order", got)
	}
	if got := modelIDs(Filter(models, AnthropicMessagesRoute)); !reflect.DeepEqual(got, []string{"anthropic-visible"}) {
		t.Errorf("Messages membership = %q, want exact visible Route", got)
	}
}
