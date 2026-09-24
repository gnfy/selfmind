package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

func TestInheritedPlanExposesPriorEvidenceWithoutGrantingCurrentVerification(t *testing.T) {
	ctx := context.Background()
	store, identity, parentTask, parent := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "inspect", []RunPlanStepInput{{
		Step: "Check target", Status: "in_progress", SuccessCriteria: "target is ready", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, parent.ID, verification.Binding{}, "/workspace")
	if err != nil || binding == nil {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	binding.Criterion = "the same target reports ready when inspected"
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_call_id": "prior-check", "status": "succeeded",
		"started_at_unix_nano": int64(10), "finished_at_unix_nano": int64(11),
		"command": map[string]interface{}{"command": "inspect target", "cwd": "/workspace", "binding": binding},
	}})
	if _, err := store.AppendEvent(ctx, Event{TaskID: parentTask.ID, RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "checked", []RunPlanStepInput{{
		StepID: first.Plan.Steps[0].StepID, Step: "Check target", Status: "completed",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	childTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue", Channel: "cli"})
	child, _ := store.StartRun(ctx, childTask, "cli", "continue")
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	imported, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || imported == nil || len(imported.Steps) != 1 {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	if imported.Steps[0].Status != "in_progress" || imported.Steps[0].StepID != first.Plan.Steps[0].StepID ||
		imported.Steps[0].SourceStepID != first.Plan.Steps[0].StepID || imported.Steps[0].SourcePlanVersion != 2 {
		t.Fatalf("unverified child must retain lineage without claiming current success: %+v", imported.Steps[0])
	}
	prior, err := store.ListInheritedPlanEvidence(ctx, identity.TenantID, child.ID)
	if err != nil || len(prior) != 1 || prior[0].SourceStatus != "completed" || prior[0].PriorVerification != "passed" || prior[0].LatestCheck != "succeeded" {
		t.Fatalf("prior evidence=%+v err=%v", prior, err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err == nil {
		t.Fatal("historical verification must not complete the child Run")
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "changed requirement", []RunPlanStepInput{{
		StepID: imported.Steps[0].StepID, Step: "Check target", Status: "in_progress",
		SuccessCriteria: "target is ready and stable", VerificationRequired: true,
	}}); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ListInheritedPlanEvidence(ctx, identity.TenantID, child.ID)
	if err != nil || len(changed) != 1 || !changed[0].CriterionChanged {
		t.Fatalf("changed criterion did not invalidate the inherited comparison: %+v err=%v", changed, err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, child.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	grandchildTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue again", Channel: "cli"})
	grandchild, _ := store.StartRun(ctx, grandchildTask, "cli", "continue again")
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, grandchild.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	deep, err := store.ListInheritedPlanEvidence(ctx, identity.TenantID, grandchild.ID)
	if err != nil || len(deep) != 1 || deep[0].SourceRunID != parent.ID || deep[0].LatestCheck != "succeeded" || !deep[0].CriterionChanged {
		t.Fatalf("multi-hop continuation lost exact historical evidence: %+v err=%v", deep, err)
	}
}

func TestQueuedContinuationDoesNotClaimParentWhenPlanImportFails(t *testing.T) {
	ctx := context.Background()
	store, identity, task, parent := newRecoveryFixture(t)
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "work", []RunPlanStepInput{{Step: "prepare", Status: "completed"}, {Step: "finish", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_child_plan BEFORE INSERT ON run_plan_versions
		WHEN NEW.run_id != '`+parent.ID+`' BEGIN SELECT RAISE(ABORT, 'injected child plan failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID}); err == nil {
		t.Fatal("claim must fail with its plan import")
	}
	var children int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE tenant_id=? AND resumes_run_id=?`, identity.TenantID, parent.ID).Scan(&children); err != nil || children != 0 {
		t.Fatalf("partial continuation claim remained: children=%d err=%v", children, err)
	}
}

func TestInheritedStepTranslatesOnlyItsExactParentWorkUnitID(t *testing.T) {
	ctx := context.Background()
	store, identity, task, parent := newRecoveryFixture(t)
	parentPlan, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "review", []RunPlanStepInput{{Step: "Review draft", Status: "completed"}, {Step: "Deliver", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	childPlan, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || childPlan == nil {
		t.Fatalf("child plan=%+v err=%v", childPlan, err)
	}
	if parentPlan.Plan.Steps[0].WorkUnitID == childPlan.Steps[0].WorkUnitID {
		t.Fatal("child Work Unit identity must be run-local")
	}
	input := []RunPlanStepInput{
		{StepID: childPlan.Steps[0].StepID, Step: "Review draft", Status: "completed", WorkUnitID: parentPlan.Plan.Steps[0].WorkUnitID},
		{StepID: childPlan.Steps[1].StepID, Step: "Deliver", Status: "cancelled"},
	}
	updated, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "user takes delivery", input)
	if err != nil {
		t.Fatalf("exact parent Work Unit id should translate: %v", err)
	}
	if updated.Plan.Steps[0].WorkUnitID != childPlan.Steps[0].WorkUnitID {
		t.Fatalf("translated Work Unit id=%s, want %s", updated.Plan.Steps[0].WorkUnitID, childPlan.Steps[0].WorkUnitID)
	}
	input[0].WorkUnitID = "wu_unrelated"
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "unrelated id", input); err == nil {
		t.Fatal("unrelated Work Unit id was accepted")
	}
}

func TestExactContinuationCanAdoptUnchangedVerifiedStep(t *testing.T) {
	ctx := context.Background()
	store, identity, parentTask, parent := newRecoveryFixture(t)
	first, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "inspect", []RunPlanStepInput{{
		Step: "Check target", Status: "in_progress", SuccessCriteria: "target is ready", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, parent.ID, verification.Binding{}, "/workspace")
	if err != nil || binding == nil {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	binding.Criterion = "the same target reports ready when inspected"
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_call_id": "check", "status": "succeeded",
		"started_at_unix_nano": int64(10), "finished_at_unix_nano": int64(11),
		"command": map[string]interface{}{"command": "inspect target", "cwd": "/workspace", "binding": binding},
	}})
	if _, err := store.AppendEvent(ctx, Event{TaskID: parentTask.ID, RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, parent.ID, "checked", []RunPlanStepInput{{
		StepID: first.Plan.Steps[0].StepID, Step: "Check target", Status: "completed",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	childTask, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRun(ctx, childTask, "cli", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	imported, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || imported == nil || len(imported.Steps) != 1 {
		t.Fatalf("imported=%+v err=%v", imported, err)
	}
	step := imported.Steps[0]
	// In the production path, work_select owns the direct continuation and
	// update_plan is an interaction too; neither is a target-changing effect.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tool_ledger
		(tenant_id, run_id, tool_call_id, tool_name, strategy, retry_class, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, identity.TenantID, child.ID, "select", "work_select", "interact", "idempotent", "completed", 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []RunPlanStepInput{
		{StepID: step.StepID, Step: step.Step, Status: "completed", ReusePriorVerification: true},
		{StepID: step.StepID, Step: step.Step, Status: "completed", SuccessCriteria: "target is ready and stable", VerificationRequired: true, ReusePriorVerification: true, ReuseReason: "prior check still covers it"},
		{StepID: step.StepID, Step: "Check a different target", Status: "completed", ReusePriorVerification: true, ReuseReason: "prior check still covers it"},
	} {
		if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "invalid reuse", []RunPlanStepInput{changed}); err == nil {
			t.Fatalf("accepted invalid reuse: %+v", changed)
		}
	}
	// Looking first is the careful order: a completed terminal read is ledgered
	// as mutate for replay purposes but changes no file, so it cannot make the
	// prior check stale — the same rule a check follows inside one Run.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tool_ledger
		(tenant_id, run_id, tool_call_id, tool_name, strategy, retry_class, status, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, identity.TenantID, child.ID, "look", "terminal", "mutate", "side_effect", "completed", 2, 2); err != nil {
		t.Fatal(err)
	}
	changed, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "mutation", "tool_call_id": "write", "tool_name": "write_file", "status": "succeeded",
		"started_at_unix_nano": int64(20), "finished_at_unix_nano": int64(21),
		"files": []map[string]interface{}{{"path": "/workspace/target", "before_sha256": "a", "after_sha256": "b"}},
	}})
	if _, err := store.AppendEvent(ctx, Event{TaskID: childTask.ID, RunID: child.ID, Type: "evidence.recorded", Payload: changed}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "file changed", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
		ReusePriorVerification: true, ReuseReason: "same target",
	}}); err == nil {
		t.Fatal("a prior check cannot be adopted after this run recorded a file change")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM task_events WHERE run_id=? AND type='evidence.recorded'`, child.ID); err != nil {
		t.Fatal(err)
	}
	var originalRoots string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(execution_roots_json,'[]') FROM runs WHERE tenant_id=? AND id=?`, identity.TenantID, child.ID).Scan(&originalRoots); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET execution_roots_json='["different-root"]' WHERE tenant_id=? AND id=?`, identity.TenantID, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "different scope", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
		ReusePriorVerification: true, ReuseReason: "same target",
	}}); err == nil {
		t.Fatal("verification from another execution scope was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET execution_roots_json=? WHERE tenant_id=? AND id=?`, originalRoots, identity.TenantID, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM task_events WHERE run_id=? AND type='evidence.recorded'`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "lost observation", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
		ReusePriorVerification: true, ReuseReason: "same target",
	}}); err == nil || !strings.Contains(err.Error(), "no bound observation") {
		t.Fatalf("missing source check should fail with an actionable reason, got %v", err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: parentTask.ID, RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	adopted, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "still valid", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
		ReusePriorVerification: true, ReuseReason: "Same target and acceptance condition; no intervening change is recorded.",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Plan.Steps[0].ReusePriorVerification || adopted.Plan.Steps[0].ReuseReason == "" ||
		len(adopted.WorkUnits) != 1 || adopted.WorkUnits[0].VerificationState != "passed" {
		t.Fatalf("adoption was not persisted and projected: %+v", adopted)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err != nil {
		t.Fatalf("adopted verification did not satisfy completion: %v", err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "echo", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
	}}); err != nil {
		t.Fatalf("snapshot echo lost accepted adoption: %v", err)
	}
	mutation, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "mutation", "status": "succeeded", "files": []interface{}{},
	}})
	if _, err := store.AppendEvent(ctx, Event{TaskID: childTask.ID, RunID: child.ID, Type: "evidence.recorded", Payload: mutation}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "later work", []RunPlanStepInput{{
		StepID: step.StepID, Step: step.Step, Status: "completed",
	}}); err != nil {
		t.Fatalf("later work must not retroactively unaccept an earlier check: %v", err)
	}
}
