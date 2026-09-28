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

// A snapshot that rewords its steps and omits their ids keeps each open step's
// id by position. Every reworded step used to become a new step, often born
// completed, and the unfinished steps it replaced vanished from the plan.
func TestRewordedPlanKeepsOpenStepsByPosition(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Inspect the release pipeline", Status: "completed"},
		{Step: "Update the deploy script", Status: "in_progress"},
		{Step: "Verify the rollout", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.Plan.Steps[0].StepID, first.Plan.Steps[1].StepID, first.Plan.Steps[2].StepID}

	reworded, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Check the release pipeline", Status: "completed"},
		{Step: "Point the deploy script at the new region", Status: "completed"},
		{Step: "Confirm the rollout is healthy", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := reworded.Plan.Steps
	if got[1].StepID != ids[1] || got[2].StepID != ids[2] || got[1].Step != "Point the deploy script at the new region" {
		t.Fatalf("reworded open steps lost their identity: %+v (ids %v)", got, ids)
	}
	if got[0].StepID == ids[0] {
		t.Fatal("a finished step was matched by position; only open steps may be")
	}

	// Word-for-word identity outranks position: the step named exactly keeps
	// its id even when a reworded step sits where it used to be.
	swapped, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Check the release pipeline", Status: "completed"},
		{Step: "Confirm the rollout is healthy", Status: "in_progress"},
		{Step: "Point the deploy script at the new region, then tag it", Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if swapped.Plan.Steps[1].StepID != ids[2] || swapped.Plan.Steps[2].StepID == ids[2] {
		t.Fatalf("position took a step named word for word: %+v", swapped.Plan.Steps)
	}

	// An inserted step shifts every position, so nothing matches by position:
	// the new first step must not take the id of the step it pushed down.
	store2, identity2, _, run2 := newRecoveryFixture(t)
	open, err := store2.SyncRunPlan(ctx, identity2.TenantID, run2.ID, "migration", []RunPlanStepInput{
		{Step: "Export the table", Status: "in_progress"},
		{Step: "Transform the rows", Status: "pending"},
		{Step: "Import the table", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	longer, err := store2.SyncRunPlan(ctx, identity2.TenantID, run2.ID, "migration", []RunPlanStepInput{
		{Step: "Snapshot the database first", Status: "completed"},
		{Step: "Export the table to CSV", Status: "in_progress"},
		{Step: "Transform the exported rows", Status: "pending"},
		{Step: "Import the transformed table", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range longer.Plan.Steps {
		for _, previous := range open.Plan.Steps {
			if step.StepID == previous.StepID {
				t.Fatalf("a longer snapshot matched step %d %q to %q by position", i, step.Step, previous.Step)
			}
		}
	}
}
