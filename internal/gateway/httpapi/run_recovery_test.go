package httpapi

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

// TestStuckRunSweeperRecoversStaleRunsButSkipsActive drives the periodic sweep
// with a short injected interval: a heartbeat-stale run with no registry entry
// must flip to interrupted together with its task, while a run registered in
// the active-run registry must never be touched regardless of heartbeat age.
func TestStuckRunSweeperRecoversStaleRunsButSkipsActive(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)

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
		// Real work was in flight: a durable plan is the evidence that keeps a
		// sweeper interruption visible as resumable Attention.
		if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, title, []control.RunPlanStepInput{{Step: title + " (start)", Status: "completed"}, {Step: title, Status: "in_progress"}}); err != nil {
			t.Fatalf("plan: %v", err)
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

// TestContinuationLadderOffersInterruptedRun pins the resumability contract
// for recovered work: 'interrupted' (and the between-turns 'in_progress') are
// non-terminal, so `继续` / `/resume` keep offering the parked run after
// recovery — via the person-wide run ladder, not any task pointer.
func TestContinuationLadderOffersInterruptedRun(t *testing.T) {
	for _, status := range []string{"interrupted", "in_progress"} {
		if terminalTaskStatus(status) {
			t.Fatalf("%q must be non-terminal so recovered tasks stay resumable", status)
		}
	}

	ctx := context.Background()
	store := controltest.NewStore(t)
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
	run, err := store.StartRun(ctx, task, "cli", "interrupted work")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "interrupted", "Interrupted by gateway restart.", nil); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	// The continuation ladder (simplification P2) offers the interrupted RUN
	// person-wide — the current-task pointer no longer matters, dangling or not.
	candidates, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 5)
	if err != nil {
		t.Fatalf("ListUnresolvedRunsForPerson: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != run.ID {
		t.Fatalf("interrupted run not offered to continuation, got %+v", candidates)
	}
}
