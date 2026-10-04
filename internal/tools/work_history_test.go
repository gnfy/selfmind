package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

func TestWorkSearchIsPersonScopedAndReturnsStructuredCards(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	other, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "RUQX-767 production release", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "release PHP services")
	_ = store.FinishRun(ctx, person.TenantID, run.ID, "interrupted")
	currentTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "RUQX-767 progress question", Channel: "weixin"})
	currentRun, _ := store.StartRun(ctx, currentTask, "weixin", "what is the RUQX-767 progress")
	otherTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: other.TenantID, PersonID: other.PersonID, Title: "RUQX-767 private other-person work", Channel: "weixin"})
	_, _ = store.StartRun(ctx, otherTask, "weixin", "must never appear")

	tool := NewWorkSearchTool(store)
	result, err := tool.Execute(map[string]interface{}{
		"query":             "RUQX-767",
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: currentTask.ID, RunID: currentRun.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, task.ID) || !strings.Contains(result, run.ID) {
		t.Fatalf("missing structured work card: %s", result)
	}
	if strings.Contains(result, otherTask.ID) || strings.Contains(result, "must never appear") {
		t.Fatalf("cross-person history leaked: %s", result)
	}
	if strings.Contains(result, currentTask.ID) || strings.Contains(result, currentRun.ID) {
		t.Fatalf("current interaction was returned as its own history hit: %s", result)
	}
	if strings.Contains(result, "transcript") {
		t.Fatalf("work search returned transcript-shaped data: %s", result)
	}
}

func TestWorkInspectReturnsBoundedRunStateWithoutRawEventContent(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "release", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "release services")
	if _, err := store.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Visibility: "task",
		Payload: json.RawMessage(`{"tool":"read_file","status":"succeeded","content":"RAW PRIVATE TRANSCRIPT"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, control.Handoff{TaskID: task.ID, RunID: run.ID, Summary: "release prepared", NextSteps: []string{"verify deployment"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, control.Artifact{TaskID: task.ID, RunID: run.ID, Kind: "file", Name: "release record", URI: "artifact://release-record"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncRunPlan(ctx, person.TenantID, run.ID, "release safely", []control.RunPlanStepInput{{Step: "deploy", Status: "completed"}, {Step: "verify deployment", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}

	tool := NewWorkInspectTool(store)
	result, err := tool.Execute(map[string]interface{}{
		"run_id":            run.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, RunID: "run_current"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"release prepared", "verify deployment", "artifact://release-record", "read_file", "work_select", "did not attach"} {
		if !strings.Contains(result, want) {
			t.Fatalf("work inspection missing %q: %s", want, result)
		}
	}
	if strings.Contains(result, "RAW PRIVATE TRANSCRIPT") {
		t.Fatalf("raw event payload leaked: %s", result)
	}
	current, err := tool.Execute(map[string]interface{}{
		"run_id":            run.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, RunID: run.ID},
	})
	if err != nil || !strings.Contains(current, "already selected") || strings.Contains(current, "did not attach") {
		t.Fatalf("current Run inspection gave historical-selection guidance: result=%s err=%v", current, err)
	}
}

// Inspection proposes a resume only when work_select accepts it. The notice
// proposed one for every past run: qwen inspected a finished plan before
// continuing it from another endpoint, followed the notice, and was refused.
func TestWorkInspectProposesOnlyAResumeThatSelectionAccepts(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	inspect, selection := NewWorkInspectTool(store), NewWorkSelectTool(store)
	for _, tc := range []struct {
		name     string
		statuses []string // the runs of one piece of work, oldest first
		resume   int      // the run a resume should target; -1 for none
	}{
		{"finished", []string{"done"}, -1},
		{"failed", []string{"failed"}, -1},
		{"interrupted", []string{"interrupted"}, 0},
		{"waiting", []string{"waiting_user"}, 0},
		{"superseded", []string{"done", "interrupted"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "config plan " + tc.name, Channel: "cli"})
			var runs []*control.Run
			for _, status := range tc.statuses {
				run, _ := store.StartRun(ctx, work, "cli", "plan the config refactor")
				_ = store.FinishRun(ctx, person.TenantID, run.ID, status)
				runs = append(runs, run)
			}
			currentTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "继续", Channel: "weixin"})
			current, _ := store.StartRun(ctx, currentTask, "weixin", "继续")
			scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: currentTask.ID, RunID: current.ID}

			result, err := inspect.Execute(map[string]interface{}{"run_id": runs[0].ID, "_invocation_scope": scope})
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Notice string `json:"selection_notice"`
			}
			if err := json.Unmarshal([]byte(result), &out); err != nil {
				t.Fatal(err)
			}
			proposes := strings.Contains(out.Notice, "action resume")
			target := runs[0]
			if tc.resume >= 0 {
				target = runs[tc.resume]
			}
			switch {
			case tc.resume < 0 && proposes:
				t.Fatalf("notice proposes resuming a %s run: %s", tc.statuses[0], out.Notice)
			case tc.resume >= 0 && !proposes:
				t.Fatalf("notice does not propose the resume: %s", out.Notice)
			case tc.resume > 0 && !strings.Contains(out.Notice, target.ID):
				t.Fatalf("notice does not name the resumable run %s: %s", target.ID, out.Notice)
			}

			_, err = selection.Execute(map[string]interface{}{"action": "resume", "run_id": target.ID, "_invocation_scope": scope})
			var refusal interface{ ToolErrorCode() string }
			refused := errors.As(err, &refusal) && refusal.ToolErrorCode() == "work_run_not_resumable"
			if tc.resume >= 0 && err != nil {
				t.Fatalf("selection refused the resume the notice proposed: %v", err)
			}
			if tc.resume < 0 && !refused {
				t.Fatalf("resume of a %s run: err=%v, want the refusal the notice avoided", tc.statuses[0], err)
			}
		})
	}
}

func TestWorkInspectRejectsAnotherPersonRun(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alice, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	bob, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "bob", "Bob")
	task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: bob.TenantID, PersonID: bob.PersonID, Title: "private", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "private")
	tool := NewWorkInspectTool(store)
	if _, err := tool.Execute(map[string]interface{}{
		"run_id":            run.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: alice.TenantID, PersonID: alice.PersonID, RunID: "run_current"},
	}); err == nil {
		t.Fatal("cross-person inspection must fail closed")
	}
}
