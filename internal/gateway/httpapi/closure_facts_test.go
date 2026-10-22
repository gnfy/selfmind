package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/verification"
)

// Use the actual durable adapter and public tool receipt. A normal accepted
// update should offer the same references that finish_run accepts, without
// first provoking an invalid-reference refusal or persisting a Main judgment.
func TestPlanReceiptOffersCurrentEvidenceBeforeFinish(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	ctx := tools.WithRunPlanProjection(context.Background(), projection)
	output, err := tools.NewUpdatePlanTool().Execute(map[string]interface{}{"_context": ctx, "plan": []interface{}{map[string]interface{}{"step": "Inspect marker", "status": "completed", "success_criteria": "marker is READY"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(output, `"plan":[`) > strings.Index(output, `"closure_facts":`) {
		t.Fatal("derived hints precede the canonical Plan identities used by historical replay")
	}
	var receipt struct {
		PlanVersion  int `json:"plan_version"`
		ClosureFacts *struct {
			PlanVersion int `json:"plan_version"`
			Evidence    []struct {
				ID string `json:"id"`
			} `json:"evidence"`
		} `json:"closure_facts"`
	}
	if err := json.Unmarshal([]byte(output), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ClosureFacts == nil || receipt.ClosureFacts.PlanVersion != receipt.PlanVersion || len(receipt.ClosureFacts.Evidence) != 1 || receipt.ClosureFacts.Evidence[0].ID != "check" {
		t.Fatalf("evidence is hidden until refusal: %s", output)
	}
	refs := []string{receipt.ClosureFacts.Evidence[0].ID}
	if _, err := store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, []verification.Assessment{{Result: "not_met", EvidenceRefs: refs, Reason: "The observed value differs."}}); err != nil {
		t.Fatal(err)
	}
	before, _ := daemon.coordinator().evidenceOutcome(ctx, identity.TenantID, run.ID)
	if before.ConditionState != "unassessed" {
		t.Fatal("read-only facts invented or persisted a judgment")
	}
}

func TestClosureFactsAreBoundedAndKeepIdentitiesExact(t *testing.T) {
	_, store, identity, run := conditionAssessmentFixture(t, false, 0)
	ctx := context.Background()
	plan, _ := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	steps := []control.RunPlanStepInput{{StepID: plan.Steps[0].StepID, Status: "completed"}}
	for i := 0; i < 12; i++ {
		steps = append(steps, control.RunPlanStepInput{Step: fmt.Sprintf("Report target %d", i), Status: "pending"})
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "remaining scope", steps); err != nil {
		t.Fatal(err)
	}
	exit, invoked := 0, true
	for i := 0; i < 12; i++ {
		e := kernel.RunEvidence{ToolCallID: fmt.Sprintf("exact-reference-%02d", i), Kind: "verification", Status: "succeeded", StartedAt: 20, FinishedAt: 21,
			Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
			Command: &kernel.CommandEvidence{Command: "private-command-sentinel", CWD: "/workspace", ExitCode: exit,
				Binding: &verification.Binding{Version: 1, Criterion: "inspect", Target: fmt.Sprintf("%d-%s", i, strings.Repeat("界", 500))}}}
		raw, _ := json.Marshal(map[string]interface{}{"evidence": e})
		if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := store.RunClosureFacts(ctx, identity.TenantID, run.ID)
	if err != nil || !facts.Available || len(facts.OpenStepIDs) != 8 || facts.OpenStepsOmitted != 4 || len(facts.Evidence) != 8 || facts.EvidenceOmitted != 5 {
		t.Fatalf("unbounded facts: %+v %v", facts, err)
	}
	for i, item := range facts.Evidence {
		want := "check"
		if i > 0 {
			want = fmt.Sprintf("exact-reference-%02d", i-1)
		}
		if item.ID != want || len([]rune(item.Target)) > 241 {
			t.Fatalf("changed identity or unbounded target: %+v", item)
		}
	}
	raw, _ := json.Marshal(facts)
	if strings.Contains(string(raw), "private-command-sentinel") {
		t.Fatal("raw command escaped into closure facts")
	}
}

func TestClosureFactsStayWithinCurrentRunAndWorkUnit(t *testing.T) {
	_, store, identity, run := conditionAssessmentFixture(t, false, 1)
	ctx := context.Background()
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	exit, invoked := 0, true
	late := kernel.RunEvidence{ToolCallID: "outside-closed-window", Kind: "verification", Status: "succeeded", Invoked: &invoked, Process: &kernel.ToolProcessResult{Started: true, ExitCode: &exit},
		StartedAt: 20, FinishedAt: 21, Command: &kernel.CommandEvidence{ExitCode: exit, CWD: "/workspace", Binding: &verification.Binding{Version: 3, StepID: plan.Steps[0].StepID, Criterion: "marker is READY", Target: "marker.txt"}}}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": late})
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	facts, err := store.RunClosureFacts(ctx, identity.TenantID, run.ID)
	if err != nil || !facts.Available || len(facts.Evidence) != 1 || facts.Evidence[0].ID != "check" {
		t.Fatalf("wrong evidence window: %+v %v", facts, err)
	}
	if _, err := store.RunClosureFacts(ctx, "other-tenant", run.ID); err == nil {
		t.Fatal("foreign tenant read facts")
	}
	_, err = store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, []verification.Assessment{{Result: "met", EvidenceRefs: []string{"outside-closed-window"}, Reason: "Later."}})
	if err == nil {
		t.Fatal("receipt and acceptance disagreed on the closed window")
	}
	before, err := store.ResolveConditionAssessments(ctx, identity.TenantID, run.ID, nil)
	if err != nil || before.State != "unassessed" {
		t.Fatal("facts changed the condition")
	}
}

func TestAcceptedPlanSurvivesUnavailableClosureFacts(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 0)
	ctx := context.Background()
	// The Plan can still commit; malformed evidence must make its auxiliary
	// preview unavailable rather than pretending there is no remaining work.
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: json.RawMessage(`{"evidence":{}}`)}); err != nil {
		t.Fatal(err)
	}
	projection := daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{})
	ctx = tools.WithRunPlanProjection(ctx, projection)
	output, err := tools.NewUpdatePlanTool().Execute(map[string]interface{}{"_context": ctx, "plan": []interface{}{map[string]interface{}{"step": "Inspect marker", "status": "completed", "success_criteria": "marker is READY"}, map[string]interface{}{"step": "Report what remains", "status": "pending"}}})
	if err != nil {
		t.Fatal("auxiliary read falsely refused an accepted Plan:", err)
	}
	var receipt struct {
		Changed      bool                      `json:"changed"`
		ClosureFacts verification.ClosureFacts `json:"closure_facts"`
	}
	if json.Unmarshal([]byte(output), &receipt) != nil || !receipt.Changed || receipt.ClosureFacts.Available {
		t.Fatal(output)
	}
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil || len(plan.Steps) != 2 || plan.Steps[1].Status != "pending" {
		t.Fatal("accepted unfinished work was lost")
	}
}
