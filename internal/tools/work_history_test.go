package tools

import (
	"context"
	"encoding/json"
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
	if _, err := store.SyncRunPlan(ctx, person.TenantID, run.ID, "release safely", []control.RunPlanStepInput{{Step: "verify deployment", Status: "in_progress"}}); err != nil {
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
	for _, want := range []string{"release prepared", "verify deployment", "artifact://release-record", "read_file"} {
		if !strings.Contains(result, want) {
			t.Fatalf("work inspection missing %q: %s", want, result)
		}
	}
	if strings.Contains(result, "RAW PRIVATE TRANSCRIPT") {
		t.Fatalf("raw event payload leaked: %s", result)
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
