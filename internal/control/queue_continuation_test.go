package control

import (
	"context"
	"testing"
)

func TestQueuedReplyFollowsClaimedParentWithoutLosingInput(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", Title: "work A", Channel: "cli-a"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "cli-a", "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	queued := QueuedTask{TenantID: "default", PersonID: "person", Channel: "im", Platform: "weixin", Content: "additional requirement", ReplyToRunID: parent.ID}
	resolved, wait, err := store.ResolveQueuedContinuation(ctx, queued)
	if err != nil || !wait || resolved.ReplyToRunID != parent.ID || resolved.TaskID != task.ID {
		t.Fatalf("running parent: resolved=%+v wait=%v err=%v", resolved, wait, err)
	}
	if err := store.FinishRun(ctx, "default", parent.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli-a", "automatic recovery", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	resolved, wait, err = store.ResolveQueuedContinuation(ctx, queued)
	if err != nil || !wait || resolved.TaskID != task.ID {
		t.Fatalf("running child: resolved=%+v wait=%v err=%v", resolved, wait, err)
	}
	system := queued
	system.IdempotencyKey = "run-recovery:" + parent.ID + ":continue"
	system.Class = QueueClassRecovery
	resolvedSystem, systemWait, systemErr := store.ResolveQueuedContinuation(ctx, system)
	if systemErr != nil || systemWait || resolvedSystem.ReplyToRunID != parent.ID {
		t.Fatalf("system recovery was retargeted after its parent was claimed: resolved=%+v wait=%v err=%v", resolvedSystem, systemWait, systemErr)
	}
	if err := store.FinishRun(ctx, "default", child.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	resolved, wait, err = store.ResolveQueuedContinuation(ctx, queued)
	if err != nil || wait || resolved.ReplyToRunID != child.ID || resolved.TaskID != task.ID || resolved.Content != queued.Content {
		t.Fatalf("unresolved child: resolved=%+v wait=%v err=%v", resolved, wait, err)
	}
	grandchild, err := store.StartRunWithOptions(ctx, task, "cli-a", "use additional requirement", StartRunOptions{ResumesRunID: resolved.ReplyToRunID})
	if err != nil || grandchild.ResumesRunID != child.ID {
		t.Fatalf("reply could not claim current exact parent: child=%+v err=%v", grandchild, err)
	}
	if err := store.FinishRun(ctx, "default", grandchild.ID, "done"); err != nil {
		t.Fatal(err)
	}
	resolved, wait, err = store.ResolveQueuedContinuation(ctx, queued)
	if err != nil || wait || resolved.ReplyToRunID != "" || resolved.TaskID != task.ID || resolved.Content != queued.Content {
		t.Fatalf("settled work: resolved=%+v wait=%v err=%v", resolved, wait, err)
	}
}

func TestQueuedReplyRejectsCrossPersonLineage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", Title: "work A", Channel: "cli-a"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "cli-a", "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.ResolveQueuedContinuation(ctx, QueuedTask{TenantID: "default", PersonID: "other", TaskID: task.ID, ReplyToRunID: parent.ID})
	if err == nil {
		t.Fatal("other person acquired exact queued lineage")
	}
}
