package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestGatewayShutdownInterruptsAndRequeuesInsteadOfCancelling(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finalize release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "finalize release")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Platform: "cli", PlatformUserID: "gnfy", Channel: "cli", Content: "finalize release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, queued.ID, control.QueueStatusStarted); err != nil {
		t.Fatal(err)
	}

	runCtx, interrupt := context.WithCancelCause(context.Background())
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, QueueID: queued.ID, Channel: "cli", StartedAt: time.Now(),
		Cancel: func() { interrupt(context.Canceled) }, Interrupt: interrupt,
	}) {
		t.Fatal("active run registration failed")
	}

	daemon.coordinator().stopAllActive("gateway shutdown")
	if !errors.Is(context.Cause(runCtx), errGatewayShutdown) {
		t.Fatalf("cancel cause = %v; want gateway shutdown", context.Cause(runCtx))
	}
	gotQueue, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || gotQueue == nil || gotQueue.Status != control.QueueStatusQueued {
		t.Fatalf("queue = %+v, %v; want queued", gotQueue, err)
	}
	gotRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || gotRun == nil || gotRun.Status != "interrupted" {
		t.Fatalf("run = %+v, %v; want interrupted", gotRun, err)
	}
	gotTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || gotTask == nil || gotTask.Status != "interrupted" || !strings.Contains(strings.ToLower(gotTask.CurrentSummary), "gateway shutdown") {
		t.Fatalf("task = %+v, %v; want resumable gateway interruption", gotTask, err)
	}
	requested, err := store.RunCancelRequested(ctx, identity.TenantID, run.ID)
	if err != nil || requested {
		t.Fatalf("cancel_requested = %v, %v; infrastructure restart is not user cancellation", requested, err)
	}
}
