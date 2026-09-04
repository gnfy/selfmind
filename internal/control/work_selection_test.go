package control

import (
	"context"
	"testing"
)

func TestEnqueueSelectedContinuationFreezesParentDomain(t *testing.T) {
	ctx := context.Background()
	store, identity, _, sourceRun := newRecoveryFixture(t)
	parentTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace-parent", Title: "parent", Channel: "cli"})
	parentRun, _ := store.StartRun(ctx, parentTask, "cli", "parent work")
	_ = store.FinishRun(ctx, identity.TenantID, parentRun.ID, "interrupted")
	queued, err := store.EnqueueSelectedContinuation(ctx, identity.TenantID, identity.PersonID, sourceRun.ID, parentRun.ID, QueuedTask{
		Platform: "weixin", PlatformUserID: "wx-user", Channel: "weixin", Content: "continue parent with this exact request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.TaskID != parentTask.ID || queued.ReplyToRunID != parentRun.ID || queued.WorkspaceID != parentRun.WorkspaceID {
		t.Fatalf("queued continuation = %+v", queued)
	}
	if queued.IdempotencyKey != "work-selection:"+sourceRun.ID+":"+parentRun.ID {
		t.Fatalf("idempotency key = %q", queued.IdempotencyKey)
	}
	retry, err := store.EnqueueSelectedContinuation(ctx, identity.TenantID, identity.PersonID, sourceRun.ID, parentRun.ID, QueuedTask{Content: "ignored retry"})
	if err != nil || retry.ID != queued.ID || retry.Content != queued.Content {
		t.Fatalf("retry = %+v err=%v", retry, err)
	}
}

func TestRunSelectionEffectBoundaryIgnoresReadOnlyAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	for _, entry := range []ToolLedgerEntry{
		{RunID: run.ID, ToolCallID: "read", ToolName: "work_inspect", ArgsHash: "a", RetryClass: "read_only"},
		{RunID: run.ID, ToolCallID: "select", ToolName: "work_select", ArgsHash: "b", RetryClass: "idempotent"},
		{RunID: run.ID, ToolCallID: "plan", ToolName: "update_plan", ArgsHash: "c", RetryClass: "idempotent"},
	} {
		claim, err := store.ClaimToolDispatch(ctx, identity.TenantID, entry)
		if err != nil || !claim.Execute {
			t.Fatalf("claim %+v: %+v %v", entry, claim, err)
		}
		_ = store.RecordToolOutcome(ctx, identity.TenantID, run.ID, entry.ToolCallID, true)
	}
	blocked, reason, err := store.RunSelectionEffectBoundary(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || blocked {
		t.Fatalf("safe selection boundary = %v %q err=%v", blocked, reason, err)
	}
	claim, err := store.ClaimToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "write", ToolName: "patch", ArgsHash: "d", RetryClass: "idempotent",
	})
	if err != nil || !claim.Execute {
		t.Fatalf("write claim: %+v %v", claim, err)
	}
	_ = store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "write", true)
	blocked, reason, err = store.RunSelectionEffectBoundary(ctx, identity.TenantID, identity.PersonID, run.ID)
	if err != nil || !blocked || reason != "tool_effect" {
		t.Fatalf("write boundary = %v %q err=%v", blocked, reason, err)
	}
}

func TestProjectInteractionTaskHidesOnlySingleRunLabel(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	if err := store.ProjectInteractionTask(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if got == nil || got.Kind != ThreadKindInteraction || got.Visibility != ThreadVisibilityUnlisted {
		t.Fatalf("interaction task = %+v", got)
	}
	if current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); current == nil || current.ID != task.ID {
		t.Fatalf("presentation projection must not hide an actively executing Run: %+v", current)
	}
}
