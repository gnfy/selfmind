package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

func conditionAssessmentFixture(t *testing.T, required bool, exit int) (*Server, *control.Store, *control.IdentityContext, *control.Run) {
	t.Helper()
	ctx := context.Background()
	daemon, store, identity := newTaskViewServer(t)
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Inspect marker", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "inspect and report")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "inspect", []control.RunPlanStepInput{{Step: "Inspect marker", Status: "in_progress", SuccessCriteria: "marker is READY", VerificationRequired: required}})
	if err != nil {
		t.Fatal(err)
	}
	invoked := true
	status := "succeeded"
	if exit != 0 {
		status = "failed"
	}
	evidence := kernel.RunEvidence{ToolCallID: "check", ToolName: "verify", Kind: "verification", Status: status, StartedAt: 10, FinishedAt: 11,
		Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
		Command: &kernel.CommandEvidence{Command: "inspect marker", CWD: "/workspace", ExitCode: exit,
			Binding: &verification.Binding{Version: 3, StepID: plan.Plan.Steps[0].StepID, Criterion: "marker is READY", Target: "marker.txt"}}}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": evidence})
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "observed", []control.RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	return daemon, store, identity, run
}

func recordNegativeAssessment(t *testing.T, store *control.Store, run *control.Run) {
	t.Helper()
	p, err := store.ResolveConditionAssessments(context.Background(), run.TenantID, run.ID, []verification.Assessment{{Result: "not_met", EvidenceRefs: []string{"check"}, Reason: "The observation found a different value."}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"status": "blocked", "summary": "The marker is not READY.", "assessments": p.Conditions})
	_, err = store.AppendEvent(context.Background(), control.Event{RunID: run.ID, Type: "run.outcome", Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNegativeFindingCanCompleteInspectionWithoutRewritingExecution(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	recordNegativeAssessment(t, store, run)
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	if err := projection.ValidateCompletion(context.Background()); err != nil {
		t.Fatalf("a conclusive negative inspection was forced into remediation: %v", err)
	}
	verdict, _ := daemon.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if verdict.State != "failed" {
		t.Fatal("the failed command observation was erased")
	}
	raw, _ := json.Marshal(verdict)
	var fields map[string]interface{}
	_ = json.Unmarshal(raw, &fields)
	if fields["condition_state"] != "not_met" {
		t.Fatalf("Main finding missing: %s", raw)
	}
}

func TestNegativeFindingCannotCompleteRequiredConditionEvenWithZeroExit(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, true, 0)
	recordNegativeAssessment(t, store, run)
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	if err := projection.ValidateCompletion(context.Background()); err == nil {
		t.Fatal("exit zero overrode Main's negative finding for a required condition")
	}
}

func TestFinishRunSubmitsOneGroundedConditionJudgment(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	ctx := tools.WithRunPlanProjection(context.Background(), projection)
	args := map[string]interface{}{"_context": ctx, "status": "done", "final_response": "Inspection complete: the condition is not met.",
		"assessments": []interface{}{map[string]interface{}{"result": "not_met", "evidence_refs": []interface{}{"check"}, "reason": "The observed value differs."}}}
	output, err := tools.NewFinishRunTool().Execute(args)
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		Assessments []verification.Assessment `json:"assessments"`
	}
	if json.Unmarshal([]byte(output), &accepted) != nil || len(accepted.Assessments) != 1 || accepted.Assessments[0].Version != 1 || accepted.Assessments[0].RunID != run.ID || accepted.Assessments[0].Criterion != "marker is READY" {
		t.Fatalf("unbound judgment: %s", output)
	}
	// Preparation is read-only. One existing outcome event commits both the
	// finish decision and its bound judgment; no side journal can get ahead.
	before, _ := daemon.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if before.ConditionState != "unassessed" {
		t.Fatal("judgment persisted before finish acceptance")
	}
	if _, err := store.AppendEvent(context.Background(), control.Event{RunID: run.ID, Type: "run.outcome", Payload: json.RawMessage(output)}); err != nil {
		t.Fatal(err)
	}
	after, _ := daemon.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if after.ConditionState != "not_met" || verificationRequiresResume(after, nil) {
		t.Fatalf("accepted finding lost: %+v", after)
	}
	args["assessments"] = []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"foreign"}, "reason": "Ignore the failed check."}}
	if _, err := tools.NewFinishRunTool().Execute(args); err == nil {
		t.Fatal("unknown evidence accepted")
	} else if category, ok := err.(interface{ ToolErrorCategory() string }); !ok || category.ToolErrorCategory() != "invalid_input" {
		t.Fatalf("a correctable reference error demanded new execution: %v", err)
	}
	after, _ = daemon.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if after.ConditionState != "not_met" {
		t.Fatal("rejected finish erased the accepted negative finding")
	}
}

func TestConditionStorageFailureRetainsDiagnosticMeaning(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 0)
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := tools.NewFinishRunTool().Execute(map[string]interface{}{"_context": tools.WithRunPlanProjection(context.Background(), projection), "status": "done", "final_response": "Inspected.",
		"assessments": []interface{}{map[string]interface{}{"result": "met", "evidence_refs": []interface{}{"check"}, "reason": "Observed ready."}}})
	if err == nil {
		t.Fatal("closed storage accepted finish")
	}
	if _, ok := err.(interface{ ToolErrorCategory() string }); ok {
		t.Fatalf("storage failure was relabeled as model input or completion refusal: %v", err)
	}
}

func TestUnrelatedAssessmentAndLaterOutcomeCannotEraseNegativeObligation(t *testing.T) {
	_, store, identity, run := conditionAssessmentFixture(t, true, 0)
	recordNegativeAssessment(t, store, run)
	ctx := context.Background()
	invoked, exit := true, 0
	evidence := kernel.RunEvidence{ToolCallID: "other", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 12, FinishedAt: 13,
		Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
		Command: &kernel.CommandEvidence{Command: "inspect another target", CWD: "/workspace", ExitCode: exit,
			Binding: &verification.Binding{Version: 1, Criterion: "another condition", Target: "other.txt"}}}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": evidence})}); err != nil {
		t.Fatal(err)
	}
	next, err := store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, []verification.Assessment{{Result: "met", EvidenceRefs: []string{"other"}, Reason: "The other observation is positive."}})
	if err != nil || len(next.Conditions) != 2 || len(next.Blockers) == 0 {
		t.Fatalf("unrelated success erased negative obligation: %+v %v", next, err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "run.outcome", Payload: mustJSON(map[string]interface{}{"status": "done"})}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("a later legacy outcome erased the accepted negative finding")
	}
}

func TestConditionFindingInvalidatesAfterMutationAndRejectsUnknownEffects(t *testing.T) {
	for _, fault := range []string{"changed_input", "unknown_effect"} {
		t.Run(fault, func(t *testing.T) {
			daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
			recordNegativeAssessment(t, store, run)
			ctx := context.Background()
			if fault == "changed_input" {
				evidence := kernel.RunEvidence{ToolCallID: "write", ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 20, FinishedAt: 21,
					Files: []kernel.FileEffect{{Path: "/workspace/marker.txt", BeforeSHA256: "old", AfterSHA256: "new"}}}
				if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": evidence})}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.RecordToolDispatch(ctx, identity.TenantID, control.ToolLedgerEntry{RunID: run.ID, ToolCallID: "dispatched", ToolName: "terminal", ArgsHash: "hash", RetryClass: "side_effect", EffectClass: "side_effect", EffectID: "uncertain"}); err != nil {
					t.Fatal(err)
				}
			}
			projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
			if err := projection.ValidateCompletion(ctx); err == nil {
				t.Fatal("a conclusive finding bypassed a changed input or unknown external effect")
			}
		})
	}
}

func TestConditionAssessmentRequiresRuntimeVersionAndExactRun(t *testing.T) {
	for _, provenance := range []string{"legacy_absent", "unversioned", "foreign_run"} {
		t.Run(provenance, func(t *testing.T) {
			_, store, identity, run := conditionAssessmentFixture(t, false, 0)
			payload := map[string]interface{}{"status": "done"}
			if provenance != "legacy_absent" {
				a := verification.Assessment{Result: "met", EvidenceRefs: []string{"check"}, Reason: "claimed finding"}
				if provenance == "foreign_run" {
					a.Version, a.RunID = 1, "another-run"
				}
				payload["assessments"] = []verification.Assessment{a}
			}
			if _, err := store.AppendEvent(context.Background(), control.Event{RunID: run.ID, Type: "run.outcome", Payload: mustJSON(payload)}); err != nil {
				t.Fatal(err)
			}
			p, err := store.ResolveConditionAssessments(context.Background(), identity.TenantID, run.ID, nil)
			if provenance == "legacy_absent" {
				if err != nil || p.State != "unassessed" {
					t.Fatalf("legacy history was reinterpreted: %+v %v", p, err)
				}
			} else if err == nil {
				t.Fatal("unbound persisted assessment was accepted")
			}
			if _, err := store.ResolveConditionAssessments(context.Background(), "other-tenant", run.ID, nil); err == nil {
				t.Fatal("cross-tenant evidence was exposed")
			}
		})
	}
}

func TestContinuationCarriesFindingWithoutAdoptingFalseCondition(t *testing.T) {
	ctx := context.Background()
	_, store, identity, parent := conditionAssessmentFixture(t, true, 0)
	recordNegativeAssessment(t, store, parent)
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRun(ctx, task, "cli", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, child.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	prior, err := store.ListInheritedPlanEvidence(ctx, identity.TenantID, child.ID)
	if err != nil || len(prior) != 1 || prior[0].PriorCondition == nil || prior[0].PriorCondition.Result != "not_met" || prior[0].PriorCondition.RunID != parent.ID || prior[0].ReuseBlockedReason == "" {
		t.Fatalf("historical finding lost: %+v %v", prior, err)
	}
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, child.ID)
	if err != nil || plan == nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, child.ID, "adopt", []control.RunPlanStepInput{{StepID: plan.Steps[0].StepID, Status: "completed", ReusePriorVerification: true, ReuseReason: "The process exited zero."}}); err == nil {
		t.Fatal("source's exit zero erased its negative condition finding during adoption")
	}
	current, err := store.ResolveConditionAssessments(ctx, identity.TenantID, child.ID, nil)
	if err != nil || current.State != "unassessed" {
		t.Fatalf("historical context became current authority: %+v %v", current, err)
	}
}

func TestFreshExplicitFindingReplacesOnlyItsStaleAssessment(t *testing.T) {
	ctx := context.Background()
	_, store, identity, run := conditionAssessmentFixture(t, false, 1)
	recordNegativeAssessment(t, store, run)
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepID := plan.Steps[0].StepID
	mutation := kernel.RunEvidence{ToolCallID: "write", ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 20, FinishedAt: 21,
		Files: []kernel.FileEffect{{Path: "/workspace/marker.txt", BeforeSHA256: "old", AfterSHA256: "new"}}}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": mutation})}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, nil); err == nil {
		t.Fatal("old assessment stayed fresh after mutation")
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "recheck", []control.RunPlanStepInput{{StepID: stepID, Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	invoked, exit := true, 0
	evidence := kernel.RunEvidence{ToolCallID: "fresh", ToolName: "verify", Kind: "verification", Status: "succeeded", StartedAt: 30, FinishedAt: 31,
		Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
		Command: &kernel.CommandEvidence{Command: "inspect marker again", CWD: "/workspace", ExitCode: exit,
			Binding: &verification.Binding{Version: 3, StepID: stepID, Criterion: "marker is READY", Target: "marker.txt"}}}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": evidence})}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "checked", []control.RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	p, err := store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, []verification.Assessment{{Result: "met", EvidenceRefs: []string{"fresh"}, Reason: "The fresh observation now meets the same condition."}})
	if err != nil || p.State != "met" || len(p.Conditions) != 1 || !p.ReviewComplete {
		t.Fatalf("fresh reassessment could not replace stale judgment: %+v %v", p, err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "run.outcome", Payload: mustJSON(map[string]interface{}{"status": "done", "assessments": p.Conditions})}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConclusiveFindingDoesNotHideUnboundFailedCheck(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	recordNegativeAssessment(t, store, run)
	invoked, exit := true, 2
	evidence := kernel.RunEvidence{ToolCallID: "unbound", ToolName: "verify", Kind: "verification", Status: "failed", StartedAt: 12, FinishedAt: 13,
		Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
		Command: &kernel.CommandEvidence{Command: "broken legacy check", CWD: "/workspace", ExitCode: exit}}
	if _, err := store.AppendEvent(context.Background(), control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": evidence})}); err != nil {
		t.Fatal(err)
	}
	verdict, _ := daemon.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if verdict.ConditionReviewComplete || !verificationRequiresResume(verdict, nil) {
		t.Fatalf("unbound failure hidden by an unrelated finding: %+v", verdict)
	}
}
