package shim_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/shim"
)

type clientMessageTransformFunc func(context.Context, *shim.Message) bool

func (f clientMessageTransformFunc) TransformClientMessage(ctx context.Context, message *shim.Message) bool {
	return f(ctx, message)
}

type serverMessageTransformFunc func(context.Context, *shim.Message) bool

func (f serverMessageTransformFunc) TransformServerMessage(ctx context.Context, message *shim.Message) bool {
	return f(ctx, message)
}

func TestWebSocketAdaptersAreNilWithoutDirectionalParticipants(t *testing.T) {
	chain := (shim.Registry{{
		Name:    "nop",
		Enabled: true,
		New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return shim.NopShim{}
		},
	}}).NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses)

	if adapter := chain.WSClientAdapter(context.Background(), nil); adapter != nil {
		t.Errorf("WSClientAdapter() = %T, want nil", adapter)
	}
	if adapter := chain.WSServerAdapter(context.Background(), nil); adapter != nil {
		t.Errorf("WSServerAdapter() = %T, want nil", adapter)
	}
}

func TestWSClientAdapterFoldsParticipantsInRegistrationOrder(t *testing.T) {
	registry := shim.Registry{}
	for _, name := range []string{"outer", "middle", "inner"} {
		name := name
		registry = append(registry, shim.Registration{
			Name:    name,
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return clientMessageTransformFunc(func(_ context.Context, message *shim.Message) bool {
					message.Data = []byte(name + "(" + string(message.Data) + ")")
					message.Kind = shim.MessageBinary
					return true
				})
			},
		})
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter(context.Background(), nil)
	if adapter == nil {
		t.Fatal("WSClientAdapter() = nil, want composed transform")
	}
	message := shim.Message{Kind: shim.MessageText, Data: []byte("seed")}

	emit := adapter(context.Background(), &message)

	if !emit {
		t.Fatal("adapter() = emit false, want true")
	}
	if got, want := string(message.Data), "inner(middle(outer(seed)))"; got != want {
		t.Errorf("message data = %q, want %q", got, want)
	}
	if message.Kind != shim.MessageBinary {
		t.Errorf("message kind = %v, want MessageBinary", message.Kind)
	}
}

func TestWSServerAdapterFoldsParticipantsInReverseRegistrationOrder(t *testing.T) {
	registry := shim.Registry{}
	for _, name := range []string{"outer", "middle", "inner"} {
		name := name
		registry = append(registry, shim.Registration{
			Name:    name,
			Enabled: true,
			New: func(context.Context, endpoint.Surface, endpoint.Route) any {
				return serverMessageTransformFunc(func(_ context.Context, message *shim.Message) bool {
					message.Data = []byte(name + "(" + string(message.Data) + ")")
					message.Kind = shim.MessageText
					return true
				})
			},
		})
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter(context.Background(), nil)
	if adapter == nil {
		t.Fatal("WSServerAdapter() = nil, want composed transform")
	}
	message := shim.Message{Kind: shim.MessageBinary, Data: []byte("seed")}

	emit := adapter(context.Background(), &message)

	if !emit {
		t.Fatal("adapter() = emit false, want true")
	}
	if got, want := string(message.Data), "outer(middle(inner(seed)))"; got != want {
		t.Errorf("message data = %q, want %q", got, want)
	}
	if message.Kind != shim.MessageText {
		t.Errorf("message kind = %v, want MessageText", message.Kind)
	}
}

func TestWSClientAdapterDropShortCircuitsRemainingParticipants(t *testing.T) {
	calls := []string{}
	clientTransform := func(name string, emit bool) clientMessageTransformFunc {
		return func(context.Context, *shim.Message) bool {
			calls = append(calls, name)
			return emit
		}
	}
	registry := shim.Registry{
		{Name: "first", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientTransform("first", true)
		}},
		{Name: "drop", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientTransform("drop", false)
		}},
		{Name: "never", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientTransform("never", true)
		}},
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter(context.Background(), nil)

	if emit := adapter(context.Background(), &shim.Message{}); emit {
		t.Fatal("adapter() = emit true, want intentional drop")
	}
	if got, want := calls, []string{"first", "drop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestWSServerAdapterDropShortCircuitsRemainingParticipants(t *testing.T) {
	calls := []string{}
	serverTransform := func(name string, emit bool) serverMessageTransformFunc {
		return func(context.Context, *shim.Message) bool {
			calls = append(calls, name)
			return emit
		}
	}
	registry := shim.Registry{
		{Name: "never", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverTransform("never", true)
		}},
		{Name: "drop", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverTransform("drop", false)
		}},
		{Name: "first", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverTransform("first", true)
		}},
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter(context.Background(), nil)

	if emit := adapter(context.Background(), &shim.Message{}); emit {
		t.Fatal("adapter() = emit true, want intentional drop")
	}
	if got, want := calls, []string{"first", "drop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}
