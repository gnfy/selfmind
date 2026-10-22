package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

// A frozen observation window must never accept a new check that closure cannot consume.
func TestClosedVerificationCycleRequiresReopenAndThenCompletes(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	p, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "observe", []RunPlanStepInput{{Step: "Inspect remote state", Status: "in_progress", SuccessCriteria: "remote is healthy", VerificationRequired: true, WorkUnit: true}})
	if err != nil {
		t.Fatal(err)
	}
	step := p.Plan.Steps[0]
	original, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, verification.Binding{StepID: step.StepID}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendObservedCheck(t, s, run.ID, "original", original, 10)
	closed, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "closed", []RunPlanStepInput{{StepID: step.StepID, Status: "completed"}})
	if err != nil {
		t.Fatal(err)
	}
	var frozen int64
	if err := s.db.QueryRowContext(ctx, `SELECT finished_cursor FROM run_work_units WHERE run_id=? AND id=?`, run.ID, closed.Plan.Steps[0].WorkUnitID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	// A closed cycle must not mask identity/condition errors or absorb standalone checks.
	for _, bad := range []verification.Binding{
		{StepID: "foreign-step"},
		{StepID: step.StepID, Replaces: "original", Reason: "retry", Criterion: "a weaker condition"},
	} {
		_, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, bad, "/workspace")
		var closed interface{ VerificationCycleClosed() bool }
		if err == nil || errors.As(err, &closed) {
			t.Fatalf("invalid binding received reopen advice: %+v %v", bad, err)
		}
	}
	independent, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, verification.Binding{Criterion: "another external condition", Target: "another operation"}, "/workspace")
	if err != nil || independent == nil || independent.StepID != "" {
		t.Fatalf("standalone check absorbed into closed cycle: %+v %v", independent, err)
	}
	for _, b := range []verification.Binding{
		{StepID: step.StepID},
		{StepID: step.StepID, Replaces: "original", Reason: "new observation"},
		{Replaces: "original", Reason: "new observation"},
	} {
		_, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, b, "/workspace")
		var refusal interface{ VerificationCycleClosed() bool }
		if !errors.As(err, &refusal) || !refusal.VerificationCycleClosed() || !strings.Contains(err.Error(), step.StepID) || !strings.Contains(err.Error(), "update_plan") {
			t.Fatalf("closed cycle accepted or lost recovery: binding=%+v err=%v", b, err)
		}
	}
	unchanged, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if unchanged.Version != closed.Plan.Version {
		t.Fatal("refusal mutated Plan")
	}
	// A later local report invalidates the conservative old observation, while
	// the fresh direct external method below declares no local dependency.
	mutation, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "mutation", "tool_name": "write_file", "tool_call_id": "report", "status": "succeeded", "finished_at_unix_nano": 20, "files": []map[string]interface{}{{"path": "/workspace/report.md", "before_sha256": "before", "after_sha256": "after"}}}})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: mutation}); err != nil {
		t.Fatal(err)
	}
	reopened, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "fresh observation", []RunPlanStepInput{{StepID: step.StepID, Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Plan.Steps[0].WorkUnitID == closed.Plan.Steps[0].WorkUnitID {
		t.Fatal("reopen reused frozen unit")
	}
	none := []string{}
	fresh, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, verification.Binding{StepID: step.StepID, Replaces: "original", Reason: "new observation", LocalDependencies: &none}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendObservedCheck(t, s, run.ID, "fresh", fresh, 30)
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{Status: "done", ExpectedPlanVersion: reopened.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: step.StepID, Status: "completed"}}, Assessments: []verification.Assessment{{StepID: step.StepID, Result: "met", EvidenceRefs: []string{"fresh"}, Reason: "Fresh observation succeeded"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Conditions) != 1 || len(candidate.Conditions[0].EvidenceRefs) != 1 || candidate.Conditions[0].EvidenceRefs[0] != "fresh" {
		t.Fatalf("completion selected old facts: %+v", candidate.Conditions)
	}
	if _, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", AssistantContent: "Observed healthy", Channel: run.Channel, Completion: candidate, Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"outcome":{"status":"done"}}`)}}); err != nil {
		t.Fatal(err)
	}
	var end int64
	if err := s.db.QueryRowContext(ctx, `SELECT finished_cursor FROM run_work_units WHERE run_id=? AND id=?`, run.ID, closed.Plan.Steps[0].WorkUnitID).Scan(&end); err != nil || end != frozen {
		t.Fatalf("historical window changed: %d -> %d, %v", frozen, end, err)
	}
	saved, _ := s.GetRun(ctx, owner.TenantID, run.ID)
	if saved.Status != "done" {
		t.Fatalf("reopened check could not close Run: %+v", saved)
	}
}
