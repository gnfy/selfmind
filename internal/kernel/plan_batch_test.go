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
		{"active step changed", strings.Replace(before, "step-a", "step-b", 1), false, false},
		{"verification requirement changed", strings.Replace(before, `"verification_required":true`, `"verification_required":false`, 1), false, false},
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

// Regressions for initial boundaries, failed siblings and later Plan updates.

type failingSiblingBackend struct{ planBatchBackend }

func (b *failingSiblingBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "read_file" {
		if p, _ := args["path"].(string); p == "bad.txt" {
			b.actions++
			return "", fmt.Errorf("injected sibling failure")
		}
	}
	return b.planBatchBackend.Dispatch(name, args)
}

// An initial Plan (no accepted boundary in the conversation yet) must hold every sibling, even when
// the receipt is readable: the identities were never read by the model.
func TestPlanBatchHoldsSiblingsWhenNoBoundaryWasAcceptedBefore(t *testing.T) {
	accounted := false
	backend := &planBatchBackend{receipt: batchPlanReceipt("unit-a", "original", "Inspect sources", "Sources agree"), accounted: &accounted}
	agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
	ctx := withToolActivationState(context.Background())
	calls := []llm.ToolCall{{ID: "plan", Function: "update_plan", Args: `{"plan":[]}`}, {ID: "read", Function: "read_file", Args: `{"path":"a.txt"}`}}
	results := agent.executePlanAwareToolCalls(ctx, "person", nil, calls, nil, func(c []llm.ToolCall, r []toolExecutionResult) {
		if c[0].Function == "update_plan" && r[0].success {
			accounted = true
		}
	})
	if results[1].success || backend.actions != 0 {
		t.Fatalf("initial Plan did not hold its sibling: actions=%d results=%+v", backend.actions, results)
	}
}

// After a sibling fails, later siblings must not run under the same accepted boundary.
func TestPlanBatchStopsLaterSiblingsAfterAFailedSibling(t *testing.T) {
	before := batchPlanReceipt("unit-a", "original", "Inspect sources", "Sources agree")
	accounted := false
	backend := &failingSiblingBackend{planBatchBackend{receipt: before, accounted: &accounted}}
	agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
	ctx := withToolActivationState(context.Background())
	calls := []llm.ToolCall{
		{ID: "plan", Function: "update_plan", Args: `{"plan":[]}`},
		{ID: "bad", Function: "read_file", Args: `{"path":"bad.txt"}`},
		{ID: "good", Function: "read_file", Args: `{"path":"a.txt"}`},
	}
	messages := []llm.Message{{Role: "tool", Name: "update_plan", Content: before}}
	results := agent.executePlanAwareToolCalls(ctx, "person", nil, calls, messages, func(c []llm.ToolCall, r []toolExecutionResult) {
		if c[0].Function == "update_plan" && r[0].success {
			accounted = true
		}
	})
	if backend.actions != 1 || results[2].success {
		t.Fatalf("a call ran after a failed sibling: actions=%d results=%+v", backend.actions, results)
	}
}

type twoPlansBackend struct {
	planBatchBackend
	receipts []string
	n        int
}

func (b *twoPlansBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	if name == "update_plan" {
		r := b.receipts[b.n]
		if b.n < len(b.receipts)-1 {
			b.n++
		}
		return r, nil
	}
	return b.planBatchBackend.Dispatch(name, args)
}

// Two Plan updates in one response: the first leaves the boundary unchanged, the second changes it.
// A sibling after the second update must be held, because the accepted boundary it was issued under changed.
func TestPlanBatchSecondPlanUpdateChangesTheBoundaryBeforeALaterSibling(t *testing.T) {
	before := batchPlanReceipt("unit-a", "original", "Inspect sources", "Sources agree")
	changed := batchPlanReceipt("unit-b", "new target", "Inspect destination", "Destination agrees")
	accounted := false
	backend := &twoPlansBackend{planBatchBackend: planBatchBackend{receipt: before, accounted: &accounted}, receipts: []string{before, changed}}
	agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
	ctx := withToolActivationState(context.Background())
	calls := []llm.ToolCall{
		{ID: "p1", Function: "update_plan", Args: `{"plan":[]}`},
		{ID: "p2", Function: "update_plan", Args: `{"plan":[]}`},
		{ID: "read", Function: "read_file", Args: `{"path":"a.txt"}`},
	}
	messages := []llm.Message{{Role: "tool", Name: "update_plan", Content: before}}
	results := agent.executePlanAwareToolCalls(ctx, "person", nil, calls, messages, func(c []llm.ToolCall, r []toolExecutionResult) {
		if c[0].Function == "update_plan" && r[0].success {
			accounted = true
		}
	})
	t.Logf("update_plan #2 success=%v; read_file success=%v executed=%d", results[1].success, results[2].success, backend.actions)
	if backend.actions != 0 {
		t.Fatalf("a sibling ran after a SECOND Plan update changed the boundary: actions=%d", backend.actions)
	}
}

func TestPlanBatchSecondFinalSnapshotCanStillFinish(t *testing.T) {
	before := batchPlanReceipt("unit-a", "original", "Inspect sources", "Sources agree")
	final := `{"plan":[{"step_id":"step-a","work_unit_id":"unit-a","step":"Inspect sources","status":"completed"}],"work_units":[{"id":"unit-a","sequence":1,"goal":"original","plan_status":"completed"}]}`
	accounted := false
	backend := &twoPlansBackend{planBatchBackend: planBatchBackend{receipt: before, accounted: &accounted}, receipts: []string{before, final}}
	agent := NewAgent(memory.NewMemoryManager(nil), backend, &boundaryProvider{}, "test", 4, 1, nil)
	calls := []llm.ToolCall{{ID: "progress", Function: "update_plan", Args: `{}`}, {ID: "final", Function: "update_plan", Args: `{}`}, {ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"checked"}`}}
	results := agent.executePlanAwareToolCalls(withToolActivationState(context.Background()), "person", nil, calls, []llm.Message{{Role: "tool", Name: "update_plan", Content: before}}, func(_ []llm.ToolCall, _ []toolExecutionResult) { accounted = true })
	if len(results) != 3 || !results[2].success || results[2].heldBeforeDispatch {
		t.Fatalf("accepted final snapshot blocked finish: %+v", results)
	}
}
