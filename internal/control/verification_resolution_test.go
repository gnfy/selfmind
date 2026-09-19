package control

import (
	"context"
	"encoding/json"
	"selfmind/internal/verification"
	"strings"
	"testing"
)

func TestVerificationResolutionAndExactBlocker(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "two conditions", []RunPlanStepInput{
		{Step: "Inspect input", Status: "in_progress", SuccessCriteria: "input is complete", VerificationRequired: true},
		{Step: "Validate output", Status: "pending", SuccessCriteria: "output preserves input", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{}, "/workspace")
	if err != nil || first == nil || first.Target != plan.Plan.Steps[0].StepID {
		t.Fatalf("binding=%+v err=%v", first, err)
	}
	second, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: plan.Plan.Steps[1].StepID}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, status string, b *verification.Binding, at int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": status, "started_at_unix_nano": at, "finished_at_unix_nano": at + 1, "command": map[string]interface{}{"command": "different methods", "cwd": "/workspace", "binding": b}}})
		if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	add("input-pass", "succeeded", first, 10)
	add("output-fail", "failed", second, 20)
	complete := []RunPlanStepInput{}
	for _, step := range plan.Plan.Steps {
		complete = append(complete, RunPlanStepInput{StepID: step.StepID, Step: step.Step, Status: "completed"})
	}
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete); err == nil || !strings.Contains(err.Error(), "Validate output") || !strings.Contains(err.Error(), "output-fail") {
		t.Fatalf("wrong blocking obligation: %v", err)
	}
	// The rejected transaction must preserve the previous plan and open window.
	unchanged, _ := s.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if unchanged.Version != plan.Plan.Version {
		t.Fatal("failed completion committed new plan")
	}
	replacement, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Replaces: "output-fail", Reason: "Use a supported reader to check the same output preservation condition"}, "/workspace")
	if err != nil || replacement == nil || replacement.Criterion != second.Criterion || replacement.StepID != second.StepID {
		t.Fatalf("inheritance=%+v err=%v", replacement, err)
	}
	add("output-corrected", "succeeded", replacement, 30)
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "verified", complete); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRowContext(ctx, "SELECT count(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'", run.ID).Scan(&count)
	if count != 3 {
		t.Fatal("failure history lost")
	}
	for _, bad := range []verification.Binding{
		{Replaces: "output-fail", Reason: "retry", Criterion: "weaker condition"},
		{Replaces: "output-fail", Reason: "retry", Target: "another output"},
		{StepID: "foreign-step"},
	} {
		if _, err = s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, bad, "/workspace"); err == nil {
			t.Fatalf("invalid reference accepted: %+v", bad)
		}
	}
	if _, err = s.ResolveVerificationBinding(ctx, "other-person-tenant", run.ID, verification.Binding{Replaces: "output-fail", Reason: "retry"}, "/workspace"); err == nil {
		t.Fatal("foreign evidence accepted")
	}
}
