package endpoint_test

import (
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
)

// embeddedForgedHTTPForward is the former interface-embedding escape: embedding
// promoted the private seal while these methods replaced every fact consumed by
// the forwarding factory.
type embeddedForgedHTTPForward struct{ endpoint.HTTPForward }

func (embeddedForgedHTTPForward) Surface() endpoint.Surface { return endpoint.OpenAI }
func (embeddedForgedHTTPForward) Patterns() []string        { return []string{"POST /forged"} }
func (embeddedForgedHTTPForward) Upstream() endpoint.Route  { return endpoint.RouteAnthropicMessages }
func (embeddedForgedHTTPForward) AllowsSSE() bool           { return true }

func TestEmbeddingAContractKindCannotForgeAnHTTPForward(t *testing.T) {
	forged := embeddedForgedHTTPForward{}
	if _, accepted := any(forged).(endpoint.HTTPForward); accepted {
		t.Fatal("an embedded wrapper was accepted as the concrete HTTPForward kind")
	}
}

func TestContractKindsAreOpaqueConcreteValues(t *testing.T) {
	tests := []struct {
		name   string
		typeOf reflect.Type
	}{
		{name: "HTTPForward is an opaque concrete kind", typeOf: reflect.TypeOf(endpoint.HTTPForward{})},
		{name: "WSForward is an opaque concrete kind", typeOf: reflect.TypeOf(endpoint.WSForward{})},
		{name: "Passthrough is an opaque concrete kind", typeOf: reflect.TypeOf(endpoint.Passthrough{})},
		{name: "Catalog is an opaque concrete kind", typeOf: reflect.TypeOf(endpoint.Catalog{})},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.typeOf.Kind() != reflect.Struct {
				t.Fatalf("kind = %v, want struct", tc.typeOf.Kind())
			}
			for i := range tc.typeOf.NumField() {
				if field := tc.typeOf.Field(i); field.IsExported() {
					t.Errorf("field %s is exported", field.Name)
				}
			}
		})
	}
}

func TestEndpointIsAnInboundOnlyInterface(t *testing.T) {
	typeOf := reflect.TypeOf((*endpoint.Endpoint)(nil)).Elem()
	if typeOf.Kind() != reflect.Interface {
		t.Fatalf("Endpoint kind = %v, want interface", typeOf.Kind())
	}

	got := make([]string, typeOf.NumMethod())
	for i := range typeOf.NumMethod() {
		got[i] = typeOf.Method(i).Name
	}
	if want := []string{"Patterns", "Surface"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Endpoint methods = %v, want inbound facts only %v", got, want)
	}
}

func TestEveryExternallyConstructibleZeroValueIsCanonical(t *testing.T) {
	tests := []struct {
		name string
		zero any
		want any
	}{
		{
			name: "HTTPForward zero is Anthropic Messages",
			zero: endpoint.HTTPForward{},
			want: endpoint.AnthropicMessages(),
		},
		{
			name: "WSForward zero is OpenAI Responses WebSocket",
			zero: endpoint.WSForward{},
			want: endpoint.OpenAIResponsesWS(),
		},
		{
			name: "Passthrough zero is Models",
			zero: endpoint.Passthrough{},
			want: endpoint.Models(),
		},
		{
			name: "Catalog zero is Anthropic Catalog",
			zero: endpoint.Catalog{},
			want: endpoint.AnthropicCatalog(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.zero, tc.want) {
				t.Errorf("zero = %#v, want canonical %#v", tc.zero, tc.want)
			}
		})
	}
}

func TestCanonicalAccessorsAreStableAndParameterless(t *testing.T) {
	tests := []struct {
		name string
		get  func() any
	}{
		{name: "Anthropic Messages", get: func() any { return endpoint.AnthropicMessages() }},
		{name: "Anthropic Count Tokens", get: func() any { return endpoint.AnthropicCountTokens() }},
		{name: "OpenAI Responses HTTP", get: func() any { return endpoint.OpenAIResponsesHTTP() }},
		{name: "OpenAI Responses WebSocket", get: func() any { return endpoint.OpenAIResponsesWS() }},
		{name: "Models", get: func() any { return endpoint.Models() }},
		{name: "Anthropic Catalog", get: func() any { return endpoint.AnthropicCatalog() }},
		{name: "OpenAI Catalog", get: func() any { return endpoint.OpenAICatalog() }},
	}

	for _, tc := range tests {
		t.Run(tc.name+" returns an immutable value", func(t *testing.T) {
			if first, second := tc.get(), tc.get(); !reflect.DeepEqual(first, second) {
				t.Errorf("successive accessor values differ: %#v and %#v", first, second)
			}
		})
	}
}

func TestContractKindsExposeNoMutatorMethods(t *testing.T) {
	tests := []struct {
		name        string
		typeOf      reflect.Type
		wantMethods []string
	}{
		{name: "HTTPForward", typeOf: reflect.TypeOf(endpoint.HTTPForward{}), wantMethods: []string{"AllowsSSE", "Patterns", "Surface", "Upstream"}},
		{name: "WSForward", typeOf: reflect.TypeOf(endpoint.WSForward{}), wantMethods: []string{"Patterns", "Surface", "Upstream"}},
		{name: "Passthrough", typeOf: reflect.TypeOf(endpoint.Passthrough{}), wantMethods: []string{"Patterns", "Surface", "Upstream"}},
		{name: "Catalog", typeOf: reflect.TypeOf(endpoint.Catalog{}), wantMethods: []string{"Patterns", "RequiredRoute", "Surface", "Upstream"}},
	}

	for _, tc := range tests {
		t.Run(tc.name+" exposes fact projections only", func(t *testing.T) {
			got := make([]string, tc.typeOf.NumMethod())
			for i := range tc.typeOf.NumMethod() {
				got[i] = tc.typeOf.Method(i).Name
			}
			if !reflect.DeepEqual(got, tc.wantMethods) {
				t.Errorf("exported methods = %v, want %v", got, tc.wantMethods)
			}
		})
	}
}
