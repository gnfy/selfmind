package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// TestQueueUserInputMovesExactSteeringIntoIndependentWork pins the Main-facing
// seam: after Main sees one live input, it can classify that exact server-issued
// input as independent without repeating its prose or changing the active plan.
func TestQueueUserInputMovesExactSteeringIntoIndependentWork(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "active work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "active work")
	if err != nil {
		t.Fatal(err)
	}
	steering, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "weixin", Platform: "weixin",
		PlatformUserID: "wx-user", Content: "prepare an unrelated weekly report",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, steering.ID); err != nil || !ok {
		t.Fatalf("consume steering: ok=%v err=%v", ok, err)
	}

	tool := NewQueueUserInputTool(store)
	result, err := tool.Execute(map[string]interface{}{
		"input_id": steering.ID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: task.ID, RunID: run.ID, ExecutionLane: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"status":"queued"`) {
		t.Fatalf("tool result = %s", result)
	}
	// Provider/tool retries converge on the same durable row.
	if _, err := tool.Execute(map[string]interface{}{
		"input_id": steering.ID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: task.ID, RunID: run.ID, ExecutionLane: "main",
		},
	}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Content != steering.Content {
		t.Fatalf("queued work = %+v", queued)
	}
	if queued[0].TaskID != "" || queued[0].ReplyToRunID != "" {
		t.Fatalf("independent input inherited active ownership: %+v", queued[0])
	}
	if queued[0].Platform != "weixin" || queued[0].PlatformUserID != "wx-user" {
		t.Fatalf("source delivery metadata was not preserved: %+v", queued[0])
	}
}

func TestQueueUserInputCanTargetExactHistoricalParent(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	historicalTask, _ := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace-old", Title: "old release", Channel: "cli",
	})
	historicalRun, _ := store.StartRunWithOptions(ctx, historicalTask, "cli", "old release", control.StartRunOptions{})
	if err := store.FinishRun(ctx, identity.TenantID, historicalRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	activeTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "active work", Channel: "cli"})
	activeRun, _ := store.StartRun(ctx, activeTask, "cli", "active work")
	steering, err := store.AcceptSteering(ctx, control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: activeRun.ID, TaskID: activeTask.ID,
		Channel: "weixin", Platform: "weixin", PlatformUserID: "wx-user", Content: "continue the old release",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.ConsumeSteeringByID(ctx, identity.TenantID, activeRun.ID, steering.ID)

	tool := NewQueueUserInputTool(store)
	if _, err := tool.Execute(map[string]interface{}{
		"input_id": steering.ID, "resumes_run_id": historicalRun.ID,
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: activeTask.ID, RunID: activeRun.ID, ExecutionLane: "main",
		},
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].ReplyToRunID != historicalRun.ID || queued[0].TaskID != historicalTask.ID {
		t.Fatalf("exact continuation queue = %+v", queued)
	}
	if queued[0].WorkspaceID != historicalTask.WorkspaceID {
		t.Fatalf("parent execution domain was not frozen: %+v", queued[0])
	}
}
