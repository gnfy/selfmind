package control

import (
	"context"
	"testing"
)

func TestRunPlanDerivesWorkUnitFromStableStep(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "two objectives", []RunPlanStepInput{
		{Step: "prepare report", Status: "completed"},
		{Step: "inspect data", Status: "in_progress", WorkUnit: true},
		{Step: "write findings", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "progress", []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Step: "prepare report", Status: "completed"},
		{StepID: first.Plan.Steps[1].StepID, Step: "inspect corrected data", Status: "completed"},
		{StepID: first.Plan.Steps[2].StepID, Step: "write findings", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.WorkUnits) != 2 || second.Plan.Steps[1].WorkUnitID != first.Plan.Steps[1].WorkUnitID || !second.Plan.Steps[1].WorkUnit {
		t.Fatalf("step update lost execution attribution: %+v", second)
	}
}

func TestRunPlanAcceptsRepeatedMembershipWithoutCreatingBoundary(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "one objective", []RunPlanStepInput{
		{Step: "inspect", Status: "completed"}, {Step: "update", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unit := first.Plan.Steps[0].WorkUnitID
	second, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "complete", []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Step: "inspect", Status: "completed", WorkUnitID: unit},
		{StepID: first.Plan.Steps[1].StepID, Step: "update", Status: "completed", WorkUnitID: unit},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.WorkUnits) != 1 {
		t.Fatalf("membership created another unit: %+v", second.WorkUnits)
	}
}
