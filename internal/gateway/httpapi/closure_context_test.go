package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestClosureContextUsesExecutingRunWithoutParent(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 1)
	task, err := store.GetTask(context.Background(), identity.TenantID, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(context.Background(), task, run, nil, "cli", "cli", "continue inspection", attachContextFull, nil)
	plan, err := store.LatestRunPlan(context.Background(), identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.PlanVersion != plan.Version || len(selected.Plan) != len(plan.Steps) {
		t.Fatalf("executing Run's accepted Plan was withheld without a historical parent: version=%d steps=%d", selected.PlanVersion, len(selected.Plan))
	}
	prompt := selected.Prompt(20000)
	if !strings.Contains(prompt, `"id":"check"`) || !strings.Contains(prompt, "Read-only closure facts") {
		t.Fatal("executing Run's valid evidence is hidden unless Main mutates update_plan")
	}
}

func TestClosureContextUsesChildFactsRatherThanParent(t *testing.T) {
	daemon, store, identity, parent := conditionAssessmentFixture(t, false, 1)
	ctx := context.Background()
	task, err := store.GetTask(ctx, identity.TenantID, parent.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue report", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, child, nil, "cli", "cli", "continue report", attachContextFull, parent)
	prompt := selected.Prompt(20000)
	if !strings.Contains(prompt, "Read-only closure facts") || !strings.Contains(prompt, `"available":true`) {
		t.Fatal("exact continuation has no current-Run closure projection")
	}
	if strings.Contains(prompt, `"id":"check"`) {
		t.Fatal("parent evidence was advertised as directly citeable in child")
	}
}

func TestClosureSnapshotRetainsPlanWhenEvidenceCannotBeRead(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 0)
	ctx := context.Background()
	task, err := store.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: json.RawMessage(`{"evidence":{}}`)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadRunPlanSnapshot(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || snapshot.Plan == nil || snapshot.ClosureFacts.Available {
		t.Fatalf("lost accepted Plan or invented available facts: %+v %v", snapshot, err)
	}
	if _, err := store.RunClosureFacts(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("strict facts caller lost the underlying preview error")
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, run, nil, "cli", "cli", "report", attachContextNone, nil)
	if selected.PlanVersion != snapshot.Plan.Version || len(selected.Plan) != 1 || !strings.Contains(selected.Prompt(20000), `"available":false`) {
		t.Fatal("auxiliary failure erased accepted Plan or looked like no work")
	}
}

func TestClosureSnapshotRejectsForeignAndMissingPerson(t *testing.T) {
	_, store, identity, run := conditionAssessmentFixture(t, false, 0)
	for _, scope := range []struct{ tenant, person string }{{identity.TenantID, "other-person"}, {"other-tenant", identity.PersonID}, {identity.TenantID, ""}} {
		if _, err := store.ReadRunPlanSnapshot(context.Background(), scope.tenant, scope.person, run.ID); err == nil {
			t.Fatalf("snapshot exposed outside scope: %+v", scope)
		}
	}
}

func TestClosureContextReflectsNewPlanAndInvalidation(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 0)
	ctx := context.Background()
	task, err := store.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ReadRunPlanSnapshot(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || len(first.ClosureFacts.Evidence) != 1 {
		t.Fatalf("missing initial control: %+v %v", first, err)
	}
	step := first.Plan.Steps[0].StepID
	mutation := kernel.RunEvidence{ToolCallID: "changed-input", ToolName: "write_file", Kind: "mutation", Status: "succeeded", StartedAt: 20, FinishedAt: 21, Files: []kernel.FileEffect{{Path: "/workspace/marker.txt", BeforeSHA256: "before", AfterSHA256: "after"}}}
	if _, err := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: mustJSON(map[string]interface{}{"evidence": mutation})}); err != nil {
		t.Fatal(err)
	}
	update, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "recheck changed input", []control.RunPlanStepInput{{StepID: step, Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, task, run, nil, "cli", "cli", "recheck", attachContextNone, nil)
	if selected.PlanVersion != update.Plan.Version || selected.Plan[0].Status != "in_progress" {
		t.Fatal("context retained an earlier accepted Plan")
	}
	prompt := selected.Prompt(20000)
	if strings.Contains(prompt, `"id":"check"`) {
		t.Fatal("changed input kept an old check citeable")
	}
	if !strings.Contains(prompt, `"open_step_ids":["`+step+`"]`) {
		t.Fatal("open work and evidence disagree")
	}
}

func TestClosureContextDoesNotExposeUnrelatedRun(t *testing.T) {
	daemon, store, identity, run := conditionAssessmentFixture(t, false, 0)
	ctx := context.Background()
	other, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Unrelated work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	selected := daemon.coordinator().selectedTaskRuntimeContextWithMode(ctx, other, run, nil, "cli", "cli", "new task", attachContextNone, nil)
	if len(selected.Plan) > 0 || strings.Contains(selected.Prompt(20000), `"id":"check"`) {
		t.Fatal("another task's accepted Plan entered current context")
	}
}
