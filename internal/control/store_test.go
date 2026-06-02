package control

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStoreIdentityWorkspaceTaskFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount failed: %v", err)
	}
	if identity.TenantID != "tenant-a" || identity.PersonID == "" || identity.AccountID == "" {
		t.Fatalf("unexpected identity: %+v", identity)
	}

	again, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount second call failed: %v", err)
	}
	if again.PersonID != identity.PersonID || again.AccountID != identity.AccountID {
		t.Fatalf("identity was not stable: first=%+v second=%+v", identity, again)
	}

	ws, err := store.RegisterWorkspace(ctx, Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "repo",
		LocalPath:     filepath.Join(t.TempDir(), "repo"),
	})
	if err != nil {
		t.Fatalf("RegisterWorkspace failed: %v", err)
	}
	currentWS, err := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatalf("CurrentWorkspace failed: %v", err)
	}
	if currentWS == nil || currentWS.ID != ws.ID {
		t.Fatalf("current workspace mismatch: %+v vs %+v", currentWS, ws)
	}

	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: ws.ID,
		Title:       "Implement sync",
		Channel:     "cli",
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	run, err := store.StartRun(ctx, task, "wechat", "continue task")
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started"}); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}
	if err := store.RecordChannelMessage(ctx, *identity, "wechat", task.ID, "user", "progress?"); err != nil {
		t.Fatalf("RecordChannelMessage failed: %v", err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatalf("FinishRun failed: %v", err)
	}
	finishedTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask after FinishRun failed: %v", err)
	}
	if finishedTask.ActiveRunID != "" {
		t.Fatalf("active run was not cleared: %+v", finishedTask)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "running", "half done", []string{"finish tests"}); err != nil {
		t.Fatalf("UpdateTaskStatus failed: %v", err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{TaskID: task.ID, Summary: "handoff summary"}); err != nil {
		t.Fatalf("SaveHandoff failed: %v", err)
	}

	currentTask, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatalf("CurrentTask failed: %v", err)
	}
	if currentTask == nil || currentTask.ID != task.ID || currentTask.CurrentSummary != "half done" {
		t.Fatalf("current task mismatch: %+v", currentTask)
	}
	handoff, err := store.LatestHandoff(ctx, task.ID)
	if err != nil {
		t.Fatalf("LatestHandoff failed: %v", err)
	}
	if handoff == nil || handoff.Summary != "handoff summary" {
		t.Fatalf("handoff mismatch: %+v", handoff)
	}
}
