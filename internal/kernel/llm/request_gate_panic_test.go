package llm

import (
	"context"
	"testing"
)

func TestRequestGateReleasesThePermitWhenAnAdapterPanics(t *testing.T) {
	for _, method := range []string{"stream", "chat", "completion"} {
		t.Run(method, func(t *testing.T) {
			gate := NewRequestGate(1)
			provider := gateTestProvider{
				chat:   func(context.Context) (*ChatResponse, error) { panic("adapter broke") },
				stream: func(context.Context) (<-chan StreamEvent, error) { panic("adapter broke") },
			}
			wrapped := gate.Wrap(provider, "shared-route")
			func() {
				defer func() {
					if recovered := recover(); recovered != "adapter broke" {
						t.Fatalf("the adapter panic changed meaning: %v", recovered)
					}
				}()
				switch method {
				case "stream":
					_, _ = wrapped.StreamChat(context.Background(), ChatRequest{})
				case "chat":
					_, _ = wrapped.Chat(context.Background(), ChatRequest{})
				case "completion":
					_, _ = wrapped.ChatCompletion(context.Background(), nil)
				}
			}()
			next := gate.Wrap(gateTestProvider{chat: func(context.Context) (*ChatResponse, error) {
				return &ChatResponse{Content: "next request"}, nil
			}}, "shared-route")
			ctx := WithProviderWaitDeferral(WithModelContext(context.Background(), ModelContext{RunID: "next-run"}))
			if response, err := next.Chat(ctx, ChatRequest{}); err != nil || response.Content != "next request" {
				t.Fatalf("the panicked adapter kept its permit: %+v, %v", response, err)
			}
		})
	}
}
