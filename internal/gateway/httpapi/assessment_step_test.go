package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

func TestFinishAssessmentSelectsCurrentStepWithoutEvidenceID(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	plan, err := store.LatestRunPlan(context.Background(), identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	ctx := tools.WithRunPlanProjection(context.Background(), projection)
	args := map[string]interface{}{"_context": ctx, "status": "done", "final_response": "Inspection complete: the condition is not met.", "assessments": []interface{}{map[string]interface{}{"step_id": plan.Steps[0].StepID, "result": "not_met", "reason": "The observed value differs from READY."}}}
	registry := tools.NewRegistry()
	registry.Register(tools.NewFinishRunTool())
	receipt, err := registry.DispatchResult("finish_run", args)
	output := receipt.Output
	if err != nil {
		t.Fatalf("runtime cannot bind Main's step judgment to the observed check: %v", err)
	}
	var accepted struct {
		Assessments []verification.Assessment `json:"assessments"`
	}
	if err := json.Unmarshal([]byte(output), &accepted); err != nil || len(accepted.Assessments) != 1 || accepted.Assessments[0].EvidenceRefs[0] != "check" {
		t.Fatalf("runtime did not preserve exact evidence: %s %v", output, err)
	}
}

func TestStepFindingCannotCompleteRequiredNegativeCondition(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, true, 0)
	plan, err := store.LatestRunPlan(context.Background(), identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	ctx := tools.WithRunPlanProjection(context.Background(), projection)
	registry := tools.NewRegistry()
	registry.Register(tools.NewFinishRunTool())
	_, err = registry.DispatchResult("finish_run", map[string]interface{}{
		"_context": ctx, "status": "done", "final_response": "The required condition is absent.",
		"assessments": []interface{}{map[string]interface{}{"step_id": plan.Steps[0].StepID, "result": "not_met", "reason": "A zero exit only confirms the observation, not the required condition."}},
	})
	if err == nil {
		t.Fatal("step selection converted Main's negative judgment into required acceptance")
	}
}
