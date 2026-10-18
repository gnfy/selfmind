package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestAcceptedPlanReceiptAndCursorShareCurrentRunState(t *testing.T) {
	ctx := WithRunExecutionState(WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "current"}), NewRunExecutionState())
	good := `{"action":"resume","status":"committed","commit_mode":"direct","run_id":"current","plan_version":3,"plan":[{"step_id":"first","step":"checked","status":"completed"},{"step_id":"next","step":"report","status":"in_progress"}]}`
	open, ok := acceptRunPlanReceipt(ctx, llm.ToolCall{Function: "work_select"}, toolExecutionResult{success: true, rawResult: good})
	v, step := CurrentRunExecutionPlan(ctx)
	if !ok || len(open) != 1 || open[0] != "report" || v != 3 || step != "next" {
		t.Fatalf("completion view/cursor differ: %v %v %d %s", open, ok, v, step)
	}
	for _, raw := range []string{
		`{"action":"observe","status":"committed","commit_mode":"direct","run_id":"current","plan_version":4,"plan":[]}`,
		`{"action":"resume","status":"committed","commit_mode":"transfer","run_id":"current","plan_version":4,"plan":[]}`,
		`{"action":"resume","status":"committed","commit_mode":"direct","run_id":"source","execution_run_id":"other","plan_version":4,"plan":[]}`,
		`{"action":"resume","status":"committed","commit_mode":"direct","run_id":"current","plan_version":2,"plan":[]}`,
	} {
		if _, ok := acceptRunPlanReceipt(ctx, llm.ToolCall{Function: "work_select"}, toolExecutionResult{success: true, rawResult: raw}); ok {
			t.Fatal("foreign/historical/transfer receipt replaced current Plan")
		}
	}
	if _, ok := acceptRunPlanReceipt(ctx, llm.ToolCall{Function: "update_plan"}, toolExecutionResult{rawResult: good}); ok {
		t.Fatal("failed update advanced the cursor")
	}
	if v, step := CurrentRunExecutionPlan(ctx); v != 3 || step != "next" {
		t.Fatal("refusal changed execution attribution")
	}
}

func TestPlanCursorSeedsFromSelectedExactRunOnce(t *testing.T) {
	for _, sameRun := range []bool{true, false} {
		ctx := WithRunExecutionState(WithToolInvocationScope(context.Background(), ToolInvocationScope{RunID: "current"}), NewRunExecutionState())
		selected := TaskRuntimeContext{RunID: "other", PlanVersion: 2, Plan: []PlanItem{{StepID: "selected", Status: "in_progress"}}}
		if sameRun {
			selected.RunID = "current"
		}
		ctx = WithTaskRuntimeContext(ctx, selected)
		seedSelectedPlanCursor(ctx)
		v, step := CurrentRunExecutionPlan(ctx)
		if sameRun && (v != 2 || step != "selected") || !sameRun && v != 0 {
			t.Fatalf("selected cursor: %d %s", v, step)
		}
		UpdateRunExecutionPlan(ctx, 4, "newer")
		seedSelectedPlanCursor(ctx)
		if v, step := CurrentRunExecutionPlan(ctx); v != 4 || step != "newer" {
			t.Fatal("startup snapshot regressed a later receipt")
		}
	}
}
