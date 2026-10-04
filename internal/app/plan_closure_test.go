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

// The answer a model writes in the same response as finish_run stays the run's
// answer when the model then closes with one line. qwen wrote a whole review
// that way; the final answer, the work history, and IM kept only the closing
// line. Narration before an action tool is still not the answer.
func TestAnswerWrittenWithFinishRunStaysTheAnswer(t *testing.T) {
	readFile := llm.ToolCall{ID: "read", Function: "read_file", Args: `{"path":"main.go"}`}
	planDoing := llm.ToolCall{ID: "plan-1", Function: "update_plan", Args: `{"plan":[{"step":"review","status":"in_progress"}]}`}
	planDone := llm.ToolCall{ID: "plan-2", Function: "update_plan", Args: `{"plan":[{"step":"review","status":"completed"}]}`}
	finish := llm.ToolCall{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"Reviewed."}`}
	for _, tc := range []struct {
		name      string
		responses []llm.ChatResponse
		want      string
	}{
		{"answer with finish_run, then a closing line", []llm.ChatResponse{
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{readFile}},
			{Content: "## Review\nOne high finding.", FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{finish}},
			{Content: "Review closed; conclusion above.", FinishReason: "stop"},
		}, "## Review\nOne high finding.\n\nReview closed; conclusion above."},
		{"answer with the final plan and finish_run", []llm.ChatResponse{
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDoing}},
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{readFile}},
			{Content: "Reviewed: no findings.", FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDone, finish}},
			{Content: "Done.", FinishReason: "stop"},
		}, "Reviewed: no findings.\n\nDone."},
		{"narration before an action tool is not the answer", []llm.ChatResponse{
			{Content: "Setting the plan first.", FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDoing}},
			{Content: "Reading the file.", FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{readFile}},
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDone}},
			{Content: "The file is fine.", FinishReason: "stop"},
		}, "The file is fine."},
		{"prose kept by a bookkeeping call survives the plan gate", []llm.ChatResponse{
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{readFile}},
			{Content: "## Review\nOne finding.", FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDoing}},
			{Content: "Closed.", FinishReason: "stop"},
			{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{planDone}},
			{Content: "Closed after reconciling the plan.", FinishReason: "stop"},
		}, "## Review\nOne finding.\n\nClosed after reconciling the plan."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := tools.NewPlanStore()
			disp := tools.NewDispatcher()
			disp.RegisterTool(tools.NewUpdatePlanToolWithStore(plans))
			disp.RegisterTool(tools.NewFinishRunToolWithStore(plans))
			disp.RegisterTool(&recordingTool{name: "read_file", param: "path"})
			agent := kernel.NewAgent(memory.NewMemoryManager(nil), disp, &scriptedStreamProvider{responses: tc.responses}, "helpful", 8, 1, nil)
			events := make(chan string, 256)
			answer, _, err := agent.RunConversation(kernel.WithEventChannel(context.Background(), events), "person", "cli", "review the change")
			if err != nil {
				t.Fatal(err)
			}
			completed := ""
			for len(events) > 0 {
				if event, ok := kernel.DecodeAgentEvent(<-events); ok && event.Type == "turn.completed" {
					completed = event.Content
				}
			}
			if answer != tc.want || completed != tc.want {
				t.Fatalf("answer=%q turn.completed=%q, want %q", answer, completed, tc.want)
			}
		})
	}
}
