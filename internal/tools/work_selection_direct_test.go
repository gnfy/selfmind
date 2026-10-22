package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

var directClaimRoots = []executionenv.RootBinding{{Path: "/workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}}

// A same-channel bare confirmation must continue the waiting run in the same
// Main turn: work_select claims the parent at once and hands back the parent's
// resume context instead of deferring to a second run.
func TestWorkSelectClaimsSameDomainResumeInTurn(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "aws 生产发布", Channel: "cli"})
	targetRun, _ := store.StartRunWithOptions(ctx, targetTask, "cli", "aws生产发布，先预检再等我确认", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	if _, err := store.SyncRunPlan(ctx, person.TenantID, targetRun.ID, "release", []control.RunPlanStepInput{
		{Step: "read-only preflight", Status: "completed"},
		{Step: "recite the release steps after confirmation", Status: "in_progress", SuccessCriteria: "include the rollback step", VerificationRequired: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{TaskID: targetTask.ID, RunID: targetRun.ID, Summary: "Preflight passed; waiting for confirmation.", NextSteps: []string{"recite the release steps"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal([]llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Function: "read_file", Args: `{"path":"plan.json"}`}}},
		{Role: "tool", ToolCallID: "read", Content: "preflight target=release-A; ready=true"},
	})
	if err := store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: person.TenantID, PersonID: person.PersonID, TaskID: targetTask.ID, RunID: targetRun.ID,
		Outcome: "complete_turn", Snapshot: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, person.TenantID, targetRun.ID, "waiting_user")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "确认执行", Channel: "cli"})
	interactionRun, _ := store.StartRunWithOptions(ctx, interactionTask, "cli", "确认执行", control.StartRunOptions{ExecutionRoots: directClaimRoots})

	tool := NewWorkSelectTool(store)
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID, ExecutionLane: "main"}
	result, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": targetRun.ID, "_invocation_scope": scope})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Status, CommitMode, ThreadID, ResumeContext string
	}
	if err := json.Unmarshal([]byte(result), &struct {
		Status        *string `json:"status"`
		CommitMode    *string `json:"commit_mode"`
		ThreadID      *string `json:"thread_id"`
		ResumeContext *string `json:"resume_context"`
	}{&decoded.Status, &decoded.CommitMode, &decoded.ThreadID, &decoded.ResumeContext}); err != nil {
		t.Fatalf("decode %s: %v", result, err)
	}
	if decoded.Status != "committed" || decoded.CommitMode != "direct" || decoded.ThreadID != targetTask.ID {
		t.Fatalf("same-domain resume must commit directly: %s", result)
	}
	for _, want := range []string{"Preflight passed", "[x] read-only preflight", "[>] recite the release steps", "Preserve completed steps", "include the rollback step", "verification_required=true", "plan.json", "preflight target=release-A; ready=true"} {
		if !strings.Contains(decoded.ResumeContext, want) {
			t.Fatalf("resume context lacks %q:\n%s", want, decoded.ResumeContext)
		}
	}
	moved, _ := store.GetRun(ctx, person.TenantID, interactionRun.ID)
	if moved == nil || moved.TaskID != targetTask.ID || moved.ResumesRunID != targetRun.ID || moved.Status != "running" {
		t.Fatalf("interaction run must now continue the parent: %+v", moved)
	}
	parentPlan, err := store.LatestRunPlan(ctx, person.TenantID, targetRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	childPlan, err := store.LatestRunPlan(ctx, person.TenantID, interactionRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childPlan == nil || len(childPlan.Steps) != 2 || childPlan.Steps[0].StepID != parentPlan.Steps[0].StepID ||
		childPlan.Steps[0].SourceStepID != parentPlan.Steps[0].StepID || childPlan.Steps[1].SourceStepID != parentPlan.Steps[1].StepID {
		t.Fatalf("direct continuation must preserve logical step ids with exact source steps: %+v", childPlan)
	}
	var receipt struct {
		Plan    []planStepView `json:"plan"`
		Version int            `json:"plan_version"`
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil || receipt.Version != childPlan.Version || len(receipt.Plan) != len(childPlan.Steps) || receipt.Plan[1].StepID != childPlan.Steps[1].StepID {
		t.Fatalf("loop receipt must carry the accepted current-Run plan: %s", result)
	}
	events, _ := store.ListRunEvents(ctx, person.TenantID, person.PersonID, targetTask.ID, interactionRun.ID, 20)
	var committed, inherited bool
	for _, event := range events {
		switch event.Type {
		case "work.selection_committed":
			committed = strings.Contains(string(event.Payload), `"commit_mode":"direct"`)
		case "plan.updated":
			var projection struct {
				Version int `json:"plan_version"`
			}
			if err := json.Unmarshal(event.Payload, &projection); err != nil || projection.Version != childPlan.Version {
				t.Fatalf("inherited event must identify the accepted child Plan: %s", event.Payload)
			}
			inherited = strings.Contains(string(event.Payload), `"source":"resumed_run"`) && strings.Contains(string(event.Payload), "recite the release steps")
		}
	}
	if !committed || !inherited {
		t.Fatalf("direct claim must record the commit and restore the plan on the continued thread: %+v", events)
	}
	var closure struct {
		Facts *struct {
			Available bool `json:"available"`
			Version   int  `json:"plan_version"`
		} `json:"closure_facts"`
	}
	if err := json.Unmarshal([]byte(result), &closure); err != nil || closure.Facts == nil || !closure.Facts.Available || closure.Facts.Version != childPlan.Version {
		t.Fatalf("selection omitted the current accepted Plan's closure facts: %s", result)
	}
	// Repeating the same selection converges without a second claim.
	again, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": targetRun.ID, "_invocation_scope": scope})
	if err != nil || !strings.Contains(again, `"commit_mode":"direct"`) {
		t.Fatalf("repeated selection: %v %s", err, again)
	}
}

// One pre-effect correction stays possible after a direct claim: the run moves
// to the corrected same-domain parent and the wrong parent is unclaimed again.
func TestWorkSelectCorrectsDirectClaimBeforeEffects(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	makeParked := func(title string, roots []executionenv.RootBinding) (*control.Task, *control.Run) {
		task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: title, Channel: "cli"})
		run, _ := store.StartRunWithOptions(ctx, task, "cli", title, control.StartRunOptions{ExecutionRoots: roots})
		_ = store.FinishRun(ctx, person.TenantID, run.ID, "interrupted")
		return task, run
	}
	wrongTask, wrong := makeParked("wrong release", directClaimRoots)
	correctTask, correct := makeParked("correct release", directClaimRoots)
	wrongPlan, err := store.SyncRunPlan(ctx, person.TenantID, wrong.ID, "wrong", []control.RunPlanStepInput{{Step: "inspect wrong target", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	correctPlan, err := store.SyncRunPlan(ctx, person.TenantID, correct.ID, "correct", []control.RunPlanStepInput{{Step: "inspect correct target", Status: "in_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	_, foreign := makeParked("foreign release", []executionenv.RootBinding{{Path: "/elsewhere", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace}})
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "continue the release", Channel: "cli"})
	interactionRun, _ := store.StartRunWithOptions(ctx, interactionTask, "cli", "continue the release", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	tool := NewWorkSelectTool(store)
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID, ExecutionLane: "main"}

	if first, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": wrong.ID, "_invocation_scope": scope}); err != nil || !strings.Contains(first, `"commit_mode":"direct"`) {
		t.Fatalf("first claim: %v %s", err, first)
	}
	if _, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": foreign.ID, "_invocation_scope": scope}); err == nil || !strings.Contains(err.Error(), "different scope") {
		t.Fatalf("a cross-domain correction after a claim must be refused with guidance, got %v", err)
	}
	corrected, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": correct.ID, "_invocation_scope": scope})
	if err != nil || !strings.Contains(corrected, `"commit_mode":"direct"`) || !strings.Contains(corrected, correct.ID) {
		t.Fatalf("same-domain correction: %v %s", err, corrected)
	}
	moved, _ := store.GetRun(ctx, person.TenantID, interactionRun.ID)
	if moved == nil || moved.TaskID != correctTask.ID || moved.ResumesRunID != correct.ID {
		t.Fatalf("run must continue the corrected parent: %+v", moved)
	}
	currentPlan, err := store.LatestRunPlan(ctx, person.TenantID, interactionRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentPlan == nil || len(currentPlan.Steps) != 1 ||
		currentPlan.Steps[0].SourceStepID != correctPlan.Plan.Steps[0].StepID ||
		currentPlan.Steps[0].SourceStepID == wrongPlan.Plan.Steps[0].StepID ||
		currentPlan.Steps[0].Step != "inspect correct target" {
		t.Fatalf("corrected claim retained the abandoned parent's plan: %+v", currentPlan)
	}
	wrongCandidates, _ := store.ListUnresolvedRuns(ctx, person.TenantID, person.PersonID, wrongTask.ID, 10)
	if len(wrongCandidates) != 1 || wrongCandidates[0].ID != wrong.ID {
		t.Fatalf("the wrong parent must be unclaimed again: %+v", wrongCandidates)
	}
	correctCandidates, _ := store.ListUnresolvedRuns(ctx, person.TenantID, person.PersonID, correctTask.ID, 10)
	if len(correctCandidates) != 0 {
		t.Fatalf("the corrected parent must be claimed: %+v", correctCandidates)
	}
}

func TestWorkSelectCorrectionToParentWithoutPlanClearsAbandonedPlan(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	makeParked := func(title string) (*control.Task, *control.Run) {
		task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: title, Channel: "cli"})
		run, _ := store.StartRunWithOptions(ctx, task, "cli", title, control.StartRunOptions{ExecutionRoots: directClaimRoots})
		_ = store.FinishRun(ctx, person.TenantID, run.ID, "interrupted")
		return task, run
	}
	_, wrong := makeParked("wrong work")
	_, correct := makeParked("correct work")
	if _, err := store.SyncRunPlan(ctx, person.TenantID, wrong.ID, "wrong", []control.RunPlanStepInput{{Step: "inspect wrong target", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "continue work", Channel: "cli"})
	interaction, _ := store.StartRunWithOptions(ctx, interactionTask, "cli", "continue work", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	tool := NewWorkSelectTool(store)
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interaction.ID, ExecutionLane: "main"}
	if _, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": wrong.ID, "_invocation_scope": scope}); err != nil {
		t.Fatal(err)
	}
	abandoned, err := store.LatestRunPlan(ctx, person.TenantID, interaction.ID)
	if err != nil || abandoned == nil || len(abandoned.Steps) != 1 {
		t.Fatalf("first claim did not import a plan: %+v, %v", abandoned, err)
	}
	abandonedUnitID := abandoned.Steps[0].WorkUnitID
	if _, err := tool.Execute(map[string]interface{}{"action": "resume", "run_id": correct.ID, "_invocation_scope": scope}); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.LatestRunPlan(ctx, person.TenantID, interaction.ID)
	if err != nil || cleared == nil || len(cleared.Steps) != 0 {
		t.Fatalf("corrected parent without a plan must clear the abandoned plan: %+v, %v", cleared, err)
	}
	fresh, err := store.SyncRunPlan(ctx, person.TenantID, interaction.ID, "fresh", []control.RunPlanStepInput{{Step: "verify corrected target", Status: "completed", SuccessCriteria: "check passes", VerificationRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Plan.Steps) != 1 || fresh.Plan.Steps[0].Status != "in_progress" ||
		fresh.Plan.Steps[0].SourceStepID != "" || fresh.Plan.Steps[0].WorkUnitID == abandonedUnitID {
		t.Fatalf("first new plan must have fresh identities and require current-run verification: %+v", fresh.Plan)
	}
}

func TestWorkSelectLegacyDisplayDoesNotClaimAnAcceptedVersion(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "legacy-owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "legacy", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRunWithOptions(ctx, task, "cli", "prior", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, RunID: parent.ID, Type: "plan.updated", Channel: "cli", Payload: mustToolJSON(map[string]interface{}{"plan": []map[string]string{{"step": "legacy remaining work", "status": "in_progress"}}})}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, person.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "reply", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, other, "cli", "continue", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewWorkSelectTool(store).Execute(map[string]interface{}{"action": "resume", "run_id": parent.ID, "_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: other.ID, RunID: child.ID, ExecutionLane: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListRunEvents(ctx, person.TenantID, person.PersonID, task.ID, child.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "plan.updated" {
			found = true
			var payload map[string]interface{}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if _, exists := payload["plan_version"]; exists {
				t.Fatalf("legacy event claimed a durable version: %s", event.Payload)
			}
		}
	}
	if !found {
		t.Fatal("legacy display disappeared")
	}
}
