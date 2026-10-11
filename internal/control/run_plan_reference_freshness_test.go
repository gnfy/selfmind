package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

func TestInheritedReferencePreservesInvalidationAcrossWaits(t *testing.T) {
	for _, kind := range []string{"unchanged", "unrelated write", "dependency write", "unknown write", "criterion", "scope"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, identity, task, run := newRecoveryFixture(t)
			plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "check", []RunPlanStepInput{{Step: "Check input", Status: "in_progress", SuccessCriteria: "input is ready", VerificationRequired: true}})
			if err != nil {
				t.Fatal(err)
			}
			stepID := plan.Plan.Steps[0].StepID
			deps := []string{"/workspace/input.txt"}
			b, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: stepID, Target: "input.txt", LocalDependencies: &deps}, "/workspace")
			if err != nil {
				t.Fatal(err)
			}
			appendPlanEvidence(t, s, run.ID, "pass", b, 10)
			if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "checked", []RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 12; i++ {
				if err := s.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
					t.Fatal(err)
				}
				run, err = s.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: run.ID})
				if err != nil {
					t.Fatal(err)
				}
				if i != 2 {
					continue
				}
				switch kind {
				case "criterion":
					if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "changed requirement", []RunPlanStepInput{{StepID: stepID, Status: "in_progress", SuccessCriteria: "input is ready and stable"}}); err != nil {
						t.Fatal(err)
					}
				case "scope":
					if _, err := s.db.ExecContext(ctx, `UPDATE runs SET execution_roots_json='["/different"]' WHERE id=?`, run.ID); err != nil {
						t.Fatal(err)
					}
				case "unrelated write", "dependency write", "unknown write":
					path := "/workspace/report.md"
					if kind == "dependency write" {
						path = "/workspace/input.txt"
					}
					files := []map[string]string{{"path": path, "before_sha256": "a", "after_sha256": "b"}}
					if kind == "unknown write" {
						files = nil
					}
					raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "mutation", "finished_at_unix_nano": 30, "files": files}})
					if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
						t.Fatal(err)
					}
				}
			}
			wantPass := kind == "unchanged" || kind == "unrelated write"
			_, closeErr := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "close without adoption", []RunPlanStepInput{{StepID: stepID, Status: "completed"}})
			if closeErr == nil {
				t.Fatal("mere completion must not adopt prior evidence")
			}
			if offered := strings.Contains(closeErr.Error(), "reuse_prior_verification=true"); offered != wantPass {
				t.Fatalf("reuse recovery option offered=%v want=%v: %v", offered, wantPass, closeErr)
			}
			_, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "adopt", []RunPlanStepInput{{StepID: stepID, Status: "completed", ReusePriorVerification: true, ReuseReason: "The observation still covers the acceptance condition."}})
			if (err == nil) != wantPass {
				t.Fatalf("reuse result mismatch: %v", err)
			}
		})
	}
}

func TestInheritedReferenceSaveFailureRollsBackContinuation(t *testing.T) {
	ctx := context.Background()
	s, identity, task, parent := newRecoveryFixture(t)
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, parent.ID, "work", []RunPlanStepInput{{Step: "Check input", Status: "in_progress", SuccessCriteria: "ready", VerificationRequired: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_source_ref BEFORE INSERT ON task_events WHEN NEW.type='plan.evidence_inherited' BEGIN SELECT RAISE(ABORT,'injected journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID}); err == nil {
		t.Fatal("continued without a committed source reference")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE resumes_run_id=?`, parent.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial continuation remained: n=%d err=%v", n, err)
	}
}

func TestInheritedEvidenceIncludesEveryRequiredPlanStep(t *testing.T) {
	ctx := context.Background()
	s, identity, task, parent := newRecoveryFixture(t)
	var input []RunPlanStepInput
	for i := 0; i < 16; i++ {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		input = append(input, RunPlanStepInput{Step: fmt.Sprintf("Check target %d", i), Status: status, SuccessCriteria: fmt.Sprintf("target %d is ready", i), VerificationRequired: true})
	}
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, parent.ID, "all conditions", input); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := s.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListInheritedPlanEvidence(ctx, identity.TenantID, child.ID)
	if err != nil || len(items) != 16 {
		t.Fatalf("required evidence disappeared: n=%d err=%v", len(items), err)
	}
}
