package control

import (
	"context"
	"errors"
	"testing"
)

func TestCompleteTaskByUserClosesLabelWithoutRewritingRunHistory(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finished elsewhere", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "do the work")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "interrupted", "unfinished", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		Question: "continue?",
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Platform: "cli", Channel: "cli", Content: "resume parked work",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.CompleteTaskByUser(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.ExpiredApprovals != 1 || result.ExpiredClarifications != 1 || result.CancelledQueueRows != 1 {
		t.Fatalf("completion result = %+v", result)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || got == nil || got.Status != "done" {
		t.Fatalf("task after completion = %+v err=%v", got, err)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "interrupted" {
		t.Fatalf("historical run was rewritten: %+v err=%v", storedRun, err)
	}
	storedQueue, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || storedQueue == nil || storedQueue.Status != QueueStatusCancelled {
		t.Fatalf("queued continuation survived completion: %+v err=%v", storedQueue, err)
	}
	candidates, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.TaskID == task.ID {
			t.Fatalf("completed task leaked into implicit continuation: %+v", candidate)
		}
	}
}

func TestCompleteTaskByUserRefusesRunningWork(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "still running"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRun(ctx, task, "cli", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTaskByUser(ctx, identity.TenantID, identity.PersonID, task.ID); !errors.Is(err, ErrTaskHasLiveWork) {
		t.Fatalf("running task completion error = %v", err)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || got == nil || got.Status == "done" {
		t.Fatalf("running task was completed: %+v err=%v", got, err)
	}
}
