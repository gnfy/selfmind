package control

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func patchPlanFixture(t *testing.T) (*Store, *IdentityContext, *Run, *RunPlan) {
	t.Helper()
	store, id, _, run := newRecoveryFixture(t)
	p, err := store.SyncRunPlan(context.Background(), id.TenantID, run.ID, "goal", []RunPlanStepInput{
		{Step: "Inspect", Status: "in_progress"},
		{Step: "Change", Status: "pending", SuccessCriteria: "requested content is present"},
		{Step: "Report", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, id, run, &p.Plan
}

func TestPlanPatchIsEquivalentToCompleteSnapshot(t *testing.T) {
	store, id, run, first := patchPlanFixture(t)
	ctx := context.Background()
	changes := []RunPlanStepInput{{StepID: first.Steps[0].StepID, Status: "completed"}, {StepID: first.Steps[1].StepID, Status: "in_progress"}}
	p, err := store.PatchRunPlan(ctx, id.TenantID, run.ID, first.Version, "progress", changes)
	if err != nil {
		t.Fatal(err)
	}
	complete := append(append([]RunPlanStepInput(nil), changes...), RunPlanStepInput{StepID: first.Steps[2].StepID, Status: "pending"})
	same, err := store.SyncRunPlan(ctx, id.TenantID, run.ID, "progress", complete)
	if err != nil || same.Changed || same.Plan.Version != p.Plan.Version {
		t.Fatalf("patch/full semantics differ: %+v %v", same, err)
	}
	if len(p.Plan.Steps) != 3 || len(p.CancellationDeferred) != 0 {
		t.Fatalf("omission became cancellation: %+v", p)
	}
	for i, step := range p.Plan.Steps {
		if step.StepID != first.Steps[i].StepID || step.Step != first.Steps[i].Step || step.SuccessCriteria != first.Steps[i].SuccessCriteria || step.WorkUnitID != first.Steps[i].WorkUnitID {
			t.Fatalf("identity/obligation drift at %d: %+v", i, step)
		}
	}
}

func TestPlanPatchRejectsStaleAmbiguousAndIncompleteChangesAtomically(t *testing.T) {
	for _, kind := range []string{"no-version", "future-version", "unknown", "duplicate", "no-id", "two-active", "missing-status"} {
		t.Run(kind, func(t *testing.T) {
			store, id, run, first := patchPlanFixture(t)
			expected := first.Version
			changes := []RunPlanStepInput{{StepID: first.Steps[2].StepID, Status: "completed"}}
			switch kind {
			case "no-version":
				expected = 0
			case "future-version":
				expected++
			case "unknown":
				changes[0].StepID = "foreign"
				changes[0].Step = first.Steps[2].Step
			case "duplicate":
				changes = append(changes, changes[0])
			case "no-id":
				changes[0].StepID = ""
				changes[0].Step = "new work"
			case "two-active":
				changes[0].Status = "in_progress"
			case "missing-status":
				changes[0].Status = ""
			}
			if _, err := store.PatchRunPlan(context.Background(), id.TenantID, run.ID, expected, "bad", changes); err == nil {
				t.Fatal("invalid patch accepted")
			}
			latest, err := store.LatestRunPlan(context.Background(), id.TenantID, run.ID)
			if err != nil || latest.Version != first.Version || len(latest.Steps) != 3 {
				t.Fatalf("refusal changed durable state: %+v %v", latest, err)
			}
		})
	}
}

func TestPlanPatchKeepsVerificationAndCancellationRequirements(t *testing.T) {
	store, id, run, first := patchPlanFixture(t)
	ctx := context.Background()
	required, err := store.SyncRunPlan(ctx, id.TenantID, run.ID, "require check", []RunPlanStepInput{
		{StepID: first.Steps[0].StepID, Status: "in_progress", VerificationRequired: true, SuccessCriteria: "the input is valid"},
		{StepID: first.Steps[1].StepID, Status: "pending"}, {StepID: first.Steps[2].StepID, Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PatchRunPlan(ctx, id.TenantID, run.ID, required.Plan.Version, "claim done", []RunPlanStepInput{
		{StepID: first.Steps[0].StepID, Status: "completed", VerificationRequired: false},
		{StepID: first.Steps[1].StepID, Status: "completed"}, {StepID: first.Steps[2].StepID, Status: "completed"},
	}); err == nil {
		t.Fatal("patch bypassed missing required verification")
	}
	p, err := store.PatchRunPlan(ctx, id.TenantID, run.ID, required.Plan.Version, "blocked", []RunPlanStepInput{{StepID: first.Steps[0].StepID, Status: "cancelled"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Plan.Steps[0].Status == "cancelled" || !p.Plan.Steps[0].VerificationRequired || p.Plan.Steps[0].SuccessCriteria != "the input is valid" {
		t.Fatalf("patch erased unfinished obligation: %+v", p)
	}
	if err = store.ValidateRunCompletion(ctx, id.TenantID, run.ID); err == nil {
		t.Fatal("necessary work disappeared")
	}
}

func TestPlanPatchRollsBackWorkUnitsAndPlanOnStorageFailure(t *testing.T) {
	store, id, run, first := patchPlanFixture(t)
	ctx := context.Background()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_plan_patch BEFORE INSERT ON run_plan_steps BEGIN SELECT RAISE(ABORT,'injected plan storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListRunWorkUnits(ctx, id.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	changes := []RunPlanStepInput{{StepID: first.Steps[0].StepID, Status: "completed"}, {StepID: first.Steps[1].StepID, Status: "in_progress"}}
	if _, err := store.PatchRunPlan(ctx, id.TenantID, run.ID, first.Version, "fail", changes); err == nil {
		t.Fatal("storage failure reported success")
	}
	p, err := store.LatestRunPlan(ctx, id.TenantID, run.ID)
	if err != nil || p.Version != first.Version || p.Steps[0].Status != "in_progress" {
		t.Fatalf("partial Plan committed: %+v %v", p, err)
	}
	after, err := store.ListRunWorkUnits(ctx, id.TenantID, run.ID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("partial work units: %+v %v", after, err)
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Status != before[i].Status || after[i].PlanStatus != before[i].PlanStatus || after[i].FinishedCursor != before[i].FinishedCursor {
			t.Fatal("work-unit state escaped rollback")
		}
	}
	if _, err = store.db.Exec(`DROP TRIGGER fail_plan_patch`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.PatchRunPlan(ctx, id.TenantID, run.ID, first.Version, "recover", changes); err != nil {
		t.Fatal(err)
	}
}

func TestPlanPatchConcurrentWritersCannotSilentlyMergeDifferentVersions(t *testing.T) {
	store, id, run, first := patchPlanFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for i := 1; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.PatchRunPlan(context.Background(), id.TenantID, run.ID, first.Version, "progress", []RunPlanStepInput{{StepID: first.Steps[i].StepID, Status: "completed"}})
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected one committed writer, got %d", succeeded)
	}
	p, err := store.LatestRunPlan(context.Background(), id.TenantID, run.ID)
	if err != nil || p.Version != first.Version+1 {
		t.Fatalf("lost version boundary: %+v %v", p, err)
	}
	_, err = store.PatchRunPlan(context.Background(), id.TenantID, run.ID, first.Version, "stale", []RunPlanStepInput{{StepID: first.Steps[0].StepID, Status: "pending"}})
	var stale *PlanVersionChangedError
	if !errors.As(err, &stale) {
		t.Fatalf("missing typed stale-version refusal: %v", err)
	}
}

func TestPlanPatchOnContinuationKeepsAdoptedProofAndSource(t *testing.T) {
	c := newAssessmentParent(t)
	child := c.continueRun(t, c.run, true)
	ctx := context.Background()
	initial, err := c.store.LatestRunPlan(ctx, c.identity.TenantID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.store.PatchRunPlan(ctx, c.identity.TenantID, child.ID, initial.Version, "report complete", []RunPlanStepInput{{StepID: c.stepIDs[1], Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Plan.Steps[0].ReusePriorVerification || p.Plan.Steps[0].SourceStepID != initial.Steps[0].SourceStepID || p.Plan.Steps[0].ReuseReason != initial.Steps[0].ReuseReason {
		t.Fatal("untouched historical proof was lost or rewritten")
	}
	if err = c.store.ValidateRunCompletion(ctx, c.identity.TenantID, child.ID); err != nil {
		t.Fatal(err)
	}
}
