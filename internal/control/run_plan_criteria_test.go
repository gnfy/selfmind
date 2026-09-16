package control

import (
	"context"
	"testing"
)

// A run once moved a step's acceptance bar from "gh api confirms the file
// exists with the right content" to "a PR is created with the right content"
// and completed it in the same snapshot. The plan still resolved, and the bar
// it resolved against was gone. Restating a criterion can be honest
// replanning, so this records the pair rather than refusing the update.
func TestSyncRunPlanReportsCriteriaRestatedOnCompletion(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Add release workflow", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "add the workflow")
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
		{Step: "write the workflow", Status: "completed", SuccessCriteria: "file written"},
		{Step: "verify it is live", Status: "in_progress", SuccessCriteria: "gh api confirms the file exists with the right content"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(first.CriteriaRestated) != 0 {
		t.Fatalf("a first snapshot has no prior bar to restate: %+v", first.CriteriaRestated)
	}
	verifyStep := first.Plan.Steps[1].StepID

	// Weakening the bar while the step is still in flight is ordinary
	// replanning and must stay quiet.
	inFlight, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "replan", []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Step: "write the workflow", Status: "completed", SuccessCriteria: "file written"},
		{StepID: verifyStep, Step: "verify it is live", Status: "in_progress", SuccessCriteria: "a PR is created with the right content"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(inFlight.CriteriaRestated) != 0 {
		t.Fatalf("replanning work still in flight must not be reported: %+v", inFlight.CriteriaRestated)
	}

	// Declaring it done under the new bar is the shape that hid the defect.
	completed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "done", []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Step: "write the workflow", Status: "completed", SuccessCriteria: "file written"},
		{StepID: verifyStep, Step: "verify it is live", Status: "completed", SuccessCriteria: "a PR is created with the right content"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(completed.CriteriaRestated) != 1 {
		t.Fatalf("the moved bar was not reported: %+v", completed.CriteriaRestated)
	}
	change := completed.CriteriaRestated[0]
	if change.StepID != verifyStep {
		t.Errorf("step id = %q, want %q", change.StepID, verifyStep)
	}
	// The baseline is the bar FIRST declared, not the previous snapshot's: the
	// run this comes from moved the bar while the step was pending and
	// completed it snapshots later, so a version-to-version diff saw only a
	// status change.
	if change.From != "gh api confirms the file exists with the right content" {
		t.Errorf("from = %q, want the originally declared bar", change.From)
	}
	if change.To != "a PR is created with the right content" {
		t.Errorf("to = %q", change.To)
	}
	// A completed step whose bar never moved must not be reported.
	for _, reported := range completed.CriteriaRestated {
		if reported.StepID == first.Plan.Steps[0].StepID {
			t.Errorf("an unchanged criterion was reported: %+v", reported)
		}
	}
}

// A moved bar is reported when the step first arrives completed under it, not
// on every later snapshot that echoes the same completed text (observed live:
// seventeen review lines in one run for three steps, and the model reopened
// finished steps in response). Moving the completed text again is a new pair.
func TestSyncRunPlanReportsMovedCriteriaOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-b", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Write back the release ledger", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "write back")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "plan", []RunPlanStepInput{
		{Step: "record the build id", Status: "in_progress", SuccessCriteria: "the ledger block names the build id"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	stepID := first.Plan.Steps[0].StepID

	completed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "done", []RunPlanStepInput{
		{StepID: stepID, Step: "record the build id", Status: "completed", SuccessCriteria: "the ledger block names the build id and its approval"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(completed.CriteriaRestated) != 1 {
		t.Fatalf("the moved bar must be reported when the step completes under it: %+v", completed.CriteriaRestated)
	}

	// The next snapshot changes something else and echoes the completed step
	// unchanged; the same moved bar must not be reported again.
	echoed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "follow-up", []RunPlanStepInput{
		{StepID: stepID, Step: "record the build id", Status: "completed", SuccessCriteria: "the ledger block names the build id and its approval"},
		{Step: "re-run the ledger check", Status: "pending", SuccessCriteria: "the check passes"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if !echoed.Changed {
		t.Fatal("adding a step must produce a new plan version")
	}
	if len(echoed.CriteriaRestated) != 0 {
		t.Fatalf("an echoed completed criterion was reported again: %+v", echoed.CriteriaRestated)
	}

	// Moving the completed text once more is a new pair and is reported.
	moved, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "moved again", []RunPlanStepInput{
		{StepID: stepID, Step: "record the build id", Status: "completed", SuccessCriteria: "the ledger block names the build id, its approval, and the deploy artifact"},
		{StepID: echoed.Plan.Steps[1].StepID, Step: "re-run the ledger check", Status: "pending", SuccessCriteria: "the check passes"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(moved.CriteriaRestated) != 1 || moved.CriteriaRestated[0].To != "the ledger block names the build id, its approval, and the deploy artifact" {
		t.Fatalf("a further change of the completed text must be reported once: %+v", moved.CriteriaRestated)
	}

	// Returning to the originally declared bar is not a moved bar.
	restored, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "restored", []RunPlanStepInput{
		{StepID: stepID, Step: "record the build id", Status: "completed", SuccessCriteria: "the ledger block names the build id"},
		{StepID: echoed.Plan.Steps[1].StepID, Step: "re-run the ledger check", Status: "pending", SuccessCriteria: "the check passes"},
	})
	if err != nil {
		t.Fatalf("SyncRunPlan: %v", err)
	}
	if len(restored.CriteriaRestated) != 0 {
		t.Fatalf("restoring the original bar was reported as a change: %+v", restored.CriteriaRestated)
	}
}
