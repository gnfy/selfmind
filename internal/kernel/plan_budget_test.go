package kernel

import (
	"context"
	"fmt"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type validBudgetProbeBackend struct {
	budgetClosureBackend
	reads, finishes int
	failBad         bool
}

func (b *validBudgetProbeBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "update_plan":
		if done, _ := args["done"].(bool); done {
			return `{"plan":[{"step_id":"step-a","work_unit_id":"unit-a","step":"Inspect sources","status":"completed"}],"work_units":[{"id":"unit-a","sequence":1,"goal":"original","plan_status":"completed"}]}`, nil
		}
		goal := "original"
		if changed, _ := args["changed"].(bool); changed {
			goal = "changed goal"
		}
		return batchPlanReceipt("unit-a", goal, "Inspect sources", "Sources agree"), nil
	case "read_file":
		b.reads++
		if b.failBad && args["path"] == "bad.txt" {
			return "", fmt.Errorf("injected execution failure")
		}
	case "finish_run":
		b.finishes++
	}
	return b.budgetClosureBackend.Dispatch(name, args)
}
func TestHeldPlanActionDoesNotConsumeBudget(t *testing.T) {
	p := &boundaryProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: `{}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p2", Function: "update_plan", Args: `{"changed":true}`}, {ID: "r1", Function: "read_file", Args: `{"path":"a.txt"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "r2", Function: "read_file", Args: `{"path":"a.txt"}`}}},
	}}
	b := &validBudgetProbeBackend{}
	a := NewAgent(memory.NewMemoryManager(&mockStorage{}), b, p, "test", 8, 1, nil)
	s := DefaultTaskStrategy()
	s.MaxActionTools = 1
	s.ActionToolBudgetLimit = 1
	s.ActionToolBudgetStep = 1
	_, _, err := a.RunConversation(WithTaskStrategy(context.Background(), s), "person", "cli", "inspect sources")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("valid accepted receipts: read dispatches=%d", b.reads)
	if b.reads != 1 {
		t.Fatalf("held call consumed budget; reads=%d want 1", b.reads)
	}
}
func TestHeldPlanFinishDoesNotConsumeAttempt(t *testing.T) {
	p := &boundaryProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: `{}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p2", Function: "update_plan", Args: `{}`}, {ID: "bad", Function: "read_file", Args: `{"path":"bad.txt"}`}, {ID: "f1", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "good", Function: "read_file", Args: `{"path":"a.txt"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p3", Function: "update_plan", Args: `{"done":true}`}, {ID: "f2", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}},
		{Content: "Finished", FinishReason: "stop"},
	}}
	b := &validBudgetProbeBackend{failBad: true}
	a := NewAgent(memory.NewMemoryManager(&mockStorage{}), b, p, "test", 8, 1, nil)
	_, _, err := a.RunConversation(context.Background(), "person", "cli", "inspect sources")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read attempts=%d finish dispatches=%d", b.reads, b.finishes)
	if b.finishes != 1 {
		t.Fatalf("an unexecuted finish exhausted lifecycle cap; finishes=%d want 1", b.finishes)
	}
}

func TestHeldPlanReservationsDoNotRefundExecutionFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result toolExecutionResult
		want   int
	}{
		{"held action", toolExecutionResult{heldBeforeDispatch: true}, 1},
		{"failed attempt", toolExecutionResult{}, 0},
		{"unknown effect", toolExecutionResult{effectState: "unknown"}, 0},
		{"backend error cannot impersonate hold", toolExecutionResult{errorCode: "plan_boundary_held", failurePhase: "preparation", effectState: "not_dispatched"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts := map[string]int{"read_file": 1}
			got := releaseHeldToolReservations([]llm.ToolCall{{Function: "read_file"}}, []toolExecutionResult{tc.result}, counts)
			if got != tc.want || counts["read_file"] != 1-tc.want {
				t.Fatalf("released=%d counts=%v", got, counts)
			}
		})
	}
}
func TestHeldFinishDoesNotChangePendingCompletionCorrection(t *testing.T) {
	correction := finishCorrection{pending: true}
	counts := map[string]int{"finish_run": 1}
	correction.observe([]toolExecutionResult{{toolName: "finish_run", heldBeforeDispatch: true}}, counts)
	if !correction.pending || correction.argumentRetryUsed || correction.used || counts["finish_run"] != 1 {
		t.Fatalf("held result changed correction: %+v counts=%v", correction, counts)
	}
	correction.observe([]toolExecutionResult{{toolName: "update_plan", success: true, rawResult: `{"changed":true}`}}, counts)
	if !correction.used || counts["finish_run"] != 0 {
		t.Fatalf("real correction was lost: %+v counts=%v", correction, counts)
	}
}
func TestFailedPlanActionStillExhaustsItsBudget(t *testing.T) {
	provider := &boundaryProvider{responses: []llm.ChatResponse{
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "p1", Function: "update_plan", Args: `{}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "bad", Function: "read_file", Args: `{"path":"bad.txt"}`}}},
		{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: "next", Function: "read_file", Args: `{"path":"a.txt"}`}}},
	}}
	backend := &validBudgetProbeBackend{failBad: true}
	agent := NewAgent(memory.NewMemoryManager(&mockStorage{}), backend, provider, "test", 8, 1, nil)
	strategy := DefaultTaskStrategy()
	strategy.MaxActionTools = 1
	strategy.ActionToolBudgetLimit = 1
	strategy.ActionToolBudgetStep = 1
	if _, _, err := agent.RunConversation(WithTaskStrategy(context.Background(), strategy), "person", "cli", "inspect sources"); err != nil {
		t.Fatal(err)
	}
	if backend.reads != 1 {
		t.Fatalf("failed attempted action was refunded; reads=%d want 1", backend.reads)
	}
}
