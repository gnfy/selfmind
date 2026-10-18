package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/verification"
)

// Historical checks keep their declared input scope through unrelated work,
// while changed inputs and current failures still prevent completion.
func TestReusedVerificationSurvivesIndependentWork(t *testing.T) {
	for _, declared := range []string{"independent-deps", "pure-external([])", "undeclared-deps(nil)", "overlapping-deps", "independent-current-failed", "independent-other-failed", "independent-other-stale"} {
		for _, bShape := range []string{"B-plain", "B-required-own-check"} {
			declared, bShape := declared, bShape
			t.Run(declared+"/"+bShape, func(t *testing.T) {
				ctx := context.Background()
				store, identity, task, run := newRecoveryFixture(t)
				bRequired := bShape == "B-required-own-check"
				first, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "initial", []RunPlanStepInput{
					{Step: "Step A", Status: "in_progress", SuccessCriteria: "condition A holds", VerificationRequired: true},
					{Step: "Step B", Status: "pending", SuccessCriteria: "condition B holds", VerificationRequired: bRequired},
				})
				if err != nil {
					t.Fatal(err)
				}
				a, b := first.Plan.Steps[0].StepID, first.Plan.Steps[1].StepID
				bind := verification.Binding{StepID: a, Target: "a", Criterion: "condition A holds"}
				switch declared {
				case "independent-deps", "overlapping-deps", "independent-current-failed", "independent-other-failed", "independent-other-stale":
					deps := []string{"/workspace/pkg/a/a.go"}
					bind.LocalDependencies = &deps
				case "pure-external([])":
					none := []string{}
					bind.LocalDependencies = &none
				}
				binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, bind, "/workspace")
				if err != nil || binding == nil {
					t.Fatalf("binding: %+v %v", binding, err)
				}
				appendPlanEvidence(t, store, run.ID, "check-a", binding, 10)
				if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "progress", []RunPlanStepInput{{StepID: a, Status: "completed"}, {StepID: b, Status: "in_progress"}}); err != nil {
					t.Fatal(err)
				}
				if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
					t.Fatal(err)
				}
				child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: run.ID, ExactContinuation: false})
				if err != nil {
					t.Fatal(err)
				}
				adopt := []RunPlanStepInput{
					{StepID: a, Status: "completed", ReusePriorVerification: true, ReuseReason: "Step A's inputs are untouched."},
					{StepID: b, Status: "in_progress"},
				}
				if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "adopt A", adopt); err != nil {
					t.Fatalf("[adopt] refused: %v", err)
				}
				mutated := "/workspace/pkg/b/b.go"
				if declared == "overlapping-deps" {
					mutated = "/workspace/pkg/a/a.go"
				}
				mut, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
					"kind": "mutation", "tool_name": "patch", "tool_call_id": "w1", "status": "succeeded",
					"started_at_unix_nano": int64(500), "finished_at_unix_nano": int64(501),
					"files": []map[string]interface{}{{"path": mutated, "before_sha256": "aa", "after_sha256": "bb"}}}})
				if _, err := store.AppendEvent(ctx, Event{RunID: child.ID, Type: "evidence.recorded", Payload: mut}); err != nil {
					t.Fatal(err)
				}
				if bRequired {
					bindB, err := store.ResolveVerificationBinding(ctx, identity.TenantID, child.ID, verification.Binding{StepID: b, Target: "b", Criterion: "condition B holds"}, "/workspace")
					if err != nil || bindB == nil {
						t.Fatalf("binding B: %+v %v", bindB, err)
					}
					appendPlanEvidence(t, store, child.ID, "check-b", bindB, 600)
				}

				if declared == "independent-current-failed" || declared == "independent-other-failed" || declared == "independent-other-stale" {
					failedBinding := verification.Binding{StepID: a, Target: "a", Criterion: "condition A holds"}
					if declared == "independent-other-failed" || declared == "independent-other-stale" {
						failedBinding = verification.Binding{Target: "other-observation", Criterion: "independent observation succeeds"}
					}
					current, bindErr := store.ResolveVerificationBinding(ctx, identity.TenantID, child.ID, failedBinding, "/workspace")
					if bindErr != nil || current == nil {
						t.Fatalf("current binding: %v", bindErr)
					}
					checkStatus, checkAt := "failed", int64(700)
					if declared == "independent-other-stale" {
						checkStatus, checkAt = "succeeded", 400
					}
					raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
						"kind": "verification", "tool_name": "verify", "tool_call_id": "current-a-failed", "status": checkStatus,
						"started_at_unix_nano": checkAt, "finished_at_unix_nano": checkAt + 1,
						"command": map[string]interface{}{"command": "check a", "cwd": "/workspace", "binding": current}}})
					if _, e := store.AppendEvent(ctx, Event{RunID: child.ID, Type: "evidence.recorded", Payload: raw}); e != nil {
						t.Fatal(e)
					}
				}
				final := []RunPlanStepInput{
					{StepID: a, Status: "completed", ReusePriorVerification: true, ReuseReason: "Step A's inputs are untouched."},
					{StepID: b, Status: "completed"},
				}

				if declared == "undeclared-deps(nil)" && !bRequired {
					prior, readErr := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					_, candidateErr := store.PrepareRunCompletion(ctx, identity.TenantID, identity.PersonID, child.ID, RunCompletionInput{ExpectedPlanVersion: prior.Version, Status: "done", PlanPatch: final})
					after, readErr := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if candidateErr == nil {
						t.Fatal("candidate incorrectly accepted nil dependency reuse after mutation")
					}
					if after.Version != prior.Version || after.Steps[0].Status != "completed" {
						t.Fatal("refused candidate was persisted")
					}
					if !strings.Contains(candidateErr.Error(), "recorded changes affect verification inputs") || !strings.Contains(candidateErr.Error(), "not committed") {
						t.Errorf("candidate refusal lost reopen cause or commit boundary: %v", candidateErr)
					}
				}
				proj, syncErr := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "finish", final)
				shouldPass := declared == "independent-deps" || declared == "pure-external([])"
				if shouldPass {
					if syncErr != nil {
						t.Errorf("valid reuse rejected: %v", syncErr)
					} else if e := store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); e != nil {
						t.Errorf("valid completion rejected: %v", e)
					}
				} else if declared == "independent-current-failed" {
					if syncErr == nil || !strings.Contains(syncErr.Error(), "current-a-failed") {
						t.Fatalf("current failed observation was hidden: %v", syncErr)
					}
				} else if declared == "independent-other-failed" || declared == "independent-other-stale" {
					if syncErr == nil && store.ValidateRunCompletion(ctx, identity.TenantID, child.ID) == nil {
						t.Fatal("adopted check hid an independent current failure")
					}
				} else {
					if syncErr != nil || len(proj.VerificationDeferred) != 1 || proj.Plan.Steps[0].Status == "completed" {
						t.Fatalf("changed inputs did not reopen their own obligation: %+v / %v", proj, syncErr)
					}
					if !strings.Contains(proj.VerificationDeferred[0].VerificationDeferralReason(), "recorded changes affect verification inputs") {
						t.Fatal("accepted projection lost the reopen cause")
					}
					if err := store.ValidateRunCompletion(ctx, identity.TenantID, child.ID); err == nil {
						t.Fatalf("unsafe reuse accepted for %s", declared)
					}
				}

			})
		}
	}
}
