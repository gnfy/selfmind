package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func interruptForAutomaticRecoveryTest(t *testing.T, store *Store, identity *IdentityContext, task *Task, run *Run) {
	t.Helper()
	ctx := context.Background()
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"outcome": map[string]interface{}{
			"completion_reason": "provider_or_transport_error", "resumable": true,
		},
	})
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.interrupted", Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticRunRecoveryDecisionSeparatesSafeUnknownAndKnownEffects(t *testing.T) {
	t.Run("safe pre-effect continuation", func(t *testing.T) {
		store, identity, task, run := newRecoveryFixture(t)
		interruptForAutomaticRecoveryTest(t, store, identity, task, run)
		decision, err := store.AutomaticRunRecoveryDecisionForRun(context.Background(), identity.TenantID, run.ID)
		if err != nil || !decision.Eligible || decision.Mode != RunRecoveryModeContinue {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})

	t.Run("unknown effect is verification only", func(t *testing.T) {
		store, identity, task, run := newRecoveryFixture(t)
		if err := store.RecordToolDispatch(context.Background(), identity.TenantID, ToolLedgerEntry{
			RunID: run.ID, ToolCallID: "call-unknown", ToolName: "terminal", ArgsHash: "hash",
			RetryClass: "side_effect", EffectID: "effect-unknown", Strategy: "mutate",
		}); err != nil {
			t.Fatal(err)
		}
		interruptForAutomaticRecoveryTest(t, store, identity, task, run)
		decision, err := store.AutomaticRunRecoveryDecisionForRun(context.Background(), identity.TenantID, run.ID)
		if err != nil || !decision.Eligible || decision.Mode != RunRecoveryModeVerifyOnly || len(decision.UncertainEffects) != 1 {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})

	t.Run("known mutation requires user resume", func(t *testing.T) {
		store, identity, task, run := newRecoveryFixture(t)
		if err := store.RecordToolDispatch(context.Background(), identity.TenantID, ToolLedgerEntry{
			RunID: run.ID, ToolCallID: "call-known", ToolName: "terminal", ArgsHash: "hash",
			RetryClass: "side_effect", EffectID: "effect-known", Strategy: "mutate",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordToolOutcome(context.Background(), identity.TenantID, run.ID, "call-known", true); err != nil {
			t.Fatal(err)
		}
		interruptForAutomaticRecoveryTest(t, store, identity, task, run)
		decision, err := store.AutomaticRunRecoveryDecisionForRun(context.Background(), identity.TenantID, run.ID)
		if err != nil || decision.Eligible || decision.Reason != "known_effect_requires_user_resume" {
			t.Fatalf("decision=%+v err=%v", decision, err)
		}
	})
}

func TestAutomaticRunRecoveryDecisionDoesNotStealSpecialistOrHistoricalRuns(t *testing.T) {
	store, identity, task, run := newRecoveryFixture(t)
	if _, err := store.CreateApprovalRequest(context.Background(), ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID, ActionType: "tool_call",
	}); err != nil {
		t.Fatal(err)
	}
	interruptForAutomaticRecoveryTest(t, store, identity, task, run)
	decision, err := store.AutomaticRunRecoveryDecisionForRun(context.Background(), identity.TenantID, run.ID)
	if err != nil || decision.Eligible || decision.Reason != "approval_recovery_owns_run" {
		t.Fatalf("specialist decision=%+v err=%v", decision, err)
	}

	store2, identity2, task2, run2 := newRecoveryFixture(t)
	if _, err := store2.db.Exec(`UPDATE runs SET recovery_contract_version=0 WHERE id=?`, run2.ID); err != nil {
		t.Fatal(err)
	}
	interruptForAutomaticRecoveryTest(t, store2, identity2, task2, run2)
	decision, err = store2.AutomaticRunRecoveryDecisionForRun(context.Background(), identity2.TenantID, run2.ID)
	if err != nil || decision.Eligible || decision.Reason != "historical_recovery_contract" {
		t.Fatalf("historical decision=%+v err=%v", decision, err)
	}
}

func TestRunPlanIssuesStableStepIDsAndVersionsCompleteSnapshots(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	if run.RecoveryContractVersion != RunRecoveryContractVersion {
		t.Fatalf("new run recovery contract=%d, want %d", run.RecoveryContractVersion, RunRecoveryContractVersion)
	}

	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "start", []RunPlanStepInput{
		{Step: "Inspect state", Status: "completed", SuccessCriteria: "inputs recorded"},
		{Step: "Apply change", Status: "in_progress"},
		{Step: "Verify behavior", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Plan.Version != 1 || len(first.Plan.Steps) != 3 {
		t.Fatalf("first projection=%+v", first)
	}
	for _, step := range first.Plan.Steps {
		if step.StepID == "" {
			t.Fatalf("server did not issue a step id: %+v", step)
		}
	}
	ids := map[string]string{}
	for _, step := range first.Plan.Steps {
		ids[step.Step] = step.StepID
	}

	second, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "reorder", []RunPlanStepInput{
		{StepID: ids["Inspect state"], Step: "Inspect state", Status: "completed", SuccessCriteria: "inputs recorded", WorkUnitID: first.Plan.Steps[0].WorkUnitID, WorkUnit: true},
		{StepID: ids["Verify behavior"], Step: "Verify behavior", Status: "in_progress"},
		{StepID: ids["Apply change"], Step: "Apply change", Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Changed || second.Plan.Version != 2 {
		t.Fatalf("second projection=%+v", second)
	}
	if second.Plan.Steps[1].StepID != ids["Verify behavior"] || second.Plan.Steps[2].StepID != ids["Apply change"] {
		t.Fatalf("reorder retargeted stable step ids: %+v", second.Plan.Steps)
	}

	// Exact semantic identity preserves ids when a provider omits them. This is
	// compatibility for existing cassettes/providers; array position is not used.
	third, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "same steps", []RunPlanStepInput{
		{Step: "Inspect state", Status: "completed", SuccessCriteria: "inputs recorded", WorkUnitID: second.Plan.Steps[0].WorkUnitID, WorkUnit: true},
		{Step: "Verify behavior", Status: "completed"},
		{Step: "Apply change", Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Plan.Steps[1].StepID != ids["Verify behavior"] || third.Plan.Steps[2].StepID != ids["Apply change"] {
		t.Fatalf("semantic compatibility changed ids: %+v", third.Plan.Steps)
	}
	var versions int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_plan_versions WHERE run_id=?`, run.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Fatalf("plan versions=%d, want complete history of 3", versions)
	}
}

func TestRunPlanRejectsForeignStepID(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: "step_foreign", Step: "Inspect", Status: "completed"}})
	var stale *StalePlanStepReferenceError
	if !errors.As(err, &stale) || len(stale.CurrentPlanStepIDs()) != 1 || stale.CurrentPlanStepIDs()[0] != first.Plan.Steps[0].StepID {
		t.Fatalf("foreign step id error=%T %+v", err, err)
	}
}

func TestRunCompletionUsesDurablePlanAndEffectState(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	projection, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Apply", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("unresolved durable plan must reject completion")
	}
	_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{
		StepID: projection.Plan.Steps[0].StepID, Step: "Apply", Status: "completed",
		WorkUnit: true, WorkUnitID: projection.Plan.Steps[0].WorkUnitID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-crash", ToolName: "terminal", ArgsHash: "hash",
		RetryClass: "side_effect", EffectID: "effect-crash", PlanVersion: 2,
		PlanStepID: projection.Plan.Steps[0].StepID, Strategy: "mutate", EffectClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("uncertain side effect must reject completion")
	}
	if err := store.RecordToolOutcomeWithRef(ctx, identity.TenantID, run.ID, "call-crash", true, "result-evidence"); err != nil {
		t.Fatal(err)
	}
	var resultRef, verification string
	if err := store.db.QueryRowContext(ctx, `SELECT result_ref, verification_state FROM tool_ledger WHERE run_id=? AND tool_call_id='call-crash'`, run.ID).Scan(&resultRef, &verification); err != nil {
		t.Fatal(err)
	}
	if resultRef != "result-evidence" || verification != "recorded" {
		t.Fatalf("durable tool outcome ref=%q verification=%q", resultRef, verification)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatalf("resolved durable plan/effect rejected completion: %v", err)
	}
}

func TestRunCompletionRequiresDeclaredVerificationEvidence(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	projection, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{
		Step: "Verify change", Status: "completed", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("required verification without evidence must reject completion")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE run_work_units SET verification_state='passed' WHERE run_id=? AND id=?`,
		run.ID, projection.Plan.Steps[0].WorkUnitID); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatalf("declared verification evidence rejected completion: %v", err)
	}
}
