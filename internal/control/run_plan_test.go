package control

import (
	"context"
	"encoding/json"
	"errors"
	"selfmind/internal/verification"
	"strings"
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

func TestFirstRunPlanIgnoresUntrustedClientStepIDs(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)

	projection, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "start", []RunPlanStepInput{
		{StepID: "read-docs", Step: "Read repository instructions", Status: "completed"},
		{StepID: "start-builds", Step: "Start the builds", Status: "in_progress"},
	})
	if err != nil {
		t.Fatalf("first plan must normalize model-authored ids instead of rejecting the snapshot: %v", err)
	}
	if len(projection.Plan.Steps) != 2 {
		t.Fatalf("first plan steps=%d, want 2", len(projection.Plan.Steps))
	}
	for _, step := range projection.Plan.Steps {
		if !strings.HasPrefix(step.StepID, "step_") {
			t.Fatalf("first plan retained untrusted client step id %q", step.StepID)
		}
	}
}

func TestRunPlanExactStepIDInheritsRuntimeOwnedText(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "start", []RunPlanStepInput{
		{Step: "Inspect the exact artifact", Status: "in_progress", SuccessCriteria: "artifact is understood"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stepID := first.Plan.Steps[0].StepID
	next, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "complete", []RunPlanStepInput{
		{StepID: stepID, Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := next.Plan.Steps[0]; got.Step != "Inspect the exact artifact" || got.SuccessCriteria != "artifact is understood" {
		t.Fatalf("exact-id update lost runtime-owned fields: %+v", got)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "new", []RunPlanStepInput{{Status: "pending"}}); err == nil || !strings.Contains(err.Error(), "step is required") {
		t.Fatalf("new step without text was accepted: %v", err)
	}
}

func TestRunPlanRejectsForeignStepID(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: "step_foreign", Step: "Replace a different step", Status: "completed"}})
	var stale *StalePlanStepReferenceError
	if !errors.As(err, &stale) || len(stale.CurrentPlanStepIDs()) != 1 || stale.CurrentPlanStepIDs()[0] != first.Plan.Steps[0].StepID {
		t.Fatalf("foreign step id error=%T %+v", err, err)
	}
}

func TestRunPlanRecoversStaleAliasOnlyFromUniqueExactStep(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
		{Step: "Inspect", Status: "in_progress"},
		{Step: "Apply", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
		{StepID: "step_1", Step: "Inspect", Status: "completed"},
		{StepID: "step_2", Step: "Apply", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plan.Steps[0].StepID != first.Plan.Steps[0].StepID || updated.Plan.Steps[1].StepID != first.Plan.Steps[1].StepID {
		t.Fatalf("stale aliases changed runtime step identity: before=%+v after=%+v", first.Plan.Steps, updated.Plan.Steps)
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
		Step: "Verify change", Status: "in_progress", SuccessCriteria: "the change behaves as requested", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("required verification without evidence must reject completion")
	}
	step := RunPlanStepInput{StepID: projection.Plan.Steps[0].StepID, Step: "Verify change", Status: "completed", VerificationRequired: true}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{step}); err == nil {
		t.Fatal("a required check must run before the work-unit evidence window closes")
	}
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil || plan.Steps[0].Status != "in_progress" || plan.Version != projection.Plan.Version {
		t.Fatalf("rejected completion changed the plan: %+v err=%v", plan, err)
	}
	evidence, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"tool_call_id": "check-pass", "kind": "verification", "status": "succeeded", "started_at_unix_nano": 10, "finished_at_unix_nano": 11,
		"command": map[string]interface{}{"command": "check", "kind": "test", "cwd": "/workspace", "binding": map[string]interface{}{
			"version": 3, "step_id": projection.Plan.Steps[0].StepID, "criterion": "the change behaves as requested", "target": projection.Plan.Steps[0].StepID,
		}},
	}})
	if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: evidence}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{step}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatalf("declared verification evidence rejected completion: %v", err)
	}
}

func TestFirstPlanKeepsPrematureVerifiedCompletionOpen(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	projection, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "already worked", []RunPlanStepInput{
		{Step: "Inspect", Status: "completed"},
		{Step: "Verify output", Status: "completed", SuccessCriteria: "output matches the request", VerificationRequired: true},
		{Step: "Report", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.VerificationDeferred) != 2 || projection.Plan.Steps[1].Status != "in_progress" || projection.Plan.Steps[2].Status != "pending" {
		t.Fatalf("first snapshot did not expose one executable verification obligation: %+v", projection)
	}
	binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Criterion: "output is exact", Target: "output.txt"}, "/workspace")
	if err != nil || binding == nil || binding.StepID != projection.Plan.Steps[1].StepID {
		t.Fatalf("verification was not bound to normalized step: binding=%+v err=%v", binding, err)
	}
}

func TestRunPlanRequiresCriterionForVerificationObligation(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "missing criterion", []RunPlanStepInput{{
		Step: "Verify output", Status: "in_progress", VerificationRequired: true,
	}}); err == nil || !strings.Contains(err.Error(), "success_criteria") {
		t.Fatalf("empty verification obligation was accepted: %v", err)
	}
}

func TestRunPlanProgressPreservesAcceptance(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "start", []RunPlanStepInput{{Step: "Export records", Status: "in_progress", SuccessCriteria: "Every input row is present in report.csv", VerificationRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "still working", []RunPlanStepInput{{StepID: first.Plan.Steps[0].StepID, Step: "Export records", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Plan.Steps[0].SuccessCriteria != first.Plan.Steps[0].SuccessCriteria || !next.Plan.Steps[0].VerificationRequired {
		t.Fatalf("progress erased acceptance: %+v", next.Plan.Steps[0])
	}
}
