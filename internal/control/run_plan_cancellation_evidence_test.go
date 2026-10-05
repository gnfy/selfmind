package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCancellationReasonAloneCannotDiscardRequiredWork(t *testing.T) {
	for _, reason := range []string{"Execution admission refused the observation", "当前执行环境不支持该检查", "The service is unavailable; continue with an offline preparation"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "inspect current state", []RunPlanStepInput{
				{Step: "Observe current load", Status: "in_progress", SuccessCriteria: "Current target load is observed"},
				{Step: "Read independent notes", Status: "pending"},
				{Step: "Prepare collection commands", Status: "pending"},
			})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "continue independent preparation", []RunPlanStepInput{
				{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: reason},
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
				{StepID: first.Plan.Steps[2].StepID, Status: "in_progress"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if updated.Plan.Steps[0].Status != "pending" || len(updated.CancellationDeferred) != 1 {
				t.Fatalf("an unsupported scope judgment erased necessary work: %+v", updated.Plan.Steps[0])
			}
			if updated.Plan.Steps[1].Status != "completed" || updated.Plan.Steps[2].Status != "in_progress" {
				t.Fatal("preserving the obligation blocked independent work")
			}
			// Omission in a later full snapshot also cannot discard it.
			updated, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish preparation", []RunPlanStepInput{
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
				{StepID: first.Plan.Steps[2].StepID, Status: "completed"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(updated.Plan.Steps) != 3 || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) == nil {
				t.Fatal("an omitted unresolved cancellation became a completed Run")
			}
		})
	}
}

func TestCancellationReplacementRetainsOriginalCriterionAndVerification(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	criterion := "All output rows are present in the delivered artifact"
	first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "alternative checks", []RunPlanStepInput{
		{Step: "Check the artifact via the original route", Status: "pending", SuccessCriteria: criterion, VerificationRequired: true},
		{Step: "Check the same artifact via an available route", Status: "in_progress", SuccessCriteria: criterion, VerificationRequired: true, WorkUnit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := []RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The alternative checks the same result", ReplacementStepID: first.Plan.Steps[1].StepID},
		{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
	}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "covered", input); err == nil {
		t.Fatal("an unverified replacement bypassed the original required check")
	}
	before, _ := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if before.Version != first.Plan.Version || before.Steps[0].Status != "pending" {
		t.Fatal("a rejected replacement committed a partial Plan")
	}
	evidence := mustCancellationJSON(t, map[string]interface{}{"evidence": map[string]interface{}{
		"tool_call_id": "replacement-check", "kind": "verification", "status": "succeeded", "started_at_unix_nano": 10, "finished_at_unix_nano": 20,
		"command": map[string]interface{}{"command": "inspect artifact", "cwd": "/workspace", "binding": map[string]interface{}{"version": 3, "step_id": first.Plan.Steps[1].StepID, "criterion": criterion, "target": first.Plan.Steps[1].StepID}},
	}})
	if _, err = store.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: evidence}); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "covered", input)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Plan.Steps[0].Status != "cancelled" || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil {
		t.Fatal("a verified replacement could not settle a redundant step")
	}
	// A later status-only update retains the exact evidence link.
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{StepID: input[0].StepID, Status: "cancelled"}, {StepID: input[1].StepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if latest.Steps[0].ReplacementStepID != first.Plan.Steps[1].StepID {
		t.Fatal("replacement provenance was erased")
	}
	if err = store.FinishRun(ctx, identity.TenantID, run.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "Confirm the same result", StartRunOptions{ResumesRunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || inherited.Steps[0].Status != "pending" || inherited.Steps[1].Status != "in_progress" {
		t.Fatalf("inherited cancellation bypassed verification reassessment: %+v err=%v", inherited, err)
	}
	input[1].ReusePriorVerification = true
	input[1].ReuseReason = "The unchanged artifact still satisfies the original criterion"
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, child.ID, "Reuse the exact successful prior check", input); err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err != nil {
		t.Fatal("exact continuation could not reuse a verified alternative:", err)
	}
}

func TestUnrelatedOrWeakenedReplacementDoesNotCloseTheGoal(t *testing.T) {
	for _, target := range []string{"other step", "self", "unknown"} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			store, identity, _, run := newRecoveryFixture(t)
			first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{Step: "Read live queue depth", Status: "pending", SuccessCriteria: "Current live queue depth is observed"},
				{Step: "Read an offline snapshot", Status: "completed", SuccessCriteria: "An old snapshot exists"},
			})
			if err != nil {
				t.Fatal(err)
			}
			ref := first.Plan.Steps[1].StepID
			if target == "self" {
				ref = first.Plan.Steps[0].StepID
			}
			if target == "unknown" {
				ref = "another-runs-step"
			}
			changed, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{
				{StepID: first.Plan.Steps[0].StepID, Status: "cancelled", SuccessCriteria: "An old snapshot exists", CancellationDisposition: "not_required", CancellationReason: "The local snapshot is enough", ReplacementStepID: ref},
				{StepID: first.Plan.Steps[1].StepID, Status: "completed"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if changed.Plan.Steps[0].Status != "pending" || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) == nil {
				t.Fatal("replacement lowered the original acceptance bar")
			}
		})
	}
}

func TestCancellationScopeQuoteIsGroundedInTheWorkLineage(t *testing.T) {
	for _, actual := range []bool{true, false} {
		ctx := context.Background()
		store, identity, _, run := newRecoveryFixture(t)
		quote := "I only need the local preparation now; exclude the live check"
		if actual {
			payload, _ := json.Marshal(map[string]interface{}{"approval_intent": map[string]interface{}{"snapshot": map[string]string{"source": "direct", "raw_user_text": quote}}})
			if _, err := store.AppendEvent(ctx, Event{RunID: run.ID, Type: "run.started", Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Read live state", Status: "cancelled", CancellationDisposition: "not_required", CancellationReason: "The user changed the scope to preparation", ScopeChangeQuote: quote}})
		if actual && (err != nil || store.ValidateRunCompletion(ctx, identity.TenantID, run.ID) != nil) {
			t.Fatalf("actual scope change was rejected: %v", err)
		}
		if !actual && err == nil {
			t.Fatal("an invented scope quote conferred authority")
		}
	}
}
