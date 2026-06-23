package kernel

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"selfmind/internal/kernel/llm"
)

type contextEngineProvider struct {
	calls atomic.Int32
}

func (p *contextEngineProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls.Add(1)
	return "## Active Task\nsummary", nil
}

func (p *contextEngineProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "summary"}, nil
}

func (p *contextEngineProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func TestBoundedHistoryMessagesKeepsRecentSessionsInChronologicalOrder(t *testing.T) {
	newBlob := func(prefix string, count int) []byte {
		msgs := make([]llm.Message, 0, count)
		for i := 0; i < count; i++ {
			msgs = append(msgs, llm.Message{Role: "user", Content: prefix + string(rune('0'+i))})
		}
		blob, err := json.Marshal(struct {
			Messages []llm.Message `json:"messages"`
		}{Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}

	got := boundedHistoryMessages([][]byte{
		newBlob("new-", 10),
		newBlob("old-", 3),
		newBlob("ignored-", 3),
	})

	if len(got) != 11 {
		t.Fatalf("expected 11 messages, got %d", len(got))
	}
	if got[0].Content != "old-0" {
		t.Fatalf("expected older selected session first, got %q", got[0].Content)
	}
	if got[len(got)-1].Content != "new-9" {
		t.Fatalf("expected latest selected message last, got %q", got[len(got)-1].Content)
	}
	if got[3].Content != "new-2" {
		t.Fatalf("expected newest session to be trimmed to last 8 messages, got %q", got[3].Content)
	}
}

func TestTruncateMessagesDoesNotSummarizeOnHotPathByDefault(t *testing.T) {
	t.Setenv("SELFMIND_SYNC_CONTEXT_SUMMARY", "")

	provider := &contextEngineProvider{}
	engine := NewContextEngine(120, 10)
	engine.SetProvider(provider)

	messages := []llm.Message{{Role: "system", Content: "system"}}
	for i := 0; i < 20; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: "long message with enough repeated words to exceed the small context window"})
	}

	got := engine.TruncateMessages(messages)
	if provider.calls.Load() != 0 {
		t.Fatalf("expected no synchronous summarization calls, got %d", provider.calls.Load())
	}
	if len(got) >= len(messages) {
		t.Fatalf("expected deterministic trimming, got %d messages from %d", len(got), len(messages))
	}
	if got[0].Role != "system" {
		t.Fatalf("expected system prompt to be preserved, got role %q", got[0].Role)
	}
}
