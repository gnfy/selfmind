package tools

import (
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
}
