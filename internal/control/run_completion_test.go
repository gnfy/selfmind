package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCompletionCandidateRollsBackPreviewAndCommitsWithResult(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	input := RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}}
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if before.Version != plan.Plan.Version || before.Steps[0].Status != "in_progress" {
		t.Fatal("preview persisted its Plan")
	}
	// Mutation of the caller's proposal cannot alter the frozen candidate.
	input.PlanPatch[0].Status = "pending"
	payload, _ := json.Marshal(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "summary": "inspected"}})
	final := RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", AssistantContent: "Inspected.", Channel: run.Channel, Completion: candidate, Event: Event{Type: "run.finished", Payload: payload}}
	first, err := s.MaterializeRunFinalization(ctx, final)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.MaterializeRunFinalization(ctx, final)
	if err != nil || first.ID != second.ID {
		t.Fatalf("not replay safe: %v", err)
	}
	after, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if after.Version != plan.Plan.Version+1 || after.Steps[0].Status != "completed" {
		t.Fatal("final Plan not committed")
	}
	if body, err := s.RunAssistantContent(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || body != "Inspected." {
		t.Fatalf("answer not atomic: %q %v", body, err)
	}
}

func TestCompletionCandidateRechecksChangesAndRollsBackFailedCommit(t *testing.T) {
	for _, scenario := range []string{"new input", "plan changed", "cancelled", "result save failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, owner, task, run := newRecoveryFixture(t)
			plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress"}})
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "new input":
				_, err = s.AcceptSteering(ctx, SteeringMessage{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, TaskID: task.ID, Channel: run.Channel, Content: "Preserve the original input"})
			case "plan changed":
				_, err = s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "pending"}})
			case "cancelled":
				_, err = s.db.ExecContext(ctx, `UPDATE runs SET cancel_requested=1 WHERE id=?`, run.ID)
			case "result save failed":
				_, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_completion_answer BEFORE INSERT ON channel_messages WHEN NEW.role='assistant' BEGIN SELECT RAISE(ABORT,'injected result failure'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", AssistantContent: "Inspected.", Channel: run.Channel, Completion: candidate, Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}})
			if err == nil {
				t.Fatal("stale or failed finalization was committed")
			}
			after, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
			if after.Steps[0].Status == "completed" {
				t.Fatal("partial final Plan escaped failed completion")
			}
			var results int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='run.finished'`, run.ID).Scan(&results); err != nil || results != 0 {
				t.Fatal("failed commit published completion")
			}
		})
	}
}

func TestFinalCompletionCannotRewriteItsObligations(t *testing.T) {
	for _, change := range []RunPlanStepInput{
		{Step: "Different goal", Status: "completed"},
		{SuccessCriteria: "Lower bar", Status: "completed"},
		{WorkUnitID: "other-unit", Status: "completed"},
		{Status: "cancelled"},
		{Status: "impossible"},
	} {
		t.Run(change.Step+change.SuccessCriteria+change.WorkUnitID+change.Status, func(t *testing.T) {
			ctx := context.Background()
			s, owner, _, run := newRecoveryFixture(t)
			plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress", SuccessCriteria: "Original condition"}})
			if err != nil {
				t.Fatal(err)
			}
			change.StepID = plan.Plan.Steps[0].StepID
			_, err = s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{change}})
			if err == nil {
				t.Fatal("invalid final change accepted")
			}
			saved, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
			if saved.Version != plan.Plan.Version {
				t.Fatal("failed preview wrote a Plan")
			}
		})
	}
}

func TestCompletionCannotOutliveItsContextOrOwner(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	input := RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}}
	if _, err := s.PrepareRunCompletion(ctx, owner.TenantID, "another-person", run.ID, input); err == nil {
		t.Fatal("candidate crossed person boundary")
	}
	lifetime, cancel := context.WithCancel(ctx)
	candidate, err := s.PrepareRunCompletion(lifetime, owner.TenantID, owner.PersonID, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", Completion: candidate, AssistantContent: "Done.", Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}})
	if err == nil {
		t.Fatal("cancelled candidate committed")
	}
	saved, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if saved.Version != plan.Plan.Version {
		t.Fatal("cancelled completion escaped rollback")
	}
}

// Body-only completion uses the same stale-input and commit-failure guards as
// compound Plan completion. A simpler interface must not invent a weaker path.
func TestBodyOnlyCompletionRechecksInputAndPersistsAtomically(t *testing.T) {
	for _, scenario := range []string{"committed", "new input", "cancelled", "result save failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, owner, task, run := newRecoveryFixture(t)
			candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{Status: "done"})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "new input":
				_, err = s.AcceptSteering(ctx, SteeringMessage{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, TaskID: task.ID, Channel: run.Channel, Content: "Also check the other source"})
			case "cancelled":
				_, err = s.db.ExecContext(ctx, `UPDATE runs SET cancel_requested=1 WHERE id=?`, run.ID)
			case "result save failed":
				_, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_body_answer BEFORE INSERT ON channel_messages WHEN NEW.role='assistant' BEGIN SELECT RAISE(ABORT,'injected result failure'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", AssistantContent: "Observed.", Channel: run.Channel, Completion: candidate, Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}})
			var n int
			if queryErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='run.finished'`, run.ID).Scan(&n); queryErr != nil {
				t.Fatal(queryErr)
			}
			if scenario == "committed" {
				if err != nil || n != 1 {
					t.Fatalf("completion lost: %d %v", n, err)
				}
			} else if err == nil || n != 0 {
				t.Fatalf("failed completion leaked: %d %v", n, err)
			}
			if plan, err := s.LatestRunPlan(ctx, owner.TenantID, run.ID); err != nil || plan != nil {
				t.Fatalf("body-only completion invented a Plan: %+v %v", plan, err)
			}
		})
	}
}
