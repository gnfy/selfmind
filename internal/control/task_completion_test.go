package control

import (
	"context"
	"errors"
	"testing"
)

func TestCompleteTaskByUserRefusesPendingControlObjects(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
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
	timeline := NewWorkTimeline(store)
	before, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(before) != 1 || before[0].RunID != run.ID || before[0].Activity != ThreadActivityNeedsAttention {
		t.Fatalf("attention before completion=%+v err=%v", before, err)
	}

	result, err := store.CompleteTaskByUser(ctx, identity.TenantID, identity.PersonID, task.ID)
	if !errors.Is(err, ErrAttentionPendingControl) || result.Changed {
		t.Fatalf("completion over pending controls: result=%+v err=%v, want ErrAttentionPendingControl", result, err)
	}
	after, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(after) != 1 || after[0].RunID != run.ID || after[0].Activity != ThreadActivityNeedsAttention {
		t.Fatalf("refused completion changed attention: before=%+v after=%+v err=%v", before, after, err)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "interrupted" {
		t.Fatalf("historical run was rewritten: %+v err=%v", storedRun, err)
	}
	storedQueue, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || storedQueue == nil || storedQueue.Status != QueueStatusQueued {
		t.Fatalf("refused completion rewrote queued work: %+v err=%v", storedQueue, err)
	}
	var pendingApprovals, pendingClarifies int
	if err := store.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM approval_requests WHERE thread_id=? AND status='pending'),
		(SELECT COUNT(*) FROM clarify_requests WHERE thread_id=? AND status='pending')`,
		task.ID, task.ID).Scan(&pendingApprovals, &pendingClarifies); err != nil {
		t.Fatal(err)
	}
	if pendingApprovals != 1 || pendingClarifies != 1 {
		t.Fatalf("refused completion rewrote pending controls: approvals=%d clarifies=%d", pendingApprovals, pendingClarifies)
	}
}

func TestCompleteTaskByUserDismissesParkedRunWithWorkEvidence(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "interrupted mid-edit", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "edit the config")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-1", ToolName: "terminal", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	timeline := NewWorkTimeline(store)
	before, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(before) != 1 || before[0].RunID != run.ID || before[0].Activity != ThreadActivityResumable {
		t.Fatalf("attention before completion=%+v err=%v", before, err)
	}

	result, err := store.CompleteTaskByUser(ctx, identity.TenantID, identity.PersonID, task.ID)
	if err != nil || !result.Changed {
		t.Fatalf("completion result=%+v err=%v", result, err)
	}
	after, err := timeline.Attention(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(after) != 0 {
		t.Fatalf("dismissed run remains in Attention: %+v err=%v", after, err)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "interrupted" {
		t.Fatalf("historical run was rewritten: %+v err=%v", storedRun, err)
	}
	candidates, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.TaskID == task.ID {
			t.Fatalf("dismissed task leaked into implicit continuation: %+v", candidate)
		}
	}
}

func TestCompleteTaskByUserRefusesRunningWork(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
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
