package control

import (
	"context"
	"strings"
	"testing"
)

func TestRecoveryHandoffProjectsPlanEffectsAndSafeAttempts(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Inspect current state", Status: "completed"},
		{Step: "Apply release", Status: "in_progress"},
		{Step: "Verify release", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-1", ToolName: "terminal", ArgsHash: "secret-hash",
		RetryClass: "side_effect", EffectID: "effect-release", PlanVersion: plan.Plan.Version,
		PlanStepID: plan.Plan.Steps[1].StepID, Strategy: "mutate", EffectClass: "external_write",
	}); err != nil {
		t.Fatal(err)
	}
	interruptForAutomaticRecoveryTest(t, store, identity, task, run)

	handoff, err := store.RecoveryHandoffForRun(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || handoff == nil {
		t.Fatalf("handoff=%+v err=%v", handoff, err)
	}
	if handoff.OriginalGoal != run.InputSummary || handoff.Cause != "provider_or_transport_error" ||
		handoff.Reason != "uncertain_effect_requires_observation" || handoff.ResumePath != "/resume "+run.ID {
		t.Fatalf("handoff identity=%+v", handoff)
	}
	if len(handoff.CompletedSteps) != 1 || handoff.CompletedSteps[0] != "Inspect current state" ||
		len(handoff.UnresolvedSteps) != 2 || len(handoff.UncertainEffects) != 1 ||
		len(handoff.AttemptedStrategies) != 1 {
		t.Fatalf("handoff evidence=%+v", handoff)
	}
	if handoff.UncertainEffects[0].EffectID != "effect-release" || handoff.AttemptedStrategies[0].Strategy != "mutate" {
		t.Fatalf("handoff summaries=%+v", handoff)
	}
	if strings.Contains(strings.Join([]string{handoff.UnlockCondition, handoff.OriginalGoal}, " "), "secret-hash") {
		t.Fatalf("handoff leaked tool arguments or hashes: %+v", handoff)
	}

	other, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "other-person", "Other Person")
	if err != nil {
		t.Fatal(err)
	}
	if leaked, err := store.RecoveryHandoffForRun(ctx, identity.TenantID, other.PersonID, run.ID); err != nil || leaked != nil {
		t.Fatalf("cross-person handoff=%+v err=%v", leaked, err)
	}
}

func TestRecoveryHandoffKeepsGoalAcrossCapacityContinuations(t *testing.T) {
	ctx := context.Background()
	store, identity, task, root := newRecoveryFixture(t)
	if _, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: root.ID, Content: "Inspect only; do not change files"}); err != nil {
		t.Fatal(err)
	}
	parent := root
	for range 3 {
		if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_external"); err != nil {
			t.Fatal(err)
		}
		child, err := store.StartRunWithOptions(ctx, task, "cli", "Continue this exact work from its durable model-call checkpoint", StartRunOptions{ResumesRunID: parent.ID})
		if err != nil {
			t.Fatal(err)
		}
		parent = child
	}
	interruptForAutomaticRecoveryTest(t, store, identity, task, parent)
	handoff, err := store.RecoveryHandoffForRun(ctx, identity.TenantID, identity.PersonID, parent.ID)
	if err != nil || handoff == nil || handoff.OriginalGoal != root.InputSummary {
		t.Fatalf("original user goal replaced by continuation instructions: %+v, %v", handoff, err)
	}
	if len(handoff.UserRequirements) != 1 || handoff.UserRequirements[0] != "Inspect only; do not change files" {
		t.Fatalf("lost user correction: %+v", handoff)
	}
	// A forged or incompatible execution edge cannot import another scope's goal.
	if _, err := store.db.ExecContext(ctx, "UPDATE runs SET workspace_id='another-workspace' WHERE id=?", root.ID); err != nil {
		t.Fatal(err)
	}
	handoff, err = store.RecoveryHandoffForRun(ctx, identity.TenantID, identity.PersonID, parent.ID)
	if err != nil || handoff == nil || handoff.OriginalGoal != "" || len(handoff.UserRequirements) != 0 {
		t.Fatalf("cross-scope original goal was exposed: %+v, %v", handoff, err)
	}
}
