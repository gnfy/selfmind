package control

import (
	"context"
	"strings"
	"testing"
)

func TestPlanIssuesCompactIDsWithoutReusingRetiredIdentity(t *testing.T) {
	store, identity, _, run := newRecoveryFixture(t)
	ctx := context.Background()
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "inspect", Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	id := first.Plan.Steps[0].StepID
	if !strings.HasPrefix(id, "step_") || len(id) > 24 {
		t.Fatalf("step identity is not compact: %q", id)
	}
	next, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "new work", []RunPlanStepInput{{Step: "write", Status: "pending"}, {Step: "review", Status: "pending"}, {Step: "report", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{id: true}
	for _, step := range next.Plan.Steps {
		if seen[step.StepID] {
			t.Fatalf("reused retired or current obligation: %s", step.StepID)
		}
		seen[step.StepID] = true
	}
	input := []RunPlanStepInput{{StepID: next.Plan.Steps[0].StepID, Status: "pending"}}
	for i := len(next.Plan.Steps) - 1; i >= 1; i-- {
		input = append(input, RunPlanStepInput{StepID: next.Plan.Steps[i].StepID, Status: "pending"})
	}
	last, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "reorder", input)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range last.Plan.Steps {
		if step.StepID != input[i].StepID {
			t.Fatal("reordering changed durable identity")
		}
	}
}

func TestLegacyStepIdentityRemainsOpaque(t *testing.T) {
	legacy := "step_6eca0cf1-3817-401f-97b7-5abdde208bb6"
	previous := &RunPlan{Steps: []RunPlanStep{{RunPlanStepInput: RunPlanStepInput{StepID: legacy, Step: "inspect", Status: "pending"}}}}
	steps, err := resolveRunPlanSteps("run", []RunPlanStepInput{{StepID: legacy, Status: "completed"}}, previous, nil)
	if err != nil || len(steps) != 1 || steps[0].StepID != legacy {
		t.Fatalf("rewrote legacy identity: %+v %v", steps, err)
	}
}

func TestClaimPreservesIndependentStepIdentities(t *testing.T) {
	store, identity, _, parent := newRecoveryFixture(t)
	ctx := context.Background()
	first, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "parent", []RunPlanStepInput{{Step: "Parent target", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Own interaction", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRun(ctx, task, "cli", "select old work")
	if err != nil {
		t.Fatal(err)
	}
	own, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "own", []RunPlanStepInput{{Step: "Independent target", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Steps[0].StepID == own.Plan.Steps[0].StepID {
		t.Fatalf("recycled child id %s from %q to %q (source=%s)", got.Steps[0].StepID, own.Plan.Steps[0].Step, got.Steps[0].Step, first.Plan.Steps[0].StepID)
	}
}

func TestRetargetPreservesIndependentParentStepIdentities(t *testing.T) {
	ctx := context.Background()
	store, identity, _, a := newRecoveryFixture(t)
	pa, err := store.SyncRunPlan(ctx, identity.TenantID, a.ID, "a", []RunPlanStepInput{{Step: "Check A", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, a.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "B", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.StartRun(ctx, taskB, "cli", "B")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := store.SyncRunPlan(ctx, identity.TenantID, b.ID, "b", []RunPlanStepInput{{Step: "Check B", Status: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.FinishRun(ctx, identity.TenantID, b.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	taskC, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.StartRun(ctx, taskC, "cli", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, c.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	first, err := store.LatestRunPlan(ctx, identity.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RetargetInteractionContinuation(ctx, identity.TenantID, identity.PersonID, c.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.LatestRunPlan(ctx, identity.TenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parent A id=%s, parent B id=%s, child first=%+v second=%+v", pa.Plan.Steps[0].StepID, pb.Plan.Steps[0].StepID, first.Steps[0], second.Steps[0])
	if first.Steps[0].StepID == second.Steps[0].StepID {
		t.Fatalf("retarget reused %s for %q -> %q", first.Steps[0].StepID, first.Steps[0].Step, second.Steps[0].Step)
	}
}
