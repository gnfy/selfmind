package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// cacheUsageProvider fakes an Anthropic-style stream: message_start usage with
// prompt-cache accounting, then text, then the output-token delta.
type cacheUsageProvider struct{}

func (p *cacheUsageProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *cacheUsageProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *cacheUsageProvider) StreamChat(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Usage: &llm.UsageStats{InputTokens: 100, CacheReadInputTokens: 80, CacheCreationInputTokens: 5}}
	ch <- llm.StreamEvent{Content: "done"}
	ch <- llm.StreamEvent{Usage: &llm.UsageStats{OutputTokens: 7}}
	close(ch)
	return ch, nil
}

// TestTokenUpdatedEventCarriesCacheAccounting pins the token.updated payload
// contract: when the provider reports prompt-cache usage, the event exposes
// cache_read_input_tokens, cache_creation_input_tokens, and
// billed_input_tokens (= input - cache_read) alongside the existing totals.
func TestTokenUpdatedEventCarriesCacheAccounting(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	agent := NewAgent(mem, &mockBackend{}, &cacheUsageProvider{}, "helpful", 1, 1, nil)

	events := make(chan string, 64)
	ctx := WithEventChannel(context.Background(), events)
	if _, usage, err := agent.RunConversation(ctx, "user123", "cli", "hello"); err != nil {
		t.Fatal(err)
	} else if usage.CacheReadInputTokens != 80 || usage.CacheCreationInputTokens != 5 {
		t.Fatalf("returned usage = %+v", usage)
	}
	close(events)

	var last AgentEvent
	seen := false
	for raw := range events {
		if event, ok := DecodeAgentEvent(raw); ok && event.Type == "token.updated" {
			last = event
			seen = true
		}
	}
	if !seen {
		t.Fatal("no token.updated event emitted")
	}
	want := map[string]float64{
		"input_tokens":                100,
		"output_tokens":               7,
		"cache_read_input_tokens":     80,
		"cache_creation_input_tokens": 5,
		"billed_input_tokens":         20,
	}
	for key, value := range want {
		got, ok := last.Payload[key].(float64)
		if !ok || got != value {
			t.Fatalf("payload[%q] = %v, want %v (payload %+v)", key, last.Payload[key], value, last.Payload)
		}
	}
}
