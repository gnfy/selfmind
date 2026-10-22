package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"selfmind/internal/verification"
)

func recoveryProjectionFixture(t *testing.T, closed bool) (*Store, *IdentityContext, *Run, *RunPlan, error) {
	t.Helper()
	ctx := context.Background()
	s, o, _, r := newRecoveryFixture(t)
	p, err := s.SyncRunPlan(ctx, o.TenantID, r.ID, "observe and report", []RunPlanStepInput{
		{Step: "Observe A", Status: "in_progress", SuccessCriteria: "A ready", VerificationRequired: true, WorkUnit: closed},
		{Step: "Observe B", Status: "pending", SuccessCriteria: "B ready", VerificationRequired: true, WorkUnit: closed},
		{Step: "Report", Status: "pending", WorkUnit: closed}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		b, e := s.ResolveVerificationBinding(ctx, o.TenantID, r.ID, verification.Binding{StepID: p.Plan.Steps[i].StepID}, "/workspace")
		if e != nil {
			t.Fatal(e)
		}
		appendObservedCheck(t, s, r.ID, fmt.Sprintf("old-%d", i), b, int64(10+2*i))
	}
	accepted, err := s.SyncRunPlan(ctx, o.TenantID, r.ID, "report active", []RunPlanStepInput{{StepID: p.Plan.Steps[0].StepID, Status: "completed"}, {StepID: p.Plan.Steps[1].StepID, Status: "completed"}, {StepID: p.Plan.Steps[2].StepID, Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "mutation", "tool_name": "write_file", "tool_call_id": "report", "status": "succeeded", "finished_at_unix_nano": 20, "files": []map[string]interface{}{{"path": "/workspace/report.md", "before_sha256": "old", "after_sha256": "new"}}}})
	if _, err = s.AppendEvent(ctx, Event{RunID: r.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	_, problem := s.ResolveConditionAssessments(ctx, o.TenantID, r.ID, []verification.Assessment{{StepID: p.Plan.Steps[0].StepID, Result: "met", EvidenceRefs: []string{"old-0"}, Reason: "Observed ready"}, {StepID: p.Plan.Steps[1].StepID, Result: "met", EvidenceRefs: []string{"old-1"}, Reason: "Observed ready"}})
	var issues *verification.EvidenceIssuesError
	if !errors.As(problem, &issues) || len(issues.Issues) != 2 {
		t.Fatalf("lost aggregate observation cause: %v", problem)
	}
	var input interface{ ConditionAssessmentInput() bool }
	if errors.As(problem, &input) && input.ConditionAssessmentInput() {
		t.Fatalf("stale evidence became input correction: %v", problem)
	}
	return s, o, r, &accepted.Plan, problem
}

func TestEvidenceRecoveryUsesAcceptedCyclesAndPreservesOwnership(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			s, o, r, p, problem := recoveryProjectionFixture(t, closed)
			snapshot, err := s.ReadRunRecoverySnapshot(context.Background(), o.TenantID, o.PersonID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			recovery := snapshot.EvidenceRecovery(problem)
			want := "verify"
			if closed {
				want = "reopen"
			}
			if len(recovery) != 2 {
				t.Fatalf("recovery=%+v", recovery)
			}
			for i, item := range recovery {
				if item.Action != want || item.Replaces != fmt.Sprintf("old-%d", i) || item.CWD != "/workspace" || item.StepID != p.Steps[i].StepID {
					t.Fatalf("wrong accepted-state advice: %+v", recovery)
				}
			}
			if snapshot.Plan.Version != p.Version || snapshot.Plan.Steps[2].Status != "in_progress" {
				t.Fatal("projection changed progress")
			}
			if _, err := s.ReadRunRecoverySnapshot(context.Background(), o.TenantID, "another person", r.ID); err == nil {
				t.Fatal("recovery crossed person")
			}
		})
	}
}

func TestCompletionRecoveryReadsCycleAfterCandidateRollback(t *testing.T) {
	s, o, r, p, problem := recoveryProjectionFixture(t, false)
	_, err := s.PrepareRunCompletion(context.Background(), o.TenantID, o.PersonID, r.ID, RunCompletionInput{Status: "done", ExpectedPlanVersion: p.Version, PlanPatch: []RunPlanStepInput{{StepID: p.Steps[2].StepID, Status: "completed"}}, Assessments: []verification.Assessment{{StepID: p.Steps[0].StepID, Result: "met", EvidenceRefs: []string{"old-0"}, Reason: "Observed ready"}}})
	if err == nil {
		t.Fatal("stale final candidate accepted")
	}
	snapshot, err := s.ReadRunRecoverySnapshot(context.Background(), o.TenantID, o.PersonID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range snapshot.EvidenceRecovery(problem) {
		if item.Action != "verify" {
			t.Fatalf("rolled-back terminal rows dictated recovery: %+v", item)
		}
	}
	if snapshot.Plan.Version != p.Version || snapshot.Plan.Steps[2].Status != "in_progress" {
		t.Fatal("candidate patch escaped rollback")
	}
}

func TestEvidenceRecoveryOffersFreshProofWithoutAnotherCheck(t *testing.T) {
	s, o, r, p, _ := recoveryProjectionFixture(t, false)
	ctx := context.Background()
	none := []string{}
	b, err := s.ResolveVerificationBinding(ctx, o.TenantID, r.ID, verification.Binding{StepID: p.Steps[0].StepID, Replaces: "old-0", Reason: "Direct current observation", LocalDependencies: &none}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendObservedCheck(t, s, r.ID, "fresh", b, 30)
	_, problem := s.ResolveConditionAssessments(ctx, o.TenantID, r.ID, []verification.Assessment{{Result: "met", EvidenceRefs: []string{"old-0"}, Reason: "Observed ready"}})
	var unavailable *verification.EvidenceUnavailableError
	if !errors.As(problem, &unavailable) {
		t.Fatalf("not superseded evidence: %v", problem)
	}
	snapshot, err := s.ReadRunRecoverySnapshot(ctx, o.TenantID, o.PersonID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovery := snapshot.EvidenceRecovery(problem)
	if len(recovery) != 1 || recovery[0].Action != "cite" || len(recovery[0].EvidenceRefs) != 1 || recovery[0].EvidenceRefs[0] != "fresh" {
		t.Fatalf("fresh proof not offered: %+v", recovery)
	}
}

func TestEvidenceRecoveryRetainsUnknownEffectAndCancellationStops(t *testing.T) {
	for _, scenario := range []string{"uncertain effect", "cancelled", "unconfirmed", "unavailable snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			s, o, r, _, problem := recoveryProjectionFixture(t, true)
			ctx := context.Background()
			switch scenario {
			case "uncertain effect":
				uncertainEffect(t, s, o, r, "publish", "registry:synthetic")
			case "cancelled":
				if _, err := s.db.ExecContext(ctx, `UPDATE runs SET cancel_requested=1 WHERE id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
			case "unconfirmed":
				problem = &conditionAssessmentEvidenceError{&verification.EvidenceIssuesError{Issues: []verification.EvidenceIssue{{Ref: "unobserved", Kind: "unconfirmed"}}}}
			}
			snapshot, err := s.ReadRunRecoverySnapshot(ctx, o.TenantID, o.PersonID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unavailable snapshot" {
				snapshot.ClosureFacts.Available = false
			}
			recovery := snapshot.EvidenceRecovery(problem)
			if len(recovery) != 1 || recovery[0].Action != "inspect" {
				t.Fatalf("unsafe automatic recovery: %+v", recovery)
			}
		})
	}
}
