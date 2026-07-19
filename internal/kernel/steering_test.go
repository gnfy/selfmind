package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

func TestDrainSteering(t *testing.T) {
	if got := drainSteering(steeringChannels{}); got != nil {
		t.Fatalf("nil channel should drain to nil, got %v", got)
	}
	ch := make(chan string, 4)
	ch <- "first"
	ch <- "  " // whitespace dropped
	ch <- "second"
	got := drainSteering(steeringChannels{legacy: ch})
	if len(got) != 2 || got[0].Content != "first" || got[1].Content != "second" {
		t.Fatalf("drain = %v, want [first second]", got)
	}
	// Drained channel yields nothing further.
	if got := drainSteering(steeringChannels{legacy: ch}); got != nil {
		t.Fatalf("second drain should be empty, got %v", got)
	}
}

func TestSteeringContextRoundTrip(t *testing.T) {
	ch := make(chan string, 1)
	ctx := WithSteering(context.Background(), ch)
	if steeringFromContext(ctx).legacy == nil {
		t.Fatal("steering channel should be retrievable from ctx")
	}
	// nil channel is a no-op (no steering attached).
	if channels := steeringFromContext(WithSteering(context.Background(), nil)); channels.inputs != nil || channels.legacy != nil {
		t.Fatal("nil steering channel must not be attached")
	}
}

func TestSteeringInputContextKeepsMailboxCorrelation(t *testing.T) {
	ch := make(chan SteeringInput, 1)
	ch <- SteeringInput{ID: "steer-1", Content: "same text", ContentHash: "hash-1"}
	got := drainSteering(steeringFromContext(WithSteeringInputs(context.Background(), ch)))
	if len(got) != 1 || got[0].ID != "steer-1" || got[0].ContentHash != "hash-1" || got[0].Content != "same text" {
		t.Fatalf("steering input = %+v", got)
	}
}

// TestSteeringInjectedIntoConversation proves a steered message is folded into
// the conversation the model sees (drained at the iteration boundary).
func TestSteeringInjectedIntoConversation(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &mockBackend{}
	provider := &recordingLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	steer := make(chan string, 1)
	steer <- "actually use SQLite, not MySQL"
	ctx := WithSteering(context.Background(), steer)

	if _, _, err := agent.RunConversation(ctx, "u", "cli", "build a login feature"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider received no requests")
	}
	found := false
	for _, m := range provider.requests[0].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "SQLite") {
			found = true
		}
	}
	if !found {
		t.Fatalf("steered guidance was not injected into the conversation: %+v", provider.requests[0].Messages)
	}
}
