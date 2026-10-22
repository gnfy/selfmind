package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/verification"
)

// recordParkedProgress gives a parked Run the state RUQX-1272 had when its
// approval was answered: one step verified and completed, the next one active.
func recordParkedProgress(t *testing.T, store *control.Store, owner *control.IdentityContext, runID, cwd string) []control.RunPlanStep {
	t.Helper()
	ctx := context.Background()
	first, err := store.SyncRunPlan(ctx, owner.TenantID, runID, "plan", []control.RunPlanStepInput{
		{Step: "Check the configuration", Status: "in_progress", SuccessCriteria: "The configuration is valid", VerificationRequired: true},
		{Step: "Write the receipt", Status: "pending", SuccessCriteria: "The receipt exists"},
	})
	if err != nil {
		t.Fatal(err)
	}
	step := first.Plan.Steps[0].StepID
	// An observation of something outside the workspace (a remote build, a
	// service state): no local file is among its inputs, so the receipt the
	// resumed Run writes does not make it stale.
	external := []string{}
	binding, err := store.ResolveVerificationBinding(ctx, owner.TenantID, runID, verification.Binding{StepID: step, LocalDependencies: &external}, cwd)
	if err != nil || binding == nil {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": "toolcall-3-0", "status": "succeeded",
		"started_at_unix_nano": int64(10), "finished_at_unix_nano": int64(11), "invoked": true,
		"process": map[string]interface{}{"started": true, "exit_code": 0},
		"command": map[string]interface{}{"command": "check config", "cwd": cwd, "exit_code": 0, "binding": binding},
	}})
	if _, err := store.AppendEvent(ctx, control.Event{RunID: runID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	progress, err := store.SyncRunPlan(ctx, owner.TenantID, runID, "progress", []control.RunPlanStepInput{
		{StepID: step, Status: "completed"}, {StepID: first.Plan.Steps[1].StepID, Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return progress.Plan.Steps
}

func resumedChild(t *testing.T, store *control.Store, owner *control.IdentityContext, approval *control.ApprovalRequest) *control.Run {
	t.Helper()
	var child *control.Run
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := store.ListTaskRuns(context.Background(), owner.TenantID, approval.TaskID, 10)
		for i := range runs {
			if runs[i].ResumesRunID == approval.RunID {
				child = &runs[i]
				return true
			}
		}
		return false
	}, "the answer did not start a continuation")
	return child
}

func planShapeOf(steps []control.RunPlanStep) string {
	var b strings.Builder
	for _, step := range steps {
		switch step.Status {
		case "completed":
			b.WriteString("C")
		case "in_progress":
			b.WriteString(">")
		case "pending":
			b.WriteString(".")
		default:
			b.WriteString("x")
		}
	}
	return b.String()
}

// RUQX-1272 lost its plan progress at every approval answer: the verified step
// came back pending and the earliest verification-required step was re-selected.
// Answering an approval resumes the same work, so the continuation starts from
// the Plan the parked Run recorded, with the verified step still completed and
// the evidence behind it still valid.
func TestApprovalAnswerContinuationKeepsTheVerifiedProgressOfTheParkedRun(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "progress-ledger")
	startHumanWait(t, daemon, ws, "progress-ledger")
	approval := waitParkedApproval(t, daemon, store, owner)
	parked := recordParkedProgress(t, store, owner, approval.RunID, ws.LocalPath)
	if got := planShapeOf(parked); got != "C>" {
		t.Fatalf("fixture: parked plan [%s]", got)
	}
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "progress-ledger", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	child := resumedChild(t, store, owner, approval)
	plan, err := store.LatestRunPlan(ctx, owner.TenantID, child.ID)
	if err != nil || plan == nil {
		t.Fatalf("continuation plan: %+v %v", plan, err)
	}
	if got := planShapeOf(plan.Steps); got != "C>" {
		t.Fatalf("the approval answer moved the plan backwards: [%s]", got)
	}
	if !plan.Steps[0].ReusePriorVerification || plan.Steps[0].StepID != parked[0].StepID {
		t.Fatalf("the verified step lost its identity or its evidence: %+v", plan.Steps[0])
	}
	prior, err := store.ListInheritedPlanEvidence(ctx, owner.TenantID, child.ID)
	if err != nil || len(prior) != 1 || prior[0].PriorVerification != "passed" || prior[0].CheckID != "toolcall-3-0" {
		t.Fatalf("the evidence behind the carried step: %+v %v", prior, err)
	}
}
