package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type planBatchBackend struct {
	budgetClosureBackend
	receipt   string
	fail      bool
	accounted *bool
	actions   int
}

func (b *planBatchBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "update_plan" {
		if b.fail {
			return "", fmt.Errorf("injected persistence failure")
		}
		return b.receipt, nil
	}
	if name == "read_file" {
		if !*b.accounted {
			return "", fmt.Errorf("Plan not consumed before dispatch")
		}
		b.actions++
	}
	return b.budgetClosureBackend.Dispatch(name, args)
}

func batchPlanReceipt(unit, goal, step, criterion string) string {
	return fmt.Sprintf(`{"plan":[{"step_id":"step-a","work_unit_id":%q,"step":%q,"success_criteria":%q,"verification_required":true,"status":"in_progress"}],"work_units":[{"id":%q,"sequence":1,"goal":%q,"plan_status":"in_progress"}]}`, unit, step, criterion, unit, goal)
}

func TestPlanBatchUsesAcceptedIdentityAndConsumesBeforeActions(t *testing.T) {
	before := batchPlanReceipt("unit-a", "original", "Inspect sources", "Sources agree")
	for _, tc := range []struct {
		name, after   string
		fail, allowed bool
	}{
		{"progress", before, false, true},
		{"same ordinal different unit", batchPlanReceipt("unit-b", "original", "Inspect sources", "Sources agree"), false, false},
		{"changed goal", batchPlanReceipt("unit-a", "new target", "Inspect sources", "Sources agree"), false, false},
		{"changed criterion", batchPlanReceipt("unit-a", "original", "Inspect sources", "File exists"), false, false},
		{"different wording", batchPlanReceipt("unit-a", "original", "Inspect destination", "Sources agree"), false, false},
		{"unreadable receipt", "ok", false, false},
		{"persistence failed", before, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounted := false
			backend := &planBatchBackend{receipt: tc.after, fail: tc.fail, accounted: &accounted}
			agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
			ctx := withToolActivationState(context.Background())
			calls := []llm.ToolCall{{ID: "plan", Function: "update_plan", Args: `{"plan":[]}`}, {ID: "read", Function: "read_file", Args: `{"path":"a.txt"}`}}
			messages := []llm.Message{{Role: "tool", Name: "update_plan", Content: before}}
			results := agent.executePlanAwareToolCalls(ctx, "person", nil, calls, messages, func(c []llm.ToolCall, r []toolExecutionResult) {
				if c[0].Function == "update_plan" && r[0].success {
					accounted = true
				}
			})
			if len(results) != 2 || results[1].success != tc.allowed || (backend.actions == 1) != tc.allowed {
				t.Fatalf("actions=%d results=%+v", backend.actions, results)
			}
			if !tc.allowed && !strings.Contains(results[1].msg.Content, "not executed") {
				t.Fatalf("missing non-dispatch explanation: %s", results[1].msg.Content)
			}
		})
	}
}
