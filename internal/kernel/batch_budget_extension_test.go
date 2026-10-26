package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestBatchUsesEarnedBudgetBeforeRefusingItsSibling(t *testing.T) {
	p := &boundaryProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "a", Function: "read_file", Args: `{"path":"one"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "b", Function: "read_file", Args: `{"path":"two"}`}, {ID: "c", Function: "read_file", Args: `{"path":"three"}`}}},
		{Content: "Inspected all three sources.", FinishReason: "stop"},
	}}
	b := &validBudgetProbeBackend{}
	a := NewAgent(memory.NewMemoryManager(&mockStorage{}), b, p, "helpful", 6, 1, nil)
	s := DefaultTaskStrategy()
	s.MaxActionTools, s.ActionToolBudgetStep, s.ActionToolBudgetLimit, s.MaxBudgetExtensions = 2, 2, 4, 1
	events := make(chan string, 256)
	_, _, err := a.RunConversation(WithEventChannel(WithTaskStrategy(context.Background(), s), events), "person", "cli", "inspect the sources")
	if err != nil || b.reads != 3 {
		t.Fatalf("reads=%d err=%v; prior real progress should fund the whole bounded batch", b.reads, err)
	}
	close(events)
	for raw := range events {
		if e, ok := DecodeAgentEvent(raw); ok && e.Type == "tool.completed" && e.Payload["error_code"] == "tool_budget_exhausted" {
			t.Fatal("a soft budget boundary caused an unnecessary rejected call")
		}
	}
}

func TestSkillBindingDoesNotConsumeActionToolBudget(t *testing.T) {
	if countsAgainstActionBudget("skill_select") || countsAgainstActionBudget("skill_fallback") {
		t.Fatal("skill binding tools must not consume action tool budget")
	}
	if lifecycleToolCap("skill_select") != 4 || lifecycleToolCap("skill_fallback") != 2 {
		t.Fatalf("unexpected caps: skill_select=%d skill_fallback=%d", lifecycleToolCap("skill_select"), lifecycleToolCap("skill_fallback"))
	}
	s := DefaultTaskStrategy()
	s.MaxActionTools = 1
	calls := []llm.ToolCall{
		{ID: "1", Function: "skill_select", Args: `{"name":"s"}`},
		{ID: "2", Function: "read_file", Args: `{"path":"a"}`},
	}
	admitted, dropped := filterToolCallsByStrategyAndBudget(calls, s, 0)
	if dropped != 0 || len(admitted) != 2 {
		t.Fatalf("admitted=%d dropped=%d; skill_select should not displace the action tool", len(admitted), dropped)
	}
	if got := countActionToolCalls(admitted); got != 1 {
		t.Fatalf("countActionToolCalls = %d, want 1", got)
	}
}
