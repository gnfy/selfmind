package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type semanticRecallCounterProvider struct {
	calls int
}

func (p *semanticRecallCounterProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls++
	return "related alternate", nil
}

func (p *semanticRecallCounterProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{}, nil
}

func (p *semanticRecallCounterProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func TestSelectRuntimeContextDoesNotRepeatSelectorRecall(t *testing.T) {
	provider := &semanticRecallCounterProvider{}
	agent := &Agent{
		memory:           memory.NewMemoryManager(nil),
		semanticExpander: memory.NewSemanticExpander(provider, true),
	}
	ctx := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{
		TaskID: "task_selected_by_daemon",
		Title:  "Existing work",
	})

	selected := agent.selectRuntimeContext(ctx, "tenant", "weixin", "continue existing work", DefaultTaskStrategy(), nil)
	if provider.calls != 0 {
		t.Fatalf("semantic expansion calls = %d; daemon-selected context must not be recalled twice", provider.calls)
	}
	bundle, ok := RuntimeContextBundleFromContext(selected)
	if !ok || bundle.Task == nil || bundle.Task.TaskID != "task_selected_by_daemon" {
		t.Fatalf("selected bundle = %+v", bundle)
	}
}

func TestSelectRuntimeContextKeepsDirectAgentRecallFallback(t *testing.T) {
	provider := &semanticRecallCounterProvider{}
	agent := &Agent{
		memory:           memory.NewMemoryManager(nil),
		semanticExpander: memory.NewSemanticExpander(provider, true),
	}

	agent.selectRuntimeContext(context.Background(), "tenant", "direct", "find related work", DefaultTaskStrategy(), nil)
	if provider.calls != 1 {
		t.Fatalf("semantic expansion calls = %d; direct callers should retain the fallback", provider.calls)
	}
}
