package control

import (
	"context"
	"encoding/json"
	"selfmind/internal/verification"
	"testing"
)

func TestVerificationReplacementIsScopedToOpenWorkUnit(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	_, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "inspect", []RunPlanStepInput{{Step: "Verify export", Status: "in_progress", SuccessCriteria: "all records exported"}})
	if err != nil {
		t.Fatal(err)
	}
	binding := &verification.Binding{Version: 1, Criterion: "all records exported", Target: "report.csv"}
	payload, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"tool_call_id": "old", "kind": "verification", "status": "failed", "started_at_unix_nano": 10, "finished_at_unix_nano": 20, "command": map[string]interface{}{"command": "broken parser", "cwd": "/workspace", "binding": binding}}})
	_, err = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	b := *binding
	b.Replaces = "old"
	b.Reason = "Correct the parser, retaining row completeness."
	if err = store.ValidateVerificationReplacement(ctx, identity.TenantID, run.ID, b, "/workspace"); err != nil {
		t.Fatal(err)
	}
	b.Target = "other.csv"
	if err = store.ValidateVerificationReplacement(ctx, identity.TenantID, run.ID, b, "/workspace"); err == nil {
		t.Fatal("foreign target accepted")
	}
	b.Target = "report.csv"
	if err = store.ValidateVerificationReplacement(ctx, "other-tenant", run.ID, b, "/workspace"); err == nil {
		t.Fatal("foreign owner accepted")
	}
	_, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "independent objective", []RunPlanStepInput{{Step: "Verify export", Status: "cancelled"}, {Step: "Verify another objective", Status: "in_progress", WorkUnit: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ValidateVerificationReplacement(ctx, identity.TenantID, run.ID, b, "/workspace"); err == nil {
		t.Fatal("closed work-unit evidence reused")
	}
}

func TestCorrectedVerificationCanCloseRequiredWorkUnit(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "Verify original target", []RunPlanStepInput{{Step: "Check result", Status: "in_progress", SuccessCriteria: "all records exported", VerificationRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, status, command string, binding *verification.Binding, start int64) {
		t.Helper()
		data, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"tool_call_id": id, "kind": "verification", "status": status, "started_at_unix_nano": start, "finished_at_unix_nano": start + 1, "command": map[string]interface{}{"command": command, "cwd": "/workspace", "binding": binding}}})
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Payload: data}); err != nil {
			t.Fatal(err)
		}
	}
	add("bad", "failed", "broken parser", &verification.Binding{Version: 1, Criterion: "all records exported", Target: "report.csv"}, 10)
	steps := []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Step: "Check result", Status: "completed"}}
	if _, err = store.SyncRunPlan(ctx, identity.TenantID, run.ID, "premature", steps); err == nil {
		t.Fatal("failed evidence closed unit")
	}
	replacement := &verification.Binding{Version: 1, Criterion: "all records exported", Target: "report.csv", Replaces: "bad", Reason: "Correct header parsing while still counting every data row"}
	if err = store.ValidateVerificationReplacement(ctx, identity.TenantID, run.ID, *replacement, "/workspace"); err != nil {
		t.Fatal(err)
	}
	add("fixed", "succeeded", "correct parser", replacement, 20)
	final, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "verified", steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.WorkUnits) != 1 || final.WorkUnits[0].VerificationState != "passed" {
		t.Fatalf("projection=%+v", final.WorkUnits)
	}
	if err = store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'", run.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history lost: %d %v", count, err)
	}
}
