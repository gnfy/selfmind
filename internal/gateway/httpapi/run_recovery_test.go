package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
)

// TestStuckRunSweeperRecoversStaleRunsButSkipsActive drives the periodic sweep
// with a short injected interval: a heartbeat-stale run with no registry entry
// must flip to interrupted together with its task, while a run registered in
// the active-run registry must never be touched regardless of heartbeat age.
func TestStuckRunSweeperRecoversStaleRunsButSkipsActive(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	deadIdentity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "dead", "Dead Session")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	liveIdentity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "live", "Live Session")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	newTaskRun := func(identity *control.IdentityContext, title string) (*control.Task, *control.Run) {
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
		})
		if err != nil {
			t.Fatalf("task: %v", err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return task, run
	}
	deadTask, _ := newTaskRun(deadIdentity, "orphaned by crash")
	liveTask, liveRun := newTaskRun(liveIdentity, "still executing")

	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if ok := daemon.coordinator().beginActive(liveIdentity.PersonID, &activeRun{
		TenantID:  liveIdentity.TenantID,
		PersonID:  liveIdentity.PersonID,
		TaskID:    liveTask.ID,
		RunID:     liveRun.ID,
		Channel:   "cli",
		StartedAt: time.Now(),
	}); !ok {
		t.Fatal("could not register active run")
	}

	// Negative threshold makes every non-excluded running run "stale", so the
	// test exercises the registry exclusion rather than waiting minutes.
	stop := daemon.startStuckRunSweeper(context.Background(), 10*time.Millisecond, -time.Second)
	defer stop()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := store.GetTask(ctx, deadIdentity.TenantID, deadTask.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status == "interrupted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweeper did not recover the stale run's task, status = %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Several sweep ticks have run by now; the registered run must be intact.
	left, err := store.ListRunningRuns(ctx, liveIdentity.TenantID, []string{liveIdentity.PersonID})
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	if len(left) != 1 || left[0].ID != liveRun.ID {
		t.Fatalf("registered run must stay running, got %+v", left)
	}
	gotLive, err := store.GetTask(ctx, liveIdentity.TenantID, liveTask.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if gotLive.Status != "running" || gotLive.ActiveRunID != liveRun.ID {
		t.Fatalf("registered run's task must be untouched, got status %q active_run_id %q", gotLive.Status, gotLive.ActiveRunID)
	}
}

// TestResolveContinueTaskOffersInterruptedTask pins the resumability contract
// for recovered tasks: 'interrupted' (and the between-turns 'in_progress') are
// non-terminal, so `继续` / `/resume` keep offering them after recovery.
func TestResolveContinueTaskOffersInterruptedTask(t *testing.T) {
	for _, status := range []string{"interrupted", "in_progress"} {
		if terminalTaskStatus(status) {
			t.Fatalf("%q must be non-terminal so recovered tasks stay resumable", status)
		}
	}

	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "recovered work", Channel: "cli",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "interrupted", "Interrupted by gateway restart.", nil); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	daemon := &Server{Control: store, DefaultTenantID: "default"}

	// Current-task pointer path (the pointer still targets the task).
	got, err := daemon.resolveContinueTask(ctx, identity)
	if err != nil {
		t.Fatalf("resolveContinueTask: %v", err)
	}
	if got == nil || got.ID != task.ID {
		t.Fatalf("interrupted task not offered via current pointer, got %+v", got)
	}

	// List-scan path (pointer dangling): the interrupted task must still be
	// picked as the most recent non-terminal task.
	if err := store.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, "task_missing"); err != nil {
		t.Fatalf("SetCurrentTask: %v", err)
	}
	got, err = daemon.resolveContinueTask(ctx, identity)
	if err != nil {
		t.Fatalf("resolveContinueTask: %v", err)
	}
	if got == nil || got.ID != task.ID {
		t.Fatalf("interrupted task not offered via list scan, got %+v", got)
	}
}
