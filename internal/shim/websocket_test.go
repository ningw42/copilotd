package shim_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ningw42/copilotd/internal/endpoint"
	"github.com/ningw42/copilotd/internal/shim"
)

type clientMessageTransformFunc func(context.Context, *shim.Message) (bool, error)

func (f clientMessageTransformFunc) TransformClientMessage(ctx context.Context, message *shim.Message) (bool, error) {
	return f(ctx, message)
}

type serverMessageTransformFunc func(context.Context, *shim.Message) (bool, error)

func (f serverMessageTransformFunc) TransformServerMessage(ctx context.Context, message *shim.Message) (bool, error) {
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

	if adapter := chain.WSClientAdapter(); adapter != nil {
		t.Errorf("WSClientAdapter() = %T, want nil", adapter)
	}
	if adapter := chain.WSServerAdapter(); adapter != nil {
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
				return clientMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
					message.Data = []byte(name + "(" + string(message.Data) + ")")
					message.Kind = shim.MessageBinary
					return true, nil
				})
			},
		})
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter()
	if adapter == nil {
		t.Fatal("WSClientAdapter() = nil, want composed transform")
	}
	message := shim.Message{Kind: shim.MessageText, Data: []byte("seed")}

	emit, err := adapter(context.Background(), &message)

	if err != nil || !emit {
		t.Fatalf("adapter() = emit %t, error %v; want emit true, nil", emit, err)
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
				return serverMessageTransformFunc(func(_ context.Context, message *shim.Message) (bool, error) {
					message.Data = []byte(name + "(" + string(message.Data) + ")")
					message.Kind = shim.MessageText
					return true, nil
				})
			},
		})
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter()
	if adapter == nil {
		t.Fatal("WSServerAdapter() = nil, want composed transform")
	}
	message := shim.Message{Kind: shim.MessageBinary, Data: []byte("seed")}

	emit, err := adapter(context.Background(), &message)

	if err != nil || !emit {
		t.Fatalf("adapter() = emit %t, error %v; want emit true, nil", emit, err)
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
		return func(context.Context, *shim.Message) (bool, error) {
			calls = append(calls, name)
			return emit, nil
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
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter()

	emit, err := adapter(context.Background(), &shim.Message{})

	if err != nil || emit {
		t.Fatalf("adapter() = emit %t, error %v; want intentional drop", emit, err)
	}
	if got, want := calls, []string{"first", "drop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestWSServerAdapterDropShortCircuitsRemainingParticipants(t *testing.T) {
	calls := []string{}
	serverTransform := func(name string, emit bool) serverMessageTransformFunc {
		return func(context.Context, *shim.Message) (bool, error) {
			calls = append(calls, name)
			return emit, nil
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
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter()

	emit, err := adapter(context.Background(), &shim.Message{})

	if err != nil || emit {
		t.Fatalf("adapter() = emit %t, error %v; want intentional drop", emit, err)
	}
	if got, want := calls, []string{"first", "drop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestWSClientAdapterPropagatesErrorAndAbortsFold(t *testing.T) {
	wantErr := errors.New("client transform failed")
	laterCalled := false
	registry := shim.Registry{
		{Name: "error", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				return true, wantErr
			})
		}},
		{Name: "later", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return clientMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				laterCalled = true
				return true, nil
			})
		}},
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSClientAdapter()

	emit, err := adapter(context.Background(), &shim.Message{})

	if emit || !errors.Is(err, wantErr) {
		t.Fatalf("adapter() = emit %t, error %v; want emit false, error %v", emit, err, wantErr)
	}
	if laterCalled {
		t.Error("participant after client transform error was called")
	}
}

func TestWSServerAdapterPropagatesErrorAndAbortsFold(t *testing.T) {
	wantErr := errors.New("server transform failed")
	laterCalled := false
	registry := shim.Registry{
		{Name: "later", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				laterCalled = true
				return true, nil
			})
		}},
		{Name: "error", Enabled: true, New: func(context.Context, endpoint.Surface, endpoint.Route) any {
			return serverMessageTransformFunc(func(context.Context, *shim.Message) (bool, error) {
				return true, wantErr
			})
		}},
	}
	adapter := registry.NewChain(context.Background(), endpoint.OpenAI, endpoint.RouteOpenAIResponses).WSServerAdapter()

	emit, err := adapter(context.Background(), &shim.Message{})

	if emit || !errors.Is(err, wantErr) {
		t.Fatalf("adapter() = emit %t, error %v; want emit false, error %v", emit, err, wantErr)
	}
	if laterCalled {
		t.Error("participant after server transform error was called")
	}
}
