package control

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/verification"
)

func TestResumeCanAdoptSavedCheckBeforePlanBookkeeping(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved proof", true: "unresolved failed attempt"}[failed], func(t *testing.T) {
			ctx := context.Background()
			s, identity, task, parent := newRecoveryFixture(t)
			p, err := s.SyncRunPlan(ctx, identity.TenantID, parent.ID, "check", []RunPlanStepInput{{Step: "Check target", Status: "in_progress", SuccessCriteria: "ready", VerificationRequired: true}})
			if err != nil {
				t.Fatal(err)
			}
			id := p.Plan.Steps[0].StepID
			b, err := s.ResolveVerificationBinding(ctx, identity.TenantID, parent.ID, verification.Binding{StepID: id}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			if failed {
				other := *b
				other.Target = "independent-target"
				raw, _ := json.Marshal(map[string]any{"evidence": map[string]any{"kind": "verification", "tool_call_id": "bad", "status": "failed", "started_at_unix_nano": 5, "finished_at_unix_nano": 6,
					"command": map[string]any{"kind": "exec", "command": "cat target", "cwd": "/workspace", "binding": &other}}})
				if _, err := s.AppendEvent(ctx, Event{RunID: parent.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			appendPlanEvidence(t, s, parent.ID, "saved-check", b, 10)
			// The process returned and evidence committed, then the turn stopped
			// before update_plan. A later Run must not re-execute for bookkeeping.
			if err := s.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted"); err != nil {
				t.Fatal(err)
			}
			child, err := s.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err == nil {
				t.Fatal("source check automatically completed work")
			}
			_, err = s.SyncRunPlan(ctx, identity.TenantID, child.ID, "assess saved proof", []RunPlanStepInput{{StepID: id, Status: "completed", ReusePriorVerification: true, ReuseReason: "The saved check establishes the unchanged condition."}})
			if (err != nil) != failed {
				t.Fatalf("failed=%t: reuse err=%v", failed, err)
			}
		})
	}
}

func TestVerificationDoesNotGuessAcrossGoals(t *testing.T) {
	for _, other := range []struct{ criterion, target string }{
		{"cluster is healthy", "cluster-b"},
		{"report contains the accepted revision", "report.md"},
		{"change contains the accepted revision", "another-change"},
	} {
		t.Run(other.target, func(t *testing.T) {
			ctx := context.Background()
			s, identity, _, run := newRecoveryFixture(t)
			plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "review", []RunPlanStepInput{{
				Step: "Review proposed change", Status: "in_progress", SuccessCriteria: "change contains the accepted revision", VerificationRequired: true,
			}})
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Version: 1, Criterion: other.criterion, Target: other.target}, "/workspace")
			if err != nil || b == nil || b.StepID != "" {
				t.Fatalf("independent observation was attached to the active goal: binding=%+v err=%v", b, err)
			}
			appendPlanEvidence(t, s, run.ID, "other-goal-pass", b, 10)
			if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}); err == nil {
				t.Fatal("another target's check completed the active goal")
			}
		})
	}
}

func TestInheritedVerificationSurvivesRepeatedMechanicalWaits(t *testing.T) {
	ctx := context.Background()
	s, identity, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "acceptance", []RunPlanStepInput{{
		Step: "Check target", Status: "in_progress", SuccessCriteria: "target is ready", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanEvidence(t, s, run.ID, "original-check", b, 10)
	stepID := plan.Plan.Steps[0].StepID
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "checked", []RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	sourceRun := run.ID
	for i := 0; i < 24; i++ {
		if err := s.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
			t.Fatal(err)
		}
		run, err = s.StartRunWithOptions(ctx, task, "cli", "approval continuation", StartRunOptions{ResumesRunID: run.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	prior, err := s.ListInheritedPlanEvidence(ctx, identity.TenantID, run.ID)
	if err != nil || len(prior) != 1 || prior[0].SourceRunID != sourceRun || prior[0].LatestCheck != "succeeded" {
		t.Fatalf("mechanical waits lost the original check: prior=%+v err=%v", prior, err)
	}
	if err := s.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("historical context granted automatic completion")
	}
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "unchanged condition", []RunPlanStepInput{{
		StepID: stepID, Status: "completed", ReusePriorVerification: true,
		ReuseReason: "The exact target and acceptance condition are unchanged, and no intervening effect is recorded.",
	}}); err != nil {
		t.Fatalf("explicit adoption could not reach the original proof: %v", err)
	}
}
