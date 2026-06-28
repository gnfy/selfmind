package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

func TestDrainSteering(t *testing.T) {
	if got := drainSteering(nil); got != nil {
		t.Fatalf("nil channel should drain to nil, got %v", got)
	}
	ch := make(chan string, 4)
	ch <- "first"
	ch <- "  "      // whitespace dropped
	ch <- "second"
	got := drainSteering(ch)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("drain = %v, want [first second]", got)
	}
	// Drained channel yields nothing further.
	if got := drainSteering(ch); got != nil {
		t.Fatalf("second drain should be empty, got %v", got)
	}
}

func TestSteeringContextRoundTrip(t *testing.T) {
	ch := make(chan string, 1)
	ctx := WithSteering(context.Background(), ch)
	if steeringFromContext(ctx) == nil {
		t.Fatal("steering channel should be retrievable from ctx")
	}
	// nil channel is a no-op (no steering attached).
	if steeringFromContext(WithSteering(context.Background(), nil)) != nil {
		t.Fatal("nil steering channel must not be attached")
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
