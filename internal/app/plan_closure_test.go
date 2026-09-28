package app

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

// A run closes its plan and records its outcome in one response: the final
// update_plan snapshot, then finish_run. finish_run used to be deferred behind
// the plan update like work in a new unit, which cost one more model call at
// the end of every planned run.
func TestFinalPlanAndFinishInOneResponseCloseTheRun(t *testing.T) {
	provider := &scriptedStreamProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "plan-1", Function: "update_plan",
			Args: `{"plan":[{"step":"publish","status":"completed"},{"step":"announce","status":"in_progress"}]}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{
			{ID: "plan-2", Function: "update_plan", Args: `{"plan":[{"step":"publish","status":"completed"},{"step":"announce","status":"completed"}]}`},
			{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"Released and announced."}`},
		}},
		{Content: "Released and announced.", FinishReason: "stop"},
	}}
	plans := tools.NewPlanStore()
	disp := tools.NewDispatcher()
	disp.RegisterTool(tools.NewUpdatePlanToolWithStore(plans))
	disp.RegisterTool(tools.NewFinishRunToolWithStore(plans))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), disp, provider, "helpful", 8, 1, nil)
	events := make(chan string, 256)
	answer, _, err := agent.RunConversation(kernel.WithEventChannel(context.Background(), events), "person", "cli", "publish and announce")
	if err != nil || !strings.Contains(answer, "Released and announced.") {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	finished := false
	for len(events) > 0 {
		event, ok := kernel.DecodeAgentEvent(<-events)
		if ok && event.Type == "tool.completed" && event.ToolCallID == "finish" {
			finished = event.Error == ""
		}
	}
	if !finished {
		t.Fatal("finish_run after the final plan update did not record the outcome")
	}
	if got := len(provider.recorded()); got != 3 {
		t.Fatalf("model calls = %d, want 3: closing the plan and the run is one response", got)
	}
}
