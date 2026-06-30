package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
	artifact, err := store.SaveArtifact(ctx, Artifact{
		TaskID: task.ID,
		RunID:  run.ID,
		Kind:   "file",
		Name:   "server.go",
		URI:    "internal/gateway/httpapi/server.go",
	})
	if err != nil {
		t.Fatalf("SaveArtifact failed: %v", err)
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
	artifacts, err := store.ListTaskArtifacts(ctx, task.ID, 10)
	if err != nil {
		t.Fatalf("ListTaskArtifacts failed: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != artifact.ID {
		t.Fatalf("artifacts mismatch: %+v", artifacts)
	}
}

func TestStoreRuntimeDeliveryAndInterruptFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Long run",
		Channel:  "telegram",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "telegram", "do work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRunHeartbeat(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestRunCancel(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RunCancelRequested(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("expected cancel flag")
	}

	delivery, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       "weixin",
		PlatformUserID: "wx-user",
		Channel:        "wx-chat",
		TaskID:         task.ID,
		RunID:          run.ID,
		Content:        "done",
		MaxAttempts:    2,
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != delivery.ID {
		t.Fatalf("due deliveries = %+v", due)
	}
	if due[0].PlatformUserID != "wx-user" || due[0].Channel != "wx-chat" {
		t.Fatalf("delivery recipient was not preserved: %+v", due[0])
	}
	if err := store.MarkDeliveryAttempt(ctx, delivery.ID, false, "network token=secret", time.Now()); err != nil {
		t.Fatal(err)
	}

	count, err := store.MarkInterruptedRuns(ctx, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("interrupted count = %d, want 1", count)
	}
	updated, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ActiveRunID != "" || updated.Status != "interrupted" {
		t.Fatalf("task after interrupt = %+v", updated)
	}
}

func TestStoreApprovalFlow(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "tenant-a", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Needs approval",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID:         identity.TenantID,
		PersonID:         identity.PersonID,
		TaskID:           task.ID,
		ActionType:       "shell",
		Payload:          []byte(`{"command":"rm file"}`),
		RequestedChannel: "cli",
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest failed: %v", err)
	}
	if approval.ID == "" || approval.Status != "pending" {
		t.Fatalf("unexpected approval: %+v", approval)
	}
	pending, err := store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != approval.ID {
		t.Fatalf("pending approvals = %+v", pending)
	}
	approved, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "wechat")
	if err != nil {
		t.Fatalf("RespondApprovalRequest failed: %v", err)
	}
	if approved.Status != "approved" || approved.ApprovedChannel != "wechat" {
		t.Fatalf("unexpected approved request: %+v", approved)
	}
	pending, err = store.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending approvals, got %+v", pending)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "rejected", "cli"); err == nil {
		t.Fatal("expected duplicate response to fail")
	}
}

func TestCurrentTaskForChannelIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const tenant, person = "t1", "p1"
	taskA, err := store.CreateTask(ctx, TaskCreate{TenantID: tenant, PersonID: person, Title: "A", Channel: "cli-A"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: tenant, PersonID: person, Title: "B", Channel: "cli-B"})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Each channel resolves to its OWN task — no cross-session bleed, even
	// though the single per-person current_task pointer now points at B.
	gotA, err := store.CurrentTaskForChannel(ctx, tenant, person, "cli-A")
	if err != nil || gotA == nil || gotA.ID != taskA.ID {
		t.Fatalf("channel cli-A should resolve to task A, got %+v (err %v)", gotA, err)
	}
	gotB, err := store.CurrentTaskForChannel(ctx, tenant, person, "cli-B")
	if err != nil || gotB == nil || gotB.ID != taskB.ID {
		t.Fatalf("channel cli-B should resolve to task B, got %+v (err %v)", gotB, err)
	}

	// A finished task is no longer the channel's current task.
	if err := store.UpdateTaskStatus(ctx, tenant, taskA.ID, "done", "", nil); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if got, _ := store.CurrentTaskForChannel(ctx, tenant, person, "cli-A"); got != nil {
		t.Fatalf("finished task should not be the channel's current task, got %+v", got)
	}
}
