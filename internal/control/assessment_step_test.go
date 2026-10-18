package control

import (
	"context"
	"encoding/json"
	"testing"

	"selfmind/internal/verification"
)

func TestAssessmentCanSelectExplicitlyAdoptedStep(t *testing.T) {
	c := newAssessmentParent(t)
	child := c.continueRun(t, c.run, true)
	p, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, []verification.Assessment{{StepID: c.stepIDs[0], Result: "met", Reason: "The explicitly adopted check still establishes this condition."}})
	if err != nil {
		t.Fatalf("Main cannot judge the adopted step without recopying a historical call id: %v", err)
	}
	if len(p.Conditions) != 1 || p.Conditions[0].StepID != c.stepIDs[0] || p.Conditions[0].Result != "met" {
		t.Fatalf("judgment lost: %+v", p)
	}
	if p.Conditions[0].Version != 2 || p.Conditions[0].Source == nil || p.Conditions[0].Source.RunID != c.run.ID || len(p.Conditions[0].EvidenceRefs) != 1 || p.Conditions[0].EvidenceRefs[0] != "check-0" {
		t.Fatalf("historical proof was relabelled or lost: %+v", p)
	}
	if pWithRef, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, []verification.Assessment{{StepID: c.stepIDs[0], EvidenceRefs: []string{"check-0"}, Result: "met", Reason: "Adopted check cited explicitly."}}); err != nil || len(pWithRef.Conditions) != 1 {
		t.Fatalf("adopted step assessment with explicit evidence_refs failed: %+v %v", pWithRef, err)
	}
	// A read-only preparation cannot create a new execution or a durable finding.
	before, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, nil)
	if err != nil || before.State != "unassessed" {
		t.Fatalf("preparation committed a judgment: %+v %v", before, err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"status": "waiting_user", "assessments": p.Conditions})
	if _, err := c.store.AppendEvent(context.Background(), Event{RunID: child.ID, Type: "run.outcome", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	after, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, nil)
	if err != nil || len(after.Conditions) != 1 || after.Conditions[0].Result != "met" {
		t.Fatalf("accepted finding cannot be revalidated: %+v %v", after, err)
	}
	c.recordMutation(t, child)
	if _, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, nil); err == nil {
		t.Fatal("a saved judgment survived changes to its adopted inputs")
	}
}

func TestAssessmentStepSelectionDoesNotAdoptHistory(t *testing.T) {
	c := newAssessmentParent(t)
	child := c.continueRun(t, c.run, false)
	if _, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, []verification.Assessment{{StepID: c.stepIDs[0], Result: "met", Reason: "The parent passed."}}); err == nil {
		t.Fatal("step selection implicitly adopted historical proof")
	}
}

// Use confirmed modern execution facts. The legacy carry fixtures deliberately
// omit process observations; adopting them does not invent those observations.
func newAssessmentParent(t *testing.T) carryRun {
	t.Helper()
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "check then report", []RunPlanStepInput{
		{Step: "Check the input", Status: "in_progress", SuccessCriteria: "the input is ready", VerificationRequired: true},
		{Step: "Report the result", Status: "pending"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{plan.Plan.Steps[0].StepID, plan.Plan.Steps[1].StepID}
	binding, err := store.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: ids[0], Target: "state.json"}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendObservedCheck(t, store, run.ID, "check-0", binding, 10)
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "checked", []RunPlanStepInput{{StepID: ids[0], Status: "completed"}, {StepID: ids[1], Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	return carryRun{store: store, identity: identity, task: task, run: run, stepIDs: ids}
}

func TestAdoptedStepJudgmentKeepsNegativeFindingAcrossContinuation(t *testing.T) {
	c := newAssessmentParent(t)
	child := c.continueRun(t, c.run, true)
	ctx := context.Background()
	p, err := c.store.ResolveConditionAssessments(ctx, c.identity.TenantID, child.ID, []verification.Assessment{{StepID: c.stepIDs[0], Result: "not_met", Reason: "The reused observation does not establish the required condition."}})
	if err != nil || len(p.Blockers) == 0 || p.Conditions[0].Version != 2 {
		t.Fatalf("negative judgment or versioned source lost: %+v %v", p, err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"status": "blocked", "assessments": p.Conditions})
	if _, err := c.store.AppendEvent(ctx, Event{RunID: child.ID, Type: "run.outcome", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if err := c.store.FinishRun(ctx, c.identity.TenantID, child.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	next := c.continueRun(t, child, true)
	plan, err := c.store.LatestRunPlan(ctx, c.identity.TenantID, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Status == "completed" && plan.Steps[0].ReusePriorVerification {
		t.Fatal("a later continuation adopted through Main's negative finding")
	}
}

func TestRecordedAdoptedAssessmentDoesNotCreateAdoptionAuthority(t *testing.T) {
	c := newAssessmentParent(t)
	child := c.continueRun(t, c.run, false)
	raw, _ := json.Marshal(map[string]interface{}{"status": "blocked", "assessments": []verification.Assessment{{Version: 2, RunID: child.ID, StepID: c.stepIDs[0], Result: "met", EvidenceRefs: []string{"check-0"}, Reason: "Forged record", Source: &verification.CheckSource{RunID: c.run.ID, StepID: c.stepIDs[0], PlanVersion: 2, Reason: "No accepted adoption."}}}})
	if _, err := c.store.AppendEvent(context.Background(), Event{RunID: child.ID, Type: "run.outcome", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.ResolveConditionAssessments(context.Background(), c.identity.TenantID, child.ID, nil); err == nil {
		t.Fatal("a JSON source record created adoption authority")
	}
}
