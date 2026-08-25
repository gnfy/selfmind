package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestUpdatePlanToolRecordsStructuredPlan(t *testing.T) {
	tool := NewUpdatePlanTool()
	result, err := tool.Execute(map[string]interface{}{
		"explanation": "starting work",
		"plan": []interface{}{
			map[string]interface{}{"step": "Inspect current state", "status": "completed"},
			map[string]interface{}{"step": "Apply protocol changes", "status": "in_progress"},
			map[string]interface{}{"step": "Verify behavior", "status": "pending"},
		},
		"_tenant_id": "tenant-a",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var state PlanState
	if err := json.Unmarshal([]byte(result), &state); err != nil {
		t.Fatalf("invalid JSON result: %v", err)
	}
	if got := len(state.Plan); got != 3 {
		t.Fatalf("plan length = %d, want 3", got)
	}
	if state.Plan[1].Status != "in_progress" {
		t.Fatalf("second step status = %q", state.Plan[1].Status)
	}
}

func TestUpdatePlanToolReturnsSynchronousWorkUnitIdentities(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx := WithPlanProjectionSink(context.Background(), func(_ context.Context, steps []PlanStep) ([]PlanWorkUnitIdentity, error) {
		if len(steps) != 2 || steps[1].RelatedTaskID != "task-b" {
			t.Fatalf("sink received wrong plan: %+v", steps)
		}
		return []PlanWorkUnitIdentity{
			{ID: "wu-a", Sequence: 1, Goal: steps[0].Step, PlanStatus: steps[0].Status},
			{
				ID: "wu-b", Sequence: 2, Goal: steps[1].Step, PlanStatus: steps[1].Status, RelatedTaskID: "task-b",
				SkillCatalog: "## Skill Candidates for Current Work Unit\n- skref_123 inspect-build: Inspect build metadata.\n",
			},
		}, nil
	})
	result, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{
			map[string]interface{}{"step": "A", "status": "completed"},
			map[string]interface{}{"step": "B", "status": "in_progress", "related_task_id": "task-b"},
		},
		"_context": ctx,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"id":"wu-b"`) || !strings.Contains(result, `"related_task_id":"task-b"`) {
		t.Fatalf("stable work-unit identities missing from tool result: %s", result)
	}
	if !strings.Contains(result, `"skill_catalog":"`) || !strings.Contains(result, "skref_123") {
		t.Fatalf("new work-unit Skill refs missing from bounded catalogue: %s", result)
	}
	if strings.Contains(result, `"skill_candidates"`) {
		t.Fatalf("tool result duplicated unbounded candidate descriptions: %s", result)
	}
}

type staleWorkUnitTestError struct{}

func (staleWorkUnitTestError) Error() string {
	return "work unit wu-old does not belong to run run-current"
}
func (staleWorkUnitTestError) CurrentWorkUnitIDs() []string { return []string{"wu-current"} }

func TestUpdatePlanToolReturnsStableStaleWorkUnitRecovery(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) {
		return nil, staleWorkUnitTestError{}
	})
	_, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{
			"step": "Continue current objective", "status": "in_progress", "work_unit_id": "wu-old",
		}},
		"_context": ctx,
	})
	if err == nil {
		t.Fatal("expected stale work-unit error")
	}
	stable, ok := err.(interface {
		ToolErrorCode() string
		ModelSafeMessage() string
	})
	if !ok || stable.ToolErrorCode() != "stale_work_unit" || !strings.Contains(stable.ModelSafeMessage(), "wu-current") {
		t.Fatalf("stale work-unit recovery = %T %v", err, err)
	}
}

func TestUpdatePlanToolRejectsMultipleInProgressSteps(t *testing.T) {
	tool := NewUpdatePlanTool()
	_, err := tool.Execute(map[string]interface{}{
		"plan": []interface{}{
			map[string]interface{}{"step": "A", "status": "in_progress"},
			map[string]interface{}{"step": "B", "status": "in_progress"},
		},
	})
	if err == nil {
		t.Fatal("expected error for multiple in_progress steps")
	}
}

func TestFinishRunToolNormalizesApprovalStatus(t *testing.T) {
	tool := NewFinishRunTool()
	result, err := tool.Execute(map[string]interface{}{
		"status":       "needs_approval",
		"summary":      "Need confirmation before continuing.",
		"next_steps":   []interface{}{"Approve the proposed action."},
		"need_approve": false,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("invalid JSON result: %v", err)
	}
	if out["status"] != "blocked" {
		t.Fatalf("status = %q, want blocked", out["status"])
	}
	if out["need_approve"] != true {
		t.Fatalf("need_approve = %v, want true", out["need_approve"])
	}
}

func TestFinishRunToolPreservesResolvedBlockerIDs(t *testing.T) {
	tool := NewFinishRunTool()
	result, err := tool.Execute(map[string]interface{}{
		"status": "done", "summary": "verified",
		"resolved_blocker_ids": []interface{}{"blocker_one", "blocker_two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"resolved_blocker_ids":["blocker_one","blocker_two"]`) {
		t.Fatalf("result=%s", result)
	}
}

func TestFinishRunToolEmitsCanonicalCompletionReason(t *testing.T) {
	tool := NewFinishRunTool()
	for status, want := range map[string]string{
		"done":             "completed",
		"waiting_external": "waiting_external",
		"waiting_user":     "waiting_user",
		"blocked":          "blocked",
		"failed":           "failed",
	} {
		t.Run(status, func(t *testing.T) {
			result, err := tool.Execute(map[string]interface{}{
				"status": status, "summary": "recorded",
			})
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(result), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["completion_reason"] != want {
				t.Fatalf("completion_reason=%v, want %q", payload["completion_reason"], want)
			}
		})
	}
}

func TestFinishRunRequiresResolvedSharedPlan(t *testing.T) {
	store := NewPlanStore()
	plan := NewUpdatePlanToolWithStore(store)
	finish := NewFinishRunToolWithStore(store)
	base := map[string]interface{}{"_tenant_id": "tenant-plan-finalization"}

	_, err := plan.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"plan": []interface{}{
			map[string]interface{}{"step": "Implement change", "status": "completed"},
			map[string]interface{}{"step": "Run verification", "status": "in_progress"},
		},
	})
	if err != nil {
		t.Fatalf("initial plan update failed: %v", err)
	}

	_, err = finish.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"status":     "done",
		"summary":    "Completed the work.",
	})
	if err == nil || !strings.Contains(err.Error(), "unresolved plan steps") {
		t.Fatalf("finish_run should reject an unresolved plan, got: %v", err)
	}

	_, err = plan.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"plan": []interface{}{
			map[string]interface{}{"step": "Implement change", "status": "completed"},
			map[string]interface{}{"step": "Run verification", "status": "completed"},
		},
	})
	if err != nil {
		t.Fatalf("final plan update failed: %v", err)
	}
	if _, ok := store.Get("tenant-plan-finalization"); ok {
		t.Fatal("resolved plan should be released from the runtime guard store")
	}
	if _, err := finish.Execute(map[string]interface{}{
		"_tenant_id": base["_tenant_id"],
		"status":     "done",
		"summary":    "Completed and verified the work.",
	}); err != nil {
		t.Fatalf("finish_run rejected a resolved plan: %v", err)
	}
	if _, ok := store.last["tenant-plan-finalization"]; ok {
		t.Fatal("finish_run should purge the final plan deduplication snapshot")
	}
}

func TestPlanToolSuppressesIdenticalVisibleSnapshots(t *testing.T) {
	store := NewPlanStore()
	plan := NewUpdatePlanToolWithStore(store)
	args := map[string]interface{}{
		"_tenant_id":  "tenant-plan-dedupe",
		"explanation": "first narration",
		"plan": []interface{}{
			map[string]interface{}{"step": "Inspect", "status": "in_progress"},
			map[string]interface{}{"step": "Verify", "status": "pending"},
		},
	}

	first, err := plan.Execute(args)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	var firstResult struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(first), &firstResult); err != nil {
		t.Fatalf("decode first result: %v", err)
	}
	if !firstResult.Changed {
		t.Fatal("first snapshot must be visible")
	}

	args["explanation"] = "different narration"
	second, err := plan.Execute(args)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	var secondResult struct {
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(second), &secondResult); err != nil {
		t.Fatalf("decode second result: %v", err)
	}
	if secondResult.Changed {
		t.Fatal("identical steps and statuses must not repaint the plan")
	}
}
